// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/resolve"
	cterm "swarmexec/client/internal/term"
)

// One `logs` invocation can follow several sources at once: several targets,
// every replica of a service, or a whole stack. Each source is its own stream
// with its own reconnect and its own format detection — services log
// differently — and they meet only at a shared sink, one whole line at a time,
// each line carrying the name of the source it came from.

// logSource is one container to stream, and what to follow when it is replaced.
type logSource struct {
	label  string // what the prefix says: svc.slot, svc@node, or the raw target
	ep     resolve.Endpoint
	follow resolve.FollowTarget
}

func sourceFromCandidate(c resolve.Candidate) logSource {
	ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
	if c.Slot > 0 {
		return logSource{label: fmt.Sprintf("%s.%d", c.Service, c.Slot), ep: ep,
			follow: resolve.FollowTarget{Service: c.Service, Slot: c.Slot}}
	}
	// A global service has no slots; one task per node is its identity.
	return logSource{label: c.Service + "@" + orDash(c.NodeName), ep: ep,
		follow: resolve.FollowTarget{Service: c.Service, NodeID: c.NodeID}}
}

// logResolver is the part of resolve.Resolver that expanding needs.
type logResolver interface {
	Services(ctx context.Context) ([]resolve.Service, error)
	Candidates(ctx context.Context, service string) ([]resolve.Candidate, error)
	Resolve(ctx context.Context, req resolve.Request) (*resolve.Endpoint, error)
}

// expandLogSources turns targets and an optional stack into sources. A service
// name means every running replica of it; anything else (service.slot, a task
// or container id) is resolved as one container. notes are things the operator
// should hear about but that do not stop the others — a service with nothing
// running is the normal case in a stack with a scaled-down worker.
func expandLogSources(ctx context.Context, r logResolver, targets []string, stack, nodeHint string) ([]logSource, []string, error) {
	var services []string
	if stack != "" {
		svcs, err := r.Services(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, s := range svcs {
			if s.Stack == stack {
				services = append(services, s.Name)
			}
		}
		if len(services) == 0 {
			return nil, nil, fmt.Errorf("no services in stack %q", stack)
		}
		sort.Strings(services)
	}

	var out []logSource
	var notes []string
	seen := map[string]bool{}
	add := func(s logSource) {
		if !seen[s.ep.ContainerID] {
			seen[s.ep.ContainerID] = true
			out = append(out, s)
		}
	}
	addService := func(name string) error {
		cands, err := r.Candidates(ctx, name)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			notes = append(notes, fmt.Sprintf("%s: no running container", name))
		}
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].Slot != cands[j].Slot {
				return cands[i].Slot < cands[j].Slot
			}
			return cands[i].NodeName < cands[j].NodeName
		})
		for _, c := range cands {
			add(sourceFromCandidate(c))
		}
		return nil
	}

	for _, name := range services {
		if err := addService(name); err != nil {
			return nil, nil, err
		}
	}
	for _, t := range targets {
		if isServiceName(ctx, r, t) {
			if err := addService(t); err != nil {
				return nil, nil, err
			}
			continue
		}
		ep, err := r.Resolve(ctx, resolve.Request{Target: t, NodeHint: nodeHint})
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", t, err)
		}
		add(logSource{label: t, ep: *ep, follow: followTargetFromTarget(t)})
	}
	if len(out) == 0 {
		return nil, notes, fmt.Errorf("nothing to stream: %s", strings.Join(notes, "; "))
	}
	return out, notes, nil
}

// isServiceName reports whether t names a service exactly. Candidates errors
// for anything that is not one, which is the cheapest way to ask.
func isServiceName(ctx context.Context, r logResolver, t string) bool {
	_, err := r.Candidates(ctx, t)
	return err == nil
}

// lineSink serialises whole lines from many sources onto one writer.
//
// The stdout and the stderr sink share one lock: both usually end up on the
// same terminal, and with a lock each, one source's stderr line could land
// between another's prefix and its stdout line. Prefix and line also go out as
// ONE write, so not even a reader of the merged stream sees them apart.
type lineSink struct {
	mu *sync.Mutex
	w  io.Writer
}

