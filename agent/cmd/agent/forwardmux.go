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

	"swarmexec/internal/forwardmux"
)

// forwardMuxFlag selects multiplexing sidecar mode: one container serves every
// connection of one forward instead of one container per connection.
//
// The old single-connection mode (-forward-to) stays, and not only for
// compatibility: it is the simpler thing to reason about, and a sidecar that
// somehow cannot speak the framing can still be run by hand.
const forwardMuxFlag = "-forward-mux-to"

// muxReadyLine is what the agent waits for on stderr. Distinct from the
// single-connection mode's "ok" so a mismatched pair is caught at handshake
// rather than by one side misreading the other's payload.
const muxReadyLine = "mux ok"

// forwardMuxTarget returns the address to forward to when args select mux mode.
func forwardMuxTarget(args []string) (string, bool) {
	return flagValue(args, forwardMuxFlag)
}

// runForwardMuxSidecar serves many connections to addr over one stdio.
//
// stderr carries exactly one control line before anything else ("mux ok" or
// "error: <reason>"), as in single-connection mode. stdout then carries FRAMES,
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

	fmt.Fprintln(os.Stderr, muxReadyLine)

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
