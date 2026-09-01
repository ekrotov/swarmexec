// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
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

// imageStatus is what a registry check yields for one image — either a pinned
// `:latest` (digest drift) or a version-pinned tag (a newer semver tag exists).
type imageStatus struct {
	pinnedDigest  string // sha256:… the service is pinned to (:latest path)
	version       string // running version: the image.version label (:latest) or the tag (version-pinned)
	currentTag    string // the running tag: "latest" or e.g. "2.11.1"
	latestDigest  string // current digest of repo:latest in the registry (:latest path)
	latestVersion string // the available newer version: the :latest label, or the newer tag
	newerTag      string // the newer semver tag found (version-pinned path only)
	updateTarget  string // exact image ref to update to, when newer ("" otherwise)
	newer         bool   // a newer image is available

	// The following support the interactive version picker (version-pinned path).
	repo          string   // the image repo, to build a repo:tag ref for a chosen version
	newerVersions []string // newer same-family tags, highest first (picker suggestions)
	knownTags     []string // every tag the repo lists, to validate a typed-in override

	ok bool // a registry lookup succeeded
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

// parsePinnedVersion splits a spec image ref into repo and its NON-latest tag —
// the version-pinned case (e.g. repo:2.11.1@sha256:… or repo:2.11.1). The
// @digest is optional. Returns ok=false for a :latest tag (handled by
// parsePinnedLatest), an untagged ref, or a digest-only ref.
func parsePinnedVersion(ref string) (repo, tag string, ok bool) {
	name := ref
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		name = ref[:at]
	}
	colon := strings.LastIndexByte(name, ':')
	if colon < 0 {
		return "", "", false
	}
	// A registry-port colon (host:5000/repo) is before the last '/', not a tag.
	if slash := strings.LastIndexByte(name, '/'); slash > colon {
		return "", "", false
	}
	repo, tag = name[:colon], name[colon+1:]
	if repo == "" || tag == "" || tag == "latest" {
		return "", "", false
	}
	return repo, tag, true
}

// checkableRef reports whether a spec image ref is one the registry checker can
// act on: a pinned :latest, or a version-pinned tag.
func checkableRef(ref string) bool {
	if _, _, ok := parsePinnedLatest(ref); ok {
		return true
	}
	_, _, ok := parsePinnedVersion(ref)
	return ok
}

// splitTag splits a docker image tag into its dotted-numeric version components
// and the trailing suffix (a variant/pre-release like "-alpine"). A leading "v"
// before a digit is ignored. "2.11.1" -> [2 11 1],""; "v1.2.3-alpine" ->
// [1 2 3],"-alpine". ok=false when there is no leading numeric component (e.g.
// "stable", "latest").
func splitTag(tag string) (nums []int, suffix string, ok bool) {
	s := tag
	if len(s) > 1 && (s[0] == 'v' || s[0] == 'V') && s[1] >= '0' && s[1] <= '9' {
		s = s[1:]
	}
	i := 0
	for {
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i {
			break // no digit run here
		}
		n, err := strconv.Atoi(s[i:j])
		if err != nil {
			break
		}
		nums = append(nums, n)
		i = j
		if i < len(s) && s[i] == '.' {
			i++
			continue
		}
		break
	}
	if len(nums) == 0 {
		return nil, "", false
	}
	return nums, s[i:], true
}

