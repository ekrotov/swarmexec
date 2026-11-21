// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/system"

	"swarmexec/client/internal/agentdeploy"
	"swarmexec/client/internal/session"
	"swarmexec/client/internal/swarmfake"
	"swarmexec/internal/deploy"
	pb "swarmexec/internal/pb"
)

// versionAgent answers Version like an agent of the given build.
type versionAgent struct {
	pb.UnimplementedAgentServer
	version, proto string
	// clockOffset moves the time this agent reports away from the test
	// machine's, so doctor's drift column has something to find. Zero means
	// "answer with our own clock"; see noClock for an agent too old to answer
	// with one at all.
	clockOffset time.Duration
	noClock     bool
}

func (a versionAgent) Version(context.Context, *pb.VersionRequest) (*pb.VersionResponse, error) {
	resp := &pb.VersionResponse{Version: a.version, ProtoVersion: a.proto}
	if !a.noClock {
		resp.Now = time.Now().UTC().Add(a.clockOffset).Format(time.RFC3339Nano)
	}
	return resp, nil
}

// doctorCluster: a manager with the agent service deployed (or not), node
// 127.0.0.1 answering as an agent of build 1.2.3, and optionally a second
// node 127.0.0.2 that does not answer.
//
// The deployed service is built by agentdeploy.Spec, not by hand: doctor now
// compares the running spec against that function, so a fixture written out
// separately would drift from it and the test would report a difference the
// product does not have.
func doctorCluster(t *testing.T, deployed, deadNode bool) *globalFlags {
	t.Helper()
	return doctorClusterSpec(t, deadNode, agentSpecOrNil(deployed))
}

func agentSpecOrNil(deployed bool) *swarm.ServiceSpec {
	if !deployed {
		return nil
	}
	spec := agentdeploy.Spec(agentdeploy.Options{
		Image:       "logleio/swarmexec-agent:1.2.3",
		ServiceName: deploy.ServiceName,
		Port:        deploy.DefaultPort,
	}, "sec-1")
	return &spec
}

// doctorClusterSpec is doctorCluster with the agent service's spec given, so
// a test can deploy a drifted one.
func doctorClusterSpec(t *testing.T, deadNode bool, spec *swarm.ServiceSpec) *globalFlags {
	t.Helper()
	return doctorClusterAgent(t, deadNode, spec, versionAgent{version: "1.2.3", proto: "7", noClock: true})
}

// doctorClusterAgent is doctorClusterSpec with the agent the nodes answer as,
// so a test can give one a clock that disagrees with this machine's.
func doctorClusterAgent(t *testing.T, deadNode bool, spec *swarm.ServiceSpec, agent versionAgent) *globalFlags {
	t.Helper()
	nodes := []swarm.Node{readyNode("n1", agentNodeHost)}
	if deadNode {
		nodes = append(nodes, readyNode("n2", "127.0.0.2"))
	}
	info := system.Info{Name: "manager-1"}
	info.Swarm.Nodes = len(nodes)
	f := &swarmfake.Swarm{Nodes: nodes, Info: &info}
	if spec != nil {
		f.Services = []swarm.Service{{ID: "s-agent", Spec: *spec}}
	}
	g := cliCluster(t, f, agent)
	g.version = Version{Binary: "1.2.3", Proto: "7"}
	return g
}

// clockedAgent answers Version with a clock offset by d from this machine's.
// The golden-output fixtures use noClock instead: a measured offset differs on
// every run, and an agent that reports no time is also what the whole fleet
// looks like until the agents carrying this field are rolled out.
func clockedAgent(d time.Duration) versionAgent {
	return versionAgent{version: "1.2.3", proto: "7", clockOffset: d}
}

