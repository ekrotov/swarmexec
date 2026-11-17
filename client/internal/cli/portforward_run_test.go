// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	pb "swarmexec/internal/pb"
)

// echoForwardAgent is an agent whose PortForward answers each byte stream with the
// bytes upper-cased, and refuses port 9. It records what it was asked to open.
type echoForwardAgent struct {
	pb.UnimplementedAgentServer
	mu     sync.Mutex
	starts []*pb.StartForward
}

func (a *echoForwardAgent) PortForward(s pb.Agent_PortForwardServer) error {
	first, err := s.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	a.mu.Lock()
	a.starts = append(a.starts, start)
	a.mu.Unlock()
	if start.GetPort() == 9 {
		return s.Send(&pb.ForwardServerMessage{Payload: &pb.ForwardServerMessage_Error{Error: "connection refused"}})
	}
	if err := s.Send(&pb.ForwardServerMessage{Payload: &pb.ForwardServerMessage_Ready{Ready: &pb.ForwardReady{}}}); err != nil {
		return err
	}
	for {
		m, err := s.Recv()
		if err != nil {
			return nil // the client closed its side
		}
		if d := m.GetData(); len(d) > 0 {
			if err := s.Send(&pb.ForwardServerMessage{Payload: &pb.ForwardServerMessage_Data{Data: []byte(strings.ToUpper(string(d)))}}); err != nil {
				return err
			}
		}
	}
}

func (a *echoForwardAgent) opened() []*pb.StartForward {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*pb.StartForward(nil), a.starts...)
}

// freePort is a loopback port nobody holds right now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// runningForward is a `port-forward` command that is serving.
type runningForward struct {
	out, errOut *syncBuf
	cancel      context.CancelFunc
	done        chan error
}

// startPortForward runs the command in the background and returns once it
// announced the forward (or failed first).
func startPortForward(t *testing.T, cmd *cobra.Command, args ...string) *runningForward {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &runningForward{out: &syncBuf{}, errOut: &syncBuf{}, cancel: cancel, done: make(chan error, 1)}
	cmd.SetContext(ctx)
	cmd.SetOut(r.out)
	cmd.SetErr(r.errOut)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	go func() { r.done <- cmd.Execute() }()
	t.Cleanup(func() { cancel(); <-r.done })
	deadline := time.After(5 * time.Second)
	for !strings.Contains(r.out.String(), "forwarding") {
		select {
		case err := <-r.done:
			t.Fatalf("port-forward ended before serving: %v (stderr %q)", err, r.errOut.String())
		case <-deadline:
			t.Fatal("port-forward never announced the forward")
		case <-time.After(5 * time.Millisecond):
		}
	}
	return r
}

// stop cancels the command (the operator's Ctrl-C) and returns what it
// returned.
func (r *runningForward) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		r.done <- err
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("port-forward did not stop on cancel")
		return nil
	}
}

// exchange opens a connection to the forward, sends msg, and returns what
// comes back until the forward closes the connection or timeout.
func exchange(t *testing.T, port int, msg string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial the forward: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	n, err := io.ReadFull(c, buf)
	if err != nil && n == 0 {
		return ""
	}
	return string(buf[:n])
}

