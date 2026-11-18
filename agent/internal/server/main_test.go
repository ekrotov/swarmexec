// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"sync"
	"testing"

	"swarmexec/internal/leakcheck"
)

// The servers the test helpers build. Their sidecar pool outlives the test that
// opened a forward — in production Serve closes it on the way out — and the
// helpers have no *testing.T to hang a cleanup on, so TestMain does it once.
var (
	serversMu sync.Mutex
	servers   []*Server
)

func trackServer(s *Server) *Server {
	serversMu.Lock()
	defer serversMu.Unlock()
	servers = append(servers, s)
	return s
}

func closeTrackedServers() {
	serversMu.Lock()
	defer serversMu.Unlock()
	for _, s := range servers {
		s.muxes.CloseAll()
	}
}

// This package starts goroutines per stream and per sidecar and promises to
// stop them; the check fails the run if a test leaves one behind.
func TestMain(m *testing.M) { leakcheck.Main(m, closeTrackedServers) }
