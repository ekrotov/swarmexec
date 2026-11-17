// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/agent/agenttest"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	"swarmexec/client/internal/swarmfake"
	pb "swarmexec/internal/pb"
)

// execAgent is an agent whose Exec records the start message and the stdin it
// received, answers with them on stdout, writes "warn" to stderr, and exits
// with the code given as `exit N` (else 0). `fail` makes it answer with an
// agent error instead.
type execAgent struct {
	pb.UnimplementedAgentServer
	mu     sync.Mutex
	starts []*pb.StartExec
	stdin  []string
}

func (a *execAgent) Exec(s pb.Agent_ExecServer) error {
	first, err := s.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	var in bytes.Buffer
	for {
		m, err := s.Recv()
		if err != nil {
			break // the client half-closed (or went away)
		}
		in.Write(m.GetStdin())
	}
	a.mu.Lock()
	a.starts = append(a.starts, start)
	a.stdin = append(a.stdin, in.String())
	a.mu.Unlock()

	cmd := start.GetCmd()
	if len(cmd) > 0 && cmd[0] == "fail" {
		return s.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_Error{Error: "container is gone"}})
	}
	out := fmt.Sprintf("ran %s in %s stdin=%q\n", strings.Join(cmd, " "), start.GetContainerId(), in.String())
	_ = s.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_Stdout{Stdout: []byte(out)}})
	_ = s.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_Stderr{Stderr: []byte("warn\n")}})
	code := 0
	if len(cmd) == 2 && cmd[0] == "exit" {
		code, _ = strconv.Atoi(cmd[1])
	}
	return s.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_ExitCode{ExitCode: int32(code)}})
}

func (a *execAgent) started() []*pb.StartExec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*pb.StartExec(nil), a.starts...)
}

// execSwarm is one node running shop_web (one replica), shop_api (two) and
// shop_db (not running).
func execSwarm() *swarmfake.Swarm {
	return &swarmfake.Swarm{
		Services: []swarm.Service{
			stackedService("s-web", "shop_web", "shop", 1),
			stackedService("s-api", "shop_api", "shop", 2),
		},
		Tasks: []swarm.Task{
			runningTask("s-web", "c-web", "n1", 1),
			runningTask("s-api", "c-api1", "n1", 1),
			runningTask("s-api", "c-api2", "n1", 2),
		},
		Nodes: []swarm.Node{readyNode("n1", agentNodeHost)},
	}
}

