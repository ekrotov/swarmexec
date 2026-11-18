// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package portfwd

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "swarmexec/internal/pb"
)

// forwardAgent answers PortForward by target port: 80 echoes upper-cased
// until the client half-closes, 81 fails after ready, 82 answers only AFTER
// the client half-closes, 83 never stops sending, 1 refuses, 2 sends data
// before ready.
type forwardAgent struct {
	pb.UnimplementedAgentServer
}

func (forwardAgent) PortForward(s pb.Agent_PortForwardServer) error {
	start, err := s.Recv()
	if err != nil {
		return err
	}
	data := func(b []byte) *pb.ForwardServerMessage {
		return &pb.ForwardServerMessage{Payload: &pb.ForwardServerMessage_Data{Data: b}}
	}
	fail := func(msg string) *pb.ForwardServerMessage {
		return &pb.ForwardServerMessage{Payload: &pb.ForwardServerMessage_Error{Error: msg}}
	}
	switch start.GetStart().GetPort() {
	case 1:
		return s.Send(fail("connection refused"))
	case 2:
		return s.Send(data([]byte("early")))
	}
	if err := s.Send(&pb.ForwardServerMessage{Payload: &pb.ForwardServerMessage_Ready{Ready: &pb.ForwardReady{}}}); err != nil {
		return err
	}
	switch start.GetStart().GetPort() {
	case 81:
		return s.Send(fail("reset by peer"))
	case 82:
		// Request/response: read to EOF, then answer. This is the exchange
		// that breaks when half-close is treated as "the forward is over".
		for {
			if _, err := s.Recv(); err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
		}
		return s.Send(data([]byte("answer-after-eof")))
	case 83:
		// Keeps talking until the stream is torn down, so a test can close
		// the local side underneath a busy forward.
		for {
			if err := s.Send(data(make([]byte, 4096))); err != nil {
				return err
			}
		}
	}
	for {
		m, err := s.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.Send(data([]byte(strings.ToUpper(string(m.GetData()))))); err != nil {
			return err
		}
	}
}

func forwardAgentClient(t *testing.T) pb.AgentClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	pb.RegisterAgentServer(gs, forwardAgent{})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return pb.NewAgentClient(cc)
}

func TestForwardConn(t *testing.T) {
	ac := forwardAgentClient(t)
	ctx := context.Background()

	// The happy path: bytes go both ways, and the local side closing ends
	// the bridge without an error.
	local, peer := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- bridge(ctx, ac, local, "c1", 80) }()
	if _, err := peer.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(peer, buf); err != nil || string(buf) != "HELLO" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	peer.Close()
	if err := <-done; err != nil {
		t.Errorf("closing the local side: %v, want nil", err)
	}

	for _, tc := range []struct {
		port uint32
		want string
	}{
		{1, "connection refused"},
		{2, "before ready"},
		{81, "reset by peer"},
	} {
		local, peer := net.Pipe()
		err := bridge(ctx, ac, local, "c1", tc.port)
		peer.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("port %d: err = %v, want it to contain %q", tc.port, err, tc.want)
		}
	}
}

// tcpPair is a real TCP connection, because this is where half-close is
// decided: net.Pipe has no write side to shut down on its own, so a test built
// on it cannot tell a half-close from a full close — which is exactly the
// distinction the pumps exist to preserve.
func tcpPair(t *testing.T) (local, peer *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	acc := make(chan res, 1)
	go func() {
		c, aerr := ln.Accept()
		acc <- res{c, aerr}
	}()
	dialed, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	a := <-acc
	if a.err != nil {
		t.Fatal(a.err)
	}
	t.Cleanup(func() { dialed.Close(); a.c.Close() })
	return a.c.(*net.TCPConn), dialed.(*net.TCPConn)
}

