// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/agenttest"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
)

// The fake cluster in fake_test.go answers whatever the test scripts, which is
// what the pool, the capability gate and the bastion counting need. These tests
// are the other half: the real agent behind the same Client, so that what the
// client sends — the connection-bound credential, the requests, the way
// Docker's errors come back — is checked against the code that will receive it.

func realAgent(t *testing.T, d *agenttest.Docker) (*agenttest.Agent, *Client, resolve.Node) {
	t.Helper()
	a := agenttest.Start(t, agenttest.Options{Secret: "s3cret", Docker: d})
	ac := New(config.Config{Port: 7000, Insecure: true, AgentSecret: "s3cret", ProxyDialer: a.Dial})
	t.Cleanup(ac.Close)
	return a, ac, resolve.Node{ID: "id-a", Name: "node-a", DialHost: "node-a"}
}

func TestRealAgent_ListContainersAndVersion(t *testing.T) {
	d := agenttest.NewDocker().
		AddContainer(agenttest.Container{ID: "c1", Name: "web.1", Service: "web", Volumes: []string{"data"}, Ports: []uint16{8080, 80}}).
		AddContainer(agenttest.Container{ID: "c2", Name: "db.1", Service: "db"})
	a, ac, n := realAgent(t, d)
	ctx := context.Background()

	vs := ac.Versions(ctx, []resolve.Node{n}, testTimeout)
	if len(vs) != 1 || vs[0].Err != nil || vs[0].Proto != pb.ProtocolVersion {
		t.Fatalf("versions = %+v, want the real agent's protocol %s", vs, pb.ProtocolVersion)
	}

	cs, err := ac.ListContainers(ctx, "node-a", "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].GetId() != "c1" || cs[0].GetName() != "web.1" ||
		len(cs[0].GetVolumes()) != 1 || fmt.Sprint(cs[0].GetPorts()) != "[80 8080]" {
		t.Errorf("containers = %v, want web.1 with its volume and sorted ports", cs)
	}
	if recs := a.AuditEvents("container_list"); len(recs) != 1 || recs[0]["filter"] != "web" {
		t.Errorf("audit = %v, want one container_list with filter web", recs)
	}
}

func TestRealAgent_Volumes(t *testing.T) {
	d := agenttest.NewDocker().AddVolume("data").AddVolume("cache")
	_, ac, n := realAgent(t, d)

	vols, errs := ac.Volumes(context.Background(), []resolve.Node{n}, testTimeout)
	if len(errs) != 0 || len(vols) != 2 {
		t.Fatalf("volumes = %+v errs = %v, want the node's two", vols, errs)
	}
}

// RunNonInteractive is how shell probing runs a command: through the real Exec
// bridge, stdout and stderr and the exit code intact.
func TestRealAgent_RunNonInteractive(t *testing.T) {
	d := agenttest.NewDocker().AddContainer(agenttest.Container{ID: "c1", Service: "web"}).
		SetExec(func(cmd []string, _ io.Reader, stdout, stderr io.Writer) int {
			fmt.Fprintf(stdout, "ran %v", cmd)
			fmt.Fprint(stderr, "warn")
			return 3
		})
	_, ac, _ := realAgent(t, d)
	var out, errOut bytes.Buffer
	code, err := ac.RunNonInteractive(context.Background(),
		resolve.Endpoint{DialHost: "node-a", ContainerID: "c1", NodeName: "node-a"},
		[]string{"echo", "hi"}, testTimeout, &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 || out.String() != "ran [echo hi]" || errOut.String() != "warn" {
		t.Errorf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

// A Docker failure comes back as the status the contract promises, not as a
// generic Internal — across the real handler, dockerStatus and the client.
func TestRealAgent_DockerErrorsKeepTheirMeaning(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"conflict", fmt.Errorf("busy: %w", cerrdefs.ErrConflict), codes.FailedPrecondition},
		{"not found", fmt.Errorf("gone: %w", cerrdefs.ErrNotFound), codes.NotFound},
		{"unavailable", fmt.Errorf("daemon down: %w", cerrdefs.ErrUnavailable), codes.Unavailable},
		{"other", fmt.Errorf("boom"), codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := agenttest.NewDocker().Fail("ContainerList", tc.err)
			_, ac, _ := realAgent(t, d)
			_, err := ac.ListContainers(context.Background(), "node-a", "")
			if got := status.Code(err); got != tc.want {
				t.Fatalf("err = %v (code %v), want %v", err, got, tc.want)
			}
		})
	}
}

// The credential the client binds to the connection is accepted by the real
// check, and a client holding the wrong secret is turned away before Docker is
// asked anything.
func TestRealAgent_ConnectionBoundSecret(t *testing.T) {
	d := agenttest.NewDocker().AddContainer(agenttest.Container{ID: "c1"})
	a := agenttest.Start(t, agenttest.Options{Secret: "s3cret", Docker: d})
	n := resolve.Node{ID: "id-a", Name: "node-a", DialHost: "node-a"}

	good := New(config.Config{Port: 7000, Insecure: true, AgentSecret: "s3cret", ProxyDialer: a.Dial})
	defer good.Close()
	if vs := good.Versions(context.Background(), []resolve.Node{n}, testTimeout); vs[0].Err != nil {
		t.Fatalf("right secret: %v", vs[0].Err)
	}

	bad := New(config.Config{Port: 7000, Insecure: true, AgentSecret: "wrong", ProxyDialer: a.Dial})
	defer bad.Close()
	if _, err := bad.ListContainers(context.Background(), "node-a", ""); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong secret: err = %v, want Unauthenticated", err)
	}
	if d.Called("ContainerList") != 0 {
		t.Error("a caller with the wrong secret reached Docker")
	}
}
