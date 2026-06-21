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
	dcli, err := newDockerClient()
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

	// Initial terminal size only matters with a TTY on a real terminal.
	var initW, initH uint32
	if tty && stdinIsTerm {
		initW, initH, _ = cterm.Size(os.Stdin.Fd())
	}

	start := &pb.StartExec{
		ContainerId: ep.ContainerID,
		Cmd:         command,
		Tty:         tty,
		Width:       initW,
		Height:      initH,
		Env:         f.env,
		WorkingDir:  f.workdir,
		User:        f.user,
	}

	// Connect (blocking, with timeout) so bad TLS/unreachable nodes fail fast.
	dctx, dcancel := context.WithTimeout(ctx, f.connectTimeout)
	conn, err := dial.Dial(dctx, ep.DialHost, cfg.Port, cfg)
	dcancel()
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	defer conn.Close()

	stream, err := pb.NewAgentClient(conn).Exec(ctx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("open exec stream: %w", err)}
	}

	// Raw-mode bridging only when interactive (TTY on a real terminal).
	interactive := tty && stdinIsTerm
	var restorer *cterm.Restorer
	var resizeEvents <-chan os.Signal
	var sizeFn func() (uint32, uint32, error)

	if interactive {
		restorer, err = cterm.MakeRaw(os.Stdin.Fd())
		if err != nil {
			return &cliError{code: session.TransportFailure, err: fmt.Errorf("set raw mode: %w", err)}
		}
		// Restore on every exit path: normal return, error, panic.
		defer restorer.Restore()

		// Restore on fatal signals (but NOT SIGINT — in raw mode Ctrl-C bytes
		// go to the remote process; REQUIREMENTS §6).
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGHUP)
		go func() {
			<-sigCh
			restorer.Restore()
			os.Exit(session.TransportFailure)
		}()

		ev, stop := cterm.NotifyResize()
		defer stop()
		resizeEvents = ev
		sizeFn = func() (uint32, uint32, error) { return cterm.Size(os.Stdin.Fd()) }
	}

	var stdinR = os.Stdin
	opts := session.Options{
		Start:        start,
		Stdout:       os.Stdout,
		Stderr:       os.Stderr,
		ResizeEvents: resizeEvents,
		SizeFn:       sizeFn,
	}
	if f.stdin {
		opts.Stdin = stdinR
	}

	code, runErr := session.Run(ctx, stream, opts)

	// Restore the terminal explicitly BEFORE returning so it is cooked again
	// even though main calls os.Exit (which skips defers). The Restorer is
	// idempotent, so the deferred call above is harmless.
	restorer.Restore()

	if runErr != nil {
		return &cliError{code: code, err: runErr}
	}
	if code != 0 {
		// Remote command exited non-zero: propagate the exact code, no message.
		return &cliError{code: code, silent: true}
	}
	return nil
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
