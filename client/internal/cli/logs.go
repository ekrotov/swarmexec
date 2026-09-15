// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dial"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	pb "swarmexec/internal/pb"
)

const (
	// logReconnectDelay is how long to wait between attempts to find the
	// replacement container after a followed log stream ends.
	logReconnectDelay = time.Second
	// logReconnectWindow bounds how long to keep waiting for a replacement before
	// giving up (a rolling update normally schedules the new task within seconds).
	logReconnectWindow = 30 * time.Second
)

type logsFlags struct {
	follow         bool
	tail           uint32
	timestamps     bool
	since          time.Duration
	node           string
	connectTimeout time.Duration
	logFormat      string // classic | json | logfmt | gelf | raw
	minLevel       string // trace..fatal; "" = no level filter
	grep           string // regexp on the (parsed) message; "" = no text filter
}

func newLogsCmd(g *globalFlags) *cobra.Command {
	f := &logsFlags{}
	cmd := &cobra.Command{
		Use:   "logs [flags] <service|service.slot|task-id|container-id>",
		Short: "Stream a container's logs from anywhere in the swarm",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd, g, f, args)
		},
	}
	fl := cmd.Flags()
	fl.BoolVarP(&f.follow, "follow", "f", false, "keep streaming new log lines")
	fl.Uint32Var(&f.tail, "tail", 0, "number of lines from the end to start with (0 = all)")
	fl.BoolVarP(&f.timestamps, "timestamps", "t", false, "prefix each line with a timestamp")
	fl.DurationVar(&f.since, "since", 0, "only logs newer than this (e.g. 10m, 1h; 0 = no limit)")
	fl.StringVar(&f.node, "node", "", "node hint/override for container-id targets")
	fl.DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to the agent")
	fl.StringVar(&f.logFormat, "log-format", "", "parse lines as: classic | json | logfmt | gelf | raw (default from config, else classic)")
	fl.StringVar(&f.minLevel, "min-level", "", "only show this level and above: trace|debug|info|warn|error|fatal")
	fl.StringVar(&f.grep, "grep", "", "only show lines whose message matches this regexp")
	return cmd
}

func runLogs(cmd *cobra.Command, g *globalFlags, f *logsFlags, args []string) error {
	dockerEP := resolveEndpoint(g.dockerContext)
	cfg, err := g.resolveConfig(cmd, dockerEP)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return &cliError{code: usageExitCode, err: err}
	}

	ctx := cmdContext(cmd)

	dcli, err := dockerEP.client()
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	r := resolve.New(dcli, addrModeOf(cfg))
	ep, err := r.Resolve(ctx, resolve.Request{Target: args[0], NodeHint: f.node})
	if err != nil {
		if amb, ok := err.(*resolve.AmbiguousError); ok {
			ep, err = pickCandidate(amb, false)
		}
		if err != nil {
			return &cliError{code: session.TransportFailure, err: err}
		}
	}

	// Optional format-aware parsing + filtering (flags override config defaults).
	format, filter, ferr := buildLogFilter(
		firstNonEmpty(f.logFormat, cfg.Logs.Format),
		firstNonEmpty(f.minLevel, cfg.Logs.MinLevel),
		f.grep,
	)
	if ferr != nil {
		return &cliError{code: usageExitCode, err: ferr}
	}
	var stdout, stderr io.Writer = os.Stdout, os.Stderr
	var flushers []*filterWriter
	if filteringActive(format, filter) {
		fo := newFilterWriter(os.Stdout, format, filter, renderPlain)
		fe := newFilterWriter(os.Stderr, format, filter, renderPlain)
		stdout, stderr, flushers = fo, fe, []*filterWriter{fo, fe}
	}

	// Follow across container replacements (rolling update / restart / reschedule)
	// so `logs -f <service>` keeps streaming after a swap, like `docker service
	// logs -f`. Reconnect notices go straight to the real stderr, bypassing the
	// filter so they are never dropped.
	err = streamServiceLogs(ctx, cfg, r, followTargetFromTarget(args[0]), *ep, logsParams{
		follow:         f.follow,
		tail:           f.tail,
		timestamps:     f.timestamps,
		since:          f.since,
		connectTimeout: f.connectTimeout,
	}, stdout, stderr, func(msg string) { fmt.Fprintln(os.Stderr, msg) })
	for _, w := range flushers {
		w.Flush()
	}
	if err != nil {
		return &cliError{code: session.TransportFailure, err: enrichAgentError(ctx, dcli, err)}
	}
	return nil
}

