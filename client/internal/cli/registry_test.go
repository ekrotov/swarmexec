// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"strings"
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
	if s := versionSuffix(imageStatus{version: "2.11.1", newer: true, latestVersion: "3.0.0"}); s != " (2.11.1) ↑ 3.0.0" {
		t.Errorf("version+newer+target = %q", s)
	}
	if s := versionSuffix(imageStatus{newer: true}); s != " ↑" {
		t.Errorf("newer only = %q", s)
	}
	if s := versionSuffix(imageStatus{}); s != "" {
		t.Errorf("empty = %q, want empty", s)
	}
}

type fakeResolver struct {
	latest map[string]string   // repo -> current :latest digest
	labels map[string]string   // repo@digest -> version
	tags   map[string][]string // repo -> tag list
}

func (f fakeResolver) latestDigest(_ context.Context, repo string) (string, error) {
	return f.latest[repo], nil
}
func (f fakeResolver) versionLabel(_ context.Context, repo, digest string) (string, error) {
	return f.labels[repo+"@"+digest], nil
}
func (f fakeResolver) listTags(_ context.Context, repo string) ([]string, error) {
	return f.tags[repo], nil
}

func TestCheckImageStatus(t *testing.T) {
	res := fakeResolver{
		latest: map[string]string{"nginx": "sha256:new"},
		labels: map[string]string{"nginx@sha256:old": "1.0.0", "nginx@sha256:new": "2.0.0"},
	}
	// Pinned to an old digest → newer available, both the running and the newer
	// version resolved.
	st := checkImageStatus(context.Background(), res, "nginx:latest@sha256:old")
	if !st.ok || !st.newer || st.version != "1.0.0" || st.latestDigest != "sha256:new" || st.latestVersion != "2.0.0" {
		t.Errorf("stale status = %+v", st)
	}
	// Pinned to the current digest → not newer.
	res.latest["nginx"] = "sha256:old"
	st = checkImageStatus(context.Background(), res, "nginx:latest@sha256:old")
	if st.newer {
		t.Errorf("up-to-date should not be newer: %+v", st)
	}
	// A version-pinned ref with a newer tag in the same family → newer, target set.
	res.tags = map[string][]string{"nginx": {"1.25", "1.26", "1.27", "latest", "1.27-alpine"}}
	st = checkImageStatus(context.Background(), res, "nginx:1.25@sha256:old")
	if !st.ok || !st.newer || st.newerTag != "1.27" || st.updateTarget != "nginx:1.27" || st.version != "1.25" {
		t.Errorf("version-pinned newer = %+v", st)
	}
	// A version-pinned ref already on the highest tag → checked but not newer.
	st = checkImageStatus(context.Background(), res, "nginx:1.27@sha256:old")
	if !st.ok || st.newer {
		t.Errorf("version-pinned up-to-date = %+v", st)
	}
	// An untagged, digest-only ref → not checkable.
	if st := checkImageStatus(context.Background(), res, "nginx@sha256:old"); st.ok || st.newer {
		t.Errorf("untagged should be zero: %+v", st)
	}
}

func TestParsePinnedVersion(t *testing.T) {
	cases := []struct {
		ref               string
		wantRepo, wantTag string
		ok                bool
	}{
		{"nginx:1.25@sha256:abc", "nginx", "1.25", true},
		{"opensearchproject/opensearch:2.11.1@sha256:abc", "opensearchproject/opensearch", "2.11.1", true},
		{"nginx:1.25", "nginx", "1.25", true}, // digest optional
		{"host:5000/app:2.0@sha256:abc", "host:5000/app", "2.0", true},
		{"nginx:latest@sha256:abc", "", "", false},  // :latest is the other path
		{"nginx@sha256:abc", "", "", false},         // no tag
		{"host:5000/app@sha256:abc", "", "", false}, // port, no tag
	}
	for _, c := range cases {
		repo, tag, ok := parsePinnedVersion(c.ref)
		if ok != c.ok || (ok && (repo != c.wantRepo || tag != c.wantTag)) {
			t.Errorf("parsePinnedVersion(%q) = (%q,%q,%v), want (%q,%q,%v)", c.ref, repo, tag, ok, c.wantRepo, c.wantTag, c.ok)
		}
	}
}

