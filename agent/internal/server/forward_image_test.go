// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"os"
	"testing"

	"github.com/moby/moby/api/types/container"

	"swarmexec/agent/internal/auth"
)

// One failed self-inspect must not be remembered: the daemon was unwell for
// that request only, and the next forward has to ask again (NEB-06).
func TestForwardImage_FailureIsNotSticky(t *testing.T) {
	d := newFakeDocker()
	host, _ := os.Hostname()
	d.inspect[host] = container.InspectResponse{ID: host, Config: &container.Config{Image: "swarmexec-agent:1"}}
	d.mu.Lock()
	d.inspectErr = map[string]error{host: context.Canceled}
	d.mu.Unlock()
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})

	if img, err := srv.forwardImage(context.Background()); err == nil {
		t.Fatalf("first call: want the daemon's error, got image %q", img)
	}

	d.mu.Lock()
	d.inspectErr = nil
	d.mu.Unlock()
	img, err := srv.forwardImage(context.Background())
	if err != nil || img != "swarmexec-agent:1" {
		t.Fatalf("second call = %q, %v; want the image after the daemon recovered", img, err)
	}

	// Success is cached: no further inspect.
	d.mu.Lock()
	before := d.inspects
	d.mu.Unlock()
	if _, err := srv.forwardImage(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	after := d.inspects
	d.mu.Unlock()
	if after != before {
		t.Errorf("inspects went %d -> %d, want the cached image", before, after)
	}
}

// The inspect must not die with the stream that triggered it.
func TestForwardImage_SurvivesCallerCancel(t *testing.T) {
	d := newFakeDocker()
	host, _ := os.Hostname()
	d.inspect[host] = container.InspectResponse{ID: host, Config: &container.Config{Image: "swarmexec-agent:1"}}
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.forwardImage(ctx); err != nil {
		t.Fatal(err)
	}
}
