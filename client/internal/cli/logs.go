// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/logfmt"
	"swarmexec/client/internal/logstream"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	cterm "swarmexec/client/internal/term"
)

type logsFlags struct {
	follow         bool
	tail           uint32
	timestamps     bool
	since          time.Duration
	node           string
	connectTimeout time.Duration
	logFormat      string // auto | classic | json | logfmt | gelf | raw
	minLevel       string // trace..fatal; "" = no level filter
	grep           string // regexp on the (parsed) message; "" = no text filter
	stack          string // follow every service of this stack
}

func newLogsCmd(g *globalFlags) *cobra.Command {
	f := &logsFlags{}
	cmd := &cobra.Command{
		Use:   "logs [flags] <service|service.slot|task-id|container-id>...",
		Short: "Stream logs of one or more containers, services or a stack from anywhere in the swarm",
		Long: `Stream logs from anywhere in the swarm.

One target that is one container streams exactly as before. Several targets,
a service with several replicas, or --stack follow every source at once: each
line is prefixed with where it came from (svc.slot, svc@node for a global
service), each source reconnects across its own container replacements, and
each has its own format detection.

A service (or service.slot) with no running container — crash-looping,
failing its health check — shows the newest exited container of each slot
instead, so the logs that say why it fails are one command away.

--min-level filters the log output shown here; swarmexec's own logging is the
global --client-log-level.`,
		Example: "  swarmexec logs -f --tail 100 web\n" +
			"  swarmexec logs --grep 'timeout|refused' -f web\n" +
			"  swarmexec logs -f --tail 20 --stack shop",
		// Every target is read: runLogs follows all of them. A target is
		// required unless --stack names what to follow.
		Args: func(cmd *cobra.Command, args []string) error {
			if s, _ := cmd.Flags().GetString("stack"); s != "" {
				return nil
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
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
	fl.StringVar(&f.logFormat, "log-format", "", "parse lines as: auto | classic | json | logfmt | gelf | raw (auto detects it from the lines themselves; default from config, else classic)")
	fl.StringVar(&f.minLevel, "min-level", "", "only show this level and above: trace|debug|info|warn|error|fatal")
	fl.StringVar(&f.grep, "grep", "", "only show lines whose message matches this regexp")
	fl.StringVar(&f.stack, "stack", "", "follow every service of this stack (in addition to any targets)")
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

	dcli, err := dockerEP.connect(ctx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	r := resolve.New(dcli, addrModeOf(cfg))
	ac := agentcall.New(cfg)
	defer ac.Close()

	ep, multi, notes, err := logTargets(ctx, r, args, f)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}

	// Optional format-aware parsing + filtering (flags override config defaults).
	formatName := firstNonEmpty(f.logFormat, cfg.Logs.Format)
	format, filter, ferr := logstream.BuildFilter(
		formatName,
		firstNonEmpty(f.minLevel, cfg.Logs.MinLevel),
		f.grep,
	)
	if ferr != nil {
		return &cliError{code: usageExitCode, err: ferr}
	}
	params := logstream.Params{
		Follow:         f.follow,
		Tail:           f.tail,
		Timestamps:     f.timestamps,
		Since:          f.since,
		ConnectTimeout: f.connectTimeout,
	}

	lf := logFiltering{format: format, filter: filter, auto: logstream.FormatAuto(formatName)}
	if multi != nil {
		for _, n := range notes {
			fmt.Fprintln(os.Stderr, "── "+n+" ──")
		}
		if err := logstream.StreamMany(ctx, logstream.Run{Agents: ac, Resolver: r, Params: params, Stdout: os.Stdout, Stderr: os.Stderr}, multi, colorOutput(os.Stdout), lf.wrap); err != nil {
			return &cliError{code: session.TransportFailure, err: err}
		}
		return nil
	}

	stdout, stderr, flushers := lf.writers()

	// Follow across container replacements (rolling update / restart / reschedule)
	// so `logs -f <service>` keeps streaming after a swap, like `docker service
	// logs -f`. Reconnect notices go straight to the real stderr, bypassing the
	// filter so they are never dropped.
	err = logstream.Follow(ctx, logstream.Run{Agents: ac, Resolver: r, Params: params, Stdout: stdout, Stderr: stderr},
		logstream.FollowTargetOf(args[0]), *ep, func(msg string) { fmt.Fprintln(os.Stderr, msg) })
	for _, w := range flushers {
		w.Flush()
	}
	if err != nil {
		return &cliError{code: session.TransportFailure, err: explainSession(ctx, cfg, dcli, err)}
	}
	return nil
}

// logTargets resolves what `logs` follows. One target that is one container
// keeps the single-stream path (ep) and its unprefixed output. A service with
// several replicas used to stop here as "ambiguous"; it now follows all of
// them (multi), like docker service logs. Several targets or --stack expand to
// sources, with notes on what they leave out.
func logTargets(ctx context.Context, r *resolve.Resolver, args []string, f *logsFlags) (*resolve.Endpoint, []logstream.Source, []string, error) {
	if len(args) != 1 || f.stack != "" {
		multi, notes, err := logstream.ExpandSources(ctx, r, args, f.stack, f.node)
		return nil, multi, notes, err
	}
	ep, err := r.Resolve(ctx, resolve.Request{Target: args[0], NodeHint: f.node})
	var multi []logstream.Source
	if amb, ok := err.(*resolve.AmbiguousError); ok {
		for _, c := range amb.Candidates {
			multi = append(multi, logstream.SourceFromCandidate(c))
		}
		err = nil
	}
	var nr *resolve.NoRunningTaskError
	if errors.As(err, &nr) {
		ep, multi, err = lastExitedSources(ctx, r, nr)
	}
	return ep, multi, nil, err
}

// logFiltering is the format-aware parsing and filtering `logs` applies;
// auto detects the format from the lines themselves.
type logFiltering struct {
	format logfmt.Format
	filter logfmt.Filter
	auto   bool
}

// active reports whether lines go through a filter at all.
func (lf logFiltering) active() bool {
	return lf.auto || logstream.FilteringActive(lf.format, lf.filter)
}

// wrap is StreamMany's per-source filter: each source and each stream detects
// its own format, as services log differently.
func (lf logFiltering) wrap(dst io.Writer) (io.Writer, func()) {
	if !lf.active() {
		return dst, func() {}
	}
	fw := logstream.NewFilterWriter(dst, lf.format, lf.filter, logstream.RenderPlain)
	if lf.auto {
		fw.DetectFormat() // per source and per stream: services log differently
	}
	return fw, fw.Flush
}

// writers are the single stream's stdout and stderr, filtered when filtering
// is active, and the filters to flush at the end.
func (lf logFiltering) writers() (stdout, stderr io.Writer, flushers []*logstream.FilterWriter) {
	if !lf.active() {
		return os.Stdout, os.Stderr, nil
	}
	fo := logstream.NewFilterWriter(os.Stdout, lf.format, lf.filter, logstream.RenderPlain)
	fe := logstream.NewFilterWriter(os.Stderr, lf.format, lf.filter, logstream.RenderPlain)
	if lf.auto {
		// Each stream is sniffed on its own: an app that logs JSON on stdout
		// and a runtime that prints plain panics on stderr is the normal
		// case, not an exotic one.
		fo.DetectFormat()
		fe.DetectFormat()
	}
	return fo, fe, []*logstream.FilterWriter{fo, fe}
}

// lastExitedSources is the fallback for a service with no running container:
// a crash-looping or failing service has nothing to follow, yet the containers
// it last ran still hold the logs that say why. It picks the newest of those per
// slot — one becomes ep, several become multi — and says on stderr that these
// are not live. With none (the task never got a container: image pull failure,
// no suitable node) the task error is the answer.
func lastExitedSources(ctx context.Context, r *resolve.Resolver, nr *resolve.NoRunningTaskError) (*resolve.Endpoint, []logstream.Source, error) {
	cands, reason, err := r.LastExited(ctx, nr.Service, nr.Slot)
	if err != nil {
		return nil, nil, err
	}
	if len(cands) == 0 {
		if reason == "" {
			return nil, nil, nr
		}
		return nil, nil, fmt.Errorf("%w; newest task: %s", nr, reason)
	}
	fmt.Fprintln(os.Stderr, "── "+lastExitedNote(nr.Service, cands)+" ──")
	if len(cands) == 1 {
		c := cands[0]
		return &resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}, nil, nil
	}
	multi := make([]logstream.Source, 0, len(cands))
	for _, c := range cands {
		multi = append(multi, logstream.SourceFromCandidate(c))
	}
	return nil, multi, nil
}

// lastExitedNote tells the operator the logs below come from containers that
// are no longer running, and how the newest of them ended.
func lastExitedNote(service string, cands []resolve.Candidate) string {
	what := "the last container"
	if len(cands) > 1 {
		what = fmt.Sprintf("the last container of each of %d replicas", len(cands))
	}
	newest := cands[0]
	for _, c := range cands[1:] {
		if c.Uptime < newest.Uptime { // Uptime runs from the task's last status change
			newest = c
		}
	}
	end := newest.State
	if newest.Err != "" {
		end += ": " + newest.Err
	}
	return fmt.Sprintf("%s has no running container; showing %s (%s)", service, what, end)
}

// colorOutput reports whether prefixes should be coloured: only on a terminal,
// and never when NO_COLOR is set (https://no-color.org).
func colorOutput(f *os.File) bool {
	_, noColor := os.LookupEnv("NO_COLOR")
	return !noColor && cterm.IsTerminal(f.Fd())
}