// cmpNums compares two dotted-numeric versions component-by-component (missing
// components count as 0): -1 if a<b, 0 if equal, 1 if a>b.
func cmpNums(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// highestNewerTag returns the highest candidate tag that is a newer version than
// current within the SAME variant family — identical trailing suffix AND the
// same number of numeric components — or "" if none is newer. The conservative
// family match avoids cross-variant (2.11.1 vs 2.11.1-alpine) and rolling-minor
// (2.11.1 vs 2.12) false positives.
func highestNewerTag(current string, candidates []string) string {
	if newer := newerTagsInFamily(current, candidates); len(newer) > 0 {
		return newer[0]
	}
	return ""
}

// newerTagsInFamily returns every candidate that is a newer version than current
// within the SAME variant family (identical trailing suffix AND the same number
// of numeric components), sorted highest first. Empty if current is not a
// parseable version tag or nothing outranks it. Same family rules as
// highestNewerTag — this is the list form that feeds the version picker.
func newerTagsInFamily(current string, candidates []string) []string {
	curNums, curSuffix, ok := splitTag(current)
	if !ok {
		return nil
	}
	type tagNums struct {
		tag  string
		nums []int
	}
	var newer []tagNums
	for _, c := range candidates {
		nums, suffix, ok := splitTag(c)
		if !ok || suffix != curSuffix || len(nums) != len(curNums) {
			continue
		}
		if cmpNums(nums, curNums) <= 0 {
			continue // same or older than what's running
		}
		newer = append(newer, tagNums{tag: c, nums: nums})
	}
	sort.Slice(newer, func(i, j int) bool { return cmpNums(newer[i].nums, newer[j].nums) > 0 })
	out := make([]string, len(newer))
	for i, t := range newer {
		out[i] = t.tag
	}
	return out
}

// isDowngrade reports whether target is an older version than current within the
// same variant family. A tag that is not a parseable same-family version (or is
// newer/equal) is not a downgrade — the picker only warns when it is certain.
func isDowngrade(current, target string) bool {
	curNums, curSuffix, ok := splitTag(current)
	if !ok {
		return false
	}
	tgtNums, tgtSuffix, ok := splitTag(target)
	if !ok || tgtSuffix != curSuffix || len(tgtNums) != len(curNums) {
		return false
	}
	return cmpNums(tgtNums, curNums) < 0
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
	listTags(ctx context.Context, repo string) ([]string, error)
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

func (c craneResolver) listTags(ctx context.Context, repo string) ([]string, error) {
	return crane.ListTags(repo, c.opts(ctx)...)
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
	if repo, digest, isLatest := parsePinnedLatest(specRef); isLatest {
		return checkLatest(ctx, resolver, repo, digest)
	}
	if repo, tag, isVersion := parsePinnedVersion(specRef); isVersion {
		return checkVersionTag(ctx, resolver, repo, tag)
	}
	return imageStatus{}
}

// checkLatest handles a pinned :latest image: a newer image is a different
// current :latest digest than the one the service is pinned to.
func checkLatest(ctx context.Context, resolver imageResolver, repo, digest string) imageStatus {
	st := imageStatus{pinnedDigest: digest, currentTag: "latest"}
	if latest, err := resolver.latestDigest(ctx, repo); err == nil {
		st.latestDigest, st.newer, st.ok = latest, computeNewer(digest, latest), true
	}
	if ver, err := resolver.versionLabel(ctx, repo, digest); err == nil {
		st.version = ver
	}
	// When a newer image exists, resolve ITS version too, so the UI can offer a
	// concrete version rather than a bare digest, and pin the update target.
	if st.newer {
		if ver, err := resolver.versionLabel(ctx, repo, st.latestDigest); err == nil {
			st.latestVersion = ver
		}
		st.updateTarget = repo + ":latest@" + st.latestDigest
	}
	return st
}

// checkVersionTag handles a version-pinned image (repo:2.11.1): a newer image is
// the highest registry tag in the same variant family that outranks the running
// tag. The update target is that plain repo:tag — the manager re-pins the digest.
func checkVersionTag(ctx context.Context, resolver imageResolver, repo, tag string) imageStatus {
	tags, err := resolver.listTags(ctx, repo)
	if err != nil {
		return imageStatus{}
	}
	st := imageStatus{currentTag: tag, version: tag, ok: true, repo: repo, knownTags: tags}
	st.newerVersions = newerTagsInFamily(tag, tags)
	if len(st.newerVersions) > 0 {
		nt := st.newerVersions[0]
		st.newer, st.newerTag, st.latestVersion = true, nt, nt
		st.updateTarget = repo + ":" + nt
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
	if !checkableRef(specRef) {
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
	if !checkableRef(specRef) {
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
		// Label the arrow with the concrete target version when known, so the row
		// says WHAT is available, not just that something is.
		if st.latestVersion != "" {
			b.WriteString(" " + st.latestVersion)
		}
	}
	return b.String()
}
