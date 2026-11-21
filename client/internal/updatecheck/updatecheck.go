// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package updatecheck answers one question: is there a swarmexec release
// newer than this one? It asks the project's own site for /latest.json
// ({"version":"vX.Y.Z"}) at most once a day and remembers the answer in a
// small file next to the client config.
//
// The request is a plain GET without a query string or headers of its own:
// it carries nothing about the operator, the machine or the cluster. Every
// failure is silent — an offline machine just never sees a notice — and a
// failed fetch is remembered for as long as a successful one, so the promise
// holds on a bad day too: one request per day, not one per start and not a
// quicker retry. An offline start therefore delays the notice by up to a day.
// Non-release builds (dev, review-…, prereleases) never ask.
package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// DefaultURL is where the site publishes the newest release (site/Dockerfile).
const DefaultURL = "https://swarmshell.dev/latest.json"

// InstallURL is the page that says how to update.
const InstallURL = "https://swarmshell.dev/docs/install"

// CacheFile is the cache's name inside the config directory.
const CacheFile = "update-check.json"

// Defaults for Checker's zero fields.
const (
	defaultTTL     = 24 * time.Hour
	defaultTimeout = 3 * time.Second
	// maxBody bounds what is read from the response: the real answer is a
	// few dozen bytes.
	maxBody = 4 << 10
)

// Checker is the check with its dependencies spelled out; the zero value
// asks DefaultURL with the defaults above. Tests set URL, Client and Now.
type Checker struct {
	URL     string
	Client  *http.Client
	Now     func() time.Time
	TTL     time.Duration // how long an answer is kept, a failed fetch included
	Timeout time.Duration // the whole request, connect to body
}

// cacheEntry is the file CacheFile holds. Latest is empty after a failed
// fetch.
type cacheEntry struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest,omitempty"`
}

// Latest reports a release newer than current, using DefaultURL and a cache
// in cfgDir. See Checker.Latest.
func Latest(ctx context.Context, current, cfgDir string) (newer string, ok bool) {
	return Checker{}.Latest(ctx, current, cfgDir)
}

// Latest reports the newest release when it is newer than current. ok is
// false for every other outcome: current is not a release (vX.Y.Z), cfgDir is
// empty (no cache, so no promise of one request a day), the site could not
// be asked, or current is up to date. Whether the check is switched on is the
// caller's decision (config ui.check_updates).
func (c Checker) Latest(ctx context.Context, current, cfgDir string) (newer string, ok bool) {
	cur, isRelease := parse(current)
	if !isRelease || cfgDir == "" {
		return "", false
	}
	path := filepath.Join(cfgDir, CacheFile)
	now := c.now()
	e, fresh := c.cached(path, now)
	if !fresh {
		e = cacheEntry{CheckedAt: now.UTC()}
		if v, err := c.fetch(ctx); err == nil {
			e.Latest = v
		}
		writeCache(path, e)
	}
	latest, valid := parse(e.Latest)
	if !valid || !less(cur, latest) {
		return "", false
	}
	return e.Latest, true
}

// cached reads the cache and reports whether it is still fresh at now. A
// timestamp in the future (a clock that was wrong) counts as stale.
func (c Checker) cached(path string, now time.Time) (cacheEntry, bool) {
	var e cacheEntry
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &e) != nil || e.CheckedAt.IsZero() {
		return cacheEntry{}, false
	}
	age := now.Sub(e.CheckedAt)
	return e, age >= 0 && age < c.ttl()
}

// fetch asks the site for the newest release.
func (c Checker) fetch(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return "", err
	}
	if _, ok := parse(body.Version); !ok {
		return "", errors.New("no release version in the answer")
	}
	return body.Version, nil
}

// writeCache stores e, best effort: a cache that cannot be written only
// means the next start asks again.
func writeCache(path string, e cacheEntry) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// semver is a release version's three numbers.
type semver [3]int

var releaseRE = regexp.MustCompile(`^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

// parse accepts exactly vMAJOR.MINOR.PATCH — no prerelease, no build
// metadata, no leading zeros.
func parse(v string) (semver, bool) {
	m := releaseRE.FindStringSubmatch(v)
	if m == nil {
		return semver{}, false
	}
	var s semver
	for i := range s {
		s[i], _ = strconv.Atoi(m[i+1]) // the pattern admits only small numbers
	}
	return s, true
}

// less reports a < b.
func less(a, b semver) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func (c Checker) url() string {
	if c.URL != "" {
		return c.URL
	}
	return DefaultURL
}

func (c Checker) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

func (c Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Checker) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return defaultTTL
}

func (c Checker) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}
