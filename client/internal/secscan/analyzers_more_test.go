// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package secscan

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
)

// svcSpec builds a service whose container spec the caller shapes.
func svcSpec(shape func(*swarm.ServiceSpec)) swarm.Service {
	var s swarm.Service
	s.Spec.Name = "svc"
	s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "app:1.0", User: "1000"}
	// A limit by default, so tests do not trip the informational limit finding
	// unless they mean to.
	s.Spec.TaskTemplate.Resources = &swarm.ResourceRequirements{
		Limits: &swarm.Limit{NanoCPUs: 1_000_000_000},
	}
	shape(&s.Spec)
	return s
}

func TestDockerSocketAnalyzer(t *testing.T) {
	for _, src := range []string{"/var/run/docker.sock", "/run/docker.sock", "/host/run/docker.sock"} {
		svc := svcSpec(func(sp *swarm.ServiceSpec) {
			sp.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{
				{Type: mount.TypeBind, Source: src, Target: "/var/run/docker.sock"},
			}
		})
		f := hasRule(Scan(svc), "docker-socket")
		if f == nil {
			t.Errorf("%s: expected a docker-socket finding", src)
			continue
		}
		if f.Severity != SevHigh {
			t.Errorf("%s: severity = %v, want high", src, f.Severity)
		}
	}

	// Read-only must still flag — the socket is an API, not file contents.
	ro := svcSpec(func(sp *swarm.ServiceSpec) {
		sp.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{
			{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/var/run/docker.sock", ReadOnly: true},
		}
	})
	f := hasRule(Scan(ro), "docker-socket")
	if f == nil {
		t.Fatal("a read-only socket mount must still be flagged")
	}
	if !strings.Contains(f.Detail, "read-only does not help") {
		t.Errorf("detail should explain why read-only is no mitigation: %q", f.Detail)
	}

	// An ordinary bind mount and a volume named like the socket must not flag.
	for _, m := range []mount.Mount{
		{Type: mount.TypeBind, Source: "/srv/data", Target: "/data"},
		{Type: mount.TypeVolume, Source: "docker.sock", Target: "/x"},
	} {
		clean := svcSpec(func(sp *swarm.ServiceSpec) { sp.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{m} })
		if hasRule(Scan(clean), "docker-socket") != nil {
			t.Errorf("unexpected docker-socket finding for %+v", m)
		}
	}
}

func TestCapabilityAnalyzer(t *testing.T) {
	cases := []struct {
		caps    []string
		wantSev Severity
		wantAny bool
	}{
		{[]string{"CAP_SYS_ADMIN"}, SevHigh, true},
		{[]string{"sys_admin"}, SevHigh, true}, // lower case, no prefix
		{[]string{"ALL"}, SevHigh, true},
		{[]string{"NET_RAW"}, SevHigh, true},
		{[]string{"CHOWN"}, SevMedium, true}, // benign-ish, still beyond default
		{nil, 0, false},
		{[]string{"  "}, 0, false},
	}
	for _, c := range cases {
		svc := svcSpec(func(sp *swarm.ServiceSpec) { sp.TaskTemplate.ContainerSpec.CapabilityAdd = c.caps })
		f := hasRule(Scan(svc), "added-capability")
		if !c.wantAny {
			if f != nil {
				t.Errorf("caps %v: unexpected finding %+v", c.caps, *f)
			}
			continue
		}
		if f == nil {
			t.Errorf("caps %v: expected a finding", c.caps)
			continue
		}
		if f.Severity != c.wantSev {
			t.Errorf("caps %v: severity = %v, want %v", c.caps, f.Severity, c.wantSev)
		}
	}
}

func TestHostNetworkAnalyzer(t *testing.T) {
	on := svcSpec(func(sp *swarm.ServiceSpec) {
		sp.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "host"}}
	})
	if f := hasRule(Scan(on), "host-network"); f == nil || f.Severity != SevHigh {
		t.Errorf("host network should be a high finding, got %+v", f)
	}
	off := svcSpec(func(sp *swarm.ServiceSpec) {
		sp.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "frontend"}}
	})
	if hasRule(Scan(off), "host-network") != nil {
		t.Error("an ordinary overlay network must not flag")
	}
}

