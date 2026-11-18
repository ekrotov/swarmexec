// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"swarmexec/agent/internal/config"
)

// SEC-08: a short hand-set secret is accepted (refusing would take existing
// agents down on upgrade) but announced at startup, with its length and never
// its value.
func TestBuildServer_WarnsOnWeakSecret(t *testing.T) {
	for _, c := range []struct {
		secret string
		warn   bool
	}{{"tiny-one", true}, {"a-long-enough-generated-secret", false}} {
		var logBuf bytes.Buffer
		_, err := buildServer(&config.Config{SelfSigned: true, AgentSecret: c.secret, AllowLegacySecret: true, DrainTimeout: time.Second}, deps{
			log:    slog.New(slog.NewTextHandler(&logBuf, nil)),
			audit:  io.Discard,
			docker: infoDocker{name: "node-1", addr: "10.0.0.7"},
		})
		if err != nil {
			t.Fatalf("%q: %v", c.secret, err)
		}
		got := strings.Contains(logBuf.String(), "shorter than the recommended minimum")
		if got != c.warn {
			t.Errorf("%q: warned = %v, want %v\n%s", c.secret, got, c.warn, logBuf.String())
		}
		if strings.Contains(logBuf.String(), c.secret) {
			t.Errorf("the secret reached the log:\n%s", logBuf.String())
		}
	}
}
