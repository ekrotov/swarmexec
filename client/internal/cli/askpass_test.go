// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseSSHVersion(t *testing.T) {
	for _, c := range []struct {
		in         string
		maj, minor int
	}{
		{"OpenSSH_9.6p1 Ubuntu-3ubuntu13.20, OpenSSL 3.0.13 30 Jan 2024", 9, 6},
		{"OpenSSH_8.4p1, LibreSSL 2.8.3", 8, 4},
		{"OpenSSH_7.4p1, OpenSSL 1.0.2k-fips  26 Jan 2017", 7, 4},
		{"OpenSSH_for_Windows_8.6p1, LibreSSL 3.4.3", 8, 6},
		{"Dropbear v2022.83", 0, 0},
		{"", 0, 0},
	} {
		if maj, mnr := parseSSHVersion(c.in); maj != c.maj || mnr != c.minor {
			t.Errorf("parseSSHVersion(%q) = %d.%d, want %d.%d", c.in, maj, mnr, c.maj, c.minor)
		}
	}
}

func TestAskpassScriptQuotesThePath(t *testing.T) {
	got := askpassScript("/opt/it's here/swarmexec")
	want := "#!/bin/sh\nexec '/opt/it'\\''s here/swarmexec' __askpass \"$@\"\n"
	if got != want {
		t.Errorf("script:\n%s\nwant:\n%s", got, want)
	}
}

func TestAskpassEchoesOnlyTheHostKeyQuestion(t *testing.T) {
	if askpassEchoes("Enter passphrase for key '/home/u/.ssh/id_ed25519': ") {
		t.Error("a passphrase must not be echoed")
	}
	if askpassEchoes("u@host's password: ") {
		t.Error("a password must not be echoed")
	}
	if !askpassEchoes("Are you sure you want to continue connecting (yes/no/[fingerprint])? ") {
		t.Error("the host-key question should be echoed")
	}
}

// newTestAskpass serves a socket answered by p, and points the helper at it.
func newTestAskpass(t *testing.T, p promptFunc) *askpassServer {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("askpass is not used on Windows")
	}
	dir, err := os.MkdirTemp("", "sxap") // short: sun_path
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	s := &askpassServer{ln: ln, dir: dir, inflight: map[string]*askpassCall{}, cancelled: map[int]bool{}}
	go s.serve()
	t.Cleanup(s.close)
	t.Setenv(askpassSockEnv, sock)
	t.Setenv("SSH_ASKPASS_PROMPT", "")
	setAskpassPrompter(p)
	t.Cleanup(func() { setAskpassPrompter(nil) })
	return s
}

func helper(prompt string) (string, int) {
	var out bytes.Buffer
	code := runAskpassHelper([]string{prompt}, &out)
	return out.String(), code
}

func TestAskpassHelperCarriesTheAnswer(t *testing.T) {
	var asked string
	newTestAskpass(t, func(req askpassRequest) (string, bool) {
		asked = req.Prompt
		return "s3cret", true
	})
	out, code := helper("Enter passphrase for key 'k': ")
	if code != 0 || out != "s3cret\n" {
		t.Fatalf("helper = %q, exit %d; want the answer and exit 0", out, code)
	}
	if asked != "Enter passphrase for key 'k': " {
		t.Errorf("prompt arrived as %q", asked)
	}
}

func TestAskpassConfirmIsTheExitStatus(t *testing.T) {
	answer := true
	newTestAskpass(t, func(req askpassRequest) (string, bool) {
		if req.Kind != "confirm" {
			t.Errorf("kind = %q, want confirm", req.Kind)
		}
		return "", answer
	})
	t.Setenv("SSH_ASKPASS_PROMPT", "confirm")
	if out, code := helper("Allow use of key k?"); code != 0 || out != "" {
		t.Errorf("yes: helper = %q, exit %d; want nothing printed, exit 0", out, code)
	}
	answer = false
	if _, code := helper("Allow use of key j?"); code == 0 {
		t.Error("no: helper must exit non-zero")
	}
}

// Several dials start together and each ssh asks the same thing before the
// shared connection exists. One dialog, one answer for all of them.
func TestAskpassSameQuestionIsAskedOnce(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	newTestAskpass(t, func(askpassRequest) (string, bool) {
		calls.Add(1)
		<-release
		return "pw", true
	})
	var wg sync.WaitGroup
	outs := make([]string, 3)
	for i := range outs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], _ = helper("Enter passphrase for key 'k': ")
		}(i)
	}
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // let the others join
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Errorf("operator asked %d times, want 1", n)
	}
	for i, o := range outs {
		if o != "pw\n" {
			t.Errorf("helper %d got %q", i, o)
		}
	}
}