func TestConfinementAnalyzer(t *testing.T) {
	sec := svcSpec(func(sp *swarm.ServiceSpec) {
		sp.TaskTemplate.ContainerSpec.Privileges = &swarm.Privileges{
			Seccomp: &swarm.SeccompOpts{Mode: swarm.SeccompModeUnconfined},
		}
	})
	if f := hasRule(Scan(sec), "unconfined"); f == nil || f.Severity != SevHigh {
		t.Errorf("unconfined seccomp should be high, got %+v", f)
	}

	// The default mode must not flag.
	def := svcSpec(func(sp *swarm.ServiceSpec) {
		sp.TaskTemplate.ContainerSpec.Privileges = &swarm.Privileges{
			Seccomp: &swarm.SeccompOpts{Mode: swarm.SeccompModeDefault},
		}
	})
	if hasRule(Scan(def), "unconfined") != nil {
		t.Error("default seccomp must not flag")
	}
	// A nil Privileges must not panic.
	if hasRule(Scan(svcSpec(func(*swarm.ServiceSpec) {})), "unconfined") != nil {
		t.Error("no Privileges set must not flag")
	}
}

// The two informational analyzers must never mark a service on their own —
// nearly every service in a real cluster trips both.
func TestInformationalAnalyzersDoNotFlag(t *testing.T) {
	bare := svcSpec(func(sp *swarm.ServiceSpec) {
		sp.TaskTemplate.ContainerSpec.Image = "nginx:latest"
		sp.TaskTemplate.Resources = nil
	})
	fs := Scan(bare)
	if hasRule(fs, "no-resource-limits") == nil {
		t.Error("expected a no-resource-limits finding")
	}
	if hasRule(fs, "unpinned-image") == nil {
		t.Error("expected an unpinned-image finding")
	}
	if Actionable(fs) {
		t.Errorf("informational findings must not mark the service: %+v", fs)
	}
}

func TestImagePinAnalyzer(t *testing.T) {
	cases := []struct {
		image string
		want  bool
	}{
		{"nginx:1.27", false},
		{"nginx:latest", true},
		{"nginx", true},
		{"registry:5000/team/app:2.1", false},
		{"registry:5000/team/app", true},   // host:port is not a tag
		{"nginx@sha256:abc", false},        // digest-pinned
		{"nginx:latest@sha256:abc", false}, // digest wins over the tag
	}
	for _, c := range cases {
		svc := svcSpec(func(sp *swarm.ServiceSpec) { sp.TaskTemplate.ContainerSpec.Image = c.image })
		got := hasRule(Scan(svc), "unpinned-image") != nil
		if got != c.want {
			t.Errorf("image %q: flagged=%v, want %v", c.image, got, c.want)
		}
	}
}

func TestResourceLimitAnalyzer(t *testing.T) {
	cases := []struct {
		name string
		res  *swarm.ResourceRequirements
		want bool
	}{
		{"no resources at all", nil, true},
		{"reservations only", &swarm.ResourceRequirements{
			Reservations: &swarm.Resources{NanoCPUs: 1}}, true},
		{"empty limits", &swarm.ResourceRequirements{Limits: &swarm.Limit{}}, true},
		{"cpu limit", &swarm.ResourceRequirements{Limits: &swarm.Limit{NanoCPUs: 1}}, false},
		{"memory limit", &swarm.ResourceRequirements{Limits: &swarm.Limit{MemoryBytes: 1}}, false},
	}
	for _, c := range cases {
		svc := svcSpec(func(sp *swarm.ServiceSpec) { sp.TaskTemplate.Resources = c.res })
		got := hasRule(Scan(svc), "no-resource-limits") != nil
		if got != c.want {
			t.Errorf("%s: flagged=%v, want %v", c.name, got, c.want)
		}
	}
}

// No analyzer may leak a secret value; the whole set runs over a service that
// carries one in several places.
func TestNoAnalyzerLeaksValues(t *testing.T) {
	svc := svcSpec(func(sp *swarm.ServiceSpec) {
		sp.TaskTemplate.ContainerSpec.Env = []string{"DB_PASSWORD=hunter2"}
		sp.TaskTemplate.ContainerSpec.CapabilityAdd = []string{"SYS_ADMIN"}
	})
	for _, f := range Scan(svc) {
		if strings.Contains(f.Detail, "hunter2") || strings.Contains(f.Title, "hunter2") {
			t.Errorf("finding %q leaks a secret value: %+v", f.Rule, f)
		}
	}
}
