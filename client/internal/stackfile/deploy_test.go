// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

// Characterization of deploySection: each "deploy:" key on its own, and
// that an all-default spec renders no deploy section at all.
func TestDeploySectionFields(t *testing.T) {
	three := uint64(3)
	cases := []struct {
		name string
		edit func(*swarm.ServiceSpec)
		want *Deploy
	}{
		{"nothing set", func(*swarm.ServiceSpec) {}, nil},
		{"global", func(s *swarm.ServiceSpec) { s.Mode.Global = &swarm.GlobalService{} }, &Deploy{Mode: "global"}},
		{"replicas", func(s *swarm.ServiceSpec) { s.Mode.Replicated = &swarm.ReplicatedService{Replicas: &three} },
			&Deploy{Replicas: &three}},
		{"replicated without count", func(s *swarm.ServiceSpec) { s.Mode.Replicated = &swarm.ReplicatedService{} }, nil},
		{"labels, stack ones dropped", func(s *swarm.ServiceSpec) {
			s.Labels = map[string]string{"com.docker.stack.namespace": "st", "tier": "web"}
		}, &Deploy{Labels: map[string]string{"tier": "web"}}},
		{"only stack labels", func(s *swarm.ServiceSpec) {
			s.Labels = map[string]string{"com.docker.stack.namespace": "st"}
		}, nil},
		{"endpoint mode dnsrr", func(s *swarm.ServiceSpec) { s.EndpointSpec = &swarm.EndpointSpec{Mode: swarm.ResolutionModeDNSRR} },
			&Deploy{EndpointMode: "dnsrr"}},
		{"endpoint mode default", func(s *swarm.ServiceSpec) { s.EndpointSpec = &swarm.EndpointSpec{Mode: swarm.ResolutionModeVIP} }, nil},
		{"endpoint mode unset", func(s *swarm.ServiceSpec) { s.EndpointSpec = &swarm.EndpointSpec{} }, nil},
		{"placement", func(s *swarm.ServiceSpec) {
			s.TaskTemplate.Placement = &swarm.Placement{Constraints: []string{"z==1", "a==1"}, MaxReplicas: 2,
				Preferences: []swarm.PlacementPreference{{Spread: &swarm.SpreadOver{SpreadDescriptor: "node.labels.b"}}, {},
					{Spread: &swarm.SpreadOver{SpreadDescriptor: "node.labels.a"}}}}
		}, &Deploy{Placement: &Placement{Constraints: []string{"a==1", "z==1"}, MaxReplicas: 2,
			Preferences: []*PlacementPreference{{Spread: "node.labels.b"}, {Spread: "node.labels.a"}}}}},
		{"max replicas only", func(s *swarm.ServiceSpec) { s.TaskTemplate.Placement = &swarm.Placement{MaxReplicas: 1} },
			&Deploy{Placement: &Placement{MaxReplicas: 1}}},
		{"empty placement", func(s *swarm.ServiceSpec) { s.TaskTemplate.Placement = &swarm.Placement{} }, nil},
		{"limits", func(s *swarm.ServiceSpec) {
			s.TaskTemplate.Resources = &swarm.ResourceRequirements{Limits: &swarm.Limit{NanoCPUs: 500_000_000, MemoryBytes: 64 << 20, Pids: 100}}
		}, &Deploy{Resources: &Resources{Limits: &ResourceSet{CPUs: "0.5", Memory: "64M", PIDs: 100}}}},
		{"reservations", func(s *swarm.ServiceSpec) {
			s.TaskTemplate.Resources = &swarm.ResourceRequirements{Reservations: &swarm.Resources{NanoCPUs: 2_000_000_000}}
		}, &Deploy{Resources: &Resources{Reservations: &ResourceSet{CPUs: "2"}}}},
		{"zero resources", func(s *swarm.ServiceSpec) {
			s.TaskTemplate.Resources = &swarm.ResourceRequirements{Limits: &swarm.Limit{}, Reservations: &swarm.Resources{}}
		}, nil},
		{"restart policy", func(s *swarm.ServiceSpec) {
			s.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionOnFailure}
		}, &Deploy{RestartPolicy: &RestartPolicy{Condition: "on-failure"}}},
		{"update config", func(s *swarm.ServiceSpec) { s.UpdateConfig = &swarm.UpdateConfig{Parallelism: 4} },
			&Deploy{UpdateConfig: updateConfig(&swarm.UpdateConfig{Parallelism: 4})}},
		{"rollback config", func(s *swarm.ServiceSpec) { s.RollbackConfig = &swarm.UpdateConfig{Parallelism: 4} },
			&Deploy{Rollback: updateConfig(&swarm.UpdateConfig{Parallelism: 4})}},
	}
	for _, c := range cases {
		got := deploySection(specWith(c.edit))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}
