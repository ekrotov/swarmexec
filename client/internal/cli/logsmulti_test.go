// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"swarmexec/client/internal/resolve"
)

// Two sources writing in chunks that end mid-line, concurrently, must still
// produce only whole lines, each with its own prefix.
func TestPrefixWriterKeepsLinesWhole(t *testing.T) {
	var out bytes.Buffer
	sink, _ := newLineSinks(&out, nil)
	a := &prefixWriter{sink: sink, prefix: "a | "}
	b := &prefixWriter{sink: sink, prefix: "b | "}

	var wg sync.WaitGroup
	for _, w := range []*prefixWriter{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				line := fmt.Sprintf("line-%03d-of-%s\n", i, strings.TrimSuffix(w.prefix, " | "))
				// Split every line in two writes, so a chunk boundary sits
				// inside it — the case a naive shared writer tears.
				_, _ = w.Write([]byte(line[:5]))
				_, _ = w.Write([]byte(line[5:]))
			}
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 400 {
		t.Fatalf("got %d lines, want 400", len(lines))
	}
	for _, l := range lines {
		src := l[:1]
		if !strings.HasPrefix(l, src+" | line-") || !strings.HasSuffix(l, "-of-"+src) {
			t.Fatalf("torn or mislabelled line %q", l)
		}
	}
}

// What the live run showed: one source on stdout and another on stderr, both
// landing on the same terminal (2>&1), tore a line between its prefix and its
// text. Each Write that reaches the shared terminal must be a whole line.
func TestStdoutAndStderrSinksNeverInterleaveInsideALine(t *testing.T) {
	term := &writeRecorder{}
	outSink, errSink := newLineSinks(term, term)
	a := &prefixWriter{sink: outSink, prefix: "web | "}
	b := &prefixWriter{sink: errSink, prefix: "fb | "}

	var wg sync.WaitGroup
	for _, w := range []*prefixWriter{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				_, _ = w.Write([]byte("message\n"))
			}
		}()
	}
	wg.Wait()
	for _, w := range term.writes() {
		if !(strings.HasPrefix(w, "web | ") || strings.HasPrefix(w, "fb | ")) || !strings.HasSuffix(w, "message\n") || strings.Count(w, "\n") != 1 {
			t.Fatalf("a write to the terminal was not one whole line: %q", w)
		}
	}
}

// writeRecorder keeps every Write as it arrived, the way a terminal sees them.
type writeRecorder struct {
	mu sync.Mutex
	ws []string
}

func (r *writeRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ws = append(r.ws, string(p))
	return len(p), nil
}

func (r *writeRecorder) writes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ws...)
}

func TestPrefixWriterFlushesATrailingPartialLine(t *testing.T) {
	var out bytes.Buffer
	sink, _ := newLineSinks(&out, nil)
	p := &prefixWriter{sink: sink, prefix: "x | "}
	_, _ = p.Write([]byte("whole\nno newline"))
	p.Flush()
	if got := out.String(); got != "x | whole\nx | no newline\n" {
		t.Errorf("got %q", got)
	}
}

func TestSourcePrefixesAlignAndColourOnlyWhenAsked(t *testing.T) {
	srcs := []logSource{{label: "api.1"}, {label: "worker.12"}}
	plain := sourcePrefixes(srcs, false)
	if plain[0] != "api.1     | " || plain[1] != "worker.12 | " {
		t.Errorf("plain prefixes not aligned: %q", plain)
	}
	if strings.Contains(plain[0], "\x1b") {
		t.Error("plain output must carry no escape codes")
	}
	col := sourcePrefixes(srcs, true)
	if !strings.HasPrefix(col[0], "\x1b[36m") || col[0] == col[1] {
		t.Errorf("coloured prefixes = %q", col)
	}
}

// fakeLogResolver answers like the manager would for a small cluster.
type fakeLogResolver struct {
	services []resolve.Service
	cands    map[string][]resolve.Candidate
	resolved map[string]resolve.Endpoint
}