// The exchange this protects: send a request, shut down the write side, read
// the answer. The local peer closing its write side means "no more from me",
// NOT "tear the forward down" — a bridge that ends there loses the answer that
// was still on the way, which is most of what a forwarded port is used for.
func TestBridgeKeepsReadingAfterTheLocalHalfClose(t *testing.T) {
	ac := forwardAgentClient(t)
	local, peer := tcpPair(t)

	done := make(chan error, 1)
	go func() { done <- bridge(context.Background(), ac, local, "c1", 82) }()

	if _, err := peer.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := peer.CloseWrite(); err != nil { // shutdown(WR): the half-close
		t.Fatal(err)
	}

	_ = peer.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, len("answer-after-eof"))
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatalf("reading the answer after the half-close: %v", err)
	}
	if string(got) != "answer-after-eof" {
		t.Errorf("answer = %q; the half-close ended the read direction too", got)
	}
	if err := <-done; err != nil {
		t.Errorf("bridge = %v, want nil after a clean exchange", err)
	}
}

// A local client that goes away mid-transfer — the browser tab closed, the
// psql killed — is not an agent-side failure. The bridge has to come back
// rather than block on a write nobody will read, and it must not report an
// error the operator would go looking for.
func TestBridgeEndsWhenTheLocalPeerResets(t *testing.T) {
	ac := forwardAgentClient(t)
	local, peer := tcpPair(t)

	done := make(chan error, 1)
	go func() { done <- bridge(context.Background(), ac, local, "c1", 83) }()

	// Read a little so the forward is genuinely in flight, then reset: linger
	// 0 makes Close send an RST instead of a FIN, which is what a killed
	// client looks like on the wire.
	buf := make([]byte, 1024)
	_ = peer.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := peer.Read(buf); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	peer.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("bridge = %v; a local peer going away is not an agent failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not return after the local peer reset")
	}
}

// The accept loop is where a forward earns the word "forward": it has to keep
// serving after a connection fails, because the operator does not restart it —
// they reconnect their client and expect that to work. A loop that returned on
// the first failure would make an outdated agent, or a container that is
// restarting, look like a forward that silently died.
func TestServeSurvivesAFailedConnectionAndEndsOnCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ac := forwardAgentClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	okCh := make(chan struct{}, 4)
	errCh := make(chan error, 4)

	// Port 1 is the fake agent's "connection refused": every connection on
	// this forward fails, and the listener must stay up through all of them.
	bad := &Forwarder{ln: ln, containerID: "c1", remotePort: 1}
	bad.SetOnConnOK(func() { okCh <- struct{}{} })

	served := make(chan error, 1)
	go func() { served <- bad.serve(ctx, ac, func(e error) { errCh <- e }) }()

	for i := range 2 {
		c, derr := net.Dial("tcp", ln.Addr().String())
		if derr != nil {
			t.Fatalf("connection %d was refused by the listener: %v", i+1, derr)
		}
		_, _ = c.Write([]byte("x"))
		c.Close()
		select {
		case e := <-errCh:
			if !strings.Contains(e.Error(), "connection refused") {
				t.Errorf("connection %d: %v, want the agent's reason", i+1, e)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("connection %d: no failure reported", i+1)
		}
	}
	select {
	case <-okCh:
		t.Error("a failed connection reported success")
	default:
	}

	cancel()
	select {
	case serr := <-served:
		if serr != nil {
			t.Errorf("Serve = %v, want nil after cancellation", serr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

// A connection that ended cleanly is reported too: the TUI takes the warning
// marker off a forward that works again, and without this it would carry the
// last failure for the rest of its life.
func TestServeReportsASuccessfulConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ok := make(chan struct{}, 1)
	f := &Forwarder{ln: ln, containerID: "c1", remotePort: 80} // 80 echoes
	f.SetOnConnOK(func() { ok <- struct{}{} })
	go func() { _ = f.serve(ctx, forwardAgentClient(t), func(error) {}) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "HELLO" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	_ = c.(*net.TCPConn).CloseWrite()

	select {
	case <-ok:
	case <-time.After(10 * time.Second):
		t.Error("a clean connection was never reported as such")
	}
}
