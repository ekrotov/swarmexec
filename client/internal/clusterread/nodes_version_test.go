// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package clusterread

import (
	"context"
	"errors"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/swarmapi"
	"swarmexec/client/internal/swarmfake"
	"swarmexec/client/internal/swarmops"
)

// Nodes carry their version on NodeInfo; the mutation is refused when the node
// moved since that read.
func TestNodeMutationsRefuseStaleVersion(t *testing.T) {
	mk := func() *swarmfake.Swarm {
		n := swarm.Node{ID: "n1"}
		n.Version.Index = 3
		n.Description.Hostname = "worker-1"
		n.Spec.Availability = swarm.NodeAvailabilityActive
		n.Spec.Labels = map[string]string{"zone": "a"}
		return &swarmfake.Swarm{Nodes: []swarm.Node{n}}
	}
	ops := []struct {
		name  string
		apply func(ctx context.Context, d swarmapi.Client, v swarm.Version) error
		check func(t *testing.T, n swarm.Node)
	}{
		{"labels", func(ctx context.Context, d swarmapi.Client, v swarm.Version) error {
			return SetNodeLabels(ctx, d, "n1", v, map[string]string{"zone": "b"})
		}, func(t *testing.T, n swarm.Node) {
			if n.Spec.Labels["zone"] != "b" {
				t.Errorf("labels = %v", n.Spec.Labels)
			}
		}},
		{"availability", func(ctx context.Context, d swarmapi.Client, v swarm.Version) error {
			return SetNodeAvailability(ctx, d, "n1", v, swarm.NodeAvailabilityDrain)
		}, func(t *testing.T, n swarm.Node) {
			if n.Spec.Availability != swarm.NodeAvailabilityDrain {
				t.Errorf("availability = %s", n.Spec.Availability)
			}
		}},
	}
	for _, op := range ops {
		t.Run(op.name+"/unchanged applies", func(t *testing.T) {
			f := mk()
			d := swarmfake.Client(t, f)
			if err := op.apply(context.Background(), d, swarm.Version{Index: 3}); err != nil {
				t.Fatal(err)
			}
			op.check(t, f.Nodes[0])
		})
		t.Run(op.name+"/concurrent change refused", func(t *testing.T) {
			f := mk()
			d := swarmfake.Client(t, f)
			// another operator relabels the node after our read (version 3).
			f.Nodes[0].Spec.Labels = map[string]string{"zone": "a", "disk": "ssd"}
			f.Nodes[0].Version.Index = 4
			err := op.apply(context.Background(), d, swarm.Version{Index: 3})
			var ce *swarmops.ConflictError
			if !errors.As(err, &ce) || ce.Kind != "node" || ce.Name != "worker-1" || ce.Loaded != 3 || ce.Current != 4 {
				t.Fatalf("err = %v", err)
			}
			if f.Nodes[0].Version.Index != 4 || f.Nodes[0].Spec.Labels["disk"] != "ssd" || f.Nodes[0].Spec.Availability != swarm.NodeAvailabilityActive {
				t.Errorf("a refused edit wrote: %+v", f.Nodes[0])
			}
		})
	}
}
