// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/swarmfake"
)

// dbSecretCluster is a cluster with a templated secret "db" and one service
// mounting it.
func dbSecretCluster() *swarmfake.Swarm {
	svc := stackService()
	svc.Spec.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{{
		SecretID: "secret-db", SecretName: "db",
		File: &swarm.SecretReferenceFileTarget{Name: "db_password", UID: "0", GID: "0", Mode: 0o400},
	}}
	sec := swarm.Secret{ID: "secret-db"}
	sec.Spec.Name = "db"
	sec.Spec.Labels = map[string]string{"team": "core"}
	sec.Spec.Templating = &swarm.Driver{Name: "golang", Options: map[string]string{"k": "v"}}
	return &swarmfake.Swarm{Services: []swarm.Service{svc}, Secrets: []swarm.Secret{sec}}
}

// KOR-06: after a rotation the service's PreviousSpec names the stand-in, which
// the last step deletes. A rollback copies PreviousSpec without looking, so it
// would start tasks on a secret that is gone — and the old value is gone too.
// RollbackService must say so instead, and write nothing.
func TestRollbackAfterRotationIsRefused(t *testing.T) {
	f := dbSecretCluster()
	d := fakeDocker(t, f)
	ctx := context.Background()
	if err := RotateSecret(ctx, d, "db", []byte("new"), RotateOptions{Poll: 1}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if len(f.Secrets) != 1 || f.Secrets[0].Spec.Name != "db" {
		t.Fatalf("secrets after rotation = %+v", f.Secrets)
	}

	// What a rollback would find: a previous spec that points at a secret
	// the cluster no longer has.
	prev := f.Services[0].PreviousSpec
	if prev == nil {
		t.Fatal("the service has no PreviousSpec after the rotation")
	}
	ref := prev.TaskTemplate.ContainerSpec.Secrets[0]
	if ref.SecretName != "db_rotating" {
		t.Fatalf("previous spec's secret = %q, want the stand-in", ref.SecretName)
	}
	for _, s := range f.Secrets {
		if s.ID == ref.SecretID {
			t.Fatalf("the stand-in %q still exists; the scenario is not the one in the finding", ref.SecretName)
		}
	}

	version := f.Services[0].Version.Index
	err := RollbackService(ctx, d, "shop_api")
	if err == nil {
		t.Fatal("rollback to a deleted secret was sent")
	}
	for _, want := range []string{"shop_api", `secret "db_rotating"`, "nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if f.Services[0].Version.Index != version {
		t.Errorf("a refused rollback updated the service (version %d -> %d)", version, f.Services[0].Version.Index)
	}
}

// A rollback whose referenced secrets and configs all exist still goes through,
// and one naming a missing config is refused like a missing secret.
func TestRollbackChecksSecretsAndConfigs(t *testing.T) {
	tests := []struct {
		name    string
		prev    func(cs *swarm.ContainerSpec)
		wantErr string // "" = rollback allowed
	}{
		{"no references", func(*swarm.ContainerSpec) {}, ""},
		{"secret present", func(cs *swarm.ContainerSpec) {
			cs.Secrets = []*swarm.SecretReference{{SecretID: "secret-db", SecretName: "db"}}
		}, ""},
		{"secret gone", func(cs *swarm.ContainerSpec) {
			cs.Secrets = []*swarm.SecretReference{{SecretID: "secret-old", SecretName: "old"}}
		}, `secret "old"`},
		{"config present", func(cs *swarm.ContainerSpec) {
			cs.Configs = []*swarm.ConfigReference{{ConfigID: "config-app", ConfigName: "app"}}
		}, ""},
		{"config gone", func(cs *swarm.ContainerSpec) {
			cs.Configs = []*swarm.ConfigReference{{ConfigID: "config-old", ConfigName: "old_cfg"}}
		}, `config "old_cfg"`},
		{"one of two gone, both named", func(cs *swarm.ContainerSpec) {
			cs.Secrets = []*swarm.SecretReference{{SecretID: "secret-x", SecretName: "x"}, {SecretID: "secret-db", SecretName: "db"}}
			cs.Configs = []*swarm.ConfigReference{{ConfigID: "config-y", ConfigName: "y"}}
		}, `secret "x", config "y"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := dbSecretCluster()
			cfg := swarm.Config{ID: "config-app"}
			cfg.Spec.Name = "app"
			f.Configs = []swarm.Config{cfg}
			prev := f.Services[0].Spec
			cs := *prev.TaskTemplate.ContainerSpec
			cs.Secrets, cs.Configs = nil, nil
			prev.TaskTemplate.ContainerSpec = &cs
			tc.prev(&cs)
			f.Services[0].PreviousSpec = &prev

			err := RollbackService(context.Background(), fakeDocker(t, f), "shop_api")
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("rollback refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want it to name %s", err, tc.wantErr)
			}
		})
	}
}

// KOR-06: Templating is part of what a secret is — a templated secret
// re-created as a plain one makes services read the template source. It is
// carried to the stand-in and to the new secret.
func TestRotateKeepsTemplating(t *testing.T) {
	f := dbSecretCluster()
	if err := RotateSecret(context.Background(), fakeDocker(t, f), "db", []byte("{{ .x }}"), RotateOptions{Poll: 1}); err != nil {
		t.Fatal(err)
	}
	if len(f.Secrets) != 1 {
		t.Fatalf("secrets = %+v", f.Secrets)
	}
	got := f.Secrets[0].Spec.Templating
	if got == nil || got.Name != "golang" || got.Options["k"] != "v" {
		t.Errorf("rotated secret's templating = %+v, want golang {k:v}", got)
	}
	if f.Secrets[0].Spec.Labels["team"] != "core" {
		t.Errorf("labels = %v", f.Secrets[0].Spec.Labels)
	}
}

// ... and a run resumed after the original is gone gets it from the stand-in.
func TestRotateResumeKeepsTemplating(t *testing.T) {
	st := rotateState{temp: &swarm.Secret{}}
	st.temp.Spec.Templating = &swarm.Driver{Name: "golang"}
	if d := st.keptTemplating(); d == nil || d.Name != "golang" {
		t.Errorf("keptTemplating with only the stand-in = %+v", d)
	}
	if d := (rotateState{orig: &swarm.Secret{}}).keptTemplating(); d != nil {
		t.Errorf("keptTemplating of a plain secret = %+v", d)
	}
}

// A secret served by an external driver has no content in swarm to replace;
// creating one with data would be refused by the manager halfway through, after
// the services were already moved. Refused up front instead.
func TestRotateRefusesDriverSecret(t *testing.T) {
	f := dbSecretCluster()
	f.Secrets[0].Spec.Driver = &swarm.Driver{Name: "vault"}
	d := fakeDocker(t, f)
	ctx := context.Background()
	err := RotateSecret(ctx, d, "db", []byte("new"), RotateOptions{Poll: 1})
	if err == nil || !strings.Contains(err.Error(), `"vault" secret driver`) {
		t.Fatalf("RotateSecret err = %v", err)
	}
	if _, perr := PlanRotation(ctx, d, "db"); perr == nil {
		t.Error("PlanRotation offered a rotation the run would refuse")
	}
	if len(f.Secrets) != 1 || f.Services[0].PreviousSpec != nil {
		t.Errorf("a refused rotation changed the cluster: secrets %d, previous spec %v", len(f.Secrets), f.Services[0].PreviousSpec)
	}
}
