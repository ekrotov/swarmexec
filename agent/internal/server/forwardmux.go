// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"swarmexec/internal/forwardmux"
)

// Until now every forwarded TCP connection cost its own container: create,
// attach, start — about 300 ms on a warm node. Invisible for one long-lived
// connection, and paid six times over by a browser opening six, or once per
// pool entry by a client with a connection pool.
//
// A forward now starts ONE sidecar and multiplexes its connections over that
// sidecar's stdio. The gRPC protocol between cli and agent is untouched — still
// one stream per TCP connection — because this only changes what the agent
// hangs that stream on. The sidecar cap from the concurrency work now bounds
// forwards rather than connections, which is what it was always meant to mean.
//
// The cost of the change is that stdout no longer carries payload. It carries
// frames (internal/forwardmux), and half-closing one connection needs its own
// frame because closing the stdio would end every connection on the sidecar.

const (
	// muxIdleLinger is how long a sidecar with no connections is kept before it
	// is removed. A forward is a sequence of connections with gaps — a browser
	// reloading a page, a client reconnecting its pool — and tearing down the
	// container between them would give back exactly the latency this removes.
	muxIdleLinger = 2 * time.Minute

	// muxOpenTimeout bounds one connection's dial through an established
	// sidecar. Short, because the sidecar is already running and the dial is a
	// loopback connect inside the target's netns.
	muxOpenTimeout = 15 * time.Second
)

// muxKey identifies a sidecar: one per target container and port.
type muxKey struct {
	container string
	port      uint32
}

// muxSidecar is one running sidecar container and the connections on it.
type muxSidecar struct {
	key  muxKey
	ch   *sidecarChannel // the container, its attach stream and its cleanup
	srv  *Server
	done chan struct{} // closed when the demux loop ends

	// outCh serialises writes to the sidecar's stdin through one goroutine.
	//
	// A mutex was the obvious choice and the wrong one: writing to the stdio
	// blocks whenever the sidecar stops reading, so closing ONE connection could
	// block forever while holding the lock every other connection needs. The
	// teardown path is exactly where that happens, because the sidecar may
	// already be gone. With a queue, closing never blocks and a wedged sidecar
	// costs the connections on it rather than the agent's goroutines.
	outCh chan forwardmux.Frame

	mu      sync.Mutex
	conns   map[uint32]*muxConn
	nextID  uint32
	idle    *time.Timer
	closed  bool
	lastErr error
}

// muxConn is one multiplexed connection, presented to runForward as the same
// forwardChannel the per-connection sidecar used to be.
type muxConn struct {
	id  uint32
	sc  *muxSidecar
	in  *io.PipeReader // target -> agent
	inW *io.PipeWriter

	openOnce sync.Once
	open     chan error

	closeOnce sync.Once
}

var _ forwardChannel = (*muxConn)(nil)

// muxPool holds the live sidecars, keyed by target. It is the whole difference
// between "one container per connection" and "one per forward".
type muxPool struct {
	mu   sync.Mutex
	subs map[muxKey]*muxSidecar
}

func newMuxPool() *muxPool { return &muxPool{subs: map[muxKey]*muxSidecar{}} }

// openConn returns a channel to the target port, starting a sidecar only if
// there is not already one for that container and port.
func (p *muxPool) openConn(ctx context.Context, s *Server, containerID string, port uint32) (*muxConn, error) {
	key := muxKey{container: containerID, port: port}

	for attempt := 0; attempt < 2; attempt++ {
		sc, fresh, err := p.acquire(ctx, s, key)
		if err != nil {
			return nil, err
		}
		c, err := sc.dial(ctx)
		if err == nil {
			return c, nil
		}
		// A sidecar that died between being cached and being used looks exactly
		// like a dial failure. Drop it and try once more with a new one rather
		// than failing a forward because of a container that is already gone.
		p.evict(sc)
		if fresh || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, errors.New("could not reach the target through a forward sidecar")
}

// acquire returns the sidecar for key, creating one if needed. fresh reports
// whether this call created it.
func (p *muxPool) acquire(ctx context.Context, s *Server, key muxKey) (*muxSidecar, bool, error) {
	p.mu.Lock()
	if sc, ok := p.subs[key]; ok && !sc.isClosed() {
		sc.holdLocked()
		p.mu.Unlock()
		return sc, false, nil
	}
	p.mu.Unlock()

	// Started outside the pool lock: creating a container takes ~300 ms and
	// holding the lock across it would serialise every forward on the node.
	ch, err := s.openSidecarChannel(ctx, key.container, key.port)
	if err != nil {
		return nil, false, err
	}

	p.mu.Lock()
	// Someone else may have won the race while we were starting ours.
	if existing, ok := p.subs[key]; ok && !existing.isClosed() {
		existing.holdLocked()
		p.mu.Unlock()
		ch.Close() // ours is redundant; give the slot back immediately
		return existing, false, nil
	}
	sc := &muxSidecar{
		key: key, ch: ch, srv: s,
		done:  make(chan struct{}),
		outCh: make(chan forwardmux.Frame, muxWriteQueue),
		conns: map[uint32]*muxConn{},
	}
	p.subs[key] = sc
	p.mu.Unlock()

	go sc.demux()
	go sc.writer()
	sc.armIdle()
	return sc, true, nil
}

func (p *muxPool) evict(sc *muxSidecar) {
	p.mu.Lock()
	if p.subs[sc.key] == sc {
		delete(p.subs, sc.key)
	}
	p.mu.Unlock()
	sc.close()
}

// closeAll tears every sidecar down, for agent shutdown.
func (p *muxPool) closeAll() {
	p.mu.Lock()
	all := p.subs
	p.subs = map[muxKey]*muxSidecar{}
	p.mu.Unlock()
	for _, sc := range all {
		sc.close()
	}
}

func (sc *muxSidecar) isClosed() bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.closed
}

