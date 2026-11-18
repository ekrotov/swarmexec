// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package imageref answers the four questions swarmexec asks about an image
// reference: what to show, what tag it carries, what it is pinned to, and
// which registry it comes from.
//
// Each of those was its own hand-written scan over the string — in the
// resolver, the security scanner, the registry checker and the init command —
// and each had to get the same subtlety right: the colon in "registry:5000/x"
// is a port, not a tag. Four copies of one rule is three chances to fix it in
// only one place.
//
// What the first three deliberately do NOT do is normalize. "nginx" is shown
// as the operator wrote it, not as "docker.io/library/nginx", because the
// reference in the spec is what `docker service inspect` will show them.
// Domain is the exception: there, the normalized registry IS the answer, so it
// uses distribution/reference.
package imageref

import (
	"strings"

	"github.com/distribution/reference"
)

// Name is the reference without its @digest — the IMAGE column, the way
// `docker service ls` renders it. A reference with no digest is unchanged.
func Name(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return ref[:i]
	}
	return ref
}

// Digest is the @-pinned digest ("sha256:…"), or "" when the reference carries
// none.
func Digest(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return ref[i+1:]
	}
	return ""
}

// Tag is the reference's tag, or "" when it carries none. The colon in a
// registry's port (registry:5000/img) is not a tag, and neither is anything
// after an @digest.
func Tag(ref string) string {
	_, tag := repoTag(Name(ref))
	return tag
}

// Repo is the reference without its tag and digest: what a registry lists tags
// for. It is "" when the reference has no tag, because then the whole
// reference is already the repository.
func Repo(ref string) string {
	repo, tag := repoTag(Name(ref))
	if tag == "" {
		return ""
	}
	return repo
}

// Split is Repo, Tag and Digest in one pass, for the registry checker, which
// needs all three and has to decide between them.
func Split(ref string) (repo, tag, digest string) {
	name := Name(ref)
	repo, tag = repoTag(name)
	if tag == "" {
		repo = ""
	}
	return repo, tag, Digest(ref)
}

// repoTag splits a digest-free reference at its tag separator. The rule that
// every copy of this had to carry: a colon before the last '/' belongs to the
// registry's port.
func repoTag(name string) (repo, tag string) {
	colon := strings.LastIndexByte(name, ':')
	if colon < 0 {
		return name, ""
	}
	if slash := strings.LastIndexByte(name, '/'); slash > colon {
		return name, ""
	}
	return name[:colon], name[colon+1:]
}

// Domain is the registry a reference is pulled from — "docker.io" for a bare
// name, which is what the credentials for it are stored under.
//
// This is the one question where normalizing is the answer rather than a
// distortion, so it goes through distribution/reference, the same parser the
// Docker CLI uses. An unparseable reference falls back to the old hand-rolled
// rule rather than to an empty string: `init` would otherwise report "no
// credentials for """ instead of naming the registry.
func Domain(ref string) string {
	if named, err := reference.ParseNormalizedNamed(ref); err == nil {
		return reference.Domain(named)
	}
	if first, _, ok := strings.Cut(ref, "/"); ok &&
		(strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return "docker.io"
}
