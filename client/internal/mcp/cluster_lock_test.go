// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cluster() used to hold the server mutex across the connect, up to 15 s, so
// every tool waited — including those for contexts connected long ago.
func TestClusterDoesNotHoldTheLockAcrossAConnect(t *testing.T) {
	s := New(Options{DockerContext: "main", Contexts: []string{"slow", "fast"}})
	release := make(chan struct{})
	entered := make(chan struct{})
	var slowDials atomic.Int32
	s.connect = func(_ context.Context, name string) (*mcpCluster, error) {
		if name == "slow" {
			if slowDials.Add(1) == 1 {
				close(entered)
			}
			<-release
		}
		return &mcpCluster{label: name}, nil
	}
	ctx := context.Background()
	if _, err := s.cluster(ctx, "fast"); err != nil {
		t.Fatal(err)
	}

	// Two callers for the slow context: one connects, the other waits for it.
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, err := s.cluster(ctx, "slow"); err != nil || c.label != "slow" {
				t.Errorf("slow: %v, %v", c, err)
			}
		}()
	}
	<-entered

	// While the connect is blocked, the connected context answers at once.
	done := make(chan error, 1)
	go func() { _, err := s.cluster(ctx, "fast"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a connected context waited on another context's connect: the mutex is held across it")
	}

	close(release)
	wg.Wait()
	if n := slowDials.Load(); n != 1 {
		t.Errorf("%d connects for one context, want the in-flight one shared", n)
	}
}

// Close during a connect must neither wait for it nor let the finished
// connection back into a session that is over.
func TestCloseDuringConnect(t *testing.T) {
	s := New(Options{DockerContext: "main", Contexts: []string{"slow"}})
	release := make(chan struct{})
	entered := make(chan struct{})
	s.connect = func(context.Context, string) (*mcpCluster, error) {
		close(entered)
		<-release
		return &mcpCluster{label: "slow"}, nil
	}
	res := make(chan error, 1)
	go func() { _, err := s.cluster(context.Background(), "slow"); res <- err }()
	<-entered

	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on an in-flight connect")
	}
	close(release)
	if err := <-res; err == nil {
		t.Error("a connect that finished after Close was handed out")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clusters) != 0 {
		t.Errorf("clusters = %v after Close, want none", s.clusters)
	}
}
