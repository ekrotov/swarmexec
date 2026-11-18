// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"fmt"
	"strings"
	"time"

	"swarmexec/client/internal/agenterr"
	"swarmexec/internal/deploy"
	"swarmexec/internal/dockerfmt"
	"swarmexec/internal/prunesched"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// The cluster-wide prune schedule is a set of CONTAINER labels on the agent
// service (internal/prunesched explains why there). Editing it is therefore a
// service update of the agent, which restarts every agent one by one — the
// warning both the TUI and this command show before saving.

// ScheduleRestartWarning is shown before every save.
const ScheduleRestartWarning = "Saving updates the agent service: the agents restart one by one, " +
	"and open shells, log streams and port-forwards on a restarting node drop."

// AgentServiceAPI is the slice of the manager API the schedule editor needs,
// so the label round trip is testable without a daemon.
type AgentServiceAPI interface {
	ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error)
	ServiceUpdate(context.Context, string, client.ServiceUpdateOptions) (client.ServiceUpdateResult, error)
}

// FindAgentService finds the deployed agent service: by the role label init
// stamps on it, else by its image (an agent deployed from a stack file without
// the label).
func FindAgentService(ctx context.Context, dcli AgentServiceAPI) (*swarm.Service, error) {
	res, err := dcli.ServiceList(ctx, client.ServiceListOptions{
		Filters: make(client.Filters).Add("label", deploy.RoleLabel+"="+deploy.RoleAgent),
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
		if cs := all.Items[i].Spec.TaskTemplate.ContainerSpec; cs != nil && IsAgentImage(cs.Image) {
			return &all.Items[i], nil
		}
	}
	return nil, agenterr.ErrNoAgent
}

// AgentSchedule reads the schedule from the agent service. A spec whose labels
// do not parse comes back with the error, so the editor can show what is wrong
// and save a valid one over it.
func AgentSchedule(svc *swarm.Service) (prunesched.Schedule, error) {
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

// SaveAgentSchedule writes s onto the agent service. Only the container
// labels change; the image digest stays (no registry query), so this never
// sneaks an agent upgrade in with a schedule change.
func SaveAgentSchedule(ctx context.Context, dcli AgentServiceAPI, svc *swarm.Service, s prunesched.Schedule) error {
	if err := s.Validate(); err != nil {
		return err
	}
	_, err := dcli.ServiceUpdate(ctx, svc.ID, client.ServiceUpdateOptions{Version: svc.Version, Spec: scheduleSpec(svc, s)})
	if err != nil {
		return fmt.Errorf("update agent service: %w", err)
	}
	return nil
}

// DescribeSchedule renders a schedule for `prune-schedule show` and the TUI
// preview: when it runs, what it prunes, and the next few runs.
func DescribeSchedule(s prunesched.Schedule, now time.Time, upcoming int) string {
	var b strings.Builder
	state := "disabled"
	if s.Enabled {
		state = "enabled"
	}
	fmt.Fprintf(&b, "schedule:          %s\n", state)
	fmt.Fprintf(&b, "days:              %s\n", dockerfmt.OrDash(prunesched.FormatDays(s.Days)))
	fmt.Fprintf(&b, "runs at:           %s (%s)\n", strings.Join(RunTimes(s), ", "), s.TZ)
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

func RunTimes(s prunesched.Schedule) []string {
	var out []string
	for _, m := range s.TimesOfDay() {
		out = append(out, prunesched.FormatClock(m))
	}
	return out
}
