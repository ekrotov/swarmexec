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

func TestAgentDeployed(t *testing.T) {
	cases := []struct {
		name string
		f    fakeServiceLister
		want bool
	}{
		{"none", fakeServiceLister{all: nil}, false},
		{"labeled", fakeServiceLister{all: []swarm.Service{labeledAgent()}}, true},
		{"by-image", fakeServiceLister{all: []swarm.Service{imagedAgent("registry.x/internal-tools/swarm-remote-exec/agent:latest")}}, true},
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
