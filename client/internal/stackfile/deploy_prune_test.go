// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"context"
	"strings"
	"testing"

	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/swarmfake"
	"swarmexec/client/internal/swarmlabel"
)

// Prune removes the stack's services the file no longer declares — and only
// those: not the services the file still declares, and not another stack's.
// Without it a file that is a subset of the stack removes nothing.
func TestApplyPrune(t *testing.T) {
	stackSvc := func(id, name, stack string) swarm.Service {
		s := swarm.Service{ID: id}
		s.Spec.Name = name
		s.Spec.Labels = map[string]string{swarmlabel.StackNamespace: stack}
		return s
	}
	fresh := func() *swarmfake.Swarm {
		return &swarmfake.Swarm{Services: []swarm.Service{
			stackSvc("id-a", "st_a", "st"),
			stackSvc("id-gone", "st_gone", "st"),
			stackSvc("id-other", "other_gone", "other"),
		}}
	}
	names := func(f *swarmfake.Swarm) string {
		var n []string
		for _, s := range f.Services {
			n = append(n, s.Spec.Name)
		}
		return strings.Join(n, ",")
	}
	cfg := &composetypes.Config{Services: composetypes.Services{svcCfg("a")}}

	f := fresh()
	res, err := Apply(context.Background(), deployFake(t, f), "st", cfg, ApplyOptions{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Removed, ","); got != "st_gone" {
		t.Errorf("removed = %q, want st_gone", got)
	}
	if got := names(f); got != "st_a,other_gone" {
		t.Errorf("services left = %s, want the declared one and the other stack's", got)
	}
	if got := strings.Join(res.Updated, ","); got != "st_a" {
		t.Errorf("updated = %q", got)
	}

	f = fresh()
	res, err = Apply(context.Background(), deployFake(t, f), "st", cfg, ApplyOptions{})
	if err != nil || len(res.Removed) != 0 || names(f) != "st_a,st_gone,other_gone" {
		t.Errorf("without Prune: err %v removed %v left %s", err, res.Removed, names(f))
	}

	// A removal the manager refuses stops the deploy with the service named.
	f = fresh()
	f.Fail = map[string]bool{"DELETE /services/id-gone": true}
	if _, err := Apply(context.Background(), deployFake(t, f), "st", cfg, ApplyOptions{Prune: true}); err == nil || !strings.Contains(err.Error(), "remove service st_gone") {
		t.Errorf("err = %v", err)
	}
}
