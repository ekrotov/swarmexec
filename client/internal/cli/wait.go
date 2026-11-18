// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/moby/moby/client"

	"github.com/moby/moby/api/types/swarm"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/endpoint"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmops"
	"swarmexec/internal/dockerfmt"
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
	health        *swarmops.ServiceHealth
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
	if v, why, decided := judgeUpdate(s, since); decided {
		return v, why
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
		if why, pending := healthPending(s); pending {
			return waitPending, why
		}
	}
	return waitConverged, progressText(s)
}

// judgeUpdate is the verdict a service's update state alone decides: a
// paused update or rollback, a rollback during the wait, one in flight.
// decided is false when the update state leaves it to the task counts.
func judgeUpdate(s waitSnapshot, since time.Time) (v waitVerdict, why string, decided bool) {
	switch s.updateState {
	case string(swarm.UpdateStatePaused):
		return waitFailed, fmt.Sprintf("its update paused (%s) — it runs a mix of old and new tasks; `docker service update %s` resumes it",
			dockerfmt.OrDash(s.updateMessage), s.name), true
	case string(swarm.UpdateStateRollbackPaused):
		return waitFailed, fmt.Sprintf("its rollback paused (%s)", dockerfmt.OrDash(s.updateMessage)), true
	case string(swarm.UpdateStateRollbackCompleted):
		if !s.updateDone.Before(since) {
			return waitFailed, fmt.Sprintf("it was rolled back during the wait (%s) — the deploy did not take", dockerfmt.OrDash(s.updateMessage)), true
		}
	case string(swarm.UpdateStateUpdating), string(swarm.UpdateStateRollbackStarted):
		return waitPending, progressText(s) + ", " + strings.ReplaceAll(s.updateState, "_", " "), true
	}
	return waitPending, "", false
}

// healthPending is why a service whose tasks all run is not yet healthy:
// health unknown on some node, or tasks unhealthy or still starting.
func healthPending(s waitSnapshot) (why string, pending bool) {
	if len(s.healthUnknown) > 0 {
		return "health unknown on " + strings.Join(s.healthUnknown, ", ") + " (agent too old or unreachable)", true
	}
	if h := s.health; h != nil && (h.Unhealthy > 0 || h.Starting > 0) {
		return fmt.Sprintf("%s, %d unhealthy, %d starting", progressText(s), h.Unhealthy, h.Starting), true
	}
	return "", false
}

func progressText(s waitSnapshot) string {
	if s.isJob {
		return fmt.Sprintf("%d/%d completed", s.have, s.want)
	}
	return fmt.Sprintf("%d/%d running", s.have, s.want)
}

func runWait(cmd *cobra.Command, g *globalFlags, f *waitFlags, args []string) error {
	ctx := cmdContext(cmd)
	dockerEP := endpoint.Resolve(g.dockerContext)
	dcli, err := dockerEP.Connect(ctx)
	if err != nil {
		return &cliError{code: exitTransport, err: err}
	}
	// Without --healthy nothing dials an agent, so the address mode is moot
	// and no agent configuration is needed: wait works with Docker access alone.
	r := resolve.New(dcli, resolve.AddrHostname)
	var health waitHealthFunc
	if f.healthy {
		cfg, err := g.resolveConfig(cmd, dockerEP)
		if err != nil {
			return &cliError{code: exitUsage, err: err}
		}
		if err := cfg.Validate(); err != nil {
			return &cliError{code: exitUsage, err: err}
		}
		r = resolve.New(dcli, addrModeOf(cfg))
		ac := agentcall.New(cfg)
		defer ac.Close()
		health = func(ctx context.Context, svcs []string) (map[string]swarmops.ServiceHealth, map[string][]string, error) {
			return waitHealth(ctx, r, ac, svcs, f.connectTimeout)
		}
	}

	names, err := waitTargets(ctx, r, args, f.stack)
	if err != nil {
		return &cliError{code: exitUsage, err: err}
	}

	w := &waitProgress{out: cmd.ErrOrStderr(), since: time.Now(), healthy: f.healthy,
		settledSince: map[string]time.Time{}, last: map[string]string{}}
	deadline := w.since.Add(f.timeout)
	for {
		snaps, err := waitPoll(ctx, dcli, r, names, health)
		if err != nil {
			return &cliError{code: exitTransport, err: err}
		}
		now := time.Now()
		if done, err := w.step(snaps, now); done || err != nil {
			return err
		}
		if now.After(deadline) {
			return w.timedOut(snaps, now, f.timeout)
		}
		select {
		case <-ctx.Done():
			return &cliError{code: exitNo, err: ctx.Err()}
		case <-time.After(waitPollInterval):
		}
	}
}

