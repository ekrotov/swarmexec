// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package endpoint

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/client/pkg/versions"
)

// writeContexts lays down a docker config directory holding two ssh contexts,
// so the endpoint resolution has something real to read.
func writeContexts(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	// Docker stores a context under <config>/contexts/meta/<sha256(name)>/meta.json.
	for name, host := range map[string]string{
		"alpha": "ssh://bastion-alpha",
		"beta":  "ssh://bastion-beta",
	} {
		sub := filepath.Join(dir, "contexts", "meta", fmt.Sprintf("%x", sha256.Sum256([]byte(name))))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		meta := fmt.Sprintf(`{"Name":%q,"Endpoints":{"docker":{"Host":%q}}}`, name, host)
		if err := os.WriteFile(filepath.Join(sub, "meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
}

// The bug this guards against: the manager API followed a cluster switch while
// the agent tunnel did not, because the two were built from two separate
// lookups of two different context names. Measured before the fix — 3/3 agents
// when started on a context directly, 0/3 after switching to the same context
// in the ui — and invisible in the tree, which comes from the manager and kept
// working.
//
// So the property worth pinning is not "the dialer works" but "the manager
// client and the agent tunnel come from the SAME resolution".
func TestEndpointFeedsBothChannelsFromOneResolution(t *testing.T) {
	writeContexts(t)

	alpha := Resolve("alpha")
	beta := Resolve("beta")
	if alpha.Host != "ssh://bastion-alpha" {
		t.Fatalf("alpha host = %q, want the context's endpoint", alpha.Host)
	}
	if beta.Host != "ssh://bastion-beta" {
		t.Fatalf("beta host = %q, want the context's endpoint", beta.Host)
	}
	// Two contexts must not resolve to the same place — otherwise this test
	// could pass while everything pointed at one cluster.
	if alpha.Host == beta.Host {
		t.Fatal("the two fixtures resolve to the same host; the test proves nothing")
	}

	// Both channels read the same field of the same value. There is no second
	// lookup left that could answer differently.
	for _, e := range []Docker{alpha, beta} {
		d, err := e.AgentDialer()
		if err != nil {
			t.Fatalf("%s: agent dialer: %v", e.Context, err)
		}
		if d == nil {
			t.Errorf("%s is an ssh endpoint and must get a tunnel", e.Context)
		}
	}
}

// A non-ssh endpoint dials the agents directly; a tunnel there would be wrong.
func TestNonSSHEndpointGetsNoTunnel(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	t.Setenv("DOCKER_CONTEXT", "")

	e := Resolve("")
	d, err := e.AgentDialer()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != nil {
		t.Error("a tcp:// endpoint must dial agents directly")
	}
}

// A context that does not exist must still fail where it always failed — from
// the manager client, with its clearer message — and must not fail config
// resolution, which would change what every command prints.
func TestUnresolvableEndpointFailsAtTheClient(t *testing.T) {
	writeContexts(t)

	e := Resolve("does-not-exist")
	if e.err == nil {
		t.Fatal("an unknown context should not resolve")
	}
	if _, err := e.Client(); err == nil {
		t.Error("the manager client must report the failure")
	}
	d, err := e.AgentDialer()
	if err != nil {
		t.Errorf("an unresolvable endpoint must not fail config resolution: %v", err)
	}
	if d != nil {
		t.Error("an unresolvable endpoint must not produce a tunnel")
	}
}

// The floor is a promise, so it is pinned here rather than left to whoever next
// edits the constant. 1.40 is Docker Engine 19.03, and the two must stay a pair
// — an error naming an API version nobody knows by heart is not actionable.
func TestMinimumDockerVersionIsStated(t *testing.T) {
	if minAPI != "1.40" {
		t.Errorf("minAPI = %q; if this moved on purpose, update site/docs/install.html too", minAPI)
	}
	if minEngine != "19.03" {
		t.Errorf("minEngine = %q, which must be the release that speaks API %s", minEngine, minAPI)
	}
}

// apiVersionTooOld is the comparison the check rests on, exercised without a
// daemon: the boundary itself must be INCLUSIVE, or a cluster running exactly
// the documented minimum would be turned away by the tool that documents it.
func TestVersionFloorBoundaryIsInclusive(t *testing.T) {
	cases := []struct {
		api string
		old bool
	}{
		{"1.24", true},  // the library's old fallback — Docker 1.12
		{"1.30", true},  // configs work here, but nothing else is tested
		{"1.39", true},  // one below
		{"1.40", false}, // exactly the minimum: must be accepted
		{"1.41", false},
		{"1.56", false}, // what the test cluster runs
	}
	for _, c := range cases {
		if got := versions.LessThan(c.api, minAPI); got != c.old {
			t.Errorf("API %s: too old = %v, want %v", c.api, got, c.old)
		}
	}
}

// The version floor is checked against a real daemon answer, not only against
// the comparison: Connect is the one door every command goes through, so a
// Ping that is never read, or a header that is read from the wrong field,
// would let an unsupported cluster through and fail one view at a time —
// exactly the failure the floor exists to replace.
func TestConnectRefusesADaemonBelowTheFloor(t *testing.T) {
	cases := []struct {
		name    string
		api     string
		wantErr bool
	}{
		{"one below the floor", "1.39", true},
		{"exactly the floor", minAPI, false},
		{"well above", "1.47", false},
		// A daemon that answers without naming a version is not evidence of
		// being too old; whatever call comes next reports the real problem.
		{"no version header", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.api != "" {
					w.Header().Set("Api-Version", tc.api)
					w.Header().Set("Ostype", "linux")
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			ep := Docker{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")}
			c, err := ep.Connect(context.Background())
			if c != nil {
				c.Close()
			}
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("API %s accepted; the floor is %s", tc.api, minAPI)
			case tc.wantErr && !strings.Contains(err.Error(), minEngine):
				t.Errorf("err = %v; it must name the Engine release an operator can act on", err)
			case !tc.wantErr && err != nil:
				t.Errorf("API %q refused: %v", tc.api, err)
			}
		})
	}
}

// An endpoint that could not be resolved carries its failure to the client,
// where a bad context has always surfaced as a connection failure. Connect
// must not swallow it on the way to the Ping.
func TestConnectReportsAnUnresolvableEndpoint(t *testing.T) {
	ep := Docker{err: errors.New("no such context: typo")}
	c, err := ep.Connect(context.Background())
	if c != nil {
		c.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "typo") {
		t.Fatalf("err = %v, want the resolution failure", err)
	}
}
