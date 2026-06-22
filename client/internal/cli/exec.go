package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dial"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	cterm "swarmexec/client/internal/term"
	pb "swarmexec/internal/pb"
)

type execFlags struct {
	stdin          bool
	tty            bool
	user           string
	workdir        string
	env            []string
	node           string
	connectTimeout time.Duration
}

func newExecCmd(g *globalFlags) *cobra.Command {
	f := &execFlags{}
	cmd := &cobra.Command{
		Use:   "exec [flags] <service|service.slot|task-id|container-id> [-- <cmd> [args...]]",
		Short: "Exec into a container running anywhere in the swarm",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExec(cmd, g, f, args)
		},
	}
	fl := cmd.Flags()
	fl.BoolVarP(&f.stdin, "stdin", "i", true, "keep stdin open")
	fl.BoolVarP(&f.tty, "tty", "t", false, "allocate a TTY (default: auto — true iff stdin is a terminal and no command)")
	fl.StringVarP(&f.user, "user", "u", "", "username or UID (e.g. 1000:1000)")
	fl.StringVarP(&f.workdir, "workdir", "w", "", "working directory inside the container")
	fl.StringArrayVarP(&f.env, "env", "e", nil, "set environment variables (KEY=VALUE, repeatable)")
	fl.StringVar(&f.node, "node", "", "node hint/override for container-id targets")
	fl.DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to the agent")
	return cmd
}

func runExec(cmd *cobra.Command, g *globalFlags, f *execFlags, args []string) error {
	target := args[0]
	command := args[1:]

	cfg, err := g.resolveConfig(cmd)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return &cliError{code: usageExitCode, err: err}
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// Decide TTY: explicit -t wins; otherwise auto.
	stdinIsTerm := cterm.IsTerminal(os.Stdin.Fd())
	noCommand := len(command) == 0
	tty := decideTTY(cmd.Flags().Changed("tty"), f.tty, stdinIsTerm, noCommand)
	if noCommand {
		command = []string{"/bin/sh"} // default interactive shell (REQUIREMENTS §3)
	}

	// Resolve node + container via the manager.
	dcli, err := newDockerClient(g.dockerContext)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	r := resolve.New(dcli, addrModeOf(cfg))
	ep, err := r.Resolve(ctx, resolve.Request{Target: target, NodeHint: f.node})
	if err != nil {
		if amb, ok := err.(*resolve.AmbiguousError); ok {
			ep, err = pickCandidate(amb, stdinIsTerm)
		}
		if err != nil {
			return &cliError{code: session.TransportFailure, err: err}
		}
	}

	code, runErr := execInto(ctx, cfg, *ep, execParams{
		command:        command,
		tty:            tty,
		env:            f.env,
		workdir:        f.workdir,
		user:           f.user,
		keepStdin:      f.stdin,
		connectTimeout: f.connectTimeout,
	})
	if runErr != nil {
		return &cliError{code: code, err: enrichAgentError(ctx, dcli, runErr)}
	}
	if code != 0 {
		// Remote command exited non-zero: propagate the exact code, no message.
		return &cliError{code: code, silent: true}
	}
	return nil
}

// execParams carries everything execInto needs beyond the dial endpoint.
type execParams struct {
	command        []string
	tty            bool
	env            []string
	workdir        string
	user           string
	keepStdin      bool
	connectTimeout time.Duration
}