// holdLocked cancels a pending idle teardown; the caller is about to use it.
func (sc *muxSidecar) holdLocked() {
	sc.mu.Lock()
	if sc.idle != nil {
		sc.idle.Stop()
		sc.idle = nil
	}
	sc.mu.Unlock()
}

// armIdle schedules teardown if nothing is using the sidecar.
func (sc *muxSidecar) armIdle() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.closed || len(sc.conns) > 0 || sc.idle != nil {
		return
	}
	sc.idle = time.AfterFunc(muxIdleLinger, func() {
		sc.srv.muxes.evict(sc)
	})
}

// muxWriteQueue is how many frames may wait for the sidecar. Deep enough to
// absorb a burst, shallow enough that a stalled target applies backpressure to
// the client instead of being buffered in the agent's memory.
const muxWriteQueue = 64

// writer drains outCh to the sidecar's stdin. One goroutine, so frames cannot
// interleave and a torn header cannot desynchronise every connection.
func (sc *muxSidecar) writer() {
	for {
		select {
		case <-sc.done:
			return
		case f := <-sc.outCh:
			if err := forwardmux.Write(sc.ch, f); err != nil {
				sc.fail(err)
				return
			}
		}
	}
}

// send queues a frame, blocking while the queue is full — which is ordinary
// backpressure from a target that is not keeping up, and must not be turned
// into buffering.
func (sc *muxSidecar) send(f forwardmux.Frame) error {
	select {
	case sc.outCh <- f:
		return nil
	case <-sc.done:
		return sc.errOrDefault()
	}
}

// sendBestEffort queues a frame if there is room and gives up otherwise. Used
// only for teardown: a queue that full means the sidecar is not draining, and
// this connection is finished either way. Blocking here is what deadlocked the
// agent, so it must not.
func (sc *muxSidecar) sendBestEffort(f forwardmux.Frame) {
	select {
	case sc.outCh <- f:
	case <-sc.done:
	default:
	}
}

// dial opens one connection through this sidecar.
func (sc *muxSidecar) dial(ctx context.Context) (*muxConn, error) {
	pr, pw := io.Pipe()
	c := &muxConn{sc: sc, in: pr, inW: pw, open: make(chan error, 1)}

	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		return nil, sc.errOrDefault()
	}
	sc.nextID++
	c.id = sc.nextID
	sc.conns[c.id] = c
	if sc.idle != nil {
		sc.idle.Stop()
		sc.idle = nil
	}
	sc.mu.Unlock()

	if err := sc.send(forwardmux.Frame{Conn: c.id, Kind: forwardmux.KindOpen}); err != nil {
		sc.drop(c.id)
		return nil, err
	}

	octx, cancel := context.WithTimeout(ctx, muxOpenTimeout)
	defer cancel()
	select {
	case err := <-c.open:
		if err != nil {
			sc.drop(c.id)
			return nil, err
		}
		return c, nil
	case <-sc.done:
		sc.drop(c.id)
		return nil, sc.errOrDefault()
	case <-octx.Done():
		sc.drop(c.id)
		return nil, fmt.Errorf("timed out opening a connection to port %d in the target container", sc.key.port)
	}
}