// `port-forward` end to end: the local listener, the stream per connection to
// the resolved container, bytes both ways, errors per connection, a clean
// stop.
func TestPortForwardRun(t *testing.T) {
	agent := &echoForwardAgent{}
	g := cliCluster(t, execSwarm(), agent)

	t.Run("bytes travel to the container port and back", func(t *testing.T) {
		port := freePort(t)
		r := startPortForward(t, newPortForwardCmd(g), "--connect-timeout", "2s", "shop_web", fmt.Sprintf("%d:8080", port))
		if want := fmt.Sprintf("forwarding 127.0.0.1:%d -> c-web:8080 (%s)", port, agentNodeHost); !strings.Contains(r.out.String(), want) {
			t.Errorf("announced %q, want %q", r.out.String(), want)
		}
		if got := exchange(t, port, "ping"); got != "PING" {
			t.Errorf("answer = %q", got)
		}
		if got := exchange(t, port, "second"); got != "SECOND" {
			t.Errorf("second connection answer = %q: connections are independent", got)
		}
		starts := agent.opened()
		if len(starts) < 2 || starts[0].GetContainerId() != "c-web" || starts[0].GetPort() != 8080 {
			t.Errorf("the agent was asked for %v", starts)
		}
		if err := r.stop(t); err != nil {
			t.Errorf("Ctrl-C is a clean stop, got %v", err)
		}
		if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second); err == nil {
			c.Close()
			t.Error("the listener outlived the command")
		}
		if r.errOut.String() != "" {
			t.Errorf("stderr = %q", r.errOut.String())
		}
	})

	t.Run("a taken local port fails before the agent is dialled", func(t *testing.T) {
		// A bare port is local and remote alike; with it taken the command
		// must say so (naming that port) rather than pick another.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		taken := l.Addr().(*net.TCPAddr).Port
		before := len(agent.opened())
		// With an agent deployed, a failure is not blamed on its absence.
		sw := execSwarm()
		sw.Services = append(sw.Services, agentService())
		g := cliCluster(t, sw, agent)
		_, _, err = runIO(t, newPortForwardCmd(g), "", "--connect-timeout", "2s", "shop_web", strconv.Itoa(taken))
		if exitCode(err) != session.TransportFailure || !strings.Contains(err.Error(), fmt.Sprintf("listen on 127.0.0.1:%d", taken)) {
			t.Errorf("err = %v, want the bind conflict", err)
		}
		if len(agent.opened()) != before {
			t.Error("the agent was contacted although the port was taken")
		}
	})

	t.Run("a refused container port fails that connection, not the forward", func(t *testing.T) {
		port := freePort(t)
		r := startPortForward(t, newPortForwardCmd(g), "--connect-timeout", "2s", "shop_web", fmt.Sprintf("%d:9", port))
		for i := 0; i < 2; i++ {
			if got := exchange(t, port, "x"); got != "" {
				t.Errorf("connection %d got %q from a refused port", i, got)
			}
		}
		deadline := time.After(3 * time.Second)
		for strings.Count(r.errOut.String(), "swarmexec: forward connection failed:") < 2 {
			select {
			case <-deadline:
				t.Fatalf("stderr = %q, want a line per failed connection", r.errOut.String())
			case <-time.After(5 * time.Millisecond):
			}
		}
		if !strings.Contains(r.errOut.String(), "connection refused") {
			t.Errorf("stderr = %q, want the agent's reason", r.errOut.String())
		}
		select {
		case err := <-r.done:
			t.Fatalf("the forward ended on a bad connection: %v", err)
		default:
		}
		if err := r.stop(t); err != nil {
			t.Errorf("stop: %v", err)
		}
	})

	t.Run("a non-loopback address is warned about", func(t *testing.T) {
		port := freePort(t)
		r := startPortForward(t, newPortForwardCmd(g), "--address", "0.0.0.0", "--connect-timeout", "2s", "shop_web", fmt.Sprintf("%d:8080", port))
		if !strings.Contains(r.errOut.String(), "warning: --address 0.0.0.0 is not loopback; the forwarded port is reachable from your network") {
			t.Errorf("stderr = %q", r.errOut.String())
		}
		_ = r.stop(t)
	})

	t.Run("a target that is not one container opens nothing", func(t *testing.T) {
		before := len(agent.opened())
		port := fmt.Sprintf("%d:80", freePort(t))
		_, _, err := runIO(t, newPortForwardCmd(g), "", "shop_api", port)
		var amb *resolve.AmbiguousError
		if !errors.As(err, &amb) || exitCode(err) != session.TransportFailure {
			t.Errorf("replicas: %v, want the ambiguity (a forward never picks for you)", err)
		}
		if _, _, err := runIO(t, newPortForwardCmd(g), "", "definitely-not-there", port); exitCode(err) != session.TransportFailure {
			t.Errorf("unknown target: %v", err)
		}
		if len(agent.opened()) != before {
			t.Error("the agent was contacted")
		}
	})

	t.Run("a bad port spec is a usage error", func(t *testing.T) {
		for _, spec := range []string{"0", "http", "9090:0"} {
			if _, _, err := runIO(t, newPortForwardCmd(g), "", "shop_web", spec); exitCode(err) != usageExitCode {
				t.Errorf("spec %q: %v", spec, err)
			}
		}
	})
}

// Without a reachable agent the forward fails with the explanation, and the
// port it had bound is free again.
func TestPortForwardRunWithoutAgent(t *testing.T) {
	g := cliCluster(t, execSwarm(), nil)
	port := freePort(t)
	_, _, err := runIO(t, newPortForwardCmd(g), "", "--connect-timeout", "2s", "shop_web", strconv.Itoa(port))
	if exitCode(err) != session.TransportFailure || !errors.Is(err, errNoAgent) {
		t.Errorf("err = %v, want the missing agent explained", err)
	}
	l, lerr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if lerr != nil {
		t.Errorf("the failed forward kept its port: %v", lerr)
	} else {
		l.Close()
	}
}
