// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dial"
	"swarmexec/client/internal/resolve"
	pb "swarmexec/internal/pb"
)

// The log view shows what a container SAID. It said nothing about being killed
// for using too much memory, about failing its health probe for the last two
// minutes, or about the exit code it died with — because none of that is in the
// log stream, and none of it reaches the manager either: a task reads "running"
// while its container fails every probe.
//
// So the view subscribes to the container's own events on its node, over its
// own connection, and writes them into the same buffer as the log lines. An
// operator watching a crash loop sees the crash, in order, next to the output
// that preceded it.

// eventRetryMin/Max bound the reconnect backoff. This is a side channel: if it
// cannot connect, the logs must keep flowing regardless, so failures are quiet
// after the first one.
const (
	eventRetryMin = 2 * time.Second
	eventRetryMax = 30 * time.Second
)

// interestingEvents is an allowlist, and that is the point.
//
// Docker emits over twenty container actions, and several of them are caused by
// this tool: opening a shell produces exec_create, exec_start and attach, and a
// resize produces one event per keystroke-sized window change. Showing those
// would make the view report on itself — the operator would watch swarmexec
// narrate swarmexec. What is left is what the container did on its own.
var interestingEvents = map[string]bool{
	"die":     true,
	"kill":    true,
	"oom":     true,
	"restart": true,
	"start":   true,
	"stop":    true,
	"pause":   true,
	"unpause": true,
}

// watchContainerEvents streams one container's runtime events into note() until
// ctx ends. It never returns an error: this is an enrichment of the log view,
// and an agent that is too old to serve it, or a node that drops the
// connection, must not take the logs down with it.
func watchContainerEvents(ctx context.Context, cfg config.Config, ep resolve.Endpoint, connectTimeout time.Duration, note func(string)) {
	backoff := eventRetryMin
	reported := false
	for ctx.Err() == nil {
		err := streamContainerEvents(ctx, cfg, ep, connectTimeout, note)
		if ctx.Err() != nil {
			return
		}
		if err != nil && !reported {
			// Once, and only once. A node that keeps dropping the connection
			// would otherwise fill the log view with its own retries — the
			// output the operator actually came for.
			reported = true
			if agentTooOld(err) {
				note("container events unavailable — the agent on " + orDash(ep.NodeName) +
					" predates this feature (swarmexec init --force)")
			} else {
				note("container events unavailable: " + err.Error())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > eventRetryMax {
			backoff = eventRetryMax
		}
	}
}

func streamContainerEvents(ctx context.Context, cfg config.Config, ep resolve.Endpoint, connectTimeout time.Duration, note func(string)) error {
	dctx, dcancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := dial.Dial(dctx, ep.DialHost, cfg.Port, cfg)
	dcancel()
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := pb.NewAgentClient(conn).WatchContainerEvents(ctx,
		&pb.WatchContainerEventsRequest{ContainerId: ep.ContainerID})
	if err != nil {
		return err
	}
	for {
		ev, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return rerr
		}
		if ev.GetError() != "" {
			return errors.New(ev.GetError())
		}
		if line := describeContainerEvent(ev); line != "" {
			note(line)
		}
	}
}

// describeContainerEvent renders one event as a log-view note, or "" for the
// ones not worth a line.
func describeContainerEvent(ev *pb.ContainerEvent) string {
	action := ev.GetAction()

	// Health transitions carry the verdict in the action itself.
	if h := ev.GetHealth(); h != "" {
		switch h {
		case "healthy":
			return "[green]● healthcheck passing[-]"
		case "unhealthy":
			// The one an operator most needs beside the output: the container
			// is running, says nothing, and the manager still calls it healthy.
			return "[red]● healthcheck FAILING[-]"
		case "starting":
			return "[gray]● healthcheck starting[-]"
		default:
			return "[gray]● healthcheck: " + h + "[-]"
		}
	}

	if !interestingEvents[action] {
		return ""
	}
	switch action {
	case "oom":
		// Named explicitly because the usual evidence is a bare exit 137 and a
		// silent log, which reads like a crash of the program rather than the
		// kernel taking it away.
		return "[red]✖ killed: out of memory[-]"
	case "die":
		if code := ev.GetExitCode(); code != 0 {
			return fmt.Sprintf("[red]✖ exited with code %d[-]", code)
		}
		return "[yellow]✖ exited (code 0)[-]"
	case "kill":
		return "[yellow]✖ killed[-]"
	case "start":
		return "[green]▶ started[-]"
	case "restart":
		return "[yellow]↻ restarted[-]"
	case "stop":
		return "[yellow]■ stopped[-]"
	case "pause", "unpause":
		return "[gray]" + strings.ToUpper(action[:1]) + action[1:] + "d[-]"
	}
	return ""
}
