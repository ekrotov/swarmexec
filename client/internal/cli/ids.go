// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"time"

	"swarmexec/client/internal/swarmops"
)

// shortID is the 12 characters an operator recognises a container or task by,
// the same length `docker ps` prints.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// uptime renders how long a container has run, compactly ("3d4h").
func uptime(d time.Duration) string { return swarmops.FormatUptime(d) }
