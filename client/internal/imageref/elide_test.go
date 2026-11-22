// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package imageref

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestElideKeepsTheTag(t *testing.T) {
	// A tag says which version is running. Cutting it leaves something that
	// still reads as a tag and is not one.
	for _, ref := range []string{
		"registry.example.com/platform/team/backend/api-server:v2.14.3-rc1",
		"ghcr.io/org/suborg/project/service:2026-11-22.4",
	} {
		got := Elide(ref, 40)
		if want := ":" + Tag(ref); !strings.HasSuffix(got, want) {
			t.Errorf("Elide(%q, 40) = %q, want it to end in %q", ref, got, want)
		}
	}
}

func TestElideFitsTheBudget(t *testing.T) {
	refs := []string{
		"nginx",
		"nginx:1.27",
		"registry:5000/app:v1",
		"registry.example.com/platform/team/backend/api-server:v2.14.3-rc1",
		"registry.example.com/a/b/c/d/e/f/g/h/i/j/k/service:v1.0.0",
		"nginx@sha256:0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8",
		"registry.example.com/team/app@sha256:0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8",
	}
	for _, ref := range refs {
		for _, max := range []int{8, 16, 24, 32, 44, 64} {
			got := Elide(ref, max)
			if n := utf8.RuneCountInString(got); n > max {
				t.Errorf("Elide(%q, %d) = %q, %d columns wide", ref, max, got, n)
			}
		}
	}
}

func TestElideDropsTheNamespaceBeforeTheRegistry(t *testing.T) {
	// A cluster pulling from two registries has to see which one; the nested
	// namespace in between is the same for every service and says nothing.
	const ref = "registry.example.com/platform/team/backend/api-server:v2.14.3-rc1"
	got := Elide(ref, 50)
	if !strings.HasPrefix(got, "registry.example.com/") {
		t.Errorf("Elide(%q, 50) = %q, want the registry kept", ref, got)
	}
	if strings.Contains(got, "platform") || strings.Contains(got, "backend") {
		t.Errorf("Elide(%q, 50) = %q, want the namespace dropped", ref, got)
	}
	if !strings.Contains(got, "api-server:v2.14.3-rc1") {
		t.Errorf("Elide(%q, 50) = %q, want the repository and tag kept", ref, got)
	}
}

func TestElideShortensADigestRatherThanTheName(t *testing.T) {
	const ref = "registry.example.com/team/app@sha256:0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"
	got := Elide(ref, 56)
	if !strings.HasPrefix(got, "registry.example.com/team/app@sha256:0f1a2b3c4d5e") {
		t.Errorf("Elide(%q, 56) = %q, want the whole name and a cut digest", ref, got)
	}
	if !strings.HasSuffix(got, Ellipsis) {
		t.Errorf("Elide(%q, 56) = %q, want the cut marked", ref, got)
	}
}

func TestElideLeavesWhatAlreadyFits(t *testing.T) {
	for _, ref := range []string{"nginx", "nginx:1.27", "registry:5000/app:v1"} {
		if got := Elide(ref, 44); got != ref {
			t.Errorf("Elide(%q, 44) = %q, want it unchanged", ref, got)
		}
	}
}

func TestElideRefusesAnImpossibleBudget(t *testing.T) {
	// Below eight columns there is no answer worth giving; the caller has a
	// layout problem, and silently returning "…x" would hide it.
	const ref = "registry.example.com/team/app:v1"
	if got := Elide(ref, 4); got != ref {
		t.Errorf("Elide(%q, 4) = %q, want it unchanged", ref, got)
	}
}
