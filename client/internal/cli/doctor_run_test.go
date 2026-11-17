// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/system"

	"swarmexec/client/internal/session"
	"swarmexec/client/internal/swarmfake"
	pb "swarmexec/internal/pb"
)

// versionAgent answers Version like an agent of the given build.
type versionAgent struct {
	pb.UnimplementedAgentServer
	version, proto string
}

func (a versionAgent) Version(context.Context, *pb.VersionRequest) (*pb.VersionResponse, error) {
	return &pb.VersionResponse{Version: a.version, ProtoVersion: a.proto}, nil
}

// doctorCluster: a manager with the agent service deployed (or not), node
// 127.0.0.1 answering as an agent of build 1.2.3, and optionally a second
// node 127.0.0.2 that does not answer.
func doctorCluster(t *testing.T, deployed, deadNode bool) *globalFlags {
	t.Helper()
	nodes := []swarm.Node{readyNode("n1", agentNodeHost)}
	if deadNode {
		nodes = append(nodes, readyNode("n2", "127.0.0.2"))
	}
	info := system.Info{Name: "manager-1"}
	info.Swarm.Nodes = len(nodes)
	f := &swarmfake.Swarm{Nodes: nodes, Info: &info}
	if deployed {
		var svc swarm.Service
		svc.ID, svc.Spec.Name = "s-agent", "swarmexec_agent"
		svc.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "logleio/swarmexec-agent:1.2.3"}
		f.Services = []swarm.Service{svc}
	}
	g := cliCluster(t, f, versionAgent{version: "1.2.3", proto: "7"})
	g.version = Version{Binary: "1.2.3", Proto: "7"}
	return g
}

// `doctor` end to end: the healthy table, the unhealthy table and JSON, and
// an unreachable manager in both forms.
func TestDoctorOutput(t *testing.T) {
	cases := []struct {
		name            string
		deployed, dead  bool
		args            []string
		want            string
		wantFail        bool
		unreachableHost bool
	}{
		{"healthy", true, false, nil,
			"client:  1.2.3 (proto 7)\n" +
				"manager: manager-1 (1 node)\n" +
				"agent:   service deployed (logleio/swarmexec-agent:1.2.3)\n\n" +
				"NODE       AGENT  VERSION  PROTO\n" +
				"127.0.0.1  ok     1.2.3    7\n\n" +
				"✓ all agents healthy\n", false, false},
		{"not deployed, node down", false, true, []string{"--connect-timeout", "2s"},
			"client:  1.2.3 (proto 7)\n" +
				"manager: manager-1 (2 nodes)\n" +
				"agent:   NOT DEPLOYED — run `swarmexec init`\n\n" +
				"NODE       AGENT        VERSION  PROTO\n" +
				"127.0.0.1  ok           1.2.3    7\n" +
				"127.0.0.2  unreachable  -        -\n\n" +
				"⚠ some checks need attention (see above)\n", true, false},
		{"json, node down", true, true, []string{"--json", "--connect-timeout", "2s"},
			`{
  "client": "1.2.3",
  "proto": "7",
  "manager": "manager-1",
  "agent_deployed": true,
  "image": "logleio/swarmexec-agent:1.2.3",
  "healthy": false,
  "nodes": [
    {
      "node": "127.0.0.1",
      "status": "ok",
      "version": "1.2.3",
      "proto": "7"
    },
    {
      "node": "127.0.0.2",
      "status": "unreachable",
      "version": "-",
      "proto": "-"
    }
  ]
}
`, true, false},
		{"json healthy", true, false, []string{"--json"},
			`{
  "client": "1.2.3",
  "proto": "7",
  "manager": "manager-1",
  "agent_deployed": true,
  "image": "logleio/swarmexec-agent:1.2.3",
  "healthy": true,
  "nodes": [
    {
      "node": "127.0.0.1",
      "status": "ok",
      "version": "1.2.3",
      "proto": "7"
    }
  ]
}
`, false, false},
		{"manager unreachable", true, false, nil, "client:  1.2.3 (proto 7)\nmanager: UNREACHABLE — ", true, true},
		{"manager unreachable json", true, false, []string{"--json"},
			`{
  "client": "1.2.3",
  "proto": "7",
  "manager": "unreachable",
  "agent_deployed": false,
  "healthy": false
}
`, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := doctorCluster(t, c.deployed, c.dead)
			if c.unreachableHost {
				t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
			}
			out, _, err := runCmd(t, newDoctorCmd(g), c.args...)
			got := out
			if c.unreachableHost && len(got) > len(c.want) && c.want[len(c.want)-1] == ' ' {
				got = got[:len(c.want)] // the rest is the dial error
			}
			if got != c.want {
				t.Errorf("stdout:\n%s\nwant:\n%s", out, c.want)
			}
			ce, isCLI := err.(*cliError)
			switch {
			case !c.wantFail && err != nil:
				t.Errorf("err = %v, want none", err)
			case c.wantFail && (!isCLI || ce.code != session.TransportFailure || !ce.silent):
				t.Errorf("err = %#v, want a silent transport failure", err)
			}
		})
	}
}
