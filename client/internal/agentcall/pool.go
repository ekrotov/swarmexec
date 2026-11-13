// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/config"
)

// Connection reuse (review A-06).
//
// Before the pool every call dialled: a TUI refresh with the Volumes tab open
// cost one TCP connection (an ssh channel, behind a bastion), one TLS
// handshake and one HTTP/2 setup per node for each of five calls, every
// cycle. dial.Dial was a good constructor with nobody owning the lifetime.
//
// The pool owns it. One grpc.ClientConn per (node address, auth config) per
// Client, and a Client belongs to exactly one cluster, so the full key is
// (cluster endpoint, node address, auth config) and a connection is never
// shared across clusters — the cluster's tunnel lives in its config.
//
// What a caller gets is the same as before: a connection that was READY when
// handed out (dial.Dial's contract). A pooled connection is reused only while
// gRPC reports it READY; one in TRANSIENT_FAILURE, SHUTDOWN, IDLE (its
// transport went away) or CONNECTING is retired and a fresh dial answers
// instead — with dial.Dial's fail-fast reason, exactly as before.
//
// Channel binding needs nothing here: the bearer credentials compute the
// x-swarmexec-binding per RPC from the TLS state of the transport carrying
// THAT RPC (credentials.RequestInfo), so a reused connection — or one gRPC
// reconnected under us to a restarted agent with a new certificate — binds to
// the certificate actually on the other end.

// poolIdle is how long an unused connection is kept. The same reasoning as
// the ssh ControlPersist (sshMuxPersist): long enough to span the TUI's
// refresh cycle and an operator's next keystroke, short enough that a tool
// left open does not hold a channel to every node forever.
const poolIdle = 2 * time.Minute

// pool is a Client's connection cache.
type pool struct {
	dialFn func(ctx context.Context, host string) (*grpc.ClientConn, error)
	idle   time.Duration
	// onDial, when set, is told about every connection the pool dials and
	// keeps (not a reuse, and not the uncached dials of a stopped pool).
	onDial func(host string)

	mu      sync.Mutex
	entries map[string]*entry
	// stopped: nothing is kept — every connection is dialled for its holder
	// and closed when released. final: stopped for good (Close).
	stopped bool
	final   bool
}

// entry is one pooled connection.
type entry struct {
	key string
	cc  *grpc.ClientConn

	// dialing is closed when the dial that creates this entry has finished;
	// err is its outcome. Concurrent callers for the same node wait on the
	// one dial instead of opening their own.
	dialing chan struct{}
	err     error

	refs    int
	retired bool        // out of the map; closed when the last holder releases
	timer   *time.Timer // idle eviction, armed while refs == 0
	idleGen int         // which arming of timer is current
}

func newPool(dialFn func(ctx context.Context, host string) (*grpc.ClientConn, error), idle time.Duration) *pool {
	return &pool{dialFn: dialFn, idle: idle, entries: map[string]*entry{}}
}

// poolKey names a connection: the node address plus a digest of everything in
// the configuration that decides who we are and whom we trust. Within one
// Client the configuration is fixed, but the secret may come from a file and
// is re-read per acquisition as dial.Dial always did — a changed secret gets a
// new connection rather than reusing one that authenticates with the old one.
func poolKey(cfg config.Config, host string) (string, error) {
	secret, err := cfg.AgentSecretValue()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%q|%q|%q|%q|%t|%t|%q|%q", cfg.CA, cfg.Cert, cfg.Key, cfg.ServerName,
		cfg.Insecure, cfg.LegacySecret, cfg.Operator, secret)
	return net.JoinHostPort(host, strconv.Itoa(cfg.Port)) + "|" + hex.EncodeToString(h.Sum(nil))[:16], nil
}