func (f *fakeLogResolver) Services(context.Context) ([]resolve.Service, error) {
	return f.services, nil
}

func (f *fakeLogResolver) Candidates(_ context.Context, svc string) ([]resolve.Candidate, error) {
	c, ok := f.cands[svc]
	if !ok {
		return nil, fmt.Errorf("no such service %q", svc)
	}
	return c, nil
}

func (f *fakeLogResolver) Resolve(_ context.Context, req resolve.Request) (*resolve.Endpoint, error) {
	ep, ok := f.resolved[req.Target]
	if !ok {
		return nil, errors.New("not found")
	}
	return &ep, nil
}

func shopCluster() *fakeLogResolver {
	return &fakeLogResolver{
		services: []resolve.Service{
			{Name: "shop_web", Stack: "shop"}, {Name: "shop_db", Stack: "shop"},
			{Name: "shop_worker", Stack: "shop"}, {Name: "other", Stack: "elsewhere"},
		},
		cands: map[string][]resolve.Candidate{
			"shop_web": {
				{Service: "shop_web", Slot: 2, ContainerID: "w2", NodeName: "n2"},
				{Service: "shop_web", Slot: 1, ContainerID: "w1", NodeName: "n1"},
			},
			"shop_db":     {{Service: "shop_db", Slot: 1, ContainerID: "d1", NodeName: "n1"}},
			"shop_worker": {}, // scaled to zero
			"agent":       {{Service: "agent", NodeID: "id3", NodeName: "n3", ContainerID: "g3"}},
		},
		resolved: map[string]resolve.Endpoint{"shop_web.1": {ContainerID: "w1"}, "abc123": {ContainerID: "c9"}},
	}
}

func labels(srcs []logSource) []string {
	out := make([]string, len(srcs))
	for i, s := range srcs {
		out[i] = s.label
	}
	return out
}

func TestExpandLogSourcesStack(t *testing.T) {
	srcs, notes, err := expandLogSources(context.Background(), shopCluster(), nil, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	// Services in name order, replicas in slot order; nothing from another stack.
	if got := strings.Join(labels(srcs), " "); got != "shop_db.1 shop_web.1 shop_web.2" {
		t.Errorf("sources = %s", got)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "shop_worker") {
		t.Errorf("the scaled-down worker should be named, got %v", notes)
	}
	if srcs[2].follow != (resolve.FollowTarget{Service: "shop_web", Slot: 2}) {
		t.Errorf("a replica follows its slot, got %+v", srcs[2].follow)
	}
}

func TestExpandLogSourcesMixedTargets(t *testing.T) {
	// A service, a pinned slot that repeats one of its replicas, a global
	// service and a raw container id.
	srcs, _, err := expandLogSources(context.Background(), shopCluster(),
		[]string{"shop_web", "shop_web.1", "agent", "abc123"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(labels(srcs), " "); got != "shop_web.1 shop_web.2 agent@n3 abc123" {
		t.Errorf("sources = %s (the repeated container must appear once)", got)
	}
	if srcs[2].follow != (resolve.FollowTarget{Service: "agent", NodeID: "id3"}) {
		t.Errorf("a global task follows its node, got %+v", srcs[2].follow)
	}
}

func TestExpandLogSourcesErrors(t *testing.T) {
	if _, _, err := expandLogSources(context.Background(), shopCluster(), nil, "nope", ""); err == nil ||
		!strings.Contains(err.Error(), `no services in stack "nope"`) {
		t.Errorf("unknown stack: %v", err)
	}
	if _, _, err := expandLogSources(context.Background(), shopCluster(), []string{"missing"}, "", ""); err == nil ||
		!strings.Contains(err.Error(), "missing") {
		t.Errorf("an unresolvable target must be named: %v", err)
	}
	if _, _, err := expandLogSources(context.Background(), shopCluster(), []string{"shop_worker"}, "", ""); err == nil ||
		!strings.Contains(err.Error(), "nothing to stream") {
		t.Errorf("only empty services: %v", err)
	}
}
