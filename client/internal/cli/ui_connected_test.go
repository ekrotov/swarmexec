// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import "testing"

// A loader that starts while the visible cluster has no connections — closed,
// or never connected — must do nothing. It used to hand nil to its worker
// goroutine and take the whole TUI down.
func TestLoadersWithNoClusterConnectedDoNothing(t *testing.T) {
	u := runningUI(t)
	onLoop(t, u, func() {
		u.clusterState.close() // dcli, agents, r, ctx all nil now
		if _, ok := u.connected(); ok {
			t.Fatal("a closed cluster reports connected")
		}
		u.refreshCluster()
		u.loadUsage()
		u.treeTab().load()
		u.treeTab().autoRefresh()
		u.volTab().load()
		u.nodeTab().reload()
		u.netTab().loadEndpoints()
	})
	onLoop(t, u, func() {}) // let anything wrongly started reach the loop
}