// execInto dials the agent at ep, opens the Exec stream, bridges the terminal
// (raw mode + resize when interactive), and returns the remote exit code. It is
// shared by the `exec` command and the interactive `ui`.
func execInto(ctx context.Context, cfg config.Config, ep resolve.Endpoint, p execParams) (int, error) {
	stdinIsTerm := cterm.IsTerminal(os.Stdin.Fd())

	var initW, initH uint32
	if p.tty && stdinIsTerm {
		initW, initH, _ = cterm.Size(os.Stdin.Fd())
	}
	start := &pb.StartExec{
		ContainerId: ep.ContainerID,
		Cmd:         p.command,
		Tty:         p.tty,
		Width:       initW,
		Height:      initH,
		Env:         p.env,
		WorkingDir:  p.workdir,
		User:        p.user,
	}

	// Connect (blocking, with timeout) so bad TLS/unreachable nodes fail fast.
	dctx, dcancel := context.WithTimeout(ctx, p.connectTimeout)
	conn, err := dial.Dial(dctx, ep.DialHost, cfg.Port, cfg)
	dcancel()
	if err != nil {
		return session.TransportFailure, err
	}
	defer conn.Close()

	stream, err := pb.NewAgentClient(conn).Exec(ctx)
	if err != nil {
		return session.TransportFailure, fmt.Errorf("open exec stream: %w", err)
	}

	// Raw-mode bridging only when interactive (TTY on a real terminal).
	interactive := p.tty && stdinIsTerm
	var restorer *cterm.Restorer
	var resizeEvents <-chan os.Signal
	var sizeFn func() (uint32, uint32, error)

	if interactive {
		restorer, err = cterm.MakeRaw(os.Stdin.Fd())
		if err != nil {
			return session.TransportFailure, fmt.Errorf("set raw mode: %w", err)
		}
		defer restorer.Restore()

		// Restore on fatal signals (but NOT SIGINT — in raw mode Ctrl-C bytes go
		// to the remote process; REQUIREMENTS §6). The goroutine exits when this
		// call returns so repeated invocations (the ui) don't leak it.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGHUP)
		defer signal.Stop(sigCh)
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-sigCh:
				restorer.Restore()
				os.Exit(session.TransportFailure)
			case <-done:
			}
		}()

		ev, stop := cterm.NotifyResize()
		defer stop()
		resizeEvents = ev
		sizeFn = func() (uint32, uint32, error) { return cterm.Size(os.Stdin.Fd()) }
	}

	opts := session.Options{
		Start:        start,
		Stdout:       os.Stdout,
		Stderr:       os.Stderr,
		ResizeEvents: resizeEvents,
		SizeFn:       sizeFn,
	}
	if p.keepStdin {
		opts.Stdin = os.Stdin
	}

	code, runErr := session.Run(ctx, stream, opts)
	if restorer != nil {
		restorer.Restore()
	}
	return code, runErr
}

// decideTTY implements the TTY-allocation rule (REQUIREMENTS §3): an explicit
// -t/--tty flag wins; otherwise allocate a TTY iff stdin is a terminal and no
// command was given (interactive shell). With a piped stdin or an explicit
// command, default to non-TTY.
func decideTTY(explicitFlag, flagValue, stdinIsTerm, noCommand bool) bool {
	if explicitFlag {
		return flagValue
	}
	return stdinIsTerm && noCommand
}

// pickCandidate resolves an AmbiguousError: interactively prompt when attached
// to a terminal, otherwise error with the candidate list (REQUIREMENTS §4, §8).
func pickCandidate(amb *resolve.AmbiguousError, interactive bool) (*resolve.Endpoint, error) {
	if !interactive {
		return nil, amb
	}
	fmt.Fprintf(os.Stderr, "service %q has %d running tasks:\n", amb.Service, len(amb.Candidates))
	for i, c := range amb.Candidates {
		fmt.Fprintf(os.Stderr, "  [%d] %s.%d  node=%s  container=%s  up=%s\n",
			i+1, amb.Service, c.Slot, c.NodeName, shortID(c.ContainerID), uptime(c.Uptime))
	}
	fmt.Fprintf(os.Stderr, "select [1-%d]: ", len(amb.Candidates))

	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return nil, fmt.Errorf("no selection made")
	}
	n, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
	if err != nil || n < 1 || n > len(amb.Candidates) {
		return nil, fmt.Errorf("invalid selection")
	}
	c := amb.Candidates[n-1]
	return &resolve.Endpoint{
		DialHost:    c.DialHost,
		ContainerID: c.ContainerID,
		NodeID:      c.NodeID,
		NodeName:    c.NodeName,
	}, nil
}
