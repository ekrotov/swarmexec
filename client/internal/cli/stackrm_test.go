// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/swarmfake"
)

// shopStackSwarm: stack "shop" owns two services, a secret and a network; an
// unrelated service, secret and network carry no stack label or another.
func shopStackSwarm() *swarmfake.Swarm {
	shop := map[string]string{"com.docker.stack.namespace": "shop"}
	other := map[string]string{"com.docker.stack.namespace": "shopping"}
	return &swarmfake.Swarm{
		Services: []swarm.Service{
			stackedService("s1", "shop_web", "shop", 1),
			stackedService("s2", "shop_db", "shop", 1),
			stackedService("s3", "shopping_web", "shopping", 1),
		},
		Secrets: []swarm.Secret{
			{ID: "sec1", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "shop_pw", Labels: shop}}},
			{ID: "sec2", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "keep_pw", Labels: other}}},
		},
		Networks: []network.Summary{
			{Network: network.Network{ID: "net1", Name: "shop_default", Labels: shop}},
			{Network: network.Network{ID: "net2", Name: "ingress"}},
		},
	}
}

// exitCode is the exit code a command's error maps to: 0 for none, -1 for
// an error that is not a cliError.
func exitCode(err error) int {
	var ce *cliError
	if errors.As(err, &ce) {
		return ce.code
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestStackRm(t *testing.T) {
	f := shopStackSwarm()
	g := cliCluster(t, f, nil)

	// No terminal, no --yes: the listing, then nothing removed.
	out, errOut, err := runCmd(t, newStackRmCmd(g), "shop")
	if exitCode(err) != 125 || !strings.Contains(errOut, "aborted") || len(f.Services) != 3 {
		t.Fatalf("unconfirmed: %q %q %v", out, errOut, err)
	}
	for _, want := range []string{`stack "shop" holds 2 services, 1 secret, 1 network`, "service  shop_web", "secret   shop_pw", "network  shop_default"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "shopping") || strings.Contains(out, "keep_pw") || strings.Contains(out, "ingress") {
		t.Errorf("listing names another stack's objects:\n%s", out)
	}

	out, _, err = runCmd(t, newStackRmCmd(g), "shop", "--yes")
	if err != nil || !strings.Contains(out, `removed stack "shop" (2 services, 1 secret, 1 network)`) {
		t.Fatalf("removed: %q %v", out, err)
	}
	if len(f.Services) != 1 || f.Services[0].Spec.Name != "shopping_web" || len(f.Secrets) != 1 || len(f.Networks) != 1 {
		t.Errorf("left: %d services, %d secrets, %d networks", len(f.Services), len(f.Secrets), len(f.Networks))
	}

	// Gone now: there is nothing labelled for it.
	_, _, err = runCmd(t, newStackRmCmd(g), "shop", "--yes")
	if exitCode(err) != 125 || !strings.Contains(err.Error(), "nothing to remove") {
		t.Errorf("empty: %v", err)
	}
}

// A secret that will not go is named; the rest is removed anyway.
func TestStackRmPartial(t *testing.T) {
	f := shopStackSwarm()
	f.Fail = map[string]bool{"DELETE /secrets/sec1": true}
	g := cliCluster(t, f, nil)
	_, errOut, err := runCmd(t, newStackRmCmd(g), "shop", "-y")
	if exitCode(err) != 125 || !strings.Contains(errOut, `stack "shop" removed, except:`) || !strings.Contains(errOut, "secret shop_pw") {
		t.Errorf("%q %v", errOut, err)
	}
	if len(f.Services) != 1 || len(f.Networks) != 1 {
		t.Errorf("the rest should be gone: %d services, %d networks", len(f.Services), len(f.Networks))
	}
}

func TestStackRmListFailure(t *testing.T) {
	f := shopStackSwarm()
	f.Fail = map[string]bool{"GET /secrets": true}
	g := cliCluster(t, f, nil)
	_, _, err := runCmd(t, newStackRmCmd(g), "shop", "--yes")
	if exitCode(err) != 125 || !strings.Contains(err.Error(), "read stack shop") || len(f.Services) != 3 {
		t.Errorf("%v", err)
	}
}

// stack check and security check are stack deploy --check: findings exit 1,
// a clean file 0, and nothing is ever deployed.
func TestStackCheckAndSecurityCheck(t *testing.T) {
	dir := t.TempDir()
	risky := filepath.Join(dir, "risky.yml")
	if err := os.WriteFile(risky, []byte("services:\n  web:\n    image: nginx:1.28\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &swarmfake.Swarm{}
	g := cliCluster(t, f, nil)
	for _, cmd := range [][]string{{"stack", "check"}, {"security", "check"}} {
		root := newRootCmd(g, Version{Binary: "dev", Proto: "1"})
		out, _, err := runCmd(t, root, append(cmd, risky)...)
		if exitCode(err) != 1 || !strings.Contains(out, "docker.sock") {
			t.Errorf("%v: %q %v", cmd, out, err)
		}
	}
	if len(f.Services) != 0 {
		t.Error("a check deployed something")
	}
	_, _, err := runCmd(t, newStackCheckCmd(g), filepath.Join(dir, "missing.yml"))
	if exitCode(err) != exitUsage {
		t.Errorf("missing file: %v", err)
	}
}
