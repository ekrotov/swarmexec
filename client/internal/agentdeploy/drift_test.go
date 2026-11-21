// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentdeploy

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"swarmexec/internal/deploy"
)

func deployed(spec swarm.ServiceSpec) swarm.Service { return swarm.Service{Spec: spec} }

func initSpec() swarm.ServiceSpec {
	return Spec(Options{
		Image:       "logleio/swarmexec-agent:1.2.3",
		ServiceName: deploy.ServiceName,
		Port:        deploy.DefaultPort,
	}, "sec-1")
}

// The spec `init` writes must not drift from itself. If this fails, Drift and
// Spec have grown apart and every cluster in the fleet is about to be told it
// is broken.
func TestDriftAcceptsWhatInitWrites(t *testing.T) {
	if d := Drift(deployed(initSpec())); len(d) != 0 {
		t.Fatalf("init's own spec reported as drifted: %v", d)
	}
}

// The regression this check exists for: the /sys mount that v1.30.0 added was
// missing on four clusters and every other check passed.
func TestDriftFindsAMissingSysfsMount(t *testing.T) {
	spec := initSpec()
	cs := spec.TaskTemplate.ContainerSpec
	var kept []mount.Mount
	for _, m := range cs.Mounts {
		if m.Source != deploy.HostSysfsPath {
			kept = append(kept, m)
		}
	}
	cs.Mounts = kept

	d := Drift(deployed(spec))
	if len(d) != 1 {
		t.Fatalf("got %d findings, want 1: %v", len(d), d)
	}
	if !strings.Contains(d[0].Field, deploy.HostSysfsPath) || !strings.Contains(d[0].Got, "nothing") {
		t.Errorf("finding reads %q", d[0])
	}
}

// A sysfs mount that is there but writable is not the mount we asked for:
// read-only is the whole reason it was acceptable to mount host state at all.
func TestDriftFindsAWritableSysfsMount(t *testing.T) {
	spec := initSpec()
	cs := spec.TaskTemplate.ContainerSpec
	for i := range cs.Mounts {
		if cs.Mounts[i].Source == deploy.HostSysfsPath {
			cs.Mounts[i].ReadOnly = false
		}
	}
	d := Drift(deployed(spec))
	if len(d) != 1 || !strings.Contains(d[0].Got, "/host/sys") {
		t.Fatalf("got %v", d)
	}
	if strings.Contains(d[0].Got, "read-only") {
		t.Errorf("a writable mount was described as read-only: %q", d[0])
	}
}

// The three policy choices that make the agent a per-node service which comes
// back, each reported on its own.
func TestDriftFindsModeAndPolicies(t *testing.T) {
	spec := initSpec()
	spec.Mode = swarm.ServiceMode{Replicated: &swarm.ReplicatedService{}}
	spec.TaskTemplate.RestartPolicy = nil
	spec.UpdateConfig = nil

	fields := map[string]string{}
	for _, f := range Drift(deployed(spec)) {
		fields[f.Field] = f.Got
	}
	for _, want := range []string{"mode", "restart condition", "update order"} {
		if _, ok := fields[want]; !ok {
			t.Errorf("no finding for %q; got %v", want, fields)
		}
	}
	if fields["mode"] != "replicated" {
		t.Errorf("mode Got = %q, want replicated", fields["mode"])
	}
}

// An mTLS cluster from agent/deploy/agent-stack.yml carries swarmexec_ca /
// _cert / _key and never our shared secret. Reporting that as drift would
// flag every manually deployed cluster, which is how a check gets ignored.
func TestDriftLeavesMTLSCredentialsAlone(t *testing.T) {
	spec := initSpec()
	spec.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{
		{SecretName: "swarmexec_ca"}, {SecretName: "swarmexec_cert"}, {SecretName: "swarmexec_key"},
	}
	if d := Drift(deployed(spec)); len(d) != 0 {
		t.Fatalf("mTLS deployment reported as drifted: %v", d)
	}
}

// No credentials at all is wrong under either deployment model.
func TestDriftFindsNoSecretsAtAll(t *testing.T) {
	spec := initSpec()
	spec.TaskTemplate.ContainerSpec.Secrets = nil
	d := Drift(deployed(spec))
	if len(d) != 1 || d[0].Field != "secrets" || d[0].Got != "none" {
		t.Fatalf("got %v", d)
	}
}

// Our secret mounted under a different file name is a real break: the agent
// is told one path and reads nothing there.
func TestDriftFindsRenamedSecretFile(t *testing.T) {
	spec := initSpec()
	spec.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{{
		SecretName: deploy.SecretName,
		File:       &swarm.SecretReferenceFileTarget{Name: "something_else"},
	}}
	d := Drift(deployed(spec))
	if len(d) != 1 || !strings.Contains(d[0].Field, deploy.SecretName) {
		t.Fatalf("got %v", d)
	}
}

// A service without a container spec says so once, rather than listing every
// check it could not run.
func TestDriftWithoutAContainerSpec(t *testing.T) {
	d := Drift(swarm.Service{})
	if len(d) != 1 || d[0].Field != "container" {
		t.Fatalf("got %v", d)
	}
}

// The hint names what `init --force` costs, so nobody runs it on a service
// they hand-edited without knowing.
func TestDriftHintNamesTheCost(t *testing.T) {
	if !strings.Contains(DriftHint, "init --force") || !strings.Contains(DriftHint, "hand edits") {
		t.Errorf("DriftHint = %q", DriftHint)
	}
}
