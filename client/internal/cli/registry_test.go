// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"testing"
)

func TestParsePinnedLatest(t *testing.T) {
	cases := []struct {
		ref              string
		wantRepo, wantOK string
		ok               bool
	}{
		{"nginx:latest@sha256:abc", "nginx", "", true},
		{"registry.example.com/team/app:latest@sha256:deadbeef", "registry.example.com/team/app", "", true},
		{"host:5000/app:latest@sha256:abc", "host:5000/app", "", true}, // port colon is not a tag
		{"nginx:1.25@sha256:abc", "", "", false},                       // not :latest
		{"nginx:latest", "", "", false},                                // no pinned digest
		{"nginx@sha256:abc", "", "", false},                            // no tag
		{"host:5000/app@sha256:abc", "", "", false},                    // port, no tag
		{"nginx:latest@md5:abc", "", "", false},                        // non-sha256
	}
	for _, c := range cases {
		repo, _, ok := parsePinnedLatest(c.ref)
		if ok != c.ok || (ok && repo != c.wantRepo) {
			t.Errorf("parsePinnedLatest(%q) = (%q,%v), want (%q,%v)", c.ref, repo, ok, c.wantRepo, c.ok)
		}
	}
}

func TestComputeNewer(t *testing.T) {
	if !computeNewer("sha256:a", "sha256:b") {
		t.Error("different digests should be newer")
	}
	if computeNewer("sha256:a", "sha256:a") {
		t.Error("same digest is not newer")
	}
	if computeNewer("sha256:a", "") || computeNewer("", "sha256:b") {
		t.Error("empty digest must not report newer")
	}
}

func TestVersionFromConfig(t *testing.T) {
	if v := versionFromConfig(map[string]string{"org.opencontainers.image.version": "1.2.3"}); v != "1.2.3" {
		t.Errorf("oci label = %q", v)
	}
	if v := versionFromConfig(map[string]string{"version": "9"}); v != "9" {
		t.Errorf("fallback label = %q", v)
	}
	if v := versionFromConfig(nil); v != "" {
		t.Errorf("no labels = %q, want empty", v)
	}
}

func TestVersionSuffix(t *testing.T) {
	if s := versionSuffix(imageStatus{version: "1.2.3"}); s != " (1.2.3)" {
		t.Errorf("version only = %q", s)
	}
	if s := versionSuffix(imageStatus{version: "1.2.3", newer: true}); s != " (1.2.3) ↑" {
		t.Errorf("version+newer = %q", s)
	}
	if s := versionSuffix(imageStatus{newer: true}); s != " ↑" {
		t.Errorf("newer only = %q", s)
	}
	if s := versionSuffix(imageStatus{}); s != "" {
		t.Errorf("empty = %q, want empty", s)
	}
}

type fakeResolver struct {
	latest map[string]string // repo -> current :latest digest
	labels map[string]string // repo@digest -> version
}

func (f fakeResolver) latestDigest(_ context.Context, repo string) (string, error) {
	return f.latest[repo], nil
}
func (f fakeResolver) versionLabel(_ context.Context, repo, digest string) (string, error) {
	return f.labels[repo+"@"+digest], nil
}

func TestCheckImageStatus(t *testing.T) {
	res := fakeResolver{
		latest: map[string]string{"nginx": "sha256:new"},
		labels: map[string]string{"nginx@sha256:old": "1.0.0"},
	}
	// Pinned to an old digest → newer available, version resolved.
	st := checkImageStatus(context.Background(), res, "nginx:latest@sha256:old")
	if !st.ok || !st.newer || st.version != "1.0.0" || st.latestDigest != "sha256:new" {
		t.Errorf("stale status = %+v", st)
	}
	// Pinned to the current digest → not newer.
	res.latest["nginx"] = "sha256:old"
	st = checkImageStatus(context.Background(), res, "nginx:latest@sha256:old")
	if st.newer {
		t.Errorf("up-to-date should not be newer: %+v", st)
	}
	// Non-:latest ref → no check.
	if st := checkImageStatus(context.Background(), res, "nginx:1.25@sha256:old"); st.ok || st.newer {
		t.Errorf("non-latest should be zero: %+v", st)
	}
}
