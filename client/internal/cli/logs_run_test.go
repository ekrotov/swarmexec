// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/session"
	"swarmexec/client/internal/swarmfake"
	pb "swarmexec/internal/pb"
)

// oneShotLogs is an agent whose log stream sends a container's lines and
// ends, like `logs` without -f; container c-bad is refused.
type oneShotLogs struct {
	pb.UnimplementedAgentServer
}

func (oneShotLogs) Logs(r *pb.LogsRequest, s pb.Agent_LogsServer) error {
	id := r.GetContainerId()
	if id == "c-bad" {
		return status.Error(codes.NotFound, "no such container: c-bad")
	}
	for _, l := range []string{
		`{"level":"info","msg":"hello from ` + id + `"}`,
		`{"level":"error","msg":"boom in ` + id + `"}`,
	} {
		if err := s.Send(&pb.LogChunk{Payload: &pb.LogChunk_Stdout{Stdout: []byte(l + "\n")}}); err != nil {
			return err
		}
	}
	return s.Send(&pb.LogChunk{Payload: &pb.LogChunk_Stderr{Stderr: []byte("warn on stderr " + id + "\n")}})
}

// logsSwarm: shop_web runs one container, shop_api two, shop_db none (its
// last one failed), bad's container is refused by the agent.
func logsSwarm() *swarmfake.Swarm {
	exited := swarm.Task{ID: "t-db", ServiceID: "s-db", Slot: 1, NodeID: "n1", DesiredState: swarm.TaskStateShutdown}
	exited.Status.State = swarm.TaskStateFailed
	exited.Status.Err = "exit 1"
	exited.Status.Timestamp = time.Now().Add(-time.Minute)
	exited.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: "c-db"}
	db := stackedService("s-db", "shop_db", "shop", 1)
	db.ServiceStatus.RunningTasks = 0
	return &swarmfake.Swarm{
		Services: []swarm.Service{
			stackedService("s-web", "shop_web", "shop", 1),
			stackedService("s-api", "shop_api", "shop", 2),
			db,
			stackedService("s-bad", "bad", "", 1),
		},
		Tasks: []swarm.Task{
			runningTask("s-web", "c-web", "n1", 1),
			runningTask("s-api", "c-api1", "n1", 1),
			runningTask("s-api", "c-api2", "n1", 2),
			runningTask("s-bad", "c-bad", "n1", 1),
			exited,
		},
		Nodes: []swarm.Node{readyNode("n1", agentNodeHost)},
	}
}

// lines is a source's log lines as oneShotLogs sends them on stdout, each
// with prefix.
func lines(prefix, id string) []string {
	return []string{
		prefix + `{"level":"info","msg":"hello from ` + id + `"}`,
		prefix + `{"level":"error","msg":"boom in ` + id + `"}`,
	}
}

// `logs` end to end: one container, filters, several sources, the
// last-exited fallback and the errors. Several sources interleave, so the
// lines are compared as sets, each stream on its own.
func TestLogsRun(t *testing.T) {
	g := cliCluster(t, logsSwarm(), oneShotLogs{})
	api := slices.Concat(lines("shop_api.1 | ", "c-api1"), lines("shop_api.2 | ", "c-api2"))
	apiErr := []string{"shop_api.1 | warn on stderr c-api1", "shop_api.2 | warn on stderr c-api2"}
	cases := []struct {
		name           string
		args           []string
		stdout, stderr []string
	}{
		{"one container", []string{"shop_web"}, lines("", "c-web"), []string{"warn on stderr c-web"}},
		{"filtered", []string{"--log-format", "json", "--min-level", "error", "shop_web"},
			[]string{"ERROR  boom in c-web"}, []string{"warn on stderr c-web"}},
		{"auto", []string{"--log-format", "auto", "--grep", "hello", "shop_web"}, lines("", "c-web")[:1], nil},
		{"replicas", []string{"shop_api"}, api, apiErr},
		{"replicas auto", []string{"--log-format", "auto", "shop_api"}, api, apiErr},
		{"replicas filtered", []string{"--log-format", "json", "--min-level", "error", "shop_api"},
			[]string{"shop_api.1 | ERROR  boom in c-api1", "shop_api.2 | ERROR  boom in c-api2"}, apiErr},
		{"two targets", []string{"shop_web", "shop_api.2"},
			slices.Concat(lines("shop_web.1 | ", "c-web"), lines("shop_api.2 | ", "c-api2")),
			[]string{"shop_web.1 | warn on stderr c-web", "shop_api.2 | warn on stderr c-api2"}},
		{"stack", []string{"--stack", "shop"},
			slices.Concat(lines("shop_web.1 | ", "c-web"), api),
			slices.Concat([]string{"── shop_db: no running container ──", "shop_web.1 | warn on stderr c-web"}, apiErr)},
		{"last exited", []string{"shop_db"}, lines("", "c-db"),
			[]string{"── shop_db has no running container; showing the last container (failed: exit 1) ──", "warn on stderr c-db"}},
	}
	set := func(ls []string) string {
		ls = slices.Clone(ls)
		slices.Sort(ls)
		return strings.Join(ls, "\n")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errOut, err := runCmd(t, newLogsCmd(g), append([]string{"--connect-timeout", "2s"}, c.args...)...)
			if err != nil {
				t.Fatal(err)
			}
			if got := set(strings.Split(strings.TrimSuffix(out, "\n"), "\n")); got != set(c.stdout) {
				t.Errorf("stdout:\n%s\nwant:\n%s", got, set(c.stdout))
			}
			if got := set(strings.Split(strings.TrimSuffix(errOut, "\n"), "\n")); got != set(c.stderr) {
				t.Errorf("stderr:\n%s\nwant:\n%s", got, set(c.stderr))
			}
		})
	}

	exit := func(err error) int {
		var ce *cliError
		if !errors.As(err, &ce) {
			return -1
		}
		return ce.code
	}
	if _, _, err := runCmd(t, newLogsCmd(g), "--min-level", "loud", "shop_web"); exit(err) != usageExitCode {
		t.Errorf("bad level: %v", err)
	}
	if _, _, err := runCmd(t, newLogsCmd(g), "shop_web.9"); exit(err) != session.TransportFailure {
		t.Errorf("no such slot: %v", err)
	}
	// One container: the error is explained — with no agent service in this
	// swarm, as the missing agent.
	if _, _, err := runCmd(t, newLogsCmd(g), "--connect-timeout", "2s", "bad"); exit(err) != session.TransportFailure || !errors.Is(err, errNoAgent) {
		t.Errorf("refused: %v", err)
	}
	if _, _, err := runCmd(t, newLogsCmd(g), "--connect-timeout", "2s", "bad", "shop_web"); exit(err) != session.TransportFailure {
		t.Errorf("refused among several: %v", err)
	}
}