// waitHealthFunc reports per service its container health and the nodes whose
// agent could not say (--healthy).
type waitHealthFunc = func(context.Context, []string) (map[string]swarmops.ServiceHealth, map[string][]string, error)

// waitProgress is what a wait carries from poll to poll: since when each
// service has held a converged verdict, and the last line printed for it.
type waitProgress struct {
	out          io.Writer
	since        time.Time
	healthy      bool
	settledSince map[string]time.Time
	last         map[string]string
}

// step judges one poll's snapshots and prints what changed. It reports done
// once every service has held its converged verdict for waitSettle, and the
// error for the first service that failed.
func (w *waitProgress) step(snaps []waitSnapshot, now time.Time) (bool, error) {
	done := 0
	for _, s := range snaps {
		v, why := judge(s, w.since, w.healthy)
		if v == waitFailed {
			// The failure is the command's error, with the swarmexec:
			// prefix every other command's error has (review C-08).
			if s.missing {
				return false, &cliError{code: exitNo, err: fmt.Errorf("no such service %q", s.name)}
			}
			return false, &cliError{code: exitNo, err: fmt.Errorf("%s: %s", s.name, why)}
		}
		if v == waitConverged {
			if w.settledSince[s.name].IsZero() {
				w.settledSince[s.name] = now
			}
			if now.Sub(w.settledSince[s.name]) >= waitSettle {
				done++
				why = "converged (" + why + ")"
			}
		} else {
			delete(w.settledSince, s.name)
		}
		if w.last[s.name] != why {
			fmt.Fprintf(w.out, "  %s: %s\n", s.name, why)
			w.last[s.name] = why
		}
	}
	if done == len(snaps) {
		fmt.Fprintf(w.out, "✓ %d service(s) converged in %s\n", len(snaps), now.Sub(w.since).Round(time.Second))
		return true, nil
	}
	return false, nil
}

// timedOut prints the services the wait is still waiting for and returns the
// timeout error counting them: those whose verdict is not converged, and
// those that converged but have not held it for waitSettle yet — still
// pending, so they count, with their state.
func (w *waitProgress) timedOut(snaps []waitSnapshot, now time.Time, timeout time.Duration) error {
	var stuck []string
	for _, s := range snaps {
		v, why := judge(s, w.since, w.healthy)
		switch {
		case v != waitConverged:
			stuck = append(stuck, s.name+": "+why)
		case now.Sub(w.settledSince[s.name]) < waitSettle:
			stuck = append(stuck, s.name+": converged, settling ("+why+")")
		}
	}
	for _, s := range stuck {
		fmt.Fprintf(w.out, "✗ %s\n", s)
	}
	return &cliError{code: exitNo, err: fmt.Errorf("timed out after %s waiting for %d service(s)", timeout, len(stuck))}
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
	health func(context.Context, []string) (map[string]swarmops.ServiceHealth, map[string][]string, error)) ([]waitSnapshot, error) {
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

	var hs map[string]swarmops.ServiceHealth
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
func waitHealth(ctx context.Context, r *resolve.Resolver, ac *agentcall.Client, names []string, connectTimeout time.Duration) (map[string]swarmops.ServiceHealth, map[string][]string, error) {
	nodes, err := r.Nodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	usage, nodeUse := ac.Usage(ctx, nodes, connectTimeout)
	out := map[string]swarmops.ServiceHealth{}
	unknown := map[string][]string{}
	for _, n := range names {
		cands, err := r.Candidates(ctx, n)
		if err != nil {
			continue
		}
		out[n] = swarmops.HealthOf(usage, cands, n)
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
