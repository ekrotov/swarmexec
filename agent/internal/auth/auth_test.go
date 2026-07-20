// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"testing"
)

func TestAllowAll_Allows(t *testing.T) {
	d := AllowAll{}.Authorize(context.Background(), Request{
		Identity:    "operator",
		ContainerID: "abc",
		Cmd:         []string{"/bin/sh"},
	})
	if !d.Allow {
		t.Fatal("AllowAll must allow")
	}
	if d.Reason == "" {
		t.Fatal("decision reason must be set for audit")
	}
}
