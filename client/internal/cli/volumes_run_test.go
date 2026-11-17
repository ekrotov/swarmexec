// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/swarm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/session"
	"swarmexec/client/internal/swarmfake"
	pb "swarmexec/internal/pb"
)

// removeVolAgent holds a few volumes; RemoveVolume drops one and records the
// request, except "busy", which is in use.
type removeVolAgent struct {
	pb.UnimplementedAgentServer
	mu      sync.Mutex
	vols    []string
	removed []string // "name" or "name!force"
}

func (a *removeVolAgent) ListVolumes(context.Context, *pb.ListVolumesRequest) (*pb.ListVolumesResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := &pb.ListVolumesResponse{}
	for _, v := range a.vols {
		out.Volumes = append(out.Volumes, &pb.VolumeInfo{Name: v, Driver: "local"})
	}
	return out, nil
}

func (a *removeVolAgent) RemoveVolume(_ context.Context, r *pb.RemoveVolumeRequest) (*pb.RemoveVolumeResponse, error) {
	if r.GetName() == "busy" {
		return nil, status.Error(codes.FailedPrecondition, "volume busy is in use")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := r.GetName()
	if r.GetForce() {
		rec += "!force"
	}
	a.removed = append(a.removed, rec)
	return &pb.RemoveVolumeResponse{}, nil
}

func (a *removeVolAgent) removals() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.removed, ",")
}

// volumeSwarm: node-a answers, node-b (127.0.0.2) refuses connections.
func volumeSwarm() *swarmfake.Swarm {
	return &swarmfake.Swarm{
		Nodes: []swarm.Node{readyNode("n1", agentNodeHost), readyNode("n2", "127.0.0.2")},
	}
}

// `volume rm` end to end: it removes only what was asked for, where it was
// asked, says what happened per node, asks first, and fails loudly.
func TestVolumeRmRun(t *testing.T) {
	agent := &removeVolAgent{vols: []string{"data", "busy"}}
	g := cliCluster(t, volumeSwarm(), agent)
	rm := func(stdin string, args ...string) (string, string, error) {
		t.Helper()
		return runIO(t, newVolumeRmCmd(g), stdin, append([]string{"--connect-timeout", "2s"}, args...)...)
	}

	t.Run("a scope is required", func(t *testing.T) {
		_, _, err := rm("", "--yes", "data")
		if exitCode(err) != usageExitCode || !strings.Contains(err.Error(), "--all or one/more --node") {
			t.Errorf("err = %v", err)
		}
		if agent.removals() != "" {
			t.Error("removed without a scope")
		}
	})

	t.Run("--all removes on the nodes holding it and says so", func(t *testing.T) {
		out, errOut, err := rm("", "--all", "--yes", "data")
		if err != nil {
			t.Fatal(err)
		}
		if out != agentNodeHost+": removed\n" {
			t.Errorf("stdout = %q", out)
		}
		if !strings.Contains(errOut, "warning: node 127.0.0.2 unreachable") {
			t.Errorf("stderr = %q: the node that could not be asked is reported", errOut)
		}
		if got := agent.removals(); got != "data" {
			t.Errorf("removed %q", got)
		}
	})

	t.Run("--force reaches the agent", func(t *testing.T) {
		agent.mu.Lock()
		agent.removed = nil
		agent.mu.Unlock()
		if _, _, err := rm("", "--all", "--force", "--yes", "data"); err != nil {
			t.Fatal(err)
		}
		if got := agent.removals(); got != "data!force" {
			t.Errorf("removed %q", got)
		}
	})

	t.Run("a volume nobody has is an error", func(t *testing.T) {
		_, _, err := rm("", "--all", "--yes", "nope")
		if exitCode(err) != session.TransportFailure || !strings.Contains(err.Error(), `volume "nope" not found on any node`) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("--node limits it, and a node without the volume is an error", func(t *testing.T) {
		before := agent.removals()
		_, _, err := rm("", "--node", "127.0.0.2", "--yes", "data")
		if exitCode(err) != session.TransportFailure || !strings.Contains(err.Error(), `volume "data" is not on any of the requested nodes`) {
			t.Errorf("err = %v", err)
		}
		if agent.removals() != before {
			t.Error("removed on a node that was not asked for")
		}
		if _, _, err := rm("", "--node", agentNodeHost, "--yes", "data"); err != nil {
			t.Errorf("--node %s: %v", agentNodeHost, err)
		}
	})

	t.Run("a volume in use fails, per node, and the command with it", func(t *testing.T) {
		out, _, err := rm("", "--all", "--yes", "busy")
		var ce *cliError
		if !errors.As(err, &ce) || ce.code != session.TransportFailure || !ce.silent {
			t.Errorf("err = %#v, want a silent failure", err)
		}
		if !strings.HasPrefix(out, agentNodeHost+": error: ") || !strings.Contains(out, "in use") {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("without --yes the operator is asked", func(t *testing.T) {
		agent.mu.Lock()
		agent.removed = nil
		agent.mu.Unlock()

		asked := usePrompts(t, "n\n", true)
		_, errOut, err := rm("", "--all", "data")
		var ce *cliError
		if !errors.As(err, &ce) || !ce.silent || ce.code != session.TransportFailure {
			t.Errorf("err = %#v", err)
		}
		if want := `Remove volume "data" on 1 node(s) [` + agentNodeHost + `]? [y/N]: `; asked.String() != want {
			t.Errorf("asked %q, want %q", asked.String(), want)
		}
		if !strings.Contains(errOut, "aborted") || agent.removals() != "" {
			t.Errorf("stderr %q removed %q after \"n\"", errOut, agent.removals())
		}

		usePrompts(t, "y\n", true)
		if _, _, err := rm("", "--all", "data"); err != nil || agent.removals() != "data" {
			t.Errorf("after \"y\": err %v removed %q", err, agent.removals())
		}

		agent.mu.Lock()
		agent.removed = nil
		agent.mu.Unlock()
		q := usePrompts(t, "y\n", false)
		_, _, err = rm("", "--all", "data")
		if err == nil || q.String() != "" || agent.removals() != "" {
			t.Errorf("without a terminal: err %v asked %q removed %q — refuse, do not read a piped answer", err, q.String(), agent.removals())
		}
	})
}

// With every node unreachable and no agent deployed, rm says so instead of
// listing refused connections.
func TestVolumeRmRunNoAgent(t *testing.T) {
	g := cliCluster(t, volumeSwarm(), nil)
	_, _, err := runIO(t, newVolumeRmCmd(g), "", "--all", "--yes", "--connect-timeout", "2s", "data")
	if exitCode(err) != session.TransportFailure || !errors.Is(err, errNoAgent) {
		t.Errorf("err = %v", err)
	}
}

// volume ls prints through the command's streams: the table and the JSON.
func TestVolumeLsRun(t *testing.T) {
	g := cliCluster(t, volumeSwarm(), &removeVolAgent{vols: []string{"data", "other"}})
	out, errOut, err := runIO(t, newVolumeLsCmd(g), "", "--connect-timeout", "2s", "dat")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "VOLUME") || !strings.Contains(out, "data") || strings.Contains(out, "other") {
		t.Errorf("table:\n%s", out)
	}
	if !strings.Contains(errOut, "warning: node 127.0.0.2 unreachable") {
		t.Errorf("stderr = %q", errOut)
	}
	out, _, err = runIO(t, newVolumeLsCmd(g), "", "--connect-timeout", "2s", "--json", "zzz")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("json of no match: %q, %v", out, err)
	}
}
