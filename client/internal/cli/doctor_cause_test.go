// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/system"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/swarmfake"
	pb "swarmexec/internal/pb"
)

// rejectingAgent refuses every call the way an agent does for a wrong secret.
type rejectingAgent struct{ pb.UnimplementedAgentServer }

func (rejectingAgent) Version(context.Context, *pb.VersionRequest) (*pb.VersionResponse, error) {
	return nil, status.Error(codes.Unauthenticated, "invalid or missing agent secret")
}

// CLI-05: every failure used to read "unreachable". A rejected secret and a
// refused connection are different problems with different fixes, and doctor
// is the command people run to tell them apart.
func TestDoctorNamesTheCauseOfAnUnreachableNode(t *testing.T) {
	f := &swarmfake.Swarm{Nodes: []swarm.Node{readyNode("n1", agentNodeHost)}, Info: &system.Info{Name: "manager-1"}}
	g := cliCluster(t, f, rejectingAgent{})
	g.version = Version{Binary: "1.2.3", Proto: "7"}
	out, _, err := runCmd(t, newDoctorCmd(g), "--json")
	if exitCode(err) != 125 {
		t.Fatalf("exit = %d, want 125", exitCode(err))
	}
	if !strings.Contains(out, `"status": "unreachable"`) || !strings.Contains(out, `"error": "agent rejected the shared secret.`) {
		t.Errorf("json lacks the cause:\n%s", out)
	}
	out, _, _ = runCmd(t, newDoctorCmd(g))
	if !strings.Contains(out, "ERROR") || !strings.Contains(out, "agent rejected the shared secret.") || strings.Contains(out, "refused") {
		t.Errorf("table lacks the cause:\n%s", out)
	}
}

// CLI-05: doctor used to refuse to run without a complete agent config, which
// is exactly the state of a machine that has not run init. It now answers
// with what it can see.
func TestDoctorWithoutAgentConfig(t *testing.T) {
	g := doctorCluster(t, false, false)
	if err := os.WriteFile(g.configPath, []byte("port: 7777\naddr_mode: hostname\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCmd(t, newDoctorCmd(g))
	if exitCode(err) != 125 {
		t.Fatalf("exit = %d, want 125 (not healthy): %v", exitCode(err), err)
	}
	for _, want := range []string{"manager: manager-1", "agent:   NOT DEPLOYED — run `swarmexec init`", "config:  agent configuration incomplete, agents not probed:"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	out, _, _ = runCmd(t, newDoctorCmd(g), "--json")
	if !strings.Contains(out, `"config_error": "`) || !strings.Contains(out, `"healthy": false`) {
		t.Errorf("json:\n%s", out)
	}
}
