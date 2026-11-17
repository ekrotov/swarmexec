// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"swarmexec/client/internal/swarmfake"
)

func deployFake(t *testing.T, f *swarmfake.Swarm) *client.Client {
	t.Helper()
	api := httptest.NewServer(f)
	t.Cleanup(api.Close)
	dcli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	return dcli
}

func svcCfg(name string, networks ...string) composetypes.ServiceConfig {
	s := composetypes.ServiceConfig{Name: name, Image: "registry.example/" + name + ":1"}
	if len(networks) > 0 {
		s.Networks = map[string]*composetypes.ServiceNetworkConfig{}
		for _, n := range networks {
			s.Networks[n] = nil
		}
	}
	return s
}

// KOR-03: one service on a named network and one on none. The second still
// lands on <stack>_default, so that network has to exist before it is created;
// before the fix only networks named by SOME service were created, and the
// deploy failed halfway.
func TestApplyDefaultNetworkPerService(t *testing.T) {
	f := &swarmfake.Swarm{StrictNetworks: true}
	cfg := &composetypes.Config{
		Services: composetypes.Services{svcCfg("a", "backend"), svcCfg("b")},
		Networks: map[string]composetypes.NetworkConfig{"backend": {}},
	}
	res, err := Apply(context.Background(), deployFake(t, f), "st", cfg, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply: %v (result %+v)", err, res)
	}
	if got := strings.Join(res.Networks, ","); got != "st_backend,st_default" {
		t.Errorf("networks created = %q, want st_backend,st_default", got)
	}
	if len(res.Created) != 2 {
		t.Errorf("created = %v, want both services", res.Created)
	}
}

// KOR-03: an existing secret or config is not silently skipped.
func TestApplyReportsKeptSecretsAndConfigs(t *testing.T) {
	file := filepath.Join(t.TempDir(), "v")
	if err := os.WriteFile(file, []byte("new content"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &swarmfake.Swarm{
		Secrets: []swarm.Secret{{ID: "s1", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "st_key"}}}},
		Configs: []swarm.Config{{ID: "c1", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "st_conf"}}}},
	}
	cfg := &composetypes.Config{
		Services: composetypes.Services{svcCfg("a")},
		Secrets:  map[string]composetypes.SecretConfig{"key": {File: file}, "fresh": {File: file}},
		Configs:  map[string]composetypes.ConfigObjConfig{"conf": {File: file}},
	}
	res, err := Apply(context.Background(), deployFake(t, f), "st", cfg, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Kept, ","); got != "secret st_key,config st_conf" {
		t.Errorf("kept = %q", got)
	}
	if got := strings.Join(res.Secrets, ","); got != "st_fresh" {
		t.Errorf("created secrets = %q", got)
	}
	notes := strings.Join(res.Notes(), "\n")
	if !strings.Contains(notes, "secret st_key already exists and was left unchanged") {
		t.Errorf("notes do not say the secret was kept:\n%s", notes)
	}
}

// KOR-03: the manager's warnings reach the result, and registry credentials
// are sent only when asked for, per image.
func TestApplyWarningsAndRegistryAuth(t *testing.T) {
	f := &swarmfake.Swarm{Warnings: []string{"no node satisfies the constraint"}}
	cfg := &composetypes.Config{Services: composetypes.Services{svcCfg("a")}}
	dcli := deployFake(t, f)
	res, err := Apply(context.Background(), dcli, "st", cfg, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(res.Warnings, "\n"); !strings.Contains(w, "st_a: no node satisfies the constraint") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	if f.RegistryAuth[0] != "" {
		t.Errorf("credentials sent without being asked: %q", f.RegistryAuth)
	}
	// Second deploy: an update, now with credentials.
	var asked []string
	res, err = Apply(context.Background(), dcli, "st", cfg, ApplyOptions{RegistryAuth: func(image string) (string, error) {
		asked = append(asked, image)
		return "tok-" + image, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updated) != 1 || len(asked) != 1 || asked[0] != "registry.example/a:1" {
		t.Errorf("updated=%v asked=%v", res.Updated, asked)
	}
	if got := f.RegistryAuth[len(f.RegistryAuth)-1]; got != "tok-registry.example/a:1" {
		t.Errorf("auth header = %q", got)
	}
}
