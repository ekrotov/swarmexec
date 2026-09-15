// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
)

func specWith(f func(*swarm.ServiceSpec)) swarm.ServiceSpec {
	spec := swarm.ServiceSpec{
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "nginx:1.27"}},
	}
	f(&spec)
	return spec
}

// The prefix may only come off something the stack OWNS. A cluster can hold a
// secret called "postgres_password" that nobody created as part of the
// "postgres" stack; trimming it anyway renames it to "password", and an
// exported file then references a secret that does not exist. Found by
// exporting a real stack and reading it back.
func TestNamespaceStrippedOnlyFromOwnedObjects(t *testing.T) {
	names := Names{
		Namespace: "postgres",
		Owned:     map[string]bool{"postgres_owned": true},
	}
	spec := specWith(func(s *swarm.ServiceSpec) {
		s.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{
			{SecretName: "postgres_owned"},   // created by the stack
			{SecretName: "postgres_foreign"}, // merely named like it
		}
	})
	got := ServiceFromSpec("postgres", spec, names)

	var sources []string
	for _, m := range got.Secrets {
		sources = append(sources, m.Source)
	}
	want := []string{"owned", "postgres_foreign"} // sorted: the owned one lost its prefix
	if strings.Join(sources, ",") != strings.Join(want, ",") {
		t.Errorf("secret sources = %v, want %v", sources, want)
	}
}

// A deployed service records a network by ID; a stack file names it. Without
// resolution every service would differ on every network — which is the exact
// false positive this package exists to prevent.
func TestNetworkIDsResolveToNames(t *testing.T) {
	names := Names{
		Namespace:   "web",
		NetworkName: func(id string) string { return map[string]string{"abc123": "web_front"}[id] },
		Owned:       map[string]bool{"web_front": true},
	}
	spec := specWith(func(s *swarm.ServiceSpec) {
		s.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "abc123", Aliases: []string{"api"}}}
	})
	got := ServiceFromSpec("web", spec, names)
	if _, ok := got.Networks["front"]; !ok {
		t.Errorf("networks = %v, want the id resolved and the prefix dropped", sortedKeys(got.Networks))
	}
}

// Docker's converter appends the service's own name to its network aliases,
// so an exported file gained a duplicate every time it was read back.
func TestAliasesAreDeduplicated(t *testing.T) {
	spec := specWith(func(s *swarm.ServiceSpec) {
		s.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{
			{Target: "net", Aliases: []string{"api", "api", "web"}},
		}
	})
	got := ServiceFromSpec("s", spec, Names{Namespace: "s"})
	if a := got.Networks["net"].Aliases; len(a) != 2 || a[0] != "api" || a[1] != "web" {
		t.Errorf("aliases = %v, want each once and sorted", a)
	}
}

// The engine stores "IP hostname"; compose writes "hostname:IP". Exporting the
// engine's spelling made extra_hosts vanish on reload.
func TestExtraHostsUseComposeSpelling(t *testing.T) {
	spec := specWith(func(s *swarm.ServiceSpec) {
		s.TaskTemplate.ContainerSpec.Hosts = []string{"127.0.0.1 cloud.example.com", "10.0.0.5 a b"}
	})
	got := ServiceFromSpec("s", spec, Names{Namespace: "s"})
	want := []string{"a:10.0.0.5", "b:10.0.0.5", "cloud.example.com:127.0.0.1"}
	if strings.Join(got.ExtraHosts, ",") != strings.Join(want, ",") {
		t.Errorf("extra_hosts = %v, want %v", got.ExtraHosts, want)
	}
}

// The daemon resolves an image to a digest at deploy time and no stack file
// carries that, so keeping it would make every service differ forever.
func TestImageDigestIsDropped(t *testing.T) {
	spec := specWith(func(s *swarm.ServiceSpec) {
		s.TaskTemplate.ContainerSpec.Image = "nginx:1.27@sha256:" + strings.Repeat("a", 64)
	})
	if got := ServiceFromSpec("s", spec, Names{}).Image; got != "nginx:1.27" {
		t.Errorf("image = %q, want the tag without the digest", got)
	}
}

