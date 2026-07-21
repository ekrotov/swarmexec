// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
)

type fakeServiceLister struct {
	all []swarm.Service
	err error
}

func (f fakeServiceLister) ServiceList(_ context.Context, opts types.ServiceListOptions) ([]swarm.Service, error) {
	if f.err != nil {
		return nil, f.err
	}
	if labels := opts.Filters.Get("label"); len(labels) > 0 {
		var out []swarm.Service
		for _, s := range f.all {
			for _, w := range labels {
				kv := strings.SplitN(w, "=", 2)
				if len(kv) == 2 && s.Spec.Labels[kv[0]] == kv[1] {
					out = append(out, s)
				}
			}
		}
		return out, nil
	}
	return f.all, nil
}

func labeledAgent() swarm.Service {
	var s swarm.Service
	s.Spec.Labels = map[string]string{agentRoleLabel: agentRoleValue}
	return s
}

func imagedAgent(img string) swarm.Service {
	var s swarm.Service
	s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: img}
	return s
}

func svcPublishingHost(name string, port uint32) swarm.Service {
	var s swarm.Service
	s.Spec.Name = name
	s.Spec.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{
		{PublishMode: swarm.PortConfigPublishModeHost, PublishedPort: port},
	}}
	return s
}

func TestPortConflict(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		services []swarm.Service
		wantSvc  string
		wantOk   bool
	}{
		{"conflict", []swarm.Service{svcPublishingHost("agent_agent", 9443)}, "agent_agent", true},
		{"own-service-ignored", []swarm.Service{svcPublishingHost("swarmexec_agent", 9443)}, "", false},
		{"different-port", []swarm.Service{svcPublishingHost("other", 8080)}, "", false},
		{"none", nil, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotSvc, gotOk := portConflict(ctx, fakeServiceLister{all: c.services}, 9443, "swarmexec_agent")
			if gotSvc != c.wantSvc || gotOk != c.wantOk {
				t.Errorf("portConflict = (%q, %v), want (%q, %v)", gotSvc, gotOk, c.wantSvc, c.wantOk)
			}
		})
	}
}

func TestIsAgentImage(t *testing.T) {
	cases := map[string]bool{
		"docker.io/logleio/swarmexec-agent:latest":                   true, // Docker Hub (current default)
		"logleio/swarmexec-agent:v1.1.4":                             true,
		"registry.logle.io/cs-public/swarm-remote-exec/agent:latest": true, // legacy GitLab image, mid-migration
		"registry.x/internal-tools/swarm-remote-exec/agent:1.0":      true, // legacy, older namespace
		"nginx:latest":              false,
		"docker.io/library/redis:7": false,
	}
	for img, want := range cases {
		if got := isAgentImage(img); got != want {
			t.Errorf("isAgentImage(%q) = %v, want %v", img, got, want)
		}
	}
}

func TestAgentDeployed(t *testing.T) {
	cases := []struct {
		name string
		f    fakeServiceLister
		want bool
	}{
		{"none", fakeServiceLister{all: nil}, false},
		{"labeled", fakeServiceLister{all: []swarm.Service{labeledAgent()}}, true},
		{"by-image-hub", fakeServiceLister{all: []swarm.Service{imagedAgent("docker.io/logleio/swarmexec-agent:latest")}}, true},
		{"by-image-gitlab-legacy", fakeServiceLister{all: []swarm.Service{imagedAgent("registry.x/cs-public/swarm-remote-exec/agent:latest")}}, true},
		{"unrelated", fakeServiceLister{all: []swarm.Service{imagedAgent("nginx:latest")}}, false},
		{"list-error-assumes-present", fakeServiceLister{err: errors.New("boom")}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentDeployed(context.Background(), c.f); got != c.want {
				t.Errorf("agentDeployed = %v, want %v", got, c.want)
			}
		})
	}
}
