// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package leakcheck

import (
	"strings"
	"testing"
)

// block is a goroutine the test controls; it shows up by name in its stack.
func block(stop <-chan struct{}, started chan<- struct{}) {
	started <- struct{}{}
	<-stop
}

func TestLeaksSeesARunningGoroutineAndForgetsItOnceItEnds(t *testing.T) {
	stop, started, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() { block(stop, started); close(done) }()
	<-started

	found := false
	for _, g := range leaks() {
		found = found || strings.Contains(g, "leakcheck.block")
	}
	if !found {
		t.Fatal("a goroutine blocked in block() was not reported")
	}

	close(stop)
	<-done
	for _, g := range wait() {
		if strings.Contains(g, "leakcheck.block") {
			t.Fatalf("the goroutine ended but is still reported:\n%s", g)
		}
	}
}
