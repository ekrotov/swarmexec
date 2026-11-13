// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

// The per-node fan-out itself lives in package fanout (fanout.Nodes): it has no
// TUI dependency, so it is the first piece of the data layer to leave cli.

// volumeDeleteFanout caps how many volumes are deleted in parallel in the UI.
// Kept modest because each volume delete already fans out across its nodes (up
// to fanout.NodeLimit), so the product bounds the load on the agents.
const volumeDeleteFanout = 6
