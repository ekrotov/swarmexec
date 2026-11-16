// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// emptyCluster has no secrets and no configs. Everything else of the API is
// left nil: the converter must not need it.
type emptyCluster struct {
	client.APIClient
	secrets []swarm.Secret
	configs []swarm.Config
}

func (e emptyCluster) SecretList(context.Context, client.SecretListOptions) (client.SecretListResult, error) {
	return client.SecretListResult{Items: e.secrets}, nil
}
func (e emptyCluster) ConfigList(context.Context, client.ConfigListOptions) (client.ConfigListResult, error) {
	return client.ConfigListResult{Items: e.configs}, nil
}

func writeStack(t *testing.T, yml string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"stack.yml": yml, "pass.txt": "s3cret\n", "greeting.txt": "hello\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "stack.yml")
}

const ownSecretsStack = `version: "3.8"
services:
  web:
    image: nginx:alpine
    secrets: [pass]
    configs:
      - source: greeting
        target: /etc/greeting.txt
secrets:
  pass:
    file: ./pass.txt
configs:
  greeting:
    file: ./greeting.txt
`

// A first deploy of a file that brings its own secret and config used to be
// refused by the check before the deploy ("secret not found"), although the
// deploy itself creates both before it needs them.
func TestPlanFile_OwnSecretsAndConfigsNeedNotExistYet(t *testing.T) {
	path := writeStack(t, ownSecretsStack)
	plan, err := PlanFile(context.Background(), emptyCluster{}, path, "shop")
	if err != nil {
		t.Fatalf("plan refused a stack whose secrets it creates itself: %v", err)
	}
	if len(plan.Services) != 1 {
		t.Fatalf("planned %d services, want 1", len(plan.Services))
	}
	if _, err := FromFile(context.Background(), emptyCluster{}, path, "shop"); err != nil {
		t.Fatalf("diff refused it too: %v", err)
	}
}

// An external secret is not the file's to create: if it does not exist, the
// deploy would fail, so the plan still says so first.
func TestPlanFile_MissingExternalSecretStillFails(t *testing.T) {
	path := writeStack(t, `version: "3.8"
services:
  web:
    image: nginx:alpine
    secrets: [dbpass]
secrets:
  dbpass:
    external: true
`)
	_, err := PlanFile(context.Background(), emptyCluster{}, path, "shop")
	if err == nil || !strings.Contains(err.Error(), "secret not found: dbpass") {
		t.Fatalf("got %v, want secret not found: dbpass", err)
	}
	var ok emptyCluster
	ok.secrets = []swarm.Secret{{ID: "sec1", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "dbpass"}}}}
	if _, err := PlanFile(context.Background(), ok, path, "shop"); err != nil {
		t.Fatalf("an existing external secret was refused: %v", err)
	}
}

// FromFile over a file with every kind of declaration, owned and external:
// what it owns renders by its short name, what it does not stays external,
// the implicit default network appears, and an unmodelled field is noted.
func TestFromFileRendersOwnedAndExternal(t *testing.T) {
	path := writeStack(t, `version: "3.8"
services:
  web:
    image: nginx:alpine
    tty: true
    networks: [front, ext]
    secrets: [pass, dbpass]
    configs: [greeting]
    volumes: [data:/data, shared:/shared, named:/named]
  worker:
    image: busybox
networks:
  front:
    driver: overlay
    attachable: true
    labels: {tier: web}
  ext:
    external: true
secrets:
  pass:
    file: ./pass.txt
  dbpass:
    external: true
configs:
  greeting:
    name: hello_v1
    file: ./greeting.txt
volumes:
  data: {}
  shared:
    external: true
  named:
    name: fixed_name
`)
	var cluster emptyCluster
	cluster.secrets = []swarm.Secret{{ID: "sec1", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "dbpass"}}}}
	st, err := FromFile(context.Background(), cluster, path, "shop")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.YAML()
	if err != nil {
		t.Fatal(err)
	}
	const want = `services:
  web:
    image: nginx:alpine
    volumes:
      - data:/data
      - fixed_name:/named
      - shared:/shared
    networks:
      ext: {}
      front: {}
    secrets:
      - source: dbpass
        target: dbpass
        uid: "0"
        gid: "0"
        mode: 292
      - source: pass
        target: pass
        uid: "0"
        gid: "0"
        mode: 292
    configs:
      - source: hello_v1
        target: greeting
        uid: "0"
        gid: "0"
        mode: 292
  worker:
    image: busybox
    networks:
      default: {}
networks:
  default:
    driver: overlay
  ext:
    external: true
    name: ext
  front:
    driver: overlay
    attachable: true
    labels:
      tier: web
volumes:
  data:
    external: true
    name: shop_data
  named:
    external: true
  shared:
    external: true
secrets:
  dbpass:
    external: true
  pass:
    external: true
    name: shop_pass
configs:
  greeting:
    external: true
    name: hello_v1
`
	if got != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", got, want)
	}
	if len(st.Notes) != 1 || !strings.Contains(st.Notes[0], "NOT compared: tty.") {
		t.Errorf("notes = %q, want the tty gap", st.Notes)
	}

	if _, err := FromFile(context.Background(), emptyCluster{}, path, "shop"); err == nil || !strings.Contains(err.Error(), "convert stack.yml") {
		t.Errorf("missing external secret: got %v", err)
	}
	if _, err := FromFile(context.Background(), emptyCluster{}, filepath.Join(t.TempDir(), "none.yml"), "shop"); err == nil {
		t.Error("a missing file must fail")
	}
}