func TestSplitTag(t *testing.T) {
	cases := []struct {
		tag    string
		nums   []int
		suffix string
		ok     bool
	}{
		{"2.11.1", []int{2, 11, 1}, "", true},
		{"2.11", []int{2, 11}, "", true},
		{"v1.2.3", []int{1, 2, 3}, "", true},
		{"2.11.1-alpine", []int{2, 11, 1}, "-alpine", true},
		{"20.04", []int{20, 4}, "", true},
		{"latest", nil, "", false},
		{"stable-alpine", nil, "", false},
	}
	for _, c := range cases {
		nums, suffix, ok := splitTag(c.tag)
		if ok != c.ok || suffix != c.suffix || !equalInts(nums, c.nums) {
			t.Errorf("splitTag(%q) = (%v,%q,%v), want (%v,%q,%v)", c.tag, nums, suffix, ok, c.nums, c.suffix, c.ok)
		}
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHighestNewerTag(t *testing.T) {
	// The OpenSearch scenario: running 2.11.1, a mix of tags — pick the highest
	// same-family (plain X.Y.Z) tag; ignore -alpine, latest, rolling minors, older.
	opensearch := []string{"2.11.1", "2.11.2", "2.12.0", "3.0.0", "2.13.0-alpine", "latest", "1.3.0", "2.12", "3.1"}
	if got := highestNewerTag("2.11.1", opensearch); got != "3.0.0" {
		t.Errorf("highestNewerTag(2.11.1) = %q, want 3.0.0", got)
	}
	// Variant family is respected: an -alpine running tag matches only -alpine.
	alpine := []string{"2.12.0", "2.12.0-alpine", "2.13.0-alpine"}
	if got := highestNewerTag("2.11.1-alpine", alpine); got != "2.13.0-alpine" {
		t.Errorf("highestNewerTag(2.11.1-alpine) = %q, want 2.13.0-alpine", got)
	}
	// Already on the newest → "".
	if got := highestNewerTag("3.0.0", opensearch); got != "" {
		t.Errorf("highestNewerTag(3.0.0) = %q, want empty", got)
	}
	// A non-numeric running tag can't be compared → "".
	if got := highestNewerTag("stable", []string{"1.0.0"}); got != "" {
		t.Errorf("highestNewerTag(stable) = %q, want empty", got)
	}
}

func TestNewerTagsInFamily(t *testing.T) {
	opensearch := []string{"2.11.1", "2.11.2", "2.12.0", "3.0.0", "2.13.0-alpine", "latest", "1.3.0", "2.12", "3.1"}
	// Newer same-family (plain X.Y.Z) tags, highest first; alpine/latest/rolling
	// minors/older excluded. This is the picker's suggestion list.
	got := newerTagsInFamily("2.11.1", opensearch)
	want := []string{"3.0.0", "2.12.0", "2.11.2"}
	if len(got) != len(want) {
		t.Fatalf("newerTagsInFamily = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("newerTagsInFamily = %v, want %v", got, want)
		}
	}
	// Already on the newest → empty.
	if got := newerTagsInFamily("3.0.0", opensearch); len(got) != 0 {
		t.Errorf("newerTagsInFamily(3.0.0) = %v, want empty", got)
	}
	// Non-numeric running tag → empty.
	if got := newerTagsInFamily("stable", []string{"1.0.0"}); len(got) != 0 {
		t.Errorf("newerTagsInFamily(stable) = %v, want empty", got)
	}
}

func TestIsDowngrade(t *testing.T) {
	cases := []struct {
		current, target string
		want            bool
	}{
		{"2.11.1", "2.10.4", true},         // older same family
		{"2.11.1", "2.11.0", true},         // older patch
		{"2.11.1", "2.12.0", false},        // newer
		{"2.11.1", "2.11.1", false},        // same
		{"2.11.1", "2.11", false},          // different component count — unknown, not a downgrade
		{"2.11.1", "2.10.0-alpine", false}, // different family — unknown
		{"stable", "1.0.0", false},         // non-numeric current — unknown
	}
	for _, c := range cases {
		if got := isDowngrade(c.current, c.target); got != c.want {
			t.Errorf("isDowngrade(%q, %q) = %v, want %v", c.current, c.target, got, c.want)
		}
	}
}

// A :latest service must also learn the repo's tags, so the version picker can
// offer concrete versions to pin to — the action the unpinned-image finding
// asks for. Previously only the version-pinned path fetched them.
func TestCheckLatestCarriesRepoAndTags(t *testing.T) {
	res := fakeResolver{
		latest: map[string]string{"nginx": "sha256:new"},
		labels: map[string]string{"nginx@sha256:old": "1.0.0"},
		tags:   map[string][]string{"nginx": {"1.0.0", "2.0.0", "latest"}},
	}
	st := checkImageStatus(context.Background(), res, "nginx:latest@sha256:old")
	if st.repo != "nginx" {
		t.Errorf("repo = %q, want nginx", st.repo)
	}
	if len(st.knownTags) != 3 {
		t.Errorf("knownTags = %v, want the repo's three tags", st.knownTags)
	}
	// The digest-bump target must survive — that is what the one-shot confirm
	// applies, and it is what keeps the service digest-pinned.
	if !strings.Contains(st.updateTarget, "@sha256:new") {
		t.Errorf("updateTarget = %q, want the new digest", st.updateTarget)
	}
	// :latest has no "newer tag" notion; suggestions come from knownTags instead.
	if len(st.newerVersions) != 0 {
		t.Errorf("newerVersions = %v, want none for a :latest image", st.newerVersions)
	}
}

// A registry that cannot list tags must not break the :latest check — the rest
// of the status still resolves, the picker just has nothing to suggest.
func TestCheckLatestToleratesTagListFailure(t *testing.T) {
	res := fakeResolver{
		latest: map[string]string{"nginx": "sha256:new"},
		labels: map[string]string{"nginx@sha256:old": "1.0.0"},
		// no tags entry → empty list
	}
	st := checkImageStatus(context.Background(), res, "nginx:latest@sha256:old")
	if !st.ok || !st.newer {
		t.Errorf("status should still resolve without a tag list: %+v", st)
	}
	if len(st.knownTags) != 0 {
		t.Errorf("knownTags = %v, want empty", st.knownTags)
	}
}
