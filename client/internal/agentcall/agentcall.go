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
// A Client is safe for concurrent use.
type Client struct {
	cfg config.Config
}

// New returns a Client for one cluster's agent configuration.
func New(cfg config.Config) *Client {
	return &Client{cfg: cfg}
}

// Config is the agent configuration this Client dials with.
func (c *Client) Config() config.Config { return c.cfg }

// Close releases the Client's connections. Conns handed out before keep
// working until their own Close.
func (c *Client) Close() {}

// Conn is one agent connection, ready when handed out. The holder must Close
// it when done; Close is idempotent.
type Conn struct {
	cc *grpc.ClientConn
}

// Agent is the generated client on this connection.
func (cn *Conn) Agent() pb.AgentClient { return pb.NewAgentClient(cn.cc) }

// Close gives the connection back.
func (cn *Conn) Close() {
	if cn == nil || cn.cc == nil {
		return
	}
	_ = cn.cc.Close()
	cn.cc = nil
}

// Conn connects to the agent on host and returns the connection READY, or
// the reason it is not (see dial.Dial): ctx bounds the connect, nothing after
// it. Streaming callers use this; unary calls go through the methods below.
func (c *Client) Conn(ctx context.Context, host string) (*Conn, error) {
	cc, err := dial.Dial(ctx, host, c.cfg.Port, c.cfg)
	if err != nil {
		return nil, err
	}
	return &Conn{cc: cc}, nil
}

// Explain turns an agent RPC error into what the operator reads, for this
// Client's auth mode. Whether an agent is deployed at all is the fan-out
// caller's question, asked once rather than per node, so it is not asked here.
func (c *Client) Explain(ctx context.Context, err error) error {
	return agenterr.Explain(ctx, err, agenterr.Options{Auth: agenterr.AuthOf(c.cfg)})
}
