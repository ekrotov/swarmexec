// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

// The target is whatever the operator typed after `exec` / `logs` /
// `port-forward`: a service, service.slot, a task id, a container id or a
// prefix of one. Against a cluster that has a few of each, any string must come
// back as an endpoint or an error — and an endpoint must name a container.
func FuzzResolveTarget(f *testing.F) {
	for _, s := range []string{"web", "web.1", "web.0", "web.99999999999999999999", "db.", ".1", "t1", "t", "cWEB", "cW",
		"", " ", "web ", "*", "node-a", "svc-web", "0", "\x00", "web.1.2"} {
		f.Add(s, "node-a")
	}
	fake := newFake()
	fake.tasks = []swarm.Task{
		task("t1", "svc-web", "node-a", 1, "cWEB"),
		task("t2", "svc-web", "node-b", 2, "cWEB2"),
		task("t3", "svc-db", "node-b", 1, "cDB"),
	}
	r := New(fake, AddrHostname)
	f.Fuzz(func(t *testing.T, target, hint string) {
		ep, err := r.Resolve(context.Background(), Request{Target: target, NodeHint: hint})
		if err == nil && (ep == nil || ep.ContainerID == "") {
			t.Fatalf("Resolve(%q) succeeded without a container: %+v", target, ep)
		}
	})
}
