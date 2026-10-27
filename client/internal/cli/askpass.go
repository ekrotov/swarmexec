// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"swarmexec/client/internal/clientlog"
	cterm "swarmexec/client/internal/term"
)

// Every ssh swarmexec starts runs without a terminal: commandconn puts it in a
// session of its own (setsid), so it has no /dev/tty to ask on. A key with a
// passphrase therefore failed with "Permission denied (publickey)", and the
// only way through was to leave, run ssh-add and come back.
//
// OpenSSH has a door for exactly this: SSH_ASKPASS names a program ssh runs
// whenever it needs an answer — a passphrase, a password, "is this host key
// right?" — and reads the answer from its stdout. SSH_ASKPASS_REQUIRE=force
// (OpenSSH 8.4) makes it use that program even without a display. We point it
// at ourselves: a small script that execs this binary as the helper, which
// carries the question over a private socket back to the swarmexec process
// that started the ssh. There the question is asked where the operator is
// looking — in the TUI as a dialog, otherwise on the terminal.
//
// Nothing is remembered. An answer goes to the ssh processes waiting on that
// question at that moment and is gone; the connection sharing in sshmux.go is
// what makes one answer enough for a whole session.

// askpassHelperArg is the argv[1] that turns this binary into the helper. It
// is deliberately not a cobra command: the helper runs once per question,
// inside ssh, and must not set up logging, read the config or print help.
const askpassHelperArg = "__askpass"

// askpassSockEnv carries the socket path from the swarmexec process, through
// ssh, to the helper.
const askpassSockEnv = "SWARMEXEC_ASKPASS_SOCK"

// askpassEnv switches the whole mechanism off (0/off/false/no), for an
// operator who wants ssh's own behaviour — or their own SSH_ASKPASS.
const askpassEnv = "SWARMEXEC_SSH_ASKPASS"

// askpassRequest is one question from ssh. Kind is ssh's SSH_ASKPASS_PROMPT:
// "" wants a typed answer, "confirm" a yes/no (the exit status), "none" is a
// notice that needs no answer (e.g. "touch your security key").
type askpassRequest struct {
	Prompt string `json:"prompt"`
	Kind   string `json:"kind,omitempty"`
	SSHPID int    `json:"ssh_pid,omitempty"` // the ssh that asks (the helper's parent)
}

type askpassResponse struct {
	OK     bool   `json:"ok"`
	Answer string `json:"answer,omitempty"`
}

// promptFunc asks the operator one question. ok=false means they declined.
type promptFunc func(req askpassRequest) (answer string, ok bool)

// runAskpassHelper is the helper side: forward the question, print the answer.
// Exit 0 with the answer on stdout is "here it is"; anything else tells ssh
// the operator declined.
func runAskpassHelper(args []string, out io.Writer) int {
	sock := os.Getenv(askpassSockEnv)
	if sock == "" {
		return 1
	}
	req := askpassRequest{Prompt: strings.Join(args, " "), Kind: os.Getenv("SSH_ASKPASS_PROMPT"), SSHPID: os.Getppid()}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return 1
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return 1
	}
	var resp askpassResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil || !resp.OK {
		return 1
	}
	if req.Kind == "" {
		fmt.Fprintln(out, resp.Answer)
	}
	return 0
}

// askpassServer answers the helpers of one swarmexec process.
type askpassServer struct {
	ln  net.Listener
	dir string

	mu        sync.Mutex
	inflight  map[string]*askpassCall // questions on screen right now
	declined  bool                    // the operator cancelled; ask nothing
	cancelled map[int]bool            // ssh processes whose sign-in was cancelled
	pending   atomic.Int32            // questions waiting for the operator

	ask sync.Mutex // one question on screen at a time
}

type askpassCall struct {
	done   chan struct{}
	answer string
	ok     bool
}