// A cancel is not undone by the next refresh, nor by ssh moving on to its
// next question — only by an explicit reload.
func TestAskpassDeclineSticksUntilReset(t *testing.T) {
	var calls atomic.Int32
	s := newTestAskpass(t, func(askpassRequest) (string, bool) {
		calls.Add(1)
		return "", false
	})
	for _, q := range []string{"Enter passphrase for key 'k': ", "Enter passphrase for key 'k': ", "u@h's password: "} {
		if _, code := helper(q); code == 0 {
			t.Fatalf("after a cancel, %q must fail the helper", q)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("asked %d times before reset, want 1", n)
	}
	s.resetDeclined()
	// A new ssh (the helpers above all share this test's pid as "their ssh").
	s.answer(askpassRequest{Prompt: "Enter passphrase for key 'k': ", SSHPID: os.Getppid() + 1})
	if n := calls.Load(); n != 2 {
		t.Errorf("asked %d times after reset, want 2", n)
	}
}

func TestAskpassHelperWithoutServerDeclines(t *testing.T) {
	t.Setenv(askpassSockEnv, "")
	if _, code := helper("x"); code == 0 {
		t.Error("no socket: helper must exit non-zero")
	}
}

// The probe's budget must not run out while the operator is typing.
func TestPromptAwareTimeoutWaitsForTheOperator(t *testing.T) {
	s := &askpassServer{}
	askpassSrv.Store(s)
	t.Cleanup(func() { askpassSrv.Store(nil) })
	s.pending.Add(1)
	ctx, cancel := promptAwareTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	time.Sleep(100 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("cancelled while a question was waiting")
	}
	s.pending.Add(-1)
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("not cancelled after the question was answered")
	}
}

func TestIsSSHAuthError(t *testing.T) {
	yes := []string{
		"error during connect: ... stderr=git@host: Permission denied (publickey).",
		"read_passphrase: can't open /dev/tty: No such device or address",
		"Received disconnect from 1.2.3.4 port 22:2: Too many authentication failures",
	}
	for _, s := range yes {
		if !isSSHAuthError(errors.New(s)) {
			t.Errorf("not recognised: %q", s)
		}
	}
	for _, s := range []string{"ssh: connect to host x port 22: Connection refused", "context deadline exceeded"} {
		if isSSHAuthError(errors.New(s)) {
			t.Errorf("wrongly recognised: %q", s)
		}
	}
	if isSSHAuthError(nil) {
		t.Error("nil is not an auth error")
	}
}

func TestParseIdentityFiles(t *testing.T) {
	home, _ := os.UserHomeDir()
	out := "user eugen\nhostname h\nidentityfile ~/.ssh/id_ed25519\nidentityfile /keys/deploy\nport 22\n"
	got := parseIdentityFiles(out)
	want := []string{filepath.Join(home, ".ssh/id_ed25519"), "/keys/deploy"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("parseIdentityFiles = %v, want %v", got, want)
	}
}

func TestSplitJumpHop(t *testing.T) {
	for _, c := range []struct{ in, user, port, host string }{
		{"bastion", "", "", "bastion"},
		{"ops@bastion", "ops", "", "bastion"},
		{"ops@bastion:2222", "ops", "2222", "bastion"},
		{"ssh://ops@bastion:2222", "ops", "2222", "bastion"},
	} {
		u, p, h := splitJumpHop(c.in)
		if u != c.user || p != c.port || h != c.host {
			t.Errorf("splitJumpHop(%q) = %q %q %q", c.in, u, p, h)
		}
	}
}

// The ssh whose sign-in was cancelled must not come back after the reload with
// its next question — only a new ssh is asked.
func TestAskpassCancelledSSHStaysCancelledAfterReset(t *testing.T) {
	var calls atomic.Int32
	s := newTestAskpass(t, func(askpassRequest) (string, bool) {
		calls.Add(1)
		return "", false
	})
	if _, ok := s.answer(askpassRequest{Prompt: "passphrase: ", SSHPID: 41}); ok {
		t.Fatal("declined")
	}
	s.resetDeclined()
	if _, ok := s.answer(askpassRequest{Prompt: "password: ", SSHPID: 41}); ok {
		t.Fatal("the cancelled ssh must stay cancelled")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("operator asked %d times, want 1", n)
	}
	s.answer(askpassRequest{Prompt: "passphrase: ", SSHPID: 42})
	if n := calls.Load(); n != 2 {
		t.Errorf("a new ssh after the reset was not asked (calls=%d)", n)
	}
}