// get returns a READY connection for key, reusing a pooled one when it is
// still READY. The caller must release it.
func (p *pool) get(ctx context.Context, key, host string) (*entry, error) {
	for {
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			// A stopped pool still works, it just keeps nothing: the entry is
			// born retired and closes on release.
			cc, err := p.dialFn(ctx, host)
			if err != nil {
				return nil, err
			}
			return &entry{key: key, cc: cc, refs: 1, retired: true}, nil
		}
		e := p.entries[key]
		if e == nil {
			e = &entry{key: key, dialing: make(chan struct{}), refs: 1}
			p.entries[key] = e
			p.mu.Unlock()
			return p.dial(ctx, e, host)
		}
		select {
		case <-e.dialing:
		default:
			// Someone is dialling this node right now: wait for that dial.
			p.mu.Unlock()
			select {
			case <-e.dialing:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			p.mu.Lock()
			if e.err != nil {
				p.mu.Unlock()
				// A refused connection or a rejected certificate is the
				// answer for us too. Only a dial that ran out of ITS time
				// says nothing about ours, so then we try ourselves.
				if errors.Is(e.err, context.DeadlineExceeded) || errors.Is(e.err, context.Canceled) {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					continue
				}
				return nil, e.err
			}
		}
		if e.err == nil && !e.retired && e.cc.GetState() == connectivity.Ready {
			e.refs++
			if e.timer != nil {
				e.timer.Stop()
				e.timer = nil
			}
			p.mu.Unlock()
			return e, nil
		}
		// Failed, or no longer READY: retire it and dial afresh.
		p.retireLocked(e)
		p.mu.Unlock()
	}
}

// dial runs the dial for a new entry and publishes its outcome.
func (p *pool) dial(ctx context.Context, e *entry, host string) (*entry, error) {
	cc, err := p.dialFn(ctx, host)
	p.mu.Lock()
	defer p.mu.Unlock()
	e.cc, e.err = cc, err
	close(e.dialing)
	if err != nil {
		e.refs = 0
		p.retireLocked(e)
		return nil, err
	}
	if p.stopped {
		e.retired = true // stopped during the dial: close on release
	} else if p.onDial != nil {
		p.onDial(host)
	}
	return e, nil
}

// release gives a connection back. failed retires it, so the next caller
// dials afresh instead of inheriting a connection that just failed.
func (p *pool) release(e *entry, failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.refs--
	if failed || (e.cc != nil && !usable(e.cc.GetState())) {
		p.retireLocked(e)
	}
	if e.refs > 0 {
		return
	}
	if e.retired {
		p.closeLocked(e)
		return
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	e.idleGen++
	gen := e.idleGen
	e.timer = time.AfterFunc(p.idle, func() { p.evictIdle(e, gen) })
}

// usable reports whether a connection in state s may be handed out again.
// Only READY: anything else is a transport that went away, is coming back
// under gRPC's own backoff, or failed — and a caller must get a connection
// that is up, or the reason it is not.
func usable(s connectivity.State) bool { return s == connectivity.Ready }

// evictIdle closes e if it has stayed unused since arming gen.
func (p *pool) evictIdle(e *entry, gen int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.refs > 0 || e.idleGen != gen {
		return // taken again since this timer was armed
	}
	clientlog.L().Debug("agentcall: closing idle agent connection", "key", e.key)
	p.retireLocked(e)
	p.closeLocked(e)
}

// retireLocked takes e out of the map, so nobody new gets it. p.mu held.
func (p *pool) retireLocked(e *entry) {
	if p.entries[e.key] == e {
		delete(p.entries, e.key)
	}
	e.retired = true
	if e.refs == 0 {
		p.closeLocked(e)
	}
}

// closeLocked closes e's connection, once. p.mu held.
func (p *pool) closeLocked(e *entry) {
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	if e.cc != nil {
		_ = e.cc.Close()
		e.cc = nil
	}
}

// stop empties the pool and keeps it empty until start: idle connections
// close now, held ones when their holder releases them, and connections
// dialled meanwhile are not kept. final makes it permanent.
func (p *pool) stop(final bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	p.final = p.final || final
	for _, e := range p.entries {
		select {
		case <-e.dialing:
			p.retireLocked(e)
		default:
			// Still dialling: dial() sees p.stopped and marks it retired.
			delete(p.entries, e.key)
		}
	}
}

// start resumes keeping connections after stop, unless the stop was final.
func (p *pool) start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.final {
		p.stopped = false
	}
}

// size is the number of pooled connections, for tests.
func (p *pool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// transportFailure reports whether err means the connection itself is in
// doubt rather than the call: Unavailable is what gRPC returns for a broken
// transport (and what a draining agent returns before it goes away).
func transportFailure(err error) bool {
	return status.Code(err) == codes.Unavailable
}
