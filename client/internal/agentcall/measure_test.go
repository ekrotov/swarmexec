// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"testing"
	"time"
)

// measured is what one scenario cost at the source: TCP connections the
// tunnel dialler was asked for (an ssh channel each, behind a bastion) and TLS
// handshakes the agents served.
type measured struct{ dials, handshakes, calls int64 }

func measure(fc *fakeCluster, before measured) measured {
	return measured{
		dials:      fc.dials.Load() - before.dials,
		handshakes: fc.handshakes.Load() - before.handshakes,
		calls:      fc.totalCalls() - before.calls,
	}
}

func snapshot(fc *fakeCluster) measured {
	return measured{fc.dials.Load(), fc.handshakes.Load(), fc.totalCalls()}
}

// tuiRefresh is what one TUI refresh asks of the agents when the Volumes tab
// is open: the usage poll, the cluster census, and the volumes load with its
// consumers and sizes.
func tuiRefresh(ctx context.Context, ac *Client, fc *fakeCluster) {
	ac.Usage(ctx, fc.nodes, testTimeout)
	ac.Versions(ctx, fc.nodes, testTimeout)
	ac.Volumes(ctx, fc.nodes, testTimeout)
	ac.VolumeUsage(ctx, fc.nodes, testTimeout)
	ac.VolumeSizes(ctx, fc.nodes, testTimeout)
}

// TestMeasureConnectionCost reports the dial and handshake count of a TUI
// session (five refreshes) and of `doctor` on a three-node cluster.
func TestMeasureConnectionCost(t *testing.T) {
	ctx := context.Background()

	fc := newFakeCluster(t, "node-a", "node-b", "node-c")
	fc.delay = 20 * time.Millisecond // what opening one ssh channel might cost
	ac := New(fc.cfg)
	began := time.Now()
	start := snapshot(fc)
	tuiRefresh(ctx, ac, fc)
	first := measure(fc, start)
	for i := 0; i < 4; i++ {
		tuiRefresh(ctx, ac, fc)
	}
	five := measure(fc, start)
	took := time.Since(began)
	ac.Close()
	t.Logf("TUI, 3 nodes: first refresh %d calls / %d dials / %d TLS handshakes; five refreshes %d calls / %d dials / %d handshakes, %v with 20ms per dial",
		first.calls, first.dials, first.handshakes, five.calls, five.dials, five.handshakes, took.Round(time.Millisecond))

	doc := newFakeCluster(t, "node-a", "node-b", "node-c")
	dac := New(doc.cfg)
	start = snapshot(doc)
	dac.Versions(ctx, doc.nodes, testTimeout)
	d := measure(doc, start)
	dac.Close()
	t.Logf("doctor, 3 nodes: %d calls / %d dials / %d TLS handshakes", d.calls, d.dials, d.handshakes)

	vl := newFakeCluster(t, "node-a", "node-b", "node-c")
	vac := New(vl.cfg)
	start = snapshot(vl)
	vac.Volumes(ctx, vl.nodes, testTimeout)
	vac.VolumeUsage(ctx, vl.nodes, testTimeout)
	vac.VolumeSizes(ctx, vl.nodes, testTimeout)
	v := measure(vl, start)
	vac.Close()
	t.Logf("volume ls --size, 3 nodes: %d calls / %d dials / %d TLS handshakes", v.calls, v.dials, v.handshakes)
}