// answer asks the operator — unless the same question is already on screen,
// in which case it waits for that answer instead. Several dials start at once
// (the API, the agent tunnels); before the shared connection exists each of
// them asks for the same passphrase, and typing it three times is the
// experience this replaces.
//
// The question also outlives the ssh that asked it. A dial that times out
// while the operator is still typing takes its ssh with it, but the dialog
// stays: the next attempt asks the identical question and receives what was
// typed, instead of a fresh, empty prompt.
//
// A cancel stands until resetDeclined, and for every question, not just the
// one cancelled: the TUI refreshes every ten seconds, and ssh follows a
// declined passphrase with the next method ("password:"), so a cancel that
// only covered its own question brought a dialog straight back. Cancel means
// "not now". An explicit reload or a cluster switch asks again — but only new
// ssh processes: one whose sign-in was cancelled stays cancelled, or it comes
// back right after the reload with its next method's question ("password:")
// ahead of the passphrase the operator is about to type.
func (s *askpassServer) answer(req askpassRequest) (string, bool) {
	key := req.Kind + "\x00" + req.Prompt
	s.mu.Lock()
	if s.declined || (req.SSHPID > 0 && s.cancelled[req.SSHPID]) {
		s.mu.Unlock()
		return "", false
	}
	if c := s.inflight[key]; c != nil {
		s.mu.Unlock()
		<-c.done
		return c.answer, c.ok
	}
	c := &askpassCall{done: make(chan struct{})}
	s.inflight[key] = c
	s.pending.Add(1)
	s.mu.Unlock()

	s.ask.Lock()
	c.answer, c.ok = currentPrompter()(req)
	s.ask.Unlock()

	s.mu.Lock()
	delete(s.inflight, key)
	if !c.ok && req.Kind != "none" {
		s.declined = true
		if req.SSHPID > 0 {
			s.cancelled[req.SSHPID] = true
		}
	}
	s.pending.Add(-1)
	s.mu.Unlock()
	close(c.done)
	return c.answer, c.ok
}

func (s *askpassServer) resetDeclined() {
	clientlog.L().Debug("ssh questions may be asked again")
	s.mu.Lock()
	s.declined = false
	s.mu.Unlock()
}

func (s *askpassServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // closed
		}
		go s.handle(conn)
	}
}

func (s *askpassServer) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var req askpassRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	clientlog.L().Info("ssh asks", "prompt", req.Prompt, "kind", req.Kind)
	answer, ok := s.answer(req)
	clientlog.L().Debug("ssh answered", "prompt", req.Prompt, "ok", ok)
	_ = json.NewEncoder(conn).Encode(askpassResponse{OK: ok, Answer: answer})
}

func (s *askpassServer) close() {
	_ = s.ln.Close()
	_ = os.RemoveAll(s.dir)
}

var (
	askpassOnce sync.Once
	askpassSrv  atomic.Pointer[askpassServer]
	prompter    atomic.Pointer[promptFunc]
)

// setAskpassPrompter routes questions to p; nil routes them to the terminal.
// The TUI sets itself while it owns the screen.
func setAskpassPrompter(p promptFunc) {
	if p == nil {
		prompter.Store(nil)
		return
	}
	prompter.Store(&p)
}

func currentPrompter() promptFunc {
	if p := prompter.Load(); p != nil {
		return *p
	}
	return terminalPrompt
}

// askpassActive reports whether ssh asks through swarmexec in this process.
func askpassActive() bool { return askpassSrv.Load() != nil }

// askpassPending reports whether a question is waiting for the operator.
func askpassPending() bool {
	s := askpassSrv.Load()
	return s != nil && s.pending.Load() > 0
}

// resetAskpassDeclines lets questions the operator declined be asked again.
func resetAskpassDeclines() {
	if s := askpassSrv.Load(); s != nil {
		s.resetDeclined()
	}
}

// prepareSSHPrompting sets SSH_ASKPASS up before the first ssh starts. Called
// where ssh is about to be used, not at startup: a command that never touches
// an ssh:// endpoint should not create a socket or run `ssh -V`.
func prepareSSHPrompting() {
	if sshNonInteractive.Load() {
		return // shell completion: never ask
	}
	askpassOnce.Do(func() {
		if why := askpassUnavailable(); why != "" {
			clientlog.L().Debug("ssh askpass off", "why", why)
			return
		}
		s, err := startAskpass()
		if err != nil {
			clientlog.L().Warn("ssh askpass unavailable", "err", err.Error())
			return
		}
		askpassSrv.Store(s)
	})
}

// stopAskpass removes the socket and the script. Deferred by Execute.
func stopAskpass() {
	if s := askpassSrv.Swap(nil); s != nil {
		s.close()
	}
}

// askpassUnavailable says why askpass is not used here, or "" if it is.
func askpassUnavailable() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(askpassEnv))) {
	case "0", "off", "false", "no":
		return askpassEnv + " is off"
	}
	if runtime.GOOS == "windows" {
		return "not on Windows"
	}
	// An operator who chose their own askpass policy keeps it.
	if os.Getenv("SSH_ASKPASS_REQUIRE") != "" {
		return "SSH_ASKPASS_REQUIRE is set by the environment"
	}
	if !sshAtLeast(8, 4) {
		return "OpenSSH older than 8.4 (" + sshVersionString() + ")"
	}
	return ""
}

// askpassSockLimit keeps the socket path inside sun_path (see sshMuxPathLimit).
const askpassSockLimit = 100

