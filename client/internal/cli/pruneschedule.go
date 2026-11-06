// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/session"
	"swarmexec/internal/prunesched"
)

// The cluster-wide prune schedule is a set of CONTAINER labels on the agent
// service (internal/prunesched explains why there). Editing it is therefore a
// service update of the agent, which restarts every agent one by one — the
// warning both the TUI and this command show before saving.

// scheduleRestartWarning is shown before every save.
const scheduleRestartWarning = "Saving updates the agent service: the agents restart one by one, " +
	"and open shells, log streams and port-forwards on a restarting node drop."

// agentServiceAPI is the slice of the manager API the schedule editor needs,
// so the label round trip is testable without a daemon.
type agentServiceAPI interface {
	ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error)
	ServiceUpdate(context.Context, string, client.ServiceUpdateOptions) (client.ServiceUpdateResult, error)
}

// findAgentService finds the deployed agent service: by the role label init
// stamps on it, else by its image (an agent deployed from a stack file without
// the label).
func findAgentService(ctx context.Context, dcli agentServiceAPI) (*swarm.Service, error) {
	res, err := dcli.ServiceList(ctx, client.ServiceListOptions{
		Filters: make(client.Filters).Add("label", agentRoleLabel+"="+agentRoleValue),
	})
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	for i := range res.Items {
		if res.Items[i].Spec.TaskTemplate.ContainerSpec != nil {
			return &res.Items[i], nil
		}
	}
	all, err := dcli.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	for i := range all.Items {
		if cs := all.Items[i].Spec.TaskTemplate.ContainerSpec; cs != nil && isAgentImage(cs.Image) {
			return &all.Items[i], nil
		}
	}
	return nil, errNoAgent
}

// agentSchedule reads the schedule from the agent service. A spec whose labels
// do not parse comes back with the error, so the editor can show what is wrong
// and save a valid one over it.
func agentSchedule(svc *swarm.Service) (prunesched.Schedule, error) {
	return prunesched.Parse(svc.Spec.TaskTemplate.ContainerSpec.Labels)
}

// scheduleSpec returns the agent's spec with the schedule labels replaced and
// every other part — other labels included — exactly as it was.
func scheduleSpec(svc *swarm.Service, s prunesched.Schedule) swarm.ServiceSpec {
	spec := svc.Spec
	cs := *spec.TaskTemplate.ContainerSpec
	cs.Labels = prunesched.Apply(cs.Labels, s)
	spec.TaskTemplate.ContainerSpec = &cs
	return spec
}

// saveAgentSchedule writes s onto the agent service. Only the container
// labels change; the image digest stays (no registry query), so this never
// sneaks an agent upgrade in with a schedule change.
func saveAgentSchedule(ctx context.Context, dcli agentServiceAPI, svc *swarm.Service, s prunesched.Schedule) error {
	if err := s.Validate(); err != nil {
		return err
	}
	_, err := dcli.ServiceUpdate(ctx, svc.ID, client.ServiceUpdateOptions{Version: svc.Version, Spec: scheduleSpec(svc, s)})
	if err != nil {
		return fmt.Errorf("update agent service: %w", err)
	}
	return nil
}

// describeSchedule renders a schedule for `prune-schedule show` and the TUI
// preview: when it runs, what it prunes, and the next few runs.
func describeSchedule(s prunesched.Schedule, now time.Time, upcoming int) string {
	var b strings.Builder
	state := "disabled"
	if s.Enabled {
		state = "enabled"
	}
	fmt.Fprintf(&b, "schedule:          %s\n", state)
	fmt.Fprintf(&b, "days:              %s\n", orDash(prunesched.FormatDays(s.Days)))
	fmt.Fprintf(&b, "runs at:           %s (%s)\n", strings.Join(runTimes(s), ", "), s.TZ)
	fmt.Fprintf(&b, "categories:        %s\n", strings.Join(s.Categories, ", "))
	fmt.Fprintf(&b, "containers kept:   %s after they stop\n", prunesched.FormatUntil(s.ContainersUntil))
	if len(s.LabelFilters) > 0 {
		fmt.Fprintf(&b, "only labelled:     %s\n", strings.Join(s.LabelFilters, ", "))
	}
	if s.Enabled && upcoming > 0 {
		next := s.Upcoming(now, upcoming)
		if len(next) == 0 {
			b.WriteString("next runs:         none (no day selected)\n")
		}
		for i, t := range next {
			label := "next runs:"
			if i > 0 {
				label = ""
			}
			fmt.Fprintf(&b, "%-18s %s\n", label, t.Format("Mon 2006-01-02 15:04 MST"))
		}
	}
	return b.String()
}

func runTimes(s prunesched.Schedule) []string {
	var out []string
	for _, m := range s.TimesOfDay() {
		out = append(out, prunesched.FormatClock(m))
	}
	return out
}

type pruneScheduleSetFlags struct {
	days, start, tz, categories, until, labelFilter string
	times                                           int
	disable, yes                                    bool
}

func newPruneScheduleCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prune-schedule",
		Short: "Show or set the cluster-wide prune schedule the agents run",
		Long: "The agents can prune their node on a schedule: stopped containers, unused\n" +
			"images, build cache and unused local networks by default; volumes only when\n" +
			"added explicitly. The schedule is stored as container labels on the agent\n" +
			"service (swarmexec.prune.*), so setting it restarts the agents one by one.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the schedule as the agent service carries it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmdContext(cmd)
			out := cmd.OutOrStdout()
			dcli, err := newDockerClient(ctx, g.dockerContext)
			if err != nil {
				return &cliError{code: usageExitCode, err: err}
			}
			svc, err := findAgentService(ctx, dcli)
			if err != nil {
				return &cliError{code: session.TransportFailure, err: err}
			}
			s, perr := agentSchedule(svc)
			if perr != nil {
				fmt.Fprintf(out, "the agent service's schedule labels are INVALID, so the agents run no schedule:\n  %v\n\n", perr)
			}
			fmt.Fprint(out, describeSchedule(s, time.Now(), 5))
			return nil
		},
	})

	f := &pruneScheduleSetFlags{}
	set := &cobra.Command{
		Use:   "set",
		Short: "Set (and enable) the schedule; flags not given keep their current value",
		Example: "  swarmexec prune-schedule set --days mon-fri --start 03:00 --times 2 --tz Europe/Berlin\n" +
			"  swarmexec prune-schedule set --categories containers,images,buildcache,networks,volumes\n" +
			"  swarmexec prune-schedule set --disable",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPruneScheduleSet(cmd, g, f)
		},
	}
	fl := set.Flags()
	fl.StringVar(&f.days, "days", "", "days to run: mon,tue,… or ranges like mon-fri (default every day)")
	fl.StringVar(&f.start, "start", "", "first run of the day, HH:MM (default 00:00)")
	fl.IntVar(&f.times, "times", 0, fmt.Sprintf("runs per day, the day split evenly from --start (1-%d)", prunesched.MaxTimesPerDay))
	fl.StringVar(&f.tz, "tz", "", "IANA time zone for --days and --start (default UTC)")
	fl.StringVar(&f.categories, "categories", "", "comma-separated: containers, images, dangling-images, buildcache, networks, volumes")
	fl.StringVar(&f.until, "containers-until", "", "keep stopped containers this long after they stop (default 24h; 0 keeps none)")
	fl.StringVar(&f.labelFilter, "label-filter", "", "only prune objects with these labels (key or key=value, comma-separated); \"-\" clears")
	fl.BoolVar(&f.disable, "disable", false, "turn the schedule off (keeps the other settings)")
	fl.BoolVarP(&f.yes, "yes", "y", false, "do not prompt before restarting the agents")
	cmd.AddCommand(set)
	return cmd
}

// applyScheduleFlags turns the given flags into a schedule over the current
// one. Pure, so what each flag does is testable without a cluster.
func applyScheduleFlags(cur prunesched.Schedule, f *pruneScheduleSetFlags, changed func(string) bool) (prunesched.Schedule, error) {
	s := cur
	s.Enabled = !f.disable
	var errs []error
	if changed("days") {
		d, err := prunesched.ParseDays(f.days)
		errs = append(errs, err)
		s.Days = d
	}
	if changed("start") {
		m, err := prunesched.ParseClock(f.start)
		errs = append(errs, err)
		s.Start = m
	}
	if changed("times") {
		s.Times = f.times
	}
	if changed("tz") {
		s.TZ = f.tz
	}
	if changed("categories") {
		c, err := prunesched.ParseCategories(f.categories)
		errs = append(errs, err)
		s.Categories = c
	}
	if changed("containers-until") {
		d, err := prunesched.ParseUntil(f.until)
		errs = append(errs, err)
		s.ContainersUntil = d
	}
	if changed("label-filter") {
		if strings.TrimSpace(f.labelFilter) == "-" {
			s.LabelFilters = nil
		} else {
			lf, err := prunesched.ParseLabelFilters(f.labelFilter)
			errs = append(errs, err)
			s.LabelFilters = lf
		}
	}
	if err := errors.Join(errs...); err != nil {
		return cur, err
	}
	return s, s.Validate()
}

func runPruneScheduleSet(cmd *cobra.Command, g *globalFlags, f *pruneScheduleSetFlags) error {
	ctx := cmdContext(cmd)
	out := cmd.OutOrStdout()
	tctx, cerr := resolveContext(out, g)
	if cerr != nil {
		return cerr
	}
	dcli, err := newDockerClient(ctx, tctx.Name)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	svc, err := findAgentService(ctx, dcli)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	// A current spec with broken labels is no reason to refuse: the defaults
	// stand in for it, and this save is what repairs it.
	cur, perr := agentSchedule(svc)
	if perr != nil {
		fmt.Fprintf(out, "note: the current schedule labels are invalid (%v); starting from the defaults\n", perr)
		cur = prunesched.Default()
	}
	s, err := applyScheduleFlags(cur, f, cmd.Flags().Changed)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	fmt.Fprintf(out, "agent service %q on context %q:\n\n", svc.Spec.Name, tctx.Name)
	fmt.Fprint(out, describeSchedule(s, time.Now(), 3))
	fmt.Fprintf(out, "\n%s\n", scheduleRestartWarning)
	if !f.yes && !confirm("Save?") {
		fmt.Fprintln(out, "aborted (non-interactive runs need --yes)")
		return &cliError{code: session.TransportFailure, silent: true}
	}
	if err := saveAgentSchedule(ctx, dcli, svc, s); err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	fmt.Fprintln(out, "✓ saved — the agents pick it up as they restart")
	return nil
}
