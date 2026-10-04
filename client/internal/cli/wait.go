// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/moby/moby/client"

	"github.com/moby/moby/api/types/swarm"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
)

// `swarmexec wait` closes the gap `stack deploy` leaves: swarm accepting a
// service is not the service working. It blocks until every named service has
// converged — and says why when one does not.
//
// A wait that reports green for a broken deploy is worse than none: the
// pipeline does not stop, it ships. So the definition below errs towards "not
// yet": a service counts as converged only after the same verdict on two polls
// a few seconds apart, and the states that look settled but are not — an update
// that paused half way, a rollback that finished during the wait — are
// failures, not successes. swarmexec observes and reports; it never rolls back
// or retries anything itself.

const (
	waitPollInterval = 1500 * time.Millisecond
	// waitSettle is how long a converged verdict must hold. It covers the gap
	// right after a deploy, before the manager has started the update and the
	// old tasks still look complete.
	waitSettle = 3 * time.Second
)

type waitFlags struct {
	stack          string
	timeout        time.Duration
	healthy        bool
	connectTimeout time.Duration
}

func newWaitCmd(g *globalFlags) *cobra.Command {
	f := &waitFlags{}
	cmd := &cobra.Command{
		Use:   "wait [flags] <service>...",
		Short: "Wait until services have converged — for CI after a deploy",
		Long: `Block until every named service (or every service of --stack) has converged,
then exit 0. Exit 1 with the reason when one does not converge in time, when its
update paused, or when it was rolled back during the wait.

Converged means: no update in flight, as many running tasks as desired (for a job:
its completions reached), held for two polls in a row. With --healthy every
container that has a healthcheck must also report healthy; a node whose agent
cannot report health counts as "not yet", never as healthy.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if s, _ := cmd.Flags().GetString("stack"); s != "" {
				return nil
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWait(cmd, g, f, args)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.stack, "stack", "", "wait for every service of this stack (in addition to any named)")
	fl.DurationVar(&f.timeout, "timeout", 5*time.Minute, "give up after this long (exit 1)")
	fl.BoolVar(&f.healthy, "healthy", false, "also require every container with a healthcheck to be healthy (asks the agents)")
	fl.DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to an agent (--healthy)")
	return cmd
}

// waitSnapshot is what one poll knows about one service.
type waitSnapshot struct {
	name          string
	missing       bool
	isJob         bool
	have, want    int    // running/desired, or completions/total for a job
	updateState   string // swarm UpdateStatus.State, "" if none
	updateMessage string
	updateDone    time.Time // UpdateStatus.CompletedAt
	taskErr       string    // the latest reason a task of this service failed or is stuck
	health        *serviceHealth
	healthUnknown []string // nodes whose agent could not report health
}

type waitVerdict int

const (
	waitPending waitVerdict = iota
	waitConverged
	waitFailed
)

// judge decides one service from one poll. since is when the wait began: a
// rollback that completed before it is history, one that completed after it
// is this deploy failing.
func judge(s waitSnapshot, since time.Time, wantHealthy bool) (waitVerdict, string) {
	if s.missing {
		return waitFailed, "no such service"
	}
	switch s.updateState {
	case string(swarm.UpdateStatePaused):
		return waitFailed, fmt.Sprintf("its update paused (%s) — it runs a mix of old and new tasks; `docker service update %s` resumes it",
			orDash(s.updateMessage), s.name)
	case string(swarm.UpdateStateRollbackPaused):
		return waitFailed, fmt.Sprintf("its rollback paused (%s)", orDash(s.updateMessage))
	case string(swarm.UpdateStateRollbackCompleted):
		if !s.updateDone.Before(since) {
			return waitFailed, fmt.Sprintf("it was rolled back during the wait (%s) — the deploy did not take", orDash(s.updateMessage))
		}
	case string(swarm.UpdateStateUpdating), string(swarm.UpdateStateRollbackStarted):
		return waitPending, progressText(s) + ", " + strings.ReplaceAll(s.updateState, "_", " ")
	}
	if s.want == 0 && !s.isJob {
		return waitConverged, "scaled to 0"
	}
	if s.have < s.want {
		why := progressText(s)
		if s.taskErr != "" {
			why += " — " + s.taskErr
		}
		return waitPending, why
	}
	if wantHealthy {
		if len(s.healthUnknown) > 0 {
			return waitPending, "health unknown on " + strings.Join(s.healthUnknown, ", ") + " (agent too old or unreachable)"
		}
		if h := s.health; h != nil && (h.Unhealthy > 0 || h.Starting > 0) {
			return waitPending, fmt.Sprintf("%s, %d unhealthy, %d starting", progressText(s), h.Unhealthy, h.Starting)
		}
	}
	return waitConverged, progressText(s)
}

func progressText(s waitSnapshot) string {
	if s.isJob {
		return fmt.Sprintf("%d/%d completed", s.have, s.want)
	}
	return fmt.Sprintf("%d/%d running", s.have, s.want)
}

func runWait(cmd *cobra.Command, g *globalFlags, f *waitFlags, args []string) error {
	ctx := cmdContext(cmd)
	dockerEP := resolveEndpoint(g.dockerContext)
	dcli, err := dockerEP.connect(ctx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	// Without --healthy nothing dials an agent, so the address mode is moot
	// and no agent configuration is needed: wait works with Docker access alone.
	r := resolve.New(dcli, resolve.AddrHostname)
	var health func(context.Context, []string) (map[string]serviceHealth, map[string][]string, error)
	if f.healthy {
		cfg, err := g.resolveConfig(cmd, dockerEP)
		if err != nil {
			return &cliError{code: usageExitCode, err: err}
		}
		if err := cfg.Validate(); err != nil {
			return &cliError{code: usageExitCode, err: err}
		}
		r = resolve.New(dcli, addrModeOf(cfg))
		gate := newStatsGate()
		health = func(ctx context.Context, svcs []string) (map[string]serviceHealth, map[string][]string, error) {
			return waitHealth(ctx, r, cfg, gate, svcs, f.connectTimeout)
		}
	}

	names, err := waitTargets(ctx, r, args, f.stack)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}

	out := cmd.ErrOrStderr()
	since := time.Now()
	deadline := since.Add(f.timeout)
	settledSince := map[string]time.Time{}
	last := map[string]string{}
	for {
		snaps, err := waitPoll(ctx, dcli, r, names, health)
		if err != nil {
			return &cliError{code: session.TransportFailure, err: err}
		}
		now := time.Now()
		done := 0
		for _, s := range snaps {
			v, why := judge(s, since, f.healthy)
			if v == waitFailed {
				fmt.Fprintf(out, "✗ %s: %s\n", s.name, why)
				return &cliError{code: 1, err: fmt.Errorf("%s did not converge: %s", s.name, why), silent: true}
			}
			if v == waitConverged {
				if settledSince[s.name].IsZero() {
					settledSince[s.name] = now
				}
				if now.Sub(settledSince[s.name]) >= waitSettle {
					done++
					why = "converged (" + why + ")"
				}
			} else {
				delete(settledSince, s.name)
			}
			if last[s.name] != why {
				fmt.Fprintf(out, "  %s: %s\n", s.name, why)
				last[s.name] = why
			}
		}
		if done == len(snaps) {
			fmt.Fprintf(out, "✓ %d service(s) converged in %s\n", len(snaps), now.Sub(since).Round(time.Second))
			return nil
		}
		if now.After(deadline) {
			var stuck []string
			for _, s := range snaps {
				if v, why := judge(s, since, f.healthy); v != waitConverged {
					stuck = append(stuck, s.name+": "+why)
				}
			}
			for _, s := range stuck {
				fmt.Fprintf(out, "✗ %s\n", s)
			}
			return &cliError{code: 1, err: fmt.Errorf("timed out after %s waiting for %d service(s)", f.timeout, len(stuck)), silent: true}
		}
		select {
		case <-ctx.Done():
			return &cliError{code: 1, err: ctx.Err()}
		case <-time.After(waitPollInterval):
		}
	}
}

// waitTargets resolves the named services and the stack into service names.
func waitTargets(ctx context.Context, r *resolve.Resolver, args []string, stack string) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	if stack != "" {
		svcs, err := r.Services(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range svcs {
			if s.Stack == stack && !seen[s.Name] {
				seen[s.Name] = true
				names = append(names, s.Name)
			}
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("no services in stack %q", stack)
		}
	}
	for _, a := range args {
		if !seen[a] {
			seen[a] = true
			names = append(names, a)
		}
	}
	sort.Strings(names)
	return names, nil
}

// waitPoll takes one snapshot of every service.
func waitPoll(ctx context.Context, dcli serviceTaskLister, r *resolve.Resolver, names []string,
	health func(context.Context, []string) (map[string]serviceHealth, map[string][]string, error)) ([]waitSnapshot, error) {
	svcs, err := r.Services(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]resolve.Service{}
	for _, s := range svcs {
		byName[s.Name] = s
	}
	rawRes, err := dcli.ServiceList(ctx, client.ServiceListOptions{})
	raw := rawRes.Items
	if err != nil {
		return nil, err
	}
	rawByName := map[string]swarm.Service{}
	for _, s := range raw {
		rawByName[s.Spec.Name] = s
	}

	var hs map[string]serviceHealth
	var unknown map[string][]string
	if health != nil {
		if hs, unknown, err = health(ctx, names); err != nil {
			return nil, err
		}
	}

	out := make([]waitSnapshot, 0, len(names))
	for _, n := range names {
		s, ok := byName[n]
		if !ok {
			out = append(out, waitSnapshot{name: n, missing: true})
			continue
		}
		snap := waitSnapshot{name: n, isJob: s.IsJob()}
		snap.have, snap.want = s.Progress()
		if rs, ok := rawByName[n]; ok {
			if us := rs.UpdateStatus; us != nil {
				snap.updateState = string(us.State)
				snap.updateMessage = us.Message
				if us.CompletedAt != nil {
					snap.updateDone = *us.CompletedAt
				}
			}
			snap.taskErr = latestTaskError(ctx, dcli, rs.ID)
		}
		if health != nil {
			if h, ok := hs[n]; ok {
				snap.health = &h
			}
			snap.healthUnknown = unknown[n]
		}
		out = append(out, snap)
	}
	return out, nil
}

// serviceTaskLister is the part of the Docker client wait reads.
type serviceTaskLister interface {
	ServiceList(ctx context.Context, opts client.ServiceListOptions) (client.ServiceListResult, error)
	TaskList(ctx context.Context, opts client.TaskListOptions) (client.TaskListResult, error)
}

// latestTaskError is why the newest task meant to run is not running: the
// scheduler's "no suitable node", a failed start, a rejected image. Empty when
// nothing is wrong or nothing can be read.
func latestTaskError(ctx context.Context, dcli serviceTaskLister, serviceID string) string {
	tasksRes, err := dcli.TaskList(ctx, client.TaskListOptions{Filters: make(client.Filters).Add("service", serviceID)})
	tasks := tasksRes.Items
	if err != nil {
		return ""
	}
	var newest *swarm.Task
	for i := range tasks {
		t := &tasks[i]
		if t.Status.State == swarm.TaskStateRunning {
			continue
		}
		if t.Status.Err == "" && !(t.DesiredState == swarm.TaskStateRunning && t.Status.State == swarm.TaskStatePending) {
			continue
		}
		if newest == nil || t.Status.Timestamp.After(newest.Status.Timestamp) {
			newest = t
		}
	}
	if newest == nil {
		return ""
	}
	if newest.Status.Err != "" {
		return newest.Status.Err
	}
	return newest.Status.Message
}

// waitHealth asks the agents for health and maps it onto the services. A node
// whose agent did not answer is reported per service as unknown, so --healthy
// cannot pass on a node it could not see.
func waitHealth(ctx context.Context, r *resolve.Resolver, cfg config.Config, gate *statsGate, names []string, connectTimeout time.Duration) (map[string]serviceHealth, map[string][]string, error) {
	nodes, err := r.Nodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	usage, nodeUse := collectUsage(ctx, cfg, nodes, connectTimeout, gate)
	out := map[string]serviceHealth{}
	unknown := map[string][]string{}
	for _, n := range names {
		cands, err := r.Candidates(ctx, n)
		if err != nil {
			continue
		}
		out[n] = healthOf(usage, cands, n)
		for _, c := range cands {
			if _, ok := nodeUse[c.NodeName]; !ok {
				if !slices.Contains(unknown[n], c.NodeName) {
					unknown[n] = append(unknown[n], c.NodeName)
				}
			}
		}
	}
	return out, unknown, nil
}