// `exec` end to end: the target is resolved on the manager, the command and
// its options reach the agent, stdin goes through, the remote exit code is
// the command's.
func TestExecRun(t *testing.T) {
	agent := &execAgent{}
	g := cliCluster(t, execSwarm(), agent)
	run := func(stdin string, args ...string) (string, string, error) {
		t.Helper()
		return runIO(t, newExecCmd(g), stdin, append([]string{"--connect-timeout", "2s"}, args...)...)
	}
	last := func() *pb.StartExec {
		t.Helper()
		s := agent.started()
		if len(s) == 0 {
			t.Fatal("the agent was never asked to exec")
		}
		return s[len(s)-1]
	}

	t.Run("command runs in the resolved container", func(t *testing.T) {
		out, errOut, err := run("", "shop_web", "--", "echo", "hi")
		if err != nil {
			t.Fatal(err)
		}
		if out != "ran echo hi in c-web stdin=\"\"\n" || errOut != "warn\n" {
			t.Errorf("stdout %q stderr %q", out, errOut)
		}
		if s := last(); s.GetTty() || s.GetContainerId() != "c-web" {
			t.Errorf("start = %v: a command on a pipe wants no TTY", s)
		}
	})

	t.Run("a slot, a task id and a container id name their container", func(t *testing.T) {
		for target, want := range map[string]string{"shop_api.2": "c-api2", "task-c-api1": "c-api1", "c-api2": "c-api2"} {
			out, _, err := run("", target, "--", "id")
			if err != nil || !strings.Contains(out, " in "+want+" ") {
				t.Errorf("exec %s: %q, %v; want container %s", target, out, err, want)
			}
		}
	})

	t.Run("options reach the agent", func(t *testing.T) {
		if _, _, err := run("", "-u", "1000:1000", "-w", "/app", "-e", "A=1", "-e", "B=two", "shop_web", "--", "env"); err != nil {
			t.Fatal(err)
		}
		s := last()
		if s.GetUser() != "1000:1000" || s.GetWorkingDir() != "/app" || strings.Join(s.GetEnv(), ",") != "A=1,B=two" {
			t.Errorf("start = %v", s)
		}
	})

	t.Run("no command opens a shell", func(t *testing.T) {
		if _, _, err := run("", "shop_web"); err != nil {
			t.Fatal(err)
		}
		if got := last().GetCmd(); len(got) != 1 || got[0] != "/bin/sh" {
			t.Errorf("cmd = %v, want /bin/sh", got)
		}
	})

	t.Run("stdin is forwarded, unless switched off", func(t *testing.T) {
		out, _, err := run("payload", "shop_web", "--", "cat")
		if err != nil || !strings.Contains(out, `stdin="payload"`) {
			t.Errorf("stdin on: %q, %v", out, err)
		}
		out, _, err = run("payload", "--stdin=false", "shop_web", "--", "cat")
		if err != nil || !strings.Contains(out, `stdin=""`) {
			t.Errorf("stdin off: %q, %v", out, err)
		}
	})

	t.Run("the remote exit code is passed through, silently", func(t *testing.T) {
		_, errOut, err := run("", "shop_web", "--", "exit", "7")
		var ce *cliError
		if !errors.As(err, &ce) || ce.code != 7 || !ce.silent {
			t.Fatalf("err = %#v, want silent exit code 7", err)
		}
		if errOut != "warn\n" {
			t.Errorf("stderr = %q: a remote failure adds no message", errOut)
		}
		if _, _, err := run("", "shop_web", "--", "exit", "0"); err != nil {
			t.Errorf("exit 0: %v", err)
		}
	})

	t.Run("an agent error fails with the transport code", func(t *testing.T) {
		_, _, err := run("", "shop_web", "--", "fail")
		if exitCode(err) != session.TransportFailure {
			t.Errorf("err = %v", err)
		}
	})

	before := len(agent.started())
	t.Run("a service with several replicas is ambiguous without a terminal", func(t *testing.T) {
		_, _, err := run("", "shop_api", "--", "id")
		var amb *resolve.AmbiguousError
		if !errors.As(err, &amb) || len(amb.Candidates) != 2 || exitCode(err) != session.TransportFailure {
			t.Errorf("err = %v, want the two candidates", err)
		}
	})

	t.Run("a target that does not exist is an error, never another container", func(t *testing.T) {
		for _, target := range []string{"definitely-not-there", "shop_web.9", "task-nope-nope", "deadbeefdeadbeef"} {
			out, _, err := run("", target, "--", "id")
			if err == nil || exitCode(err) != session.TransportFailure || out != "" {
				t.Errorf("exec %s: out %q err %v, want a failure that ran nothing", target, out, err)
			}
		}
	})
	if n := len(agent.started()); n != before {
		t.Errorf("%d exec(s) ran for targets that must not resolve", n-before)
	}

	t.Run("a bad flag value is a usage error", func(t *testing.T) {
		if _, _, err := run("", "--connect-timeout", "soon", "shop_web"); err == nil {
			t.Error("bad duration accepted")
		}
	})
}

// With no agent listening the failure is explained, not a bare dial error.
func TestExecRunWithoutAgent(t *testing.T) {
	g := cliCluster(t, execSwarm(), nil)
	_, _, err := runIO(t, newExecCmd(g), "", "--connect-timeout", "2s", "shop_web", "--", "id")
	if exitCode(err) != session.TransportFailure || !errors.Is(err, errNoAgent) {
		t.Errorf("err = %v, want the missing agent explained", err)
	}
}

