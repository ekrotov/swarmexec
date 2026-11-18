// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"swarmexec/internal/forwardmux"
)

// forwardMuxFlag selects sidecar mode. The agent binary doubles as the
// port-forward sidecar: the image is already on every node, so a forward never
// waits on a registry pull (DESIGN-port-forward.md §3). One container serves
// every connection of one forward.
//
// There used to be a second, single-connection mode behind -forward-to. The
// agent stopped starting it when muxpool landed, nothing else ever did, and
// code only reachable by hand is code nobody tests — so it is gone, along with
// the "ok" readiness line it used.
const forwardMuxFlag = "-forward-mux-to"

// forwardDialTimeout bounds the dial to the target port. The sidecar shares the
// target's network namespace, so this is a loopback connect — a slow one means
// nothing is listening behind a firewall rule, not a slow network.
const forwardDialTimeout = 10 * time.Second

// forwardMuxTarget returns the address to forward to when args select sidecar
// mode. It parses args before the real flag set exists: getting it wrong would
// either break sidecar mode or, worse, make a normal agent start silently
// forward instead of serving.
func forwardMuxTarget(args []string) (string, bool) {
	return flagValue(args, forwardMuxFlag)
}

// flagValue finds "name value" or "name=value" in args. The presence of the
// flag is reported separately from its value so an empty one is a reported
// error rather than "not in sidecar mode", which would silently start a second
// agent.
func flagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		switch {
		case a == name:
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		case len(a) > len(name)+1 && a[:len(name)+1] == name+"=":
			return a[len(name)+1:], true
		}
	}
	return "", false
}

// dialReason strips the net.OpError wrapping so the message the operator sees
// is the reason ("connection refused"), not the plumbing.
func dialReason(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Err.Error()
	}
	return err.Error()
}

// runForwardMuxSidecar serves many connections to addr over one stdio.
//
// stderr carries exactly one control line before anything else (ReadyLine or
// "error: <reason>"). stdout then carries FRAMES,
// not payload — see internal/forwardmux. That is the one thing about this mode
// that must not be forgotten: the bytes on stdout are no longer the target's
// bytes, and a reader expecting raw payload would find plausible-looking
// rubbish.
func runForwardMuxSidecar(addr string) error {
	if addr == "" {
		fmt.Fprintln(os.Stderr, "error: "+forwardMuxFlag+" requires an address")
		return errors.New("missing forward address")
	}

	// Probe once, so a target that is not listening is reported the same way and
	// at the same moment as before: at startup, not on the first connection.
	probe, err := net.DialTimeout("tcp", addr, forwardDialTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", dialReason(err))
		return err
	}
	_ = probe.Close()

	fmt.Fprintln(os.Stderr, forwardmux.ReadyLine)

	m := &muxSidecar{addr: addr, conns: map[uint32]*muxTarget{}}
	return m.serve(os.Stdin, os.Stdout)
}

// muxSidecar owns the connections multiplexed over one stdio.
type muxSidecar struct {
	addr string

	// out serialises frame writes. Every target connection writes to the same
	// stdout, and a torn header desynchronises every connection on it, not just
	// the writer's own.
	outMu sync.Mutex
	out   io.Writer

	mu    sync.Mutex
	conns map[uint32]*muxTarget
}

type muxTarget struct {
	conn net.Conn
	once sync.Once
}

func (m *muxSidecar) serve(in io.Reader, out io.Writer) error {
	m.out = out
	defer m.closeAll()

	for {
		f, err := forwardmux.Read(in)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil // the agent is done with this forward
			}
			return err
		}
		switch f.Kind {
		case forwardmux.KindOpen:
			go m.open(f.Conn)
		case forwardmux.KindData:
			m.write(f.Conn, f.Payload)
		case forwardmux.KindCloseWrite:
			m.closeWrite(f.Conn)
		case forwardmux.KindClose:
			m.close(f.Conn)
		default:
			// An unknown kind means the two sides disagree about the protocol.
			// Carrying on would risk treating a control frame as payload, so
			// this ends the sidecar and the agent reports it.
			return fmt.Errorf("unexpected frame %s on conn %d", forwardmux.KindName(f.Kind), f.Conn)
		}
	}
}

func (m *muxSidecar) send(f forwardmux.Frame) error {
	m.outMu.Lock()
	defer m.outMu.Unlock()
	return forwardmux.Write(m.out, f)
}

// open dials the target for one connection id and pumps it until it ends.
func (m *muxSidecar) open(id uint32) {
	conn, err := net.DialTimeout("tcp", m.addr, forwardDialTimeout)
	if err != nil {
		_ = m.send(forwardmux.Frame{Conn: id, Kind: forwardmux.KindOpenErr, Payload: []byte(dialReason(err))})
		return
	}

	t := &muxTarget{conn: conn}
	m.mu.Lock()
	m.conns[id] = t
	m.mu.Unlock()

	if err := m.send(forwardmux.Frame{Conn: id, Kind: forwardmux.KindOpenOK}); err != nil {
		m.close(id)
		return
	}

	buf := make([]byte, 32*1024)
	for {
		n, rerr := conn.Read(buf)
		if n > 0 {
			if serr := m.send(forwardmux.Frame{Conn: id, Kind: forwardmux.KindData, Payload: buf[:n]}); serr != nil {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	// The target closed (or the write side failed): tell the agent, once.
	_ = m.send(forwardmux.Frame{Conn: id, Kind: forwardmux.KindClose})
	m.close(id)
}

func (m *muxSidecar) target(id uint32) *muxTarget {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns[id]
}

func (m *muxSidecar) write(id uint32, b []byte) {
	if t := m.target(id); t != nil {
		if _, err := t.conn.Write(b); err != nil {
			m.close(id)
		}
	}
	// A frame for an unknown id is dropped on purpose: it is the normal race
	// between the agent sending and this side having just closed the target.
}

func (m *muxSidecar) closeWrite(id uint32) {
	if t := m.target(id); t != nil {
		if tcp, ok := t.conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}
}

func (m *muxSidecar) close(id uint32) {
	m.mu.Lock()
	t := m.conns[id]
	delete(m.conns, id)
	m.mu.Unlock()
	if t != nil {
		t.once.Do(func() { _ = t.conn.Close() })
	}
}

func (m *muxSidecar) closeAll() {
	m.mu.Lock()
	all := m.conns
	m.conns = map[uint32]*muxTarget{}
	m.mu.Unlock()
	for _, t := range all {
		t.once.Do(func() { _ = t.conn.Close() })
	}
}
