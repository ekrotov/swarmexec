// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/session"
	"swarmexec/client/internal/swarmops"
	"swarmexec/internal/prunesched"
)

type pruneScheduleSetFlags struct {
	days, start, tz, categories, until, labelFilter string
	times                                           int
	disable, yes                                    bool
}

// newPruneScheduleCmd is `node prune-schedule`; path is the command path its
// examples are written with. The top-level `prune-schedule` it used to be is
// the same command, hidden (review P-02), so scripts keep working.
func newPruneScheduleCmd(g *globalFlags, path string) *cobra.Command {
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
			svc, err := swarmops.FindAgentService(ctx, dcli)
			if err != nil {
				return &cliError{code: session.TransportFailure, err: err}
			}
			s, perr := swarmops.AgentSchedule(svc)
			if perr != nil {
				fmt.Fprintf(out, "the agent service's schedule labels are INVALID, so the agents run no schedule:\n  %v\n\n", perr)
			}
			fmt.Fprint(out, swarmops.DescribeSchedule(s, time.Now(), 5))
			return nil
		},
	})

	f := &pruneScheduleSetFlags{}
	set := &cobra.Command{
		Use:   "set",
		Short: "Set (and enable) the schedule; flags not given keep their current value",
		Example: "  " + path + " set --days mon-fri --start 03:00 --times 2 --tz Europe/Berlin\n" +
			"  " + path + " set --categories containers,images,buildcache,networks,volumes\n" +
			"  " + path + " set --disable",
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

// newHiddenPruneScheduleCmd is the old top-level `prune-schedule`: it still
// works, out of the help.
func newHiddenPruneScheduleCmd(g *globalFlags) *cobra.Command {
	cmd := newPruneScheduleCmd(g, "swarmexec node prune-schedule")
	cmd.Hidden = true
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
	svc, err := swarmops.FindAgentService(ctx, dcli)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	// A current spec with broken labels is no reason to refuse: the defaults
	// stand in for it, and this save is what repairs it.
	cur, perr := swarmops.AgentSchedule(svc)
	if perr != nil {
		fmt.Fprintf(out, "note: the current schedule labels are invalid (%v); starting from the defaults\n", perr)
		cur = prunesched.Default()
	}
	s, err := applyScheduleFlags(cur, f, cmd.Flags().Changed)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	fmt.Fprintf(out, "agent service %q on context %q:\n\n", svc.Spec.Name, tctx.Name)
	fmt.Fprint(out, swarmops.DescribeSchedule(s, time.Now(), 3))
	fmt.Fprintf(out, "\n%s\n", swarmops.ScheduleRestartWarning)
	if !f.yes && !confirm("Save?") {
		fmt.Fprintln(out, "aborted (non-interactive runs need --yes)")
		return &cliError{code: session.TransportFailure, silent: true}
	}
	if err := swarmops.SaveAgentSchedule(ctx, dcli, svc, s); err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	fmt.Fprintln(out, "✓ saved — the agents pick it up as they restart")
	return nil
}