func startAskpass() (*askpassServer, error) {
	base := sshMuxDir() // per-user, 0700: nobody else can reach the socket
	if base == "" {
		return nil, fmt.Errorf("no private runtime directory")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, fmt.Sprintf("askpass-%d", os.Getpid()))
	_ = os.RemoveAll(dir) // a previous process with our pid
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "s")
	if len(sock) > askpassSockLimit {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("socket path too long: %s", sock)
	}
	script := filepath.Join(dir, "askpass")
	if err := os.WriteFile(script, []byte(askpassScript(exe)), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	_ = os.Chmod(sock, 0o600)
	s := &askpassServer{ln: ln, dir: dir, inflight: map[string]*askpassCall{}, cancelled: map[int]bool{}}
	go s.serve()
	// The process environment, because commandconn starts ssh with it and
	// offers no way to pass a per-command one.
	_ = os.Setenv("SSH_ASKPASS", script)
	_ = os.Setenv("SSH_ASKPASS_REQUIRE", "force")
	_ = os.Setenv(askpassSockEnv, sock)
	return s, nil
}

// askpassScript is what SSH_ASKPASS points at. ssh runs SSH_ASKPASS with the
// prompt as its only argument and cannot pass one of its own, hence a script
// rather than the binary itself.
func askpassScript(exe string) string {
	return "#!/bin/sh\nexec " + shQuote(exe) + " " + askpassHelperArg + " \"$@\"\n"
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// askpassEchoes reports whether the answer may be shown while typed. ssh asks
// the host-key question ("yes/no/[fingerprint]") through the same channel as
// a passphrase; that one is not a secret, and typing a fingerprint blind is
// hopeless.
func askpassEchoes(prompt string) bool {
	return strings.Contains(prompt, "(yes/no")
}

var ttyAsk sync.Mutex

// terminalPrompt asks on the controlling terminal — the CLI's way, and the
// TUI's before it has taken the screen.
func terminalPrompt(req askpassRequest) (string, bool) {
	ttyAsk.Lock()
	defer ttyAsk.Unlock()
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", false
	}
	defer tty.Close()
	prompt := strings.TrimRight(req.Prompt, " \n")
	switch req.Kind {
	case "none":
		fmt.Fprintf(tty, "ssh: %s\n", prompt)
		return "", true
	case "confirm":
		fmt.Fprintf(tty, "ssh: %s [y/N] ", prompt)
		line, _ := bufio.NewReader(tty).ReadString('\n')
		a := strings.ToLower(strings.TrimSpace(line))
		return "", a == "y" || a == "yes"
	}
	fmt.Fprintf(tty, "ssh: %s ", prompt)
	if askpassEchoes(prompt) {
		line, err := bufio.NewReader(tty).ReadString('\n')
		if err != nil {
			return "", false
		}
		return strings.TrimRight(line, "\r\n"), true
	}
	b, err := cterm.ReadPassword(tty.Fd())
	fmt.Fprintln(tty)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// promptAwareTimeout is context.WithTimeout that does not count the time the
// operator spends answering ssh. A reachability probe with a 15-second budget
// would otherwise give up while the passphrase is half typed.
func promptAwareTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		t := time.NewTimer(d)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !askpassPending() {
					cancel()
					return
				}
				t.Reset(time.Second) // check again once the question is answered
			}
		}
	}()
	return ctx, cancel
}

var (
	sshVerOnce  sync.Once
	sshVerMajor int
	sshVerMinor int
	sshVerRaw   string
)

var openSSHVersion = regexp.MustCompile(`OpenSSH[_A-Za-z]*_(\d+)\.(\d+)`)

func loadSSHVersion() {
	sshVerOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(ctx, "ssh", "-V").CombinedOutput() // ssh -V prints to stderr
		sshVerRaw = strings.TrimSpace(string(out))
		sshVerMajor, sshVerMinor = parseSSHVersion(sshVerRaw)
	})
}

// parseSSHVersion reads "OpenSSH_9.6p1 Ubuntu-3ubuntu13…" → 9, 6; anything
// that is not OpenSSH → 0, 0.
func parseSSHVersion(s string) (int, int) {
	m := openSSHVersion.FindStringSubmatch(s)
	if m == nil {
		return 0, 0
	}
	maj, _ := strconv.Atoi(m[1])
	mnr, _ := strconv.Atoi(m[2])
	return maj, mnr
}

func sshAtLeast(major, minor int) bool {
	loadSSHVersion()
	return sshVerMajor > major || (sshVerMajor == major && sshVerMinor >= minor)
}

func sshVersionString() string {
	loadSSHVersion()
	if sshVerRaw == "" {
		return "ssh not found"
	}
	if i := strings.IndexAny(sshVerRaw, ", "); i > 0 {
		return sshVerRaw[:i]
	}
	return sshVerRaw
}