// `doctor` end to end: the healthy table, the unhealthy table and JSON, and
// an unreachable manager in both forms.
var portRE = regexp.MustCompile(`:\d{4,5}\b`)

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
				"NODE       AGENT        VERSION  PROTO  ERROR\n" +
				"127.0.0.1  ok           1.2.3    7      -\n" +
				"127.0.0.2  unreachable  -        -      cannot reach agent on 127.0.0.2:PORT (mTLS/connection error: connection error: desc = \"transport: Error while dialing: dial tcp 127.0.0.2:PORT: connect: connection refused\")\n\n" +
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
      "proto": "-",
      "error": "cannot reach agent on 127.0.0.2:PORT (mTLS/connection error: connection error: desc = \"transport: Error while dialing: dial tcp 127.0.0.2:PORT: connect: connection refused\")"
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
			got := portRE.ReplaceAllString(out, ":PORT")
			if c.unreachableHost && len(got) > len(c.want) && c.want[len(c.want)-1] == ' ' {
				got = got[:len(c.want)] // the rest is the dial error
			}
			if got != c.want {
				out = got
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

// F17: a newer release is an extra line (and a JSON field), information
// only — a healthy cluster stays healthy, exit code 0. Switched off, the
// check is not even asked.
func TestDoctorReportsNewerRelease(t *testing.T) {
	calls := stubUpdate(t, "v1.27.0")
	t.Setenv("SWARMEXEC_NO_UPDATE_CHECK", "")
	g := doctorCluster(t, true, false)
	out, _, err := runCmd(t, newDoctorCmd(g))
	if err != nil {
		t.Fatalf("err = %v, want none: a newer release is not a failure", err)
	}
	want := "client:  1.2.3 (proto 7)\nupdate:  v1.27.0 is available — see https://swarmshell.dev/docs/install\nmanager: manager-1"
	if !strings.HasPrefix(out, want) || !strings.HasSuffix(out, "✓ all agents healthy\n") {
		t.Errorf("stdout:\n%s", out)
	}
	if c := calls.list(); len(c) != 1 || c[0].current != "1.2.3" {
		t.Errorf("check calls %+v", c)
	}

	out, _, _ = runCmd(t, newDoctorCmd(g), "--json")
	if !strings.Contains(out, `"latest_release": "v1.27.0"`) {
		t.Errorf("json lacks latest_release:\n%s", out)
	}

	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	out, _, _ = runCmd(t, newDoctorCmd(g))
	if !strings.HasPrefix(out, "client:  1.2.3 (proto 7)\nupdate:  v1.27.0 is available") {
		t.Errorf("unreachable manager:\n%s", out)
	}

	t.Setenv("SWARMEXEC_NO_UPDATE_CHECK", "1")
	n := len(calls.list())
	out, _, _ = runCmd(t, newDoctorCmd(g))
	if strings.Contains(out, "update:") || len(calls.list()) != n {
		t.Errorf("switched off, still asked or printed:\n%s", out)
	}
}

// A7: `doctor` compares the deployed agent spec against the one `init` writes.
//
// The case this is built from is real: on 2026-11-20 four clusters ran an
// agent service without the /sys mount that v1.30.0 added, and `doctor` said
// "all agents healthy" on every one of them — reachable, right version, right
// protocol, and a spec nobody checked.
func TestDoctorReportsSpecDrift(t *testing.T) {
	spec := agentSpecOrNil(true)
	cs := spec.TaskTemplate.ContainerSpec
	var kept []mount.Mount
	for _, m := range cs.Mounts {
		if m.Source != deploy.HostSysfsPath {
			kept = append(kept, m)
		}
	}
	cs.Mounts = kept

	g := doctorClusterSpec(t, false, spec)
	out, _, err := runCmd(t, newDoctorCmd(g))
	for _, want := range []string{"spec:", "DRIFTED", deploy.HostSysfsPath, "init --force"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "all agents healthy") {
		t.Errorf("a drifted cluster was called healthy:\n%s", out)
	}
	// Drift must fail the command, or a pipeline that runs `doctor` keeps
	// passing on exactly the fleet this check was written for.
	ce, isCLI := err.(*cliError)
	if !isCLI || ce.code != session.TransportFailure {
		t.Errorf("err = %#v, want a transport failure", err)
	}
}

// The same finding in JSON, since that is what a pipeline reads.
func TestDoctorSpecDriftJSON(t *testing.T) {
	spec := agentSpecOrNil(true)
	spec.TaskTemplate.ContainerSpec.Secrets = nil

	g := doctorClusterSpec(t, false, spec)
	out, _, _ := runCmd(t, newDoctorCmd(g), "--json")
	var got struct {
		Healthy   bool                  `json:"healthy"`
		SpecDrift []agentdeploy.Finding `json:"spec_drift"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Healthy {
		t.Error("healthy=true with drift reported")
	}
	if len(got.SpecDrift) != 1 || got.SpecDrift[0].Field != "secrets" {
		t.Errorf("spec_drift = %+v", got.SpecDrift)
	}
}

// A cluster whose spec matches says nothing about it: a check that prints a
// line when all is well is a line people learn to skip.
func TestDoctorSaysNothingWhenTheSpecMatches(t *testing.T) {
	out, _, err := runCmd(t, newDoctorCmd(doctorCluster(t, true, false)))
	if strings.Contains(out, "spec:") {
		t.Errorf("a matching spec was reported:\n%s", out)
	}
	if err != nil {
		t.Errorf("err = %v, want none", err)
	}
}

// A node whose clock is far enough off to mistime certificates and scramble
// log correlation gets a column, a mark and a non-zero exit.
func TestDoctorReportsClockDrift(t *testing.T) {
	g := doctorClusterAgent(t, false, agentSpecOrNil(true), clockedAgent(90*time.Second))
	out, _, err := runCmd(t, newDoctorCmd(g))
	if !strings.Contains(out, "CLOCK") {
		t.Errorf("no clock column:\n%s", out)
	}
	if !strings.Contains(out, "⚠") {
		t.Errorf("drift not marked:\n%s", out)
	}
	if strings.Contains(out, "all agents healthy") {
		t.Errorf("a node 90s out was called healthy:\n%s", out)
	}
	if err == nil {
		t.Error("err = nil, want a transport failure")
	}
}

// An agent too old to report its time leaves the column out entirely, rather
// than filling it with dashes or a zero nobody measured.
func TestDoctorOmitsTheClockColumnForOlderAgents(t *testing.T) {
	out, _, err := runCmd(t, newDoctorCmd(doctorCluster(t, true, false)))
	if strings.Contains(out, "CLOCK") {
		t.Errorf("clock column printed for an agent that sent no time:\n%s", out)
	}
	if err != nil {
		t.Errorf("err = %v, want none", err)
	}
}

// A clock within the threshold is shown but does not fail the command: the
// number is useful, the judgement is only past clockskew.Warn.
func TestDoctorSmallClockOffsetStaysHealthy(t *testing.T) {
	g := doctorClusterAgent(t, false, agentSpecOrNil(true), clockedAgent(200*time.Millisecond))
	out, _, err := runCmd(t, newDoctorCmd(g))
	if !strings.Contains(out, "CLOCK") {
		t.Errorf("no clock column:\n%s", out)
	}
	if strings.Contains(out, "⚠") {
		t.Errorf("an offset inside the threshold was marked:\n%s", out)
	}
	if err != nil {
		t.Errorf("err = %v, want none", err)
	}
}
