// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package imageref answers the questions swarmexec asks about an image
// reference: what to show, what tag it carries, what it is pinned to, which
// registry it comes from, and how to shorten it for a fixed-width column.
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
	"unicode/utf8"

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

// Ellipsis marks a reference that Elide shortened. It is one column wide in
// every terminal swarmexec targets, which is what lets a fixed-width column
// budget for it.
const Ellipsis = "…"

// Elide shortens a reference to at most max columns, for a table whose column
// width is shared by every row.
//
// Without a bound, one service pinned to
// "registry.example.com/platform/team/backend/api-server:v2.14.3-rc1" sets the
// image column for all of them, and everything to its right — ports, the
// resource readings — is pushed off the edge of the view.
//
// What it drops, in this order: the digest's hex down to twelve characters
// (the length an id is shown at everywhere else), then the namespace path
// between the registry and the final repository element, then the registry,
// and only then characters from the left of what is left. The **tag is never
// cut**: it is the part that says which version is running, and a half tag is
// worse than no tag because it still looks like one.
//
// The namespace goes before the registry because it is usually identical for
// every service in the cluster and so distinguishes nothing, while a cluster
// that pulls from two registries needs to see which.
//
// A max below 8 returns the reference unchanged: there is no useful answer at
// that width, and a caller asking for one has a layout problem rather than a
// long image.
func Elide(ref string, max int) string {
	if max < 8 || width(ref) <= max {
		return ref
	}
	name := Name(ref)
	suffix := ""
	if d := Digest(ref); d != "" {
		suffix = "@" + shortDigest(d)
	}
	if width(name+suffix) <= max {
		return name + suffix
	}

	repo, tag := repoTag(name)
	tagPart := ""
	if tag != "" {
		tagPart = ":" + tag
	}
	if segs := strings.Split(repo, "/"); len(segs) > 2 {
		head, last := segs[0], segs[len(segs)-1]
		for _, cand := range []string{
			head + "/" + Ellipsis + "/" + last + tagPart + suffix,
			Ellipsis + "/" + last + tagPart + suffix,
		} {
			if width(cand) <= max {
				return cand
			}
		}
	}

	// Nothing structural is left to drop, so keep the tail: the end of the
	// repository name and the whole tag.
	r := []rune(name + suffix)
	return Ellipsis + string(r[len(r)-(max-1):])
}

// shortDigest cuts a digest's hex to twelve characters and marks the cut, so
// it reads as an abbreviation rather than as a digest that does not match.
func shortDigest(d string) string {
	algo, hex, ok := strings.Cut(d, ":")
	if !ok || len(hex) <= 12 {
		return d
	}
	return algo + ":" + hex[:12] + Ellipsis
}

// width is the column count of a reference. Everything legal in a reference is
// one column wide, and so is Ellipsis, so counting runes is counting columns.
func width(s string) int { return utf8.RuneCountInString(s) }
