// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package logstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"swarmexec/client/internal/resolve"
)

// One `logs` invocation can follow several sources at once: several targets,
// every replica of a service, or a whole stack. Each source is its own stream
// with its own reconnect and its own format detection — services log
// differently — and they meet only at a shared sink, one whole line at a time,
// each line carrying the name of the source it came from.

// Source is one container to stream, and what to follow when it is replaced.
type Source struct {
	Label    string // what the prefix says: svc.slot, svc@node, or the raw target
	Endpoint resolve.Endpoint
	Target   resolve.FollowTarget
}

func SourceFromCandidate(c resolve.Candidate) Source {
	ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
	if c.Slot > 0 {
		return Source{Label: fmt.Sprintf("%s.%d", c.Service, c.Slot), Endpoint: ep,
			Target: resolve.FollowTarget{Service: c.Service, Slot: c.Slot}}
	}
	// A global service has no slots; one task per node is its identity.
	return Source{Label: c.Service + "@" + orDash(c.NodeName), Endpoint: ep,
		Target: resolve.FollowTarget{Service: c.Service, NodeID: c.NodeID}}
}

// Resolver is the part of resolve.Resolver that expanding needs.
type Resolver interface {
	Services(ctx context.Context) ([]resolve.Service, error)
	Candidates(ctx context.Context, service string) ([]resolve.Candidate, error)
	Resolve(ctx context.Context, req resolve.Request) (*resolve.Endpoint, error)
}

// ExpandSources turns targets and an optional stack into sources. A service
// name means every running replica of it; anything else (service.slot, a task
// or container id) is resolved as one container. notes are things the operator
// should hear about but that do not stop the others — a service with nothing
// running is the normal case in a stack with a scaled-down worker.
func ExpandSources(ctx context.Context, r Resolver, targets []string, stack, nodeHint string) ([]Source, []string, error) {
	var services []string
	if stack != "" {
		var err error
		if services, err = stackServices(ctx, r, stack); err != nil {
			return nil, nil, err
		}
	}

	set := sourceSet{seen: map[string]bool{}}
	for _, name := range services {
		if err := set.addService(ctx, r, name); err != nil {
			return nil, nil, err
		}
	}
	for _, t := range targets {
		if isServiceName(ctx, r, t) {
			if err := set.addService(ctx, r, t); err != nil {
				return nil, nil, err
			}
			continue
		}
		ep, err := r.Resolve(ctx, resolve.Request{Target: t, NodeHint: nodeHint})
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", t, err)
		}
		set.add(Source{Label: t, Endpoint: *ep, Target: FollowTargetOf(t)})
	}
	if len(set.out) == 0 {
		return nil, set.notes, fmt.Errorf("nothing to stream: %s", strings.Join(set.notes, "; "))
	}
	return set.out, set.notes, nil
}

// stackServices names the services of stack, sorted; none is an error.
func stackServices(ctx context.Context, r Resolver, stack string) ([]string, error) {
	svcs, err := r.Services(ctx)
	if err != nil {
		return nil, err
	}
	var services []string
	for _, s := range svcs {
		if s.Stack == stack {
			services = append(services, s.Name)
		}
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("no services in stack %q", stack)
	}
	sort.Strings(services)
	return services, nil
}

// sourceSet collects sources once per container, in the order added, plus
// the notes ExpandSources hands back.
type sourceSet struct {
	out   []Source
	notes []string
	seen  map[string]bool // container IDs already in out
}

func (set *sourceSet) add(s Source) {
	if !set.seen[s.Endpoint.ContainerID] {
		set.seen[s.Endpoint.ContainerID] = true
		set.out = append(set.out, s)
	}
}

// addService adds every running replica of service name, by slot then node.
func (set *sourceSet) addService(ctx context.Context, r Resolver, name string) error {
	cands, err := r.Candidates(ctx, name)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		set.notes = append(set.notes, fmt.Sprintf("%s: no running container", name))
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Slot != cands[j].Slot {
			return cands[i].Slot < cands[j].Slot
		}
		return cands[i].NodeName < cands[j].NodeName
	})
	for _, c := range cands {
		set.add(SourceFromCandidate(c))
	}
	return nil
}

// isServiceName reports whether t names a service exactly. Candidates errors
// for anything that is not one, which is the cheapest way to ask.
func isServiceName(ctx context.Context, r Resolver, t string) bool {
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
func sourcePrefixes(sources []Source, color bool) []string {
	width := 0
	for _, s := range sources {
		width = max(width, len(s.Label))
	}
	out := make([]string, len(sources))
	for i, s := range sources {
		p := fmt.Sprintf("%-*s | ", width, s.Label)
		if color {
			p = "\x1b[" + prefixColors[i%len(prefixColors)] + "m" + fmt.Sprintf("%-*s", width, s.Label) + "\x1b[0m | "
		}
		out[i] = p
	}
	return out
}

// FilterFactory builds one source's filter chain in front of dst, or returns
// dst unchanged when no filtering is asked for.
type FilterFactory func(dst io.Writer) (io.Writer, func())

// StreamsFailedError is StreamMany's error: which sources failed, out of how
// many, so a caller can tell "one of three" from "all of them".
type StreamsFailedError struct {
	Failed []string // labels, sorted
	Total  int
}

func (e *StreamsFailedError) Error() string {
	return fmt.Sprintf("%d of %d log streams failed: %s", len(e.Failed), e.Total, strings.Join(e.Failed, ", "))
}

// StreamMany follows every source concurrently into the run's stdout and
// Stderr, each line prefixed with its source (coloured when color is set) and
// passed through the filter chain wrap builds. A source that fails is reported
// in its own lines and does not stop the others; the returned error says how
// many failed.
func StreamMany(ctx context.Context, run Run, sources []Source, color bool, wrap FilterFactory) error {
	outSink, errSink := newLineSinks(run.Stdout, run.Stderr)
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
			one := run
			one.Stdout, one.Stderr = so, se
			err := Follow(ctx, one, src.Target, src.Endpoint, notify)
			flushOut()
			flushErr()
			po.Flush()
			pe.Flush()
			if err != nil {
				notify("── stream ended: " + err.Error() + " ──")
				mu.Lock()
				failed = append(failed, src.Label)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(failed) > 0 {
		sort.Strings(failed)
		return &StreamsFailedError{Failed: failed, Total: len(sources)}
	}
	return nil
}
