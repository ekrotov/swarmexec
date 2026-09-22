// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// forwardFlag selects sidecar mode. The agent binary doubles as the port-forward
// sidecar: the image is already on every node, so a forward never waits on a
// registry pull (DESIGN-port-forward.md §3).
const forwardFlag = "-forward-to"

// forwardDialTimeout bounds the dial to the target port. The sidecar shares the
// target's network namespace, so this is a loopback connect — a slow one means
// nothing is listening behind a firewall rule, not a slow network.
const forwardDialTimeout = 10 * time.Second

// forwardTarget returns the address to forward to when args select
// single-connection sidecar mode.
func forwardTarget(args []string) (string, bool) {
	return flagValue(args, forwardFlag)
}

// flagValue finds "name value" or "name=value" in args. Shared by both sidecar
// modes; the presence of the flag is reported separately from its value so an
// empty one is a reported error rather than "not in sidecar mode", which would
// silently start a second agent.
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

// runForwardSidecar dials the target and pipes it to stdio until either side
// closes.
//
// The control protocol with the agent is one line on stderr, before any payload:
//
//	"ok"                -> connected; everything after this on stdout is payload
//	"error: <reason>"   -> the dial failed; the process exits non-zero
//
// stdout carries payload only. The agent attaches without a TTY, so docker
// keeps the two apart with stdcopy framing and binary payloads pass through
// unaltered — a TTY would apply line-discipline translation and corrupt them.
func runForwardSidecar(addr string) error {
	if addr == "" {
		fmt.Fprintln(os.Stderr, "error: "+forwardFlag+" requires an address")
		return errors.New("missing forward address")
	}

	conn, err := net.DialTimeout("tcp", addr, forwardDialTimeout)
	if err != nil {
		// Unwrap to the operator-facing cause: "connection refused" tells them
		// nothing is listening, which is the common real-world mistake.
		fmt.Fprintf(os.Stderr, "error: %s\n", dialReason(err))
		return err
	}
	defer conn.Close()

	fmt.Fprintln(os.Stderr, "ok")

	// Target -> agent. This direction decides when we are done: once the target
	// closes, the forwarded connection is over.
	done := make(chan error, 1)
	go func() {
		_, cerr := io.Copy(os.Stdout, conn)
		done <- cerr
	}()

	// Agent -> target. On stdin EOF the client half-closed, so half-close the
	// target too and keep reading the other direction.
	go func() {
		if _, cerr := io.Copy(conn, os.Stdin); cerr == nil {
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
		}
	}()

	if cerr := <-done; cerr != nil && !errors.Is(cerr, net.ErrClosed) {
		return cerr
	}
	return nil
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
