// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/peer"
)

type failCount struct {
	NopMetrics
	by map[string]int
}

func (f *failCount) AuthFailed(r string) { f.by[r]++ }

func peerCtx(addr string) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(addr), Port: 4000}})
}

// A credential-less call is a failure from any non-loopback peer (SEC-04), is
// counted every time, and logged on the first occurrence, then once a minute
// with the number it stands for.
func TestSecretAuth_MissingFromRemoteIsCountedAndLogThrottled(t *testing.T) {
	var buf bytes.Buffer
	m := &failCount{by: map[string]int{}}
	now := time.Unix(0, 0)
	a := &SecretAuth{Secret: "s", Log: slog.New(slog.NewTextHandler(&buf, nil)), Metrics: m, now: func() time.Time { return now }}

	for i := 0; i < 3; i++ {
		if err := a.check(peerCtx("203.0.113.9")); err == nil {
			t.Fatal("check passed without credentials")
		}
	}
	if m.by["missing"] != 3 {
		t.Errorf("missing = %d, want 3", m.by["missing"])
	}
	if n := strings.Count(buf.String(), "authentication failed"); n != 1 {
		t.Fatalf("%d lines, want 1 (first not hidden):\n%s", n, buf.String())
	}
	now = now.Add(failLogInterval)
	_ = a.check(peerCtx("203.0.113.9"))
	if !strings.Contains(buf.String(), "suppressed_since_last=2") {
		t.Errorf("second line does not carry the held-back count:\n%s", buf.String())
	}

	if err := a.check(peerCtx("127.0.0.1")); err == nil || m.by["missing"] != 4 {
		t.Errorf("loopback probe was counted: missing = %d, want 4", m.by["missing"])
	}
}
