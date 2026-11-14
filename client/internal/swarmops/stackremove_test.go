// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
)

// fakeStackAPI records what was asked of it and in which order, so the two
// things that matter about a destructive operation can be proven: that it acts
// on exactly the labelled objects, and that it acts on them in an order where
// each step can actually succeed.
type fakeStackAPI struct {
	svcs []swarm.Service
	secs []swarm.Secret
	cfgs []swarm.Config
	nets []network.Summary

	labels  []string // the label filter of every list call
	removed []string // "kind:id", in call order

	failRemove  map[string]error // id -> error
	netFailsFor int              // fail NetworkRemove this many times, then succeed
	netCalls    int
}

func labelOf(f client.Filters) string {
	if got := filterValues(f, "label"); len(got) == 1 {
		return got[0]
	}
	return "<none>"
}

func (f *fakeStackAPI) ServiceList(_ context.Context, o client.ServiceListOptions) (client.ServiceListResult, error) {
	f.labels = append(f.labels, labelOf(o.Filters))
	return client.ServiceListResult{Items: f.svcs}, nil
}
func (f *fakeStackAPI) SecretList(_ context.Context, o client.SecretListOptions) (client.SecretListResult, error) {
	f.labels = append(f.labels, labelOf(o.Filters))
	return client.SecretListResult{Items: f.secs}, nil
}
func (f *fakeStackAPI) ConfigList(_ context.Context, o client.ConfigListOptions) (client.ConfigListResult, error) {
	f.labels = append(f.labels, labelOf(o.Filters))
	return client.ConfigListResult{Items: f.cfgs}, nil
}
func (f *fakeStackAPI) NetworkList(_ context.Context, o client.NetworkListOptions) (client.NetworkListResult, error) {
	f.labels = append(f.labels, labelOf(o.Filters))
	return client.NetworkListResult{Items: f.nets}, nil
}

func (f *fakeStackAPI) remove(kind, id string) error {
	f.removed = append(f.removed, kind+":"+id)
	return f.failRemove[id]
}
func (f *fakeStackAPI) ServiceRemove(_ context.Context, id string, _ client.ServiceRemoveOptions) (client.ServiceRemoveResult, error) {
	return client.ServiceRemoveResult{}, f.remove("service", id)
}
func (f *fakeStackAPI) SecretRemove(_ context.Context, id string, _ client.SecretRemoveOptions) (client.SecretRemoveResult, error) {
	return client.SecretRemoveResult{}, f.remove("secret", id)
}
func (f *fakeStackAPI) ConfigRemove(_ context.Context, id string, _ client.ConfigRemoveOptions) (client.ConfigRemoveResult, error) {
	return client.ConfigRemoveResult{}, f.remove("config", id)
}
func (f *fakeStackAPI) NetworkRemove(_ context.Context, id string, _ client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	f.netCalls++
	f.removed = append(f.removed, "network:"+id)
	if f.netCalls <= f.netFailsFor {
		return client.NetworkRemoveResult{}, errors.New("network has active endpoints")
	}
	return client.NetworkRemoveResult{}, f.failRemove[id]
}

func stackFixture() *fakeStackAPI {
	svc := swarm.Service{ID: "svc1"}
	svc.Spec.Name = "shop_web"
	sec := swarm.Secret{ID: "sec1"}
	sec.Spec.Name = "shop_dbpass"
	cfg := swarm.Config{ID: "cfg1"}
	cfg.Spec.Name = "shop_nginx"
	return &fakeStackAPI{
		svcs: []swarm.Service{svc},
		secs: []swarm.Secret{sec},
		cfgs: []swarm.Config{cfg},
		nets: []network.Summary{{ID: "net1", Name: "shop_default"}},
	}
}

// Every list is filtered at the daemon by the exact label. Anything looser —
// a prefix match, a client-side filter — would let a stack named "shop" reach
// into "shop-staging".
func TestStackContentsFiltersByTheExactLabel(t *testing.T) {
	f := stackFixture()
	c, err := StackContentsOf(context.Background(), f, "shop")
	if err != nil {
		t.Fatalf("stackContentsOf: %v", err)
	}
	if len(f.labels) != 4 {
		t.Fatalf("made %d list calls, want 4 (services, secrets, configs, networks)", len(f.labels))
	}
	for _, got := range f.labels {
		if got != "com.docker.stack.namespace=shop" {
			t.Errorf("list filtered by %q, want the exact stack label", got)
		}
	}
	if len(c.Services) != 1 || c.Services[0].Name != "shop_web" || c.Services[0].ID != "svc1" {
		t.Errorf("services = %+v", c.Services)
	}
	if len(c.Secrets) != 1 || len(c.Configs) != 1 || len(c.Networks) != 1 {
		t.Errorf("contents = %+v", c)
	}
	if c.Networks[0].Name != "shop_default" {
		t.Errorf("network = %+v", c.Networks[0])
	}
}