// Values the DAEMON fills in must not read as changes: a file that omits
// restart_policy and a deployment carrying the default are in sync, and a tool
// that reports that will be ignored within a day.
func TestDaemonDefaultsAreElided(t *testing.T) {
	delay := 5 * time.Second
	spec := specWith(func(s *swarm.ServiceSpec) {
		s.TaskTemplate.ContainerSpec.Isolation = "default"
		s.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{
			Condition: swarm.RestartPolicyConditionAny, Delay: &delay,
		}
		s.UpdateConfig = &swarm.UpdateConfig{Parallelism: 1, FailureAction: "pause", Order: "stop-first"}
		s.EndpointSpec = &swarm.EndpointSpec{Mode: swarm.ResolutionModeVIP}
	})
	got := ServiceFromSpec("s", spec, Names{})
	if got.Isolation != "" {
		t.Errorf("isolation = %q, want elided", got.Isolation)
	}
	if got.Deploy != nil && got.Deploy.RestartPolicy != nil {
		t.Errorf("default restart policy should be elided, got %+v", got.Deploy.RestartPolicy)
	}
	if got.Deploy != nil && got.Deploy.UpdateConfig != nil {
		t.Errorf("default update config should be elided, got %+v", got.Deploy.UpdateConfig)
	}
	if got.Deploy != nil && got.Deploy.EndpointMode != "" {
		t.Errorf("default endpoint mode should be elided, got %q", got.Deploy.EndpointMode)
	}
}

// Eliding must not swallow a value that is NOT the default — that would hide a
// real difference, which is the one thing worse than showing a false one.
func TestNonDefaultPoliciesSurvive(t *testing.T) {
	delay := 30 * time.Second
	spec := specWith(func(s *swarm.ServiceSpec) {
		s.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{
			Condition: swarm.RestartPolicyConditionOnFailure, Delay: &delay,
		}
		s.UpdateConfig = &swarm.UpdateConfig{Parallelism: 2, Order: "start-first"}
	})
	got := ServiceFromSpec("s", spec, Names{})
	rp := got.Deploy.RestartPolicy
	if rp == nil || rp.Condition != "on-failure" || rp.Delay != "30s" {
		t.Errorf("restart policy = %+v, want the non-default values kept", rp)
	}
	uc := got.Deploy.UpdateConfig
	if uc == nil || uc.Parallelism == nil || *uc.Parallelism != 2 || uc.Order != "start-first" {
		t.Errorf("update config = %+v, want the non-default values kept", uc)
	}
}

// Placement preferences are applied in order by swarm, so that one list must
// NOT be sorted — unlike every other list here.
func TestPlacementPreferencesKeepTheirOrder(t *testing.T) {
	spec := specWith(func(s *swarm.ServiceSpec) {
		s.TaskTemplate.Placement = &swarm.Placement{Preferences: []swarm.PlacementPreference{
			{Spread: &swarm.SpreadOver{SpreadDescriptor: "node.labels.zone"}},
			{Spread: &swarm.SpreadOver{SpreadDescriptor: "node.labels.rack"}},
		}}
	})
	got := ServiceFromSpec("s", spec, Names{}).Deploy.Placement
	if len(got.Preferences) != 2 ||
		got.Preferences[0].Spread != "node.labels.zone" || got.Preferences[1].Spread != "node.labels.rack" {
		t.Errorf("preferences = %+v, want the declared order preserved", got.Preferences)
	}
}

// Unmodelled is what keeps a clean diff honest: without it a stack differing
// only in a field the model ignores would compare equal and say so.
func TestUnmodelledNamesWhatIsNotCompared(t *testing.T) {
	spec := specWith(func(s *swarm.ServiceSpec) {
		s.TaskTemplate.ContainerSpec.TTY = true
		s.TaskTemplate.ContainerSpec.Privileges = &swarm.Privileges{NoNewPrivileges: true}
	})
	got := Unmodelled(spec)
	if len(got) != 2 {
		t.Fatalf("Unmodelled = %v, want both set fields named", got)
	}
	if n := Unmodelled(specWith(func(*swarm.ServiceSpec) {})); len(n) != 0 {
		t.Errorf("a plain spec should report nothing unmodelled, got %v", n)
	}
}
