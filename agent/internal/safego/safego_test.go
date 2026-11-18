// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package safego

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestGoRecoversAndRunsHook(t *testing.T) {
	var buf syncBuf
	log := slog.New(slog.NewTextHandler(&buf, nil))
	hooked := make(chan struct{})
	Go(log, "boom", func() { panic("kaput") }, func() { close(hooked) })
	select {
	case <-hooked:
	case <-time.After(5 * time.Second):
		t.Fatal("onPanic hook never ran")
	}
	if out := buf.String(); !strings.Contains(out, "boom") || !strings.Contains(out, "kaput") {
		t.Errorf("panic not logged with name and value: %s", out)
	}
}

func TestGoRunsNormally(t *testing.T) {
	done := make(chan struct{})
	Go(nil, "ok", func() { close(done) }, func() { t.Error("hook ran without a panic") })
	<-done
}