func (sc *muxSidecar) errOrDefault() error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.lastErr != nil {
		return sc.lastErr
	}
	return errors.New("forward sidecar is gone")
}

func (sc *muxSidecar) conn(id uint32) *muxConn {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.conns[id]
}

// drop forgets a connection and arms the idle teardown if it was the last one.
func (sc *muxSidecar) drop(id uint32) {
	sc.mu.Lock()
	delete(sc.conns, id)
	empty := len(sc.conns) == 0 && !sc.closed
	sc.mu.Unlock()
	if empty {
		sc.armIdle()
	}
}

// demux reads frames from the sidecar and routes them to their connections,
// until the sidecar ends or is closed.
func (sc *muxSidecar) demux() {
	defer close(sc.done)
	for {
		f, err := forwardmux.Read(sc.ch)
		if err != nil {
			sc.fail(err)
			return
		}
		c := sc.conn(f.Conn)
		switch f.Kind {
		case forwardmux.KindOpenOK:
			if c != nil {
				c.openOnce.Do(func() { c.open <- nil })
			}
		case forwardmux.KindOpenErr:
			if c != nil {
				c.openOnce.Do(func() { c.open <- errors.New(string(f.Payload)) })
			}
		case forwardmux.KindData:
			if c != nil {
				if _, werr := c.inW.Write(f.Payload); werr != nil {
					sc.drop(f.Conn)
				}
			}
		case forwardmux.KindCloseWrite, forwardmux.KindClose:
			if c != nil {
				_ = c.inW.Close() // EOF toward the gRPC stream
				sc.drop(f.Conn)
			}
		default:
			// The two sides disagree about the protocol. Continuing would risk
			// reading a control frame as payload.
			sc.fail(fmt.Errorf("unexpected frame %s from the forward sidecar", forwardmux.KindName(f.Kind)))
			return
		}
	}
}

// fail ends every connection on this sidecar with the same reason.
func (sc *muxSidecar) fail(err error) {
	if errors.Is(err, io.EOF) {
		err = errors.New("forward sidecar exited")
	}
	sc.mu.Lock()
	if sc.lastErr == nil {
		sc.lastErr = err
	}
	conns := sc.conns
	sc.conns = map[uint32]*muxConn{}
	sc.mu.Unlock()

	for _, c := range conns {
		c.openOnce.Do(func() { c.open <- err })
		_ = c.inW.CloseWithError(err)
	}
	sc.srv.muxes.evict(sc)
}

func (sc *muxSidecar) close() {
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		return
	}
	sc.closed = true
	if sc.idle != nil {
		sc.idle.Stop()
		sc.idle = nil
	}
	conns := sc.conns
	sc.conns = map[uint32]*muxConn{}
	sc.mu.Unlock()

	for _, c := range conns {
		_ = c.inW.CloseWithError(errors.New("forward sidecar closed"))
	}
	sc.ch.Close()
}

// --- forwardChannel, one connection's view of the sidecar ---

func (c *muxConn) Write(p []byte) (int, error) {
	// Copied into the frame by the codec's single Write; the caller's buffer is
	// not retained.
	if err := c.sc.send(forwardmux.Frame{Conn: c.id, Kind: forwardmux.KindData, Payload: p}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// CloseWrite half-closes THIS connection. It needs a frame of its own: closing
// the sidecar's stdin would half-close every connection multiplexed on it.
func (c *muxConn) CloseWrite() {
	// Half-close is a normal part of a request/response exchange, so it queues
	// like data rather than being dropped — but it still must not outlive the
	// sidecar, which send handles.
	_ = c.sc.send(forwardmux.Frame{Conn: c.id, Kind: forwardmux.KindCloseWrite})
}

func (c *muxConn) CopyTo(w io.Writer) error {
	_, err := io.Copy(w, c.in)
	return err
}

// Close ends this connection without touching the sidecar, which keeps serving
// the others — and stays available for the next one.
func (c *muxConn) Close() {
	c.closeOnce.Do(func() {
		// Best effort: this runs on teardown, when the sidecar may already be
		// gone, and a close that waits for a dead peer would hold the forward
		// open forever.
		c.sc.sendBestEffort(forwardmux.Frame{Conn: c.id, Kind: forwardmux.KindClose})
		_ = c.in.Close()
		c.sc.drop(c.id)
	})
}

// muxConns reports how many connections a sidecar is carrying, for tests and
// diagnostics.
func (sc *muxSidecar) muxConns() int {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return len(sc.conns)
}

// liveSidecars reports how many sidecars the pool holds.
func (p *muxPool) liveSidecars() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.subs)
}
