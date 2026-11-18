// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmexec/client/internal/clusterread"
)

// The endpoint view names a node by hostname only, an endpoint only when its
// service is visible, and drops node addresses and agent error texts.
func TestEndpointsRowRedacts(t *testing.T) {
	v := &mcpView{byName: map[string]mcpService{"shop_web": {}}}
	e := &clusterread.NetEndpoints{
		Running: 2, Addrs: 4, LB: 1,
		Nodes: []clusterread.NodeEndpoints{
			{Node: "node-a", State: clusterread.NodeEndpointsOK, Expected: 2, Containers: 2, LB: 1, MACs: 3, Known: 5, Stale: 2},
			{Node: "node-b", State: clusterread.NodeUnreachable, Err: "dial tcp 192.168.7.2:7000: refused"},
			{Node: "idle", State: clusterread.NodeNotOnNetwork},
		},
		Stale: []clusterread.StaleEndpoint{
			{IP: "10.0.1.30", Name: "shop_web.3.0000000000000000000000001", Service: "shop_web", Owner: "192.168.7.2", OwnerIP: "192.168.7.2"},
			{IP: "10.0.1.31", Name: "secret_svc.1.0000000000000000000000002", Owner: "node-a", OwnerIP: "192.168.7.1", Orphan: true},
		},
		DupIPs: []clusterread.Duplicate{{Addr: "10.0.1.5", Endpoints: []string{"shop_web.1.0000000000000000000000003", "secret_svc.2.0000000000000000000000004"}}},
	}
	b, _ := json.Marshal(endpointsRow(e, v))
	out := string(b)
	for _, leak := range []string{"192.168.7", "secret_svc", "idle", "refused"} {
		if strings.Contains(out, leak) {
			t.Errorf("row reveals %q: %s", leak, out)
		}
	}
	for _, want := range []string{`"owner_node":"node-a"`, "shop_web.3.", "(not visible)", `"state":"agent unreachable"`,
		`"endpoint_without_container":true`, `"note":`, `"remedy":`} {
		if !strings.Contains(out, want) {
			t.Errorf("row lacks %q: %s", want, out)
		}
	}
	if r := endpointsRow(nil, v); r == nil || r.Remedy != "" {
		t.Errorf("no analysis: %+v", r)
	}
}
