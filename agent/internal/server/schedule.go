// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/moby/moby/client"
	"google.golang.org/protobuf/proto"

	"swarmexec/agent/internal/prunecat"
	"swarmexec/internal/pb"
	"swarmexec/internal/prunesched"
)

// The prune schedule is read from the agent's OWN container labels, once, at
// start. That is the only configuration a worker's agent can read — it has the
// local socket, not the manager API — and since a label change is a service
// update, every agent restarts with the new schedule anyway: there is nothing
// to watch.

// schedulerIdentity is who a scheduled run acts as, for the policy and the
// audit trail. A policy rule can therefore refuse the schedule a category the
// operator may still prune by hand, or the reverse.
const schedulerIdentity = "scheduler"

// schedulerMaxSleep bounds one wait for the next run. Sleeping the whole
// interval would trust the monotonic clock across a suspend or an NTP step;
// waking each minute and re-reading the wall clock does not.
const schedulerMaxSleep = time.Minute

type pruneScheduler struct {
	s        *Server
	log      *slog.Logger
	now      func() time.Time
	maxSleep time.Duration

	mu     sync.RWMutex
	loaded bool
	sched  prunesched.Schedule
	err    error
	last   *pb.PruneRun
}

func newPruneScheduler(s *Server, log *slog.Logger) *pruneScheduler {
	return &pruneScheduler{s: s, log: log, now: time.Now, maxSleep: schedulerMaxSleep, sched: prunesched.Default()}
}

// StartPruneScheduler reads the schedule from the agent's own container labels
// and runs it until ctx ends. Invalid labels are logged and disable the
// schedule; they never stop the agent. Call it once after New.
func (s *Server) StartPruneScheduler(ctx context.Context) {
	go func() {
		labels, err := s.selfLabels(ctx)
		if err != nil {
			s.log.Warn("prune schedule: cannot read the agent's own labels; schedule off", "err", err)
			s.scheduler.set(prunesched.Default(), fmt.Errorf("cannot read the agent's own labels: %w", err))
			return
		}
		s.scheduler.load(labels)
		s.scheduler.run(ctx)
	}()
}

// selfLabels returns the labels of the container this agent runs in, found by
// hostname — the same self-inspection the port-forward sidecar image uses.
func (s *Server) selfLabels(ctx context.Context) (map[string]string, error) {
	host, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	ictx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := s.docker.ContainerInspect(ictx, host, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	if res.Container.Config == nil {
		return nil, nil
	}
	return res.Container.Config.Labels, nil
}

// load parses the labels and logs what the agent will do.
func (p *pruneScheduler) load(labels map[string]string) {
	sched, err := prunesched.Parse(labels)
	p.set(sched, err)
	switch {
	case err != nil:
		p.log.Error("prune schedule labels are invalid; schedule disabled", "err", err)
	case !sched.Enabled:
		p.log.Info("prune schedule disabled")
	default:
		next, _ := sched.Next(p.now())
		p.log.Info("prune schedule enabled",
			"days", prunesched.FormatDays(sched.Days), "times", timesOfDay(sched), "tz", sched.TZ,
			"categories", sched.Categories, "containers_until", sched.ContainersUntil.String(),
			"label_filters", sched.LabelFilters, "next_run", next.Format(time.RFC3339))
	}
}

func (p *pruneScheduler) set(s prunesched.Schedule, err error) {
	p.mu.Lock()
	p.sched, p.err, p.loaded = s, err, true
	p.mu.Unlock()
}

func (p *pruneScheduler) schedule() (prunesched.Schedule, error, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sched, p.err, p.loaded
}

// ContainersUntil is the age filter the reclaim measurement uses: the
// schedule's, so the figure an operator reads is what the schedule will take.
func (p *pruneScheduler) ContainersUntil() time.Duration {
	s, err, _ := p.schedule()
	if err != nil {
		return prunesched.DefaultContainersUntil
	}
	return s.ContainersUntil
}

// run waits for each scheduled time and prunes, until ctx ends.
func (p *pruneScheduler) run(ctx context.Context) {
	sched, err, _ := p.schedule()
	if err != nil || !sched.Enabled {
		return
	}
	for {
		next, ok := sched.Next(p.now())
		if !ok {
			p.log.Warn("prune schedule selects no day; nothing will run")
			return
		}
		if !p.waitUntil(ctx, next) {
			return
		}
		p.runOnce(ctx, sched)
	}
}

// waitUntil sleeps until t by the wall clock, in steps of at most
// schedulerMaxSleep. It reports false when ctx ended first.
func (p *pruneScheduler) waitUntil(ctx context.Context, t time.Time) bool {
	for {
		d := t.Sub(p.now())
		if d <= 0 {
			return true
		}
		if d > p.maxSleep {
			d = p.maxSleep
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
		}
	}
}

// runOnce performs one scheduled prune and records it as the last run.
func (p *pruneScheduler) runOnce(ctx context.Context, sched prunesched.Schedule) *pb.PruneRun {
	cats, err := prunecat.FromNames(sched.Categories)
	run := &pb.PruneRun{StartedAt: p.now().UTC().Format(time.RFC3339)}
	if err != nil {
		run.Error = err.Error() // Parse rejects these; belt and braces
	} else {
		p.log.Info("scheduled prune starting", "categories", prunecat.Names(cats))
		run.Results = p.s.runPrune(ctx, schedulerIdentity, "schedule", cats,
			pruneOptions{containersUntil: sched.ContainersUntil, labelFilters: sched.LabelFilters})
		var freed int64
		for _, r := range run.Results {
			freed += r.GetFreedBytes()
			if r.GetError() != "" && run.Error == "" {
				pc, _ := prunecat.Info(r.GetCategory())
				run.Error = pc.Name + ": " + r.GetError()
			}
		}
		p.log.Info("scheduled prune finished", "freed_bytes", freed, "error", run.Error)
	}
	run.FinishedAt = p.now().UTC().Format(time.RFC3339)
	p.mu.Lock()
	p.last = run
	p.mu.Unlock()
	p.s.reclaim.Kick()
	return run
}

// status renders the schedule and the last run for ReclaimStats.
func (p *pruneScheduler) status(now time.Time) (*pb.PruneSchedule, *pb.PruneRun) {
	sched, err, loaded := p.schedule()
	p.mu.RLock()
	last := p.last
	p.mu.RUnlock()
	if last != nil {
		last = proto.Clone(last).(*pb.PruneRun)
	}
	if !loaded {
		return &pb.PruneSchedule{}, last
	}
	out := &pb.PruneSchedule{
		Enabled:         sched.Enabled && err == nil,
		Start:           prunesched.FormatClock(sched.Start),
		TimesPerDay:     uint32(max(sched.Times, 0)),
		Tz:              sched.TZ,
		ContainersUntil: sched.ContainersUntil.String(),
		LabelFilters:    sched.LabelFilters,
	}
	if err != nil {
		out.Error = err.Error()
	}
	for _, wd := range prunesched.WeekOrder() {
		if sched.Days[wd] {
			out.Days = append(out.Days, prunesched.DayName(wd))
		}
	}
	out.Categories, _ = prunecat.FromNames(sched.Categories)
	if out.Enabled {
		if next, ok := sched.Next(now); ok {
			out.NextRun = next.Format(time.RFC3339)
		}
	}
	return out, last
}

func timesOfDay(s prunesched.Schedule) []string {
	var out []string
	for _, m := range s.TimesOfDay() {
		out = append(out, prunesched.FormatClock(m))
	}
	return out
}
