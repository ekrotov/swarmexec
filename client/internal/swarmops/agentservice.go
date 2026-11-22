// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"fmt"
	"strings"

	"swarmexec/client/internal/swarmapi"
	"swarmexec/internal/deploy"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// ServiceByName returns the service with the given name, or nil.
func ServiceByName(ctx context.Context, dcli swarmapi.Client, name string) (*swarm.Service, error) {
	listRes, err := dcli.ServiceList(ctx, client.ServiceListOptions{
		Filters: make(client.Filters).Add("name", name),
	})
	list := listRes.Items
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	for i := range list {
		if list[i].Spec.Name == name {
			return &list[i], nil
		}
	}
	return nil, nil
}

// AgentDeployed reports whether a swarmexec agent service exists in the swarm —
// either tagged with our role label or recognizable by its image. Used to turn
// "can't reach an agent" into the actionable "run swarmexec init" hint.
// ServiceLister is the slice of the Docker manager API AgentDeployed needs (so
// it is unit-testable without a real client).
type ServiceLister interface {
	ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error)
}

func AgentDeployed(ctx context.Context, dcli ServiceLister) bool {
	if list, err := dcli.ServiceList(ctx, client.ServiceListOptions{
		Filters: make(client.Filters).Add("label", deploy.RoleLabel+"="+deploy.RoleAgent),
	}); err == nil && len(list.Items) > 0 {
		return true
	}
	listRes, err := dcli.ServiceList(ctx, client.ServiceListOptions{})
	list := listRes.Items
	if err != nil {
		return true // can't tell — don't claim the agent is missing
	}
	for _, s := range list {
		if cs := s.Spec.TaskTemplate.ContainerSpec; cs != nil && IsAgentImage(cs.Image) {
			return true
		}
	}
	return false
}

// IsAgentImage reports whether an image reference looks like the swarmexec
// agent. Label matching is the primary signal (agentServiceSpec always sets
// swarmexec.role=agent); this is the fallback for services deployed without
// that label. It matches both the Docker Hub name (logleio/swarmexec-agent)
// and the older GitLab-registry name (…/swarm-remote-exec/agent), so a cluster
// mid-migration is still recognized whichever image it runs.
func IsAgentImage(image string) bool {
	return strings.Contains(image, "swarmexec-agent") ||
		strings.Contains(image, "swarm-remote-exec/agent")
}

// AgentService returns the deployed agent service, or nil when none is found.
// It looks by role label first and falls back to the image, the same two
// signals in the same order as AgentDeployed — a cluster deployed before the
// label existed, or by hand from the stack file, is still recognized.
func AgentService(ctx context.Context, dcli ServiceLister) *swarm.Service {
	if list, err := dcli.ServiceList(ctx, client.ServiceListOptions{
		Filters: make(client.Filters).Add("label", deploy.RoleLabel+"="+deploy.RoleAgent),
	}); err == nil && len(list.Items) > 0 {
		return &list.Items[0]
	}
	listRes, err := dcli.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return nil
	}
	list := listRes.Items
	for i := range list {
		if cs := list[i].Spec.TaskTemplate.ContainerSpec; cs != nil && IsAgentImage(cs.Image) {
			return &list[i]
		}
	}
	return nil
}

// AgentServiceImage returns the image (including any pinned @sha256 digest) of
// the deployed agent service, or "" if none is found.
func AgentServiceImage(ctx context.Context, dcli ServiceLister) string {
	svc := AgentService(ctx, dcli)
	if svc == nil || svc.Spec.TaskTemplate.ContainerSpec == nil {
		return ""
	}
	return svc.Spec.TaskTemplate.ContainerSpec.Image
}
