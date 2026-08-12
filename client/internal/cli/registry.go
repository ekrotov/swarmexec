// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Registry version checks: a swarm service deployed from `repo:latest` has its
// image rewritten to `repo:latest@sha256:<pinned>` in the spec (the manager
// resolves the tag at deploy time). Comparing that pinned digest to the
// registry's CURRENT `:latest` digest tells us whether a newer image is
// available, and the pinned image's OCI `image.version` label tells us which
// version is really running behind `:latest`. Best-effort: any registry error
// just leaves the service unannotated.

// imageStatus is what a registry check yields for one pinned `:latest` image.
type imageStatus struct {
	pinnedDigest string // sha256:… the service is pinned to (from the spec)
	version      string // org.opencontainers.image.version of the pinned image, "" if unknown
	latestDigest string // current digest of repo:latest in the registry
	newer        bool   // registry :latest differs from the pinned digest
	ok           bool   // a registry lookup succeeded (latest digest fetched)
}

// parsePinnedLatest splits a spec image ref into repo and pinned digest, but only
// when it is pinned on the ":latest" tag — the case we can check. Returns ok=false
// for any other tag, or when there is no @sha256 digest.
func parsePinnedLatest(ref string) (repo, digest string, ok bool) {
	at := strings.IndexByte(ref, '@')
	if at < 0 {
		return "", "", false
	}
	name, dig := ref[:at], ref[at+1:]
	if !strings.HasPrefix(dig, "sha256:") {
		return "", "", false
	}
	colon := strings.LastIndexByte(name, ':')
	if colon < 0 {
		return "", "", false
	}
	// A registry-port colon (host:5000/repo) is before the last '/', not a tag.
	if slash := strings.LastIndexByte(name, '/'); slash > colon {
		return "", "", false
	}
	repo, tag := name[:colon], name[colon+1:]
	if tag != "latest" || repo == "" {
		return "", "", false
	}
	return repo, dig, true
}

// computeNewer reports whether the registry's current :latest (latest) differs
// from the pinned digest — i.e. a newer image is available.
func computeNewer(pinned, latest string) bool {
	return pinned != "" && latest != "" && latest != pinned
}

// versionFromConfig pulls the version out of an image config's labels, preferring
// the OCI label and falling back to a plain "version" label.
func versionFromConfig(labels map[string]string) string {
	for _, k := range []string{"org.opencontainers.image.version", "version"} {
		if v := strings.TrimSpace(labels[k]); v != "" {
			return v
		}
	}
	return ""
}

// imageResolver is the registry surface the checker needs — an interface so the
// cache logic is unit-testable with a fake (the real one talks to the registry).
type imageResolver interface {
	latestDigest(ctx context.Context, repo string) (string, error)
	versionLabel(ctx context.Context, repo, digest string) (string, error)
}

// craneResolver is the real resolver, using go-containerregistry with the
// operator's docker credentials (the default keychain reads ~/.docker/config.json).
type craneResolver struct{}

func (craneResolver) opts(ctx context.Context) []crane.Option {
	return []crane.Option{crane.WithContext(ctx), crane.WithAuthFromKeychain(authn.DefaultKeychain)}
}

func (c craneResolver) latestDigest(ctx context.Context, repo string) (string, error) {
	return crane.Digest(repo+":latest", c.opts(ctx)...)
}

func (c craneResolver) versionLabel(ctx context.Context, repo, digest string) (string, error) {
	opts := append(c.opts(ctx), crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: "amd64"}))
	raw, err := crane.Config(repo+"@"+digest, opts...)
	if err != nil {
		return "", err
	}
	var cf struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &cf); err != nil {
		return "", err
	}
	return versionFromConfig(cf.Config.Labels), nil
}