func newLineSinks(stdout, stderr io.Writer) (*lineSink, *lineSink) {
	mu := &sync.Mutex{}
	return &lineSink{mu: mu, w: stdout}, &lineSink{mu: mu, w: stderr}
}

func (s *lineSink) writeLine(prefix string, line []byte) {
	buf := make([]byte, 0, len(prefix)+len(line))
	buf = append(append(buf, prefix...), line...)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.w.Write(buf)
}

// prefixWriter cuts what one source writes into lines and hands each whole
// line, with its prefix, to the shared sink. A stream chunk can end mid-line
// (and two sources' chunks arrive in any order), so a partial line waits here
// for the rest rather than being interleaved with another source's output.
type prefixWriter struct {
	sink   *lineSink
	prefix string
	mu     sync.Mutex
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		p.sink.writeLine(p.prefix, p.buf[:i+1])
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}

// Flush emits a trailing line that never got its newline.
func (p *prefixWriter) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) > 0 {
		p.sink.writeLine(p.prefix, append(p.buf, '\n'))
		p.buf = nil
	}
}

// prefixColors cycles through the six ANSI colours that read on both dark and
// light terminals; the prefix is the only coloured part of a line.
var prefixColors = []string{"36", "33", "32", "35", "34", "31"}

// sourcePrefixes renders "label | " for every source, padded to one width so
// the messages line up, coloured when color is true.
func sourcePrefixes(sources []logSource, color bool) []string {
	width := 0
	for _, s := range sources {
		width = max(width, len(s.label))
	}
	out := make([]string, len(sources))
	for i, s := range sources {
		p := fmt.Sprintf("%-*s | ", width, s.label)
		if color {
			p = "\x1b[" + prefixColors[i%len(prefixColors)] + "m" + fmt.Sprintf("%-*s", width, s.label) + "\x1b[0m | "
		}
		out[i] = p
	}
	return out
}

// colorOutput reports whether prefixes should be coloured: only on a terminal,
// and never when NO_COLOR is set (https://no-color.org).
func colorOutput(f *os.File) bool {
	_, noColor := os.LookupEnv("NO_COLOR")
	return !noColor && cterm.IsTerminal(f.Fd())
}

// logFilterFactory builds one source's filter chain in front of dst, or returns
// dst unchanged when no filtering is asked for.
type logFilterFactory func(dst io.Writer) (io.Writer, func())

// streamManyLogs follows every source concurrently into the shared sinks. A
// source that fails is reported in its own lines and does not stop the others;
// the returned error says how many failed.
func streamManyLogs(ctx context.Context, cfg config.Config, r *resolve.Resolver, sources []logSource, p logsParams, stdout, stderr io.Writer, color bool, wrap logFilterFactory) error {
	outSink, errSink := newLineSinks(stdout, stderr)
	prefixes := sourcePrefixes(sources, color)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed []string
	for i, src := range sources {
		po := &prefixWriter{sink: outSink, prefix: prefixes[i]}
		pe := &prefixWriter{sink: errSink, prefix: prefixes[i]}
		so, flushOut := wrap(po)
		se, flushErr := wrap(pe)
		notify := func(msg string) { errSink.writeLine(prefixes[i], []byte(msg+"\n")) }

		wg.Add(1)
		go func() {
			defer wg.Done()
			err := streamServiceLogs(ctx, cfg, r, src.follow, src.ep, p, so, se, notify)
			flushOut()
			flushErr()
			po.Flush()
			pe.Flush()
			if err != nil {
				notify("── stream ended: " + err.Error() + " ──")
				mu.Lock()
				failed = append(failed, src.label)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(failed) > 0 {
		sort.Strings(failed)
		return fmt.Errorf("%d of %d log streams failed: %s", len(failed), len(sources), strings.Join(failed, ", "))
	}
	return nil
}
