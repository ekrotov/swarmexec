// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
)

const testTimeout = 3 * time.Second

// Every call the package offers, against a three-node fake cluster: the
// answers come back in the shapes the views use, and each call carried a
// binding that matches the certificate of the agent it reached.
func TestCallsAgainstAFakeCluster(t *testing.T) {
	fc := newFakeCluster(t, "node-a", "node-b", "node-c")
	ac := New(fc.cfg)
	defer ac.Close()
	ctx := context.Background()
	n := fc.nodes[0]

	if ac.Config().Port != 7000 {
		t.Fatalf("Config() = %+v", ac.Config())
	}

	for _, v := range ac.Versions(ctx, fc.nodes, testTimeout) {
		if v.Err != nil || v.Version != "v9" || v.Proto != pb.ProtocolVersion {
			t.Errorf("version of %s = %+v", v.Node.Name, v)
		}
	}

	byC, byN := ac.Usage(ctx, fc.nodes, testTimeout, NewStatsGate())
	if len(byC) != 3 || len(byN) != 3 || byC["c-node-b"].Health != "healthy" || byN["node-c"].NodeCPUs != 4 {
		t.Errorf("usage = %v / %v", byC, byN)
	}

	vols, errs := ac.Volumes(ctx, fc.nodes, testTimeout)
	if len(errs) != 0 || len(vols) != 1 || vols[0].Name != "data" || len(vols[0].Nodes) != 3 || vols[0].Created.IsZero() {
		t.Errorf("volumes = %+v errs = %v", vols, errs)
	}
	if sizes := ac.VolumeSizes(ctx, fc.nodes, testTimeout); sizes["data"] != 3<<20 {
		t.Errorf("sizes = %v, want the three copies summed", sizes)
	}
	if usage := ac.VolumeUsage(ctx, fc.nodes, testTimeout); len(usage["data"]) != 3 || usage["data"][0].Service != "web" {
		t.Errorf("usage = %v", usage)
	}
	for _, r := range ac.RemoveVolume(ctx, fc.nodes[:2], "data", false, testTimeout) {
		if r.Err != nil {
			t.Errorf("remove on %s: %v", r.Node.Name, r.Err)
		}
	}
	for _, r := range ac.CreateVolume(ctx, fc.nodes[:1], &pb.CreateVolumeRequest{Name: "new"}, testTimeout) {
		if r.Err != nil {
			t.Errorf("create on %s: %v", r.Node.Name, r.Err)
		}
	}

	if ni, err := ac.Images(ctx, n, testTimeout); err != nil || ni.Total != 100 || ni.Reclaimable() != 10 || ni.Count != 2 {
		t.Errorf("images = %+v, %v", ni, err)
	}
	if rec, del, err := ac.PruneImages(ctx, n, false, testTimeout); err != nil || rec != 10 || del != 1 {
		t.Errorf("prune images = %d %d %v", rec, del, err)
	}
	if _, err := ac.ReclaimStats(ctx, n, true, testTimeout); err != nil {
		t.Errorf("reclaim stats: %v", err)
	}
	if _, err := ac.Prune(ctx, n, &pb.PruneRequest{}, testTimeout); err != nil {
		t.Errorf("prune: %v", err)
	}

	list, err := ac.ListContainers(ctx, n.DialHost, "web")
	if err != nil || len(list) != 1 || !slices.Equal(list[0].GetPorts(), []uint32{80}) {
		t.Errorf("containers = %v, %v", list, err)
	}

	ep := resolve.Endpoint{DialHost: n.DialHost, ContainerID: "abc"}
	var out bytes.Buffer
	code, err := ac.RunNonInteractive(ctx, ep, []string{"echo", "hi"}, testTimeout, &out, io.Discard)
	if err != nil || code != 0 || out.String() != "ran: echo hi" {
		t.Errorf("exec = %d %v %q", code, err, out.String())
	}
	fc.agents["node-a"].execOut = "/bin/bash\n/bin/sh\n"
	if bash, sh := ac.ProbeShells(ctx, ep, testTimeout); !bash || !sh {
		t.Errorf("probe = bash %v sh %v, want both", bash, sh)
	}

	if bad := fc.badBindings(); bad != 0 {
		t.Errorf("%d call(s) carried a binding for another connection's certificate", bad)
	}
}

// Errors from a node: a refused connection is reported per node, an agent
// that predates an RPC is explained as too old, and the rest of the cluster
// still answers.
func TestNodeFailuresStayPerNode(t *testing.T) {
	fc := newFakeCluster(t, "node-a", "node-b")
	fc.setDown("node-b", true)
	fc.agents["node-a"].unimplemented = map[string]bool{"ListImages": true}
	ac := New(fc.cfg)
	defer ac.Close()
	ctx := context.Background()

	vols, errs := ac.Volumes(ctx, fc.nodes, testTimeout)
	if len(vols) != 1 || errs["node-b"] == nil || errs["node-a"] != nil {
		t.Fatalf("volumes = %v, errs = %v; want node-b alone failing", vols, errs)
	}
	if !strings.Contains(errs["node-b"].Error(), "cannot reach agent") {
		t.Errorf("node-b error = %v", errs["node-b"])
	}

	_, err := ac.Images(ctx, fc.nodes[0], testTimeout)
	if !errors.Is(err, agenterr.ErrAgentTooOld) || status.Code(err) != codes.Unimplemented {
		t.Errorf("images on an old agent = %v, want ErrAgentTooOld wrapping Unimplemented", err)
	}

	got := ac.Versions(ctx, fc.nodes, testTimeout)
	if got[0].Err != nil || got[1].Err == nil {
		t.Errorf("versions = %+v", got)
	}
	if _, _, err := ac.PruneImages(ctx, fc.nodes[1], true, testTimeout); err == nil {
		t.Error("prune on a down node should fail")
	}
	if _, err := ac.ReclaimStats(ctx, fc.nodes[1], false, testTimeout); err == nil {
		t.Error("reclaim on a down node should fail")
	}
	if _, err := ac.Prune(ctx, fc.nodes[1], &pb.PruneRequest{}, testTimeout); err == nil {
		t.Error("prune on a down node should fail")
	}
	if _, err := ac.ListContainers(ctx, "node-b", ""); err == nil {
		t.Error("containers on a down node should fail")
	}
	if _, err := ac.RunNonInteractive(ctx, resolve.Endpoint{DialHost: "node-b"}, []string{"true"}, testTimeout, io.Discard, io.Discard); err == nil {
		t.Error("exec on a down node should fail")
	}
	if bash, sh := ac.ProbeShells(ctx, resolve.Endpoint{DialHost: "node-b"}, testTimeout); bash || sh {
		t.Error("an unreachable container has no shells")
	}
}

// A Conn is closed exactly once, however often the holder says so.
func TestConnCloseIsIdempotent(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	ac := New(fc.cfg)
	defer ac.Close()
	conn, err := ac.Conn(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Agent().Version(context.Background(), &pb.VersionRequest{}); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	conn.Close()
	var none *Conn
	none.Close()
}