// checkImageStatus does a single, blocking best-effort registry check for a spec
// image ref. Returns a zero status (with ok=false) for non-:latest images or on
// any registry error.
func checkImageStatus(ctx context.Context, resolver imageResolver, specRef string) imageStatus {
	repo, digest, isLatest := parsePinnedLatest(specRef)
	if !isLatest {
		return imageStatus{}
	}
	st := imageStatus{pinnedDigest: digest}
	if latest, err := resolver.latestDigest(ctx, repo); err == nil {
		st.latestDigest, st.newer, st.ok = latest, computeNewer(digest, latest), true
	}
	if ver, err := resolver.versionLabel(ctx, repo, digest); err == nil {
		st.version = ver
	}
	return st
}

// registryCache memoises image checks so the tree's periodic re-render (and many
// services sharing an image) don't hammer the registry. Successful lookups are
// cached for successTTL, failures for the shorter failTTL (to retry sooner).
type registryCache struct {
	mu         sync.Mutex
	entries    map[string]*regEntry
	successTTL time.Duration
	failTTL    time.Duration
	resolver   imageResolver
	onUpdate   func() // called (off the lock) when a fetch produced a usable result
}

type regEntry struct {
	status    imageStatus
	fetchedAt time.Time
	inflight  bool
	attempted bool
}

func newRegistryCache(onUpdate func()) *registryCache {
	return &registryCache{
		entries:    map[string]*regEntry{},
		successTTL: 10 * time.Minute,
		failTTL:    1 * time.Minute,
		resolver:   craneResolver{},
		onUpdate:   onUpdate,
	}
}

// status returns the best-known status for a spec image ref and, when the entry
// is missing or stale, kicks off an async refresh (returning the last-known
// status meanwhile — the zero value if never fetched). Non-:latest refs return
// the zero status without any registry call.
func (c *registryCache) status(ctx context.Context, specRef string) imageStatus {
	if _, _, ok := parsePinnedLatest(specRef); !ok {
		return imageStatus{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[specRef]
	if e != nil && e.inflight {
		return e.status
	}
	ttl := c.successTTL
	if e != nil && !e.status.ok {
		ttl = c.failTTL
	}
	if e != nil && time.Since(e.fetchedAt) < ttl {
		return e.status
	}
	if e == nil {
		e = &regEntry{}
		c.entries[specRef] = e
	}
	e.inflight = true
	go c.fetch(ctx, specRef)
	return e.status
}

// statusNow returns a usable status for an on-demand caller (the inspect view):
// the cached value when it's warm, otherwise a bounded synchronous fetch whose
// result is also cached (so the tree benefits too). Non-:latest refs return zero.
func (c *registryCache) statusNow(ctx context.Context, specRef string, timeout time.Duration) imageStatus {
	if _, _, ok := parsePinnedLatest(specRef); !ok {
		return imageStatus{}
	}
	c.mu.Lock()
	if e := c.entries[specRef]; e != nil && e.status.ok && time.Since(e.fetchedAt) < c.successTTL {
		st := e.status
		c.mu.Unlock()
		return st
	}
	c.mu.Unlock()
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	st := checkImageStatus(fctx, c.resolver, specRef)
	c.mu.Lock()
	c.entries[specRef] = &regEntry{status: st, fetchedAt: time.Now(), attempted: true}
	c.mu.Unlock()
	return st
}

func (c *registryCache) fetch(ctx context.Context, specRef string) {
	fctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	st := checkImageStatus(fctx, c.resolver, specRef)
	c.mu.Lock()
	c.entries[specRef] = &regEntry{status: st, fetchedAt: time.Now(), attempted: true}
	c.mu.Unlock()
	if st.ok && c.onUpdate != nil {
		c.onUpdate()
	}
}

// versionSuffix is the compact annotation appended to a service row: the resolved
// version in parens and an up-arrow when a newer image is available. Empty when
// nothing is known yet.
func versionSuffix(st imageStatus) string {
	var b strings.Builder
	if st.version != "" {
		b.WriteString(" (" + st.version + ")")
	}
	if st.newer {
		b.WriteString(" ↑")
	}
	return b.String()
}
