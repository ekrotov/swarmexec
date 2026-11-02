// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"strings"
	"time"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dial"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	pb "swarmexec/internal/pb"
)

// probeShells reports which of bash/sh the container can actually start. It runs
// a quick non-interactive `command -v` through sh; if sh itself is missing it
// falls back to probing bash directly. Used to grey out unavailable shells.
func probeShells(ctx context.Context, cfg config.Config, ep resolve.Endpoint, connectTimeout time.Duration) (bash, sh bool) {
	out, code, err := runProbe(ctx, cfg, ep, []string{"sh", "-c", "command -v bash; command -v sh"}, connectTimeout)
	if err == nil && code == 0 {
		// sh ran (so it exists); its output names whichever shells were found.
		return strings.Contains(out, "bash"), true
	}
	_, bc, be := runProbe(ctx, cfg, ep, []string{"bash", "-c", "exit 0"}, connectTimeout)
	return be == nil && bc == 0, false
}

// runProbe runs cmd non-interactively in the container and returns its stdout,
// exit code, and any transport/exec error.
func runProbe(ctx context.Context, cfg config.Config, ep resolve.Endpoint, cmd []string, connectTimeout time.Duration) (string, int, error) {
	var buf strings.Builder
	code, err := runNonInteractive(ctx, cfg, ep, cmd, connectTimeout+5*time.Second, &buf, io.Discard)
	return buf.String(), code, err
}

// runNonInteractive is runProbe with the output streams and the overall
// deadline left to the caller: no TTY, no stdin, one command, its exit code.
// The MCP server's exec runs through here, so a command an assistant runs
// takes exactly the path a shell probe does — same dial, same agent RPC, same
// policy on the agent's side.
func runNonInteractive(ctx context.Context, cfg config.Config, ep resolve.Endpoint, cmd []string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := dial.Dial(pctx, ep.DialHost, cfg.Port, cfg)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	stream, err := pb.NewAgentClient(conn).Exec(pctx)
	if err != nil {
		return 0, err
	}
	return session.Run(pctx, stream, session.Options{
		Start:  &pb.StartExec{ContainerId: ep.ContainerID, Cmd: cmd},
		Stdout: stdout,
		Stderr: stderr,
	})
}
