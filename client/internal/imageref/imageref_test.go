// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package imageref

import "testing"

// The rule every hand-written copy of this had to get right: the colon in a
// registry's port is not a tag. Getting it wrong made "registry:5000/api" read
// as repository "registry" on tag "5000/api", which is a repository nobody has
// and a version check that silently never fires.
func TestPortColonIsNotATag(t *testing.T) {
	cases := []struct {
		ref               string
		name, tag, digest string
		repo              string
	}{
		{ref: "nginx", name: "nginx", repo: ""},
		{ref: "nginx:1.27", name: "nginx:1.27", tag: "1.27", repo: "nginx"},
		{ref: "registry:5000/api", name: "registry:5000/api", repo: ""},
		{ref: "registry:5000/api:2.1", name: "registry:5000/api:2.1", tag: "2.1", repo: "registry:5000/api"},
		{
			ref:    "reg.example/team/api:latest@sha256:abc",
			name:   "reg.example/team/api:latest",
			tag:    "latest",
			repo:   "reg.example/team/api",
			digest: "sha256:abc",
		},
		// Digest-only: no tag, so nothing to split a repository off.
		{ref: "nginx@sha256:abc", name: "nginx", digest: "sha256:abc", repo: ""},
	}
	for _, c := range cases {
		t.Run(c.ref, func(t *testing.T) {
			if got := Name(c.ref); got != c.name {
				t.Errorf("Name = %q, want %q", got, c.name)
			}
			if got := Tag(c.ref); got != c.tag {
				t.Errorf("Tag = %q, want %q", got, c.tag)
			}
			if got := Digest(c.ref); got != c.digest {
				t.Errorf("Digest = %q, want %q", got, c.digest)
			}
			if got := Repo(c.ref); got != c.repo {
				t.Errorf("Repo = %q, want %q", got, c.repo)
			}
			repo, tag, digest := Split(c.ref)
			if repo != c.repo || tag != c.tag || digest != c.digest {
				t.Errorf("Split = (%q, %q, %q), want (%q, %q, %q)", repo, tag, digest, c.repo, c.tag, c.digest)
			}
		})
	}
}

// Domain is the one question where normalizing is the answer: the credentials
// for a bare name are stored under docker.io, not under "".
func TestDomain(t *testing.T) {
	for ref, want := range map[string]string{
		"nginx":                         "docker.io",
		"library/nginx:latest":          "docker.io",
		"docker.io/logleio/agent:1":     "docker.io",
		"registry.logle.io/x/agent:1":   "registry.logle.io",
		"localhost:5000/agent:1.0":      "localhost:5000",
		"gcr.io/proj/img":               "gcr.io",
		"reg.example/a/b@sha256:abcdef": "reg.example",
		// Not a parseable reference: the fallback must still name a registry.
		"REG.example/Agent:1": "REG.example",
	} {
		if got := Domain(ref); got != want {
			t.Errorf("Domain(%q) = %q, want %q", ref, got, want)
		}
	}
}
