// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"swarmexec/internal/pb"
	"swarmexec/internal/prunesched"
)

// fakeDocker answers the agent's self-inspection.
type fakeDocker struct {
	labels map[string]string
	err    error
	asked  string
}

func (f *fakeDocker) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.asked = id
	if f.err != nil {
		return client.ContainerInspectResult{}, f.err
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{Config: &container.Config{Labels: f.labels}}}, nil
}

// fakePrune records the calls the scheduler makes and answers with one result
// per category.
type fakePrune struct {
	mu    sync.Mutex
	calls []pruneCall
	fail  map[pb.PruneCategory]string
}

type pruneCall struct {
	identity, trigger string
	cats              []pb.PruneCategory
	until             time.Duration
	labels            []string
}

func (f *fakePrune) prune(_ context.Context, identity, trigger string, cats []pb.PruneCategory, until time.Duration, labels []string) []*pb.PruneCategoryResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pruneCall{identity, trigger, cats, until, labels})
	var out []*pb.PruneCategoryResult
	for _, c := range cats {
		out = append(out, &pb.PruneCategoryResult{Category: c, FreedBytes: 10, Error: f.fail[c]})
	}
	return out
}

func newTestScheduler(d Docker) (*Scheduler, *fakePrune, *int) {
	p := &fakePrune{}
	kicks := 0
	s := New(d, p.prune, func() { kicks++ }, slog.New(slog.DiscardHandler))
	s.hostname = func() (string, error) { return "agent-host", nil }
	return s, p, &kicks
}

// A scheduled run prunes the schedule's categories as the scheduler identity
// with the schedule's knobs, keeps the run for ReclaimStats and tells the
// reclaim cache its figures are stale.
func TestRunOncePrunesAsTheSchedulerAndKeepsTheRun(t *testing.T) {
	s, p, kicks := newTestScheduler(&fakeDocker{})
	s.Load(map[string]string{
		prunesched.LabelEnabled:         "true",
		prunesched.LabelCategories:      "networks,containers",
		prunesched.LabelContainersUntil: "48h",
	})
	sched, err, loaded := s.Schedule()
	if err != nil || !loaded || !sched.Enabled {
		t.Fatalf("schedule = %+v, %v, loaded %v", sched, err, loaded)
	}
	p.fail = map[pb.PruneCategory]string{pb.PruneCategory_PRUNE_CATEGORY_NETWORKS: "in use"}

	run := s.RunOnce(context.Background(), sched)
	if len(p.calls) != 1 {
		t.Fatalf("%d prune calls, want 1", len(p.calls))
	}
	c := p.calls[0]
	if c.identity != Identity || c.trigger != "schedule" || c.until != 48*time.Hour {
		t.Errorf("prune call = %+v", c)
	}
	if !slices.Contains(c.cats, pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS) || !slices.Contains(c.cats, pb.PruneCategory_PRUNE_CATEGORY_NETWORKS) {
		t.Errorf("categories = %v", c.cats)
	}
	if run.GetError() != "networks: in use" || run.GetStartedAt() == "" || run.GetFinishedAt() == "" {
		t.Errorf("run = %+v", run)
	}
	if *kicks != 1 {
		t.Errorf("afterRun called %d times, want 1", *kicks)
	}
	if _, last := s.Status(time.Now()); last.GetError() != run.GetError() || len(last.GetResults()) != 2 {
		t.Errorf("last run = %+v", last)
	}
}

func TestInvalidScheduleLabelsDisable(t *testing.T) {
	s, _, _ := newTestScheduler(&fakeDocker{})
	s.Load(map[string]string{prunesched.LabelEnabled: "true", prunesched.LabelTimes: "lots"})
	st, _ := s.Status(time.Now())
	if st.GetEnabled() || !strings.Contains(st.GetError(), prunesched.LabelTimes) || st.GetNextRun() != "" {
		t.Errorf("invalid labels must disable the schedule and say why: %+v", st)
	}
	if s.ContainersUntil() != prunesched.DefaultContainersUntil {
		t.Errorf("an invalid schedule's age filter = %v, want the default", s.ContainersUntil())
	}
	// run returns at once for a disabled schedule instead of spinning.
	done := make(chan struct{})
	go func() { s.run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("run kept going on an invalid schedule")
	}
}

func TestSchedulerWaitUntilUsesTheWallClock(t *testing.T) {
	s, _, _ := newTestScheduler(&fakeDocker{})
	base := time.Now()
	calls := 0
	s.maxSleep = time.Millisecond
	s.now = func() time.Time { calls++; return base.Add(time.Duration(calls) * time.Hour) }
	if !s.waitUntil(context.Background(), base.Add(90*time.Minute)) {
		t.Fatal("waitUntil gave up")
	}
	// It woke after one short sleep because the WALL clock passed the target,
	// not because the sleep was as long as the gap.
	if calls != 2 {
		t.Errorf("clock read %d times, want 2", calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.now = time.Now
	if s.waitUntil(ctx, time.Now().Add(time.Hour)) {
		t.Error("a cancelled context must stop the wait")
	}
}

// Start reads the schedule from the agent's own container, found by hostname.
// An unreadable self-inspection turns the schedule off and says why — it never
// stops the agent.
func TestStartReadsOwnLabels(t *testing.T) {
	d := &fakeDocker{labels: map[string]string{prunesched.LabelEnabled: "false"}}
	s, _, _ := newTestScheduler(d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	waitLoaded(t, s)
	if d.asked != "agent-host" {
		t.Errorf("inspected %q, want the agent's own hostname", d.asked)
	}
	if st, _ := s.Status(time.Now()); st.GetEnabled() || st.GetError() != "" {
		t.Errorf("status = %+v, want a valid disabled schedule", st)
	}

	broken, _, _ := newTestScheduler(&fakeDocker{err: errors.New("no such container")})
	broken.Start(ctx)
	waitLoaded(t, broken)
	if st, _ := broken.Status(time.Now()); st.GetEnabled() || !strings.Contains(st.GetError(), "no such container") {
		t.Errorf("status = %+v, want the inspect error", st)
	}
}

// Before Start has loaded anything, Status reports an empty schedule rather
// than the disabled default dressed up as a loaded one.
func TestStatusBeforeLoad(t *testing.T) {
	s, _, _ := newTestScheduler(&fakeDocker{})
	st, last := s.Status(time.Now())
	if st.GetEnabled() || st.GetTz() != "" || last != nil {
		t.Errorf("status before load = %+v, last %+v", st, last)
	}
}

func waitLoaded(t *testing.T, s *Scheduler) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, loaded := s.Schedule(); loaded {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the schedule was never loaded")
}
