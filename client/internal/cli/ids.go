// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"time"

	"swarmexec/client/internal/clusterread"
)

// uptime renders how long a container has run, compactly ("3d4h").
func uptime(d time.Duration) string { return clusterread.FormatUptime(d) }