// Order is not cosmetic: while a service exists, its secrets, configs and
// networks are in use by definition, so every other removal would fail.
func TestRemoveStackRemovesServicesFirstAndNetworksLast(t *testing.T) {
	f := stackFixture()
	c, _ := StackContentsOf(context.Background(), f, "shop")
	if errs := RemoveStack(context.Background(), f, c); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	want := []string{"service:svc1", "secret:sec1", "config:cfg1", "network:net1"}
	if strings.Join(f.removed, ",") != strings.Join(want, ",") {
		t.Errorf("removal order = %v, want %v", f.removed, want)
	}
}

// A stack left half-removed because one object refused is worse than one
// removed as far as it can be, with a report of what is left.
func TestRemoveStackContinuesPastAFailure(t *testing.T) {
	f := stackFixture()
	f.failRemove = map[string]error{"sec1": errors.New("secret is in use")}
	c, _ := StackContentsOf(context.Background(), f, "shop")

	errs := RemoveStack(context.Background(), f, c)
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly the one refusal", errs)
	}
	if got := errs[0].Error(); !strings.Contains(got, "secret shop_dbpass") || !strings.Contains(got, "in use") {
		t.Errorf("error = %q, want it to name the object and the reason", got)
	}
	// The config and the network still went.
	if !strings.Contains(strings.Join(f.removed, ","), "config:cfg1") ||
		!strings.Contains(strings.Join(f.removed, ","), "network:net1") {
		t.Errorf("removal stopped at the failure: %v", f.removed)
	}
}

// Removing a service is not synchronous with its tasks going away, so the first
// attempt at the network routinely fails. docker stack rm gives up there and
// leaves the network behind; waiting a moment removes it.
func TestRemoveStackWaitsForTheNetworkEndpointsToDrain(t *testing.T) {
	old := networkRetryWait
	networkRetryWait = time.Millisecond
	defer func() { networkRetryWait = old }()

	f := stackFixture()
	f.netFailsFor = 2 // busy twice, then free
	c, _ := StackContentsOf(context.Background(), f, "shop")

	if errs := RemoveStack(context.Background(), f, c); len(errs) != 0 {
		t.Fatalf("errors = %v, want none — the network frees up on the third try", errs)
	}
	if f.netCalls != 3 {
		t.Errorf("network attempts = %d, want 3", f.netCalls)
	}
}

// A network still held after the wait is held by something outside this stack,
// and then saying so is the right outcome rather than retrying forever.
func TestRemoveStackGivesUpOnAPermanentlyBusyNetwork(t *testing.T) {
	old := networkRetryWait
	networkRetryWait = time.Millisecond
	defer func() { networkRetryWait = old }()

	f := stackFixture()
	f.netFailsFor = 1000
	c, _ := StackContentsOf(context.Background(), f, "shop")

	errs := RemoveStack(context.Background(), f, c)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "network shop_default") {
		t.Fatalf("errors = %v, want one naming the network", errs)
	}
	if f.netCalls != networkRetries {
		t.Errorf("network attempts = %d, want %d", f.netCalls, networkRetries)
	}
}

// The confirm has to say what will go, and only what is actually there: a line
// reading "0 configs" invites the reader to skim it.
func TestStackContentsCounts(t *testing.T) {
	c := StackContents{
		Services: []stackObject{{Name: "a"}, {Name: "b"}},
		Networks: []stackObject{{Name: "n"}},
	}
	if got := c.Counts(); got != "2 services, 1 network" {
		t.Errorf("counts = %q", got)
	}
	if (StackContents{}).Empty() != true {
		t.Error("an empty stackContents must report empty")
	}
	if got := (StackContents{}).Counts(); got != "nothing" {
		t.Errorf("empty counts = %q", got)
	}
}
