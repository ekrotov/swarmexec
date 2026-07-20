// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"

	"swarmexec/agent/internal/auth"
	"swarmexec/agent/internal/version"
	"swarmexec/internal/pb"
)

func TestVersion(t *testing.T) {
	srv, _ := newTestServer(newFakeDocker(), auth.AllowAll{}, Options{})
	resp, err := srv.Version(context.Background(), &pb.VersionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Version != version.Version || resp.ProtoVersion != version.Protocol {
		t.Errorf("got version=%q proto=%q, want %q/%q", resp.Version, resp.ProtoVersion, version.Version, version.Protocol)
	}
}
