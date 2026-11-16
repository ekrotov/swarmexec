// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/swarmops"
)

// The ways this package hands an agent error to agenterr.Explain. They differ
// only in what they know; the mapping itself is agenterr's. (A per-node RPC
// is explained inside agentcall, by Client.Explain.)

// explainSession explains the failure that ended a whole agent session — CLI
// exec, logs, port-forward, the TUI terminal. Any failure there, a dial error
// included, may mean no agent is deployed, so dcli is asked about that.
func explainSession(ctx context.Context, cfg config.Config, dcli swarmops.ServiceLister, err error) error {
	return agenterr.Explain(ctx, err, agenterr.Options{
		Auth:          agenterr.AuthOf(cfg),
		AgentDeployed: deployedProbe(dcli),
		Session:       true,
	})
}

// agentDialFailure matches the dial package's "cannot reach agent on <addr> (…)"
// and captures the address.
var agentDialFailure = regexp.MustCompile(`cannot reach agent on (\S+) \(`)

// shortAgentError is an agent error cut down to what fits one line of a TUI
// view: the first cause and what to run next. The full text — gRPC's nested
// "connection error: desc = transport: Error while dialing: …" — wrapped over
// three lines of the node detail and buried the one word that mattered. It is
// still in the client log (every async load logs its error in full), so
// nothing is lost by shortening it on screen.
//
// A dial failure becomes "agent not reachable on host:9443: connection
// refused — swarmexec doctor"; any other error keeps its first line.
func shortAgentError(err error) string {
	msg := err.Error()
	if m := agentDialFailure.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("agent not reachable on %s: %s — swarmexec doctor", m[1], innermostCause(msg))
	}
	first, _, _ := strings.Cut(msg, "\n")
	return first
}

// innermostCause is the last ": "-separated part of an error chain, without the
// closing quotes and parentheses its wrappers left behind: "connection
// refused", "i/o timeout", "certificate signed by unknown authority".
func innermostCause(msg string) string {
	msg = strings.TrimRight(msg, `)"' `)
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	return msg
}

// deployedProbe adapts swarmops.AgentDeployed to agenterr's question, or returns nil
// when there is no manager client to ask.
func deployedProbe(dcli swarmops.ServiceLister) func(context.Context) bool {
	if dcli == nil {
		return nil
	}
	return func(ctx context.Context) bool { return swarmops.AgentDeployed(ctx, dcli) }
}
