// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package session_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"swarmexec/agent/agenttest"
	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/session"
	pb "swarmexec/internal/pb"
)

// An external test package: agentcall (which agent connections go through)
// imports session, so an internal test could not import it.
//
// These run Run against the real agent's Exec handler (agenttest): real TLS,
// the real interceptor chain and bridge, a fake Docker process behind it.

type execResult struct {
	code        int
	out, errOut string
	err         error
}

// execOn dials the agent and runs one session: start, with stdin as given.
func execOn(t *testing.T, a *agenttest.Agent, start *pb.StartExec, stdin string) execResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Through agentcall, the one place agent connections are made.
	ac := agentcall.New(config.Config{Port: 7000, Insecure: true, AgentSecret: a.Secret, ProxyDialer: a.Dial})
	defer ac.Close()
	conn, err := ac.Conn(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := conn.Agent().Exec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code, runErr := session.Run(ctx, stream, session.Options{Start: start, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb})
	return execResult{code: code, out: out.String(), errOut: errb.String(), err: runErr}
}

func execAgent(t *testing.T) *agenttest.Agent {
	t.Helper()
	d := agenttest.NewDocker().AddContainer(agenttest.Container{ID: "c1", Service: "web"})
	return agenttest.Start(t, agenttest.Options{Secret: "s3cret", Docker: d})
}

func TestExecEndToEndOverGRPC(t *testing.T) {
	a := execAgent(t)
	r := execOn(t, a, &pb.StartExec{ContainerId: "c1", Cmd: []string{"/bin/cat"}}, "ping-pong")
	if r.err != nil {
		t.Fatalf("run error: %v", r.err)
	}
	if r.code != 0 {
		t.Errorf("exit code = %d, want 0", r.code)
	}
	if r.out != "ping-pong" {
		t.Errorf("stdout = %q, want ping-pong", r.out)
	}
	// The same session as the operator's audit trail sees it.
	starts := a.AuditEvents("session_start")
	if len(starts) != 1 || starts[0]["container_id"] != "c1" || starts[0]["service"] != "web" {
		t.Errorf("audit session_start = %v, want one record for c1 of service web", starts)
	}
	if ends := a.AuditEvents("session_end"); len(ends) != 1 || ends[0]["exit_code"] != float64(0) {
		t.Errorf("audit session_end = %v, want one record with exit code 0", ends)
	}
}

// The container's exit code and its two output streams come back as they were.
func TestExecCarriesExitCodeAndSeparatesStderr(t *testing.T) {
	d := agenttest.NewDocker().AddContainer(agenttest.Container{ID: "c1"}).
		SetExec(func(cmd []string, _ io.Reader, stdout, stderr io.Writer) int {
			_, _ = io.WriteString(stdout, "to stdout")
			_, _ = io.WriteString(stderr, "to stderr")
			return 7
		})
	a := agenttest.Start(t, agenttest.Options{Secret: "s3cret", Docker: d})

	r := execOn(t, a, &pb.StartExec{ContainerId: "c1", Cmd: []string{"false"}}, "")
	if r.err != nil {
		t.Fatalf("run error: %v", r.err)
	}
	if r.code != 7 || r.out != "to stdout" || r.errOut != "to stderr" {
		t.Errorf("code=%d stdout=%q stderr=%q, want 7, to stdout, to stderr", r.code, r.out, r.errOut)
	}
}

// A TTY session is one raw stream: there is no stderr to separate.
func TestExecTTYIsOneRawStream(t *testing.T) {
	a := execAgent(t)
	r := execOn(t, a, &pb.StartExec{ContainerId: "c1", Cmd: []string{"sh"}, Tty: true, Width: 80, Height: 24}, "typed")
	if r.err != nil || r.out != "typed" {
		t.Errorf("err=%v stdout=%q, want the stdin echoed raw", r.err, r.out)
	}
}

// A container the node does not have: the daemon's error reaches the client as
// the session's error (Exec reports setup failures in-band, as an error
// message, not as a status), and no process ever ran.
func TestExecUnknownContainerIsAnError(t *testing.T) {
	a := execAgent(t)
	r := execOn(t, a, &pb.StartExec{ContainerId: "nope", Cmd: []string{"sh"}}, "")
	if r.err == nil || !strings.Contains(r.err.Error(), "no such container") {
		t.Fatalf("err = %v, want the daemon's no-such-container", r.err)
	}
	if n := a.Docker.Called("ExecAttach"); n != 0 {
		t.Errorf("ExecAttach called %d times for a container that does not exist", n)
	}
}
