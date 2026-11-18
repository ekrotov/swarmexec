// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package secscan

import (
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

// The manager can store a service's host-network attachment by network id, not
// the literal "host"; the cluster-aware scan must still flag it, and must not
// flag an overlay that is not the host network.
func TestHostNetworkAnalyzerByID(t *testing.T) {
	svc := svcSpec(func(sp *swarm.ServiceSpec) {
		sp.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "k3j9hostnetid"}}
	})
	if hasRule(Scan(svc), "host-network") != nil {
		t.Fatal("without cluster knowledge an opaque id cannot be classified")
	}
	hostNets := map[string]bool{"k3j9hostnetid": true, "host": true}
	if f := hasRule(ScanWithHostNetworks(svc, hostNets), "host-network"); f == nil || f.Severity != SevHigh {
		t.Errorf("host network by id should be a high finding, got %+v", f)
	}
	other := svcSpec(func(sp *swarm.ServiceSpec) {
		sp.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "someoverlayid"}}
	})
	if hasRule(ScanWithHostNetworks(other, hostNets), "host-network") != nil {
		t.Error("an overlay id must not flag")
	}
}
