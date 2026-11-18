// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"testing"

	"swarmexec/internal/leakcheck"
)

// This package starts goroutines (see the package comment) and promises to stop
// them; the check fails the run if a test leaves one behind.
func TestMain(m *testing.M) { leakcheck.Main(m) }
