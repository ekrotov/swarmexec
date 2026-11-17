// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/swarmfake"
)

// A cluster whose agent service is deployed but whose agents all refuse the
// connection (the harness gets no agent): the case where an empty answer is
// not "nothing there".
func allAgentsDownSwarm() *swarmfake.Swarm {
	f := nodeOpsSwarm()
	var agent swarm.Service
	agent.ID, agent.Spec.Name = "s-agent", "swarmexec_agent"
	agent.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "logleio/swarmexec-agent:1.2.3"}
	f.Services = append(f.Services, agent)
	return f
}

// CLI-04: when every agent fails, a fan-out read command used to print an
// empty list and exit 0, so a monitoring script could not tell "no volumes"
// from "nobody answered". It is now 125 with nothing on stdout.
func TestVolumeLsAllAgentsDownIs125(t *testing.T) {
	g := cliCluster(t, allAgentsDownSwarm(), nil)
	for _, args := range [][]string{{"--connect-timeout", "2s"}, {"--json", "--connect-timeout", "2s"}} {
		out, errOut, err := runCmd(t, newVolumeLsCmd(g), args...)
		if err == nil || exitCode(err) != 125 || !strings.Contains(err.Error(), "no node answered (2 of 2 unreachable)") {
			t.Fatalf("%v: err = %v (code %d)", args, err, exitCode(err))
		}
		if out != "" {
			t.Errorf("%v: stdout must stay empty on a total failure, got %q", args, out)
		}
		if !strings.Contains(errOut, "warning: node 127.0.0.2 unreachable") {
			t.Errorf("%v: per-node reasons missing: %q", args, errOut)
		}
	}
}

func TestStatsAllAgentsDownIs125(t *testing.T) {
	g := cliCluster(t, allAgentsDownSwarm(), nil)
	for _, args := range [][]string{{"--wait", "0", "--connect-timeout", "2s"}, {"--json", "--wait", "0", "--connect-timeout", "2s"}} {
		out, _, err := runCmd(t, newStatsCmd(g), args...)
		if err == nil || exitCode(err) != 125 || !strings.Contains(err.Error(), "no node agent answered the stats request") {
			t.Fatalf("%v: err = %v (code %d)", args, err, exitCode(err))
		}
		if out != "" {
			t.Errorf("%v: stdout = %q", args, out)
		}
	}
}
