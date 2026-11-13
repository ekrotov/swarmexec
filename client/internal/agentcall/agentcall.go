// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package agentcall is the client's one way to talk to a node's agent. Every
// agent connection the cli and the TUI open goes through a Client: the
// per-node unary calls (volumes, images, stats, prune, version) and their
// fan-outs live here as methods, and the streaming paths (exec, logs, events,
// port-forward) take a Conn from here and drive the stream themselves.
//
// It is the first cut of review finding A-01: what used to be seventeen
// dial.Dial call sites scattered over cli is one acquisition point, which is
// what makes a connection pool (A-06) and per-RPC capability tracking (A-07)
// possible without touching the callers again. The package knows nothing of
// tview; errors are explained through agenterr, fan-outs run through fanout.
package agentcall

import (
	"context"
	"sync"

	"google.golang.org/grpc"

	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dial"
	"swarmexec/internal/pb"
)

// Client talks to the agents of ONE cluster: its configuration carries that
// cluster's tunnel (config.ProxyDialer) and credentials, so a Client must
// never be shared between clusters. Create one per cluster (the TUI) or per
// command (the cli), and Close it when that cluster or command is done.
//
// A Client keeps one connection per node and reuses it (see pool.go).
//
// A Client is safe for concurrent use.
type Client struct {
	cfg  config.Config
	pool *pool
	gate *capGate
}

// New returns a Client for one cluster's agent configuration.
func New(cfg config.Config) *Client {
	c := &Client{cfg: cfg, gate: newCapGate()}
	c.pool = newPool(func(ctx context.Context, host string) (*grpc.ClientConn, error) {
		return dial.Dial(ctx, host, cfg.Port, cfg)
	}, poolIdle)
	// A new connection may be a new agent: ask its capabilities again.
	c.pool.onDial = c.gate.forget
	return c
}

// Config is the agent configuration this Client dials with.
func (c *Client) Config() config.Config { return c.cfg }

// Close releases the Client's connections for good: idle ones now, ones still
// held (a running exec, a port-forward) when their holder closes them. The
// Client keeps working afterwards but keeps nothing: each call dials anew.
func (c *Client) Close() {
	if c != nil {
		c.pool.stop(true)
	}
}

// Suspend releases the Client's connections like Close but only until
// Resume. The TUI suspends a cluster it switches away from: its tunnels are
// not held open while the operator looks elsewhere, and what is still running
// there (a port-forward survives a switch) keeps its connection.
func (c *Client) Suspend() {
	if c != nil {
		c.pool.stop(false)
	}
}

// Resume keeps connections again after Suspend. No-op after Close.
//
// Close, Suspend and Resume are no-ops on a nil Client.
func (c *Client) Resume() {
	if c != nil {
		c.pool.start()
	}
}

// Conn is one agent connection, ready when handed out. The holder must Close
// it when done; Close is idempotent. The connection may be shared with other
// holders: Close gives it back, it does not tear it down.
type Conn struct {
	cc   *grpc.ClientConn
	p    *pool
	e    *entry
	once sync.Once
}

// Agent is the generated client on this connection.
func (cn *Conn) Agent() pb.AgentClient { return pb.NewAgentClient(cn.cc) }

// Close gives the connection back. A connection whose transport failed is
// retired rather than handed to the next caller.
func (cn *Conn) Close() { cn.done(nil) }

// done is Close after a call that returned err: a transport failure retires
// the connection even while gRPC still reports it READY.
func (cn *Conn) done(err error) {
	if cn == nil || cn.e == nil {
		return
	}
	cn.once.Do(func() { cn.p.release(cn.e, transportFailure(err)) })
}

// Conn returns a READY connection to the agent on host — a pooled one when
// it is still up, otherwise a fresh dial — or the reason there is none (see
// dial.Dial): ctx bounds the connect, nothing after it. Streaming callers use
// this; unary calls go through the methods below.
func (c *Client) Conn(ctx context.Context, host string) (*Conn, error) {
	key, err := poolKey(c.cfg, host)
	if err != nil {
		return nil, err
	}
	e, err := c.pool.get(ctx, key, host)
	if err != nil {
		return nil, err
	}
	return &Conn{cc: e.cc, p: c.pool, e: e}, nil
}

// Explain turns an agent RPC error into what the operator reads, for this
// Client's auth mode. Whether an agent is deployed at all is the fan-out
// caller's question, asked once rather than per node, so it is not asked here.
func (c *Client) Explain(ctx context.Context, err error) error {
	return agenterr.Explain(ctx, err, agenterr.Options{Auth: agenterr.AuthOf(c.cfg)})
}
