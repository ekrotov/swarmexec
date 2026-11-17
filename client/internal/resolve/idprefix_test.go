// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

func idFake() *fakeDocker {
	f := newFake()
	f.tasks = []swarm.Task{
		task("abcdef1111111111111111111", "svc-web", "node-a", 1, "cafe0123456789aa"),
		task("abcdef2222222222222222222", "svc-web", "node-b", 2, "cafe0123999999bb"),
		task("zz9999aaaaaaaaaaaaaaaaaaa", "svc-db", "node-a", 1, "db00112233445566"),
	}
	return f
}

func resolveTarget(f *fakeDocker, target string) (*Endpoint, error) {
	return New(f, AddrHostname).Resolve(context.Background(), Request{Target: target})
}

// KOR-04: a prefix shared by two tasks or two containers is an error naming
// the candidates, not the first of them.
func TestResolveAmbiguousIDPrefix(t *testing.T) {
	for _, tc := range []struct{ target, kind string }{
		{"abcdef", "task"},
		{"cafe01", "container"},
	} {
		_, err := resolveTarget(idFake(), tc.target)
		var amb *AmbiguousIDError
		if !errors.As(err, &amb) {
			t.Fatalf("%s: err = %v, want *AmbiguousIDError", tc.target, err)
		}
		if amb.Kind != tc.kind || len(amb.Matches) != 2 {
			t.Errorf("%s: %+v", tc.target, amb)
		}
		msg := err.Error()
		for _, want := range []string{"ambiguous", "web.1", "web.2", "type more of the ID"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: message %q lacks %q", tc.target, msg, want)
			}
		}
	}
}

// A longer prefix that names one task or container, and an exact ID that is
// also the prefix of another, still resolve.
func TestResolveUniqueIDPrefix(t *testing.T) {
	f := idFake()
	for target, want := range map[string]string{
		"abcdef1":                   "cafe0123456789aa", // task prefix
		"cafe012345":                "cafe0123456789aa", // container prefix
		"db0011":                    "db00112233445566",
		"abcdef1111111111111111111": "cafe0123456789aa",
	} {
		ep, err := resolveTarget(f, target)
		if err != nil {
			t.Errorf("%s: %v", target, err)
			continue
		}
		if ep.ContainerID != want {
			t.Errorf("%s: container %q, want %q", target, ep.ContainerID, want)
		}
	}
	// Exact beats prefix.
	f.tasks = append(f.tasks, task("abcdef11111111111111111112", "svc-web", "node-a", 3, "cafe0123aaaaaaaa"))
	ep, err := resolveTarget(f, "abcdef1111111111111111111")
	if err != nil || ep.ContainerID != "cafe0123456789aa" {
		t.Errorf("exact ID: %v %v", ep, err)
	}
}

// KOR-04: a short hex-looking name is a mistyped name, not an ID. Before the
// fix "db" ran in whichever container's ID began with "db".
func TestResolveShortHexIsNotAnID(t *testing.T) {
	f := idFake()
	f.services = nil // no service named "db" or "cafe"
	for _, target := range []string{"db", "cafe", "db001"} {
		ep, err := resolveTarget(f, target)
		if err == nil {
			t.Fatalf("%s resolved to %+v, want an error", target, ep)
		}
		if !strings.Contains(err.Error(), "at least 6 characters") {
			t.Errorf("%s: err = %q", target, err)
		}
	}
}