// followTargetFromTarget derives the logical replica to keep following from the
// raw target argument: "service.slot" pins the slot, a bare name is treated as a
// service. A task/container id also lands here as a Service name that will not
// resolve, so Successor reports the target gone and following simply stops — the
// same outcome as before, just with a friendly notice.
func followTargetFromTarget(target string) resolve.FollowTarget {
	target = strings.TrimSpace(target)
	if i := strings.LastIndex(target, "."); i > 0 {
		if slot, err := strconv.Atoi(target[i+1:]); err == nil {
			return resolve.FollowTarget{Service: target[:i], Slot: slot}
		}
	}
	return resolve.FollowTarget{Service: target}
}

// logsParams carries the Logs request options.
type logsParams struct {
	follow         bool
	tail           uint32
	timestamps     bool
	since          time.Duration
	connectTimeout time.Duration
}

// streamLogs dials the agent and streams the container's logs into stdout/stderr.
// It is shared by the `logs` command and the interactive `ui`. Returns nil when
// the stream ends or ctx is cancelled.
func streamLogs(ctx context.Context, cfg config.Config, ep resolve.Endpoint, p logsParams, stdout, stderr io.Writer) error {
	dctx, dcancel := context.WithTimeout(ctx, p.connectTimeout)
	conn, err := dial.Dial(dctx, ep.DialHost, cfg.Port, cfg)
	dcancel()
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := pb.NewAgentClient(conn).Logs(ctx, &pb.LogsRequest{
		ContainerId:  ep.ContainerID,
		Follow:       p.follow,
		Tail:         p.tail,
		Timestamps:   p.timestamps,
		SinceSeconds: uint32(p.since.Seconds()),
	})
	if err != nil {
		return fmt.Errorf("open logs stream: %w", err)
	}

	for {
		chunk, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled || errors.Is(err, context.Canceled) {
				return nil
			}
			if st, ok := status.FromError(err); ok {
				return fmt.Errorf("%s: %s", st.Code(), st.Message())
			}
			return err
		}
		switch pl := chunk.Payload.(type) {
		case *pb.LogChunk_Stdout:
			_, _ = stdout.Write(pl.Stdout)
		case *pb.LogChunk_Stderr:
			_, _ = stderr.Write(pl.Stderr)
		case *pb.LogChunk_Error:
			return fmt.Errorf("agent: %s", pl.Error)
		}
	}
}

// streamServiceLogs follows a target's logs across container replacements. It
// streams the current container; when the stream ends cleanly while following
// (the container was replaced by a rolling update, restart or reschedule), it
// re-resolves the target's running container and reconnects, emitting a notice
// via notify. It returns when: follow is false and the stream ends; ctx is
// cancelled; a real transport/agent error occurs; or the target has no running
// container left. notify may be nil.
func streamServiceLogs(ctx context.Context, cfg config.Config, r *resolve.Resolver, t resolve.FollowTarget, ep resolve.Endpoint, p logsParams, stdout, stderr io.Writer, notify func(string)) error {
	lastCID := ep.ContainerID
	params := p
	for {
		if err := streamLogs(ctx, cfg, ep, params, stdout, stderr); err != nil {
			return err
		}
		if !p.follow || ctx.Err() != nil || t.Service == "" {
			return nil
		}

		// The container ended while following. Wait for its successor — during a
		// rolling update the replacement may still be scheduling.
		c, ok, err := waitForSuccessor(ctx, r, t, notify)
		if !ok {
			return err
		}
		ep = resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		params = p
		if c.ContainerID == lastCID {
			params.tail = 0 // same container still up (spurious end): resume, don't re-dump the tail
		} else if notify != nil {
			notify(fmt.Sprintf("── container replaced; reconnected to %s on %s ──", shortID(c.ContainerID), orDash(c.NodeName)))
		}
		lastCID = c.ContainerID
	}
}

// waitForSuccessor polls for the FollowTarget's replacement container until one
// is running, the service is gone, the wait window elapses, or ctx is cancelled.
// It returns ok=false (with a nil error on a clean stop) when following should
// end; err is non-nil only for an unrecovered resolve error.
func waitForSuccessor(ctx context.Context, r *resolve.Resolver, t resolve.FollowTarget, notify func(string)) (resolve.Candidate, bool, error) {
	var lastErr error
	for waited := time.Duration(0); ; waited += logReconnectDelay {
		c, ok, err := r.Successor(ctx, t)
		switch {
		case ok:
			return c, true, nil
		case errors.Is(err, resolve.ErrTargetGone):
			if notify != nil {
				notify("── target has no running container left; stopping ──")
			}
			return resolve.Candidate{}, false, nil
		case ctx.Err() != nil:
			return resolve.Candidate{}, false, nil
		}
		lastErr = err // nil while the replacement is merely still scheduling
		if waited >= logReconnectWindow {
			if notify != nil {
				notify("── no replacement container appeared; stopping ──")
			}
			return resolve.Candidate{}, false, lastErr
		}
		select {
		case <-ctx.Done():
			return resolve.Candidate{}, false, nil
		case <-time.After(logReconnectDelay):
		}
	}
}