// pickCandidate asks which replica on a terminal and refuses to guess
// without one.
func TestPickCandidate(t *testing.T) {
	amb := &resolve.AmbiguousError{Service: "shop_api", Candidates: []resolve.Candidate{
		{Slot: 1, NodeName: "node-a", ContainerID: "aaaaaaaaaaaaaaaa", DialHost: "10.0.0.1", NodeID: "n1"},
		{Slot: 2, NodeName: "node-b", ContainerID: "bbbbbbbbbbbbbbbb", DialHost: "10.0.0.2", NodeID: "n2"},
	}}
	t.Run("without a terminal the candidates are the error", func(t *testing.T) {
		var w bytes.Buffer
		ep, err := pickCandidate(amb, false, &w)
		if ep != nil || err != amb || w.Len() != 0 {
			t.Errorf("ep %v err %v printed %q", ep, err, w.String())
		}
	})
	t.Run("an answer picks that replica", func(t *testing.T) {
		asked := usePrompts(t, " 2 \n", true)
		var w bytes.Buffer
		ep, err := pickCandidate(amb, true, &w)
		if err != nil || ep.ContainerID != "bbbbbbbbbbbbbbbb" || ep.DialHost != "10.0.0.2" || ep.NodeName != "node-b" || ep.NodeID != "n2" {
			t.Fatalf("ep %+v err %v", ep, err)
		}
		for _, want := range []string{`service "shop_api" has 2 running tasks`, "[1] shop_api.1  node=node-a", "[2] shop_api.2  node=node-b  container=bbbbbbbbbbbb"} {
			if !strings.Contains(w.String(), want) {
				t.Errorf("list lacks %q:\n%s", want, w.String())
			}
		}
		if asked.String() != "select [1-2]: " {
			t.Errorf("asked %q", asked.String())
		}
	})
	for _, c := range []struct{ name, input, want string }{
		{"out of range", "3\n", "invalid selection"},
		{"zero", "0\n", "invalid selection"},
		{"not a number", "two\n", "invalid selection"},
		{"no answer", "", "no selection made"},
	} {
		t.Run(c.name, func(t *testing.T) {
			usePrompts(t, c.input, true)
			ep, err := pickCandidate(amb, true, io.Discard)
			if ep != nil || err == nil || err.Error() != c.want {
				t.Errorf("ep %v err %v, want %q", ep, err, c.want)
			}
		})
	}
}

// Against the real agent — its handlers, interceptors and shared-secret
// check, with a fake Docker behind it — the remote command's output and exit
// code survive the whole path, and so does stdin.
func TestExecRunRealAgent(t *testing.T) {
	d := agenttest.NewDocker().AddContainer(agenttest.Container{ID: "c-web", Service: "shop_web"})
	d.SetExec(func(cmd []string, stdin io.Reader, stdout, stderr io.Writer) int {
		in, _ := io.ReadAll(stdin)
		fmt.Fprintf(stdout, "out:%s|%s", strings.Join(cmd, " "), in)
		fmt.Fprint(stderr, "err-line")
		return 3
	})
	agent := agenttest.Start(t, agenttest.Options{Secret: "s3cret", Docker: d})
	useSwarm(t, execSwarm())
	g := clientConfig(t, agent.Port)

	out, errOut, err := runIO(t, newExecCmd(g), "typed", "--connect-timeout", "5s", "shop_web", "--", "run", "it")
	if exitCode(err) != 3 {
		t.Fatalf("err = %v, want exit code 3 from the container's process", err)
	}
	if out != "out:run it|typed" || errOut != "err-line" {
		t.Errorf("stdout %q stderr %q", out, errOut)
	}
	if got := d.Called("ExecCreate"); got != 1 {
		t.Errorf("ExecCreate called %d times", got)
	}
}
