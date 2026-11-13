// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"

	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/config"
)

// The three ways this package hands an agent error to agenterr.Explain. They
// differ only in what they know; the mapping itself is agenterr's.

// explainSession explains the failure that ended a whole agent session — CLI
// exec, logs, port-forward, the TUI terminal. Any failure there, a dial error
// included, may mean no agent is deployed, so dcli is asked about that.
func explainSession(ctx context.Context, cfg config.Config, dcli serviceLister, err error) error {
	return agenterr.Explain(ctx, err, agenterr.Options{
		Auth:          agenterr.AuthOf(cfg),
		AgentDeployed: deployedProbe(dcli),
		Session:       true,
	})
}

// wrapGRPC explains the error of one per-node agent RPC (volumes, images,
// stats, prune). Whether an agent is deployed at all is the fan-out caller's
// question, asked once rather than per node, so it is not asked here.
func wrapGRPC(ctx context.Context, cfg config.Config, err error) error {
	return agenterr.Explain(ctx, err, agenterr.Options{Auth: agenterr.AuthOf(cfg)})
}

// deployedProbe adapts agentDeployed to agenterr's question, or returns nil
// when there is no manager client to ask.
func deployedProbe(dcli serviceLister) func(context.Context) bool {
	if dcli == nil {
		return nil
	}
	return func(ctx context.Context) bool { return agentDeployed(ctx, dcli) }
}
