// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package updatecheck

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// site is a fake /latest.json that counts the requests it gets and records
// what they carried.
type site struct {
	srv   *httptest.Server
	hits  atomic.Int32
	query atomic.Value // last raw query
	ua    atomic.Value // last User-Agent
}

func newSite(t *testing.T, status int, body string) *site {
	t.Helper()
	s := &site{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.query.Store(r.URL.RawQuery)
		s.ua.Store(r.Header.Get("User-Agent"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// clock is a settable Now.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func checker(s *site, c *clock) Checker {
	return Checker{URL: s.srv.URL + "/latest.json", Client: s.srv.Client(), Now: c.now}
}

func TestReportsNewerReleaseAndCachesADay(t *testing.T) {
	s := newSite(t, 200, `{"version":"v1.27.0"}`)
	c := &clock{time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	ch := checker(s, c)

	got, ok := ch.Latest(context.Background(), "v1.26.0", dir)
	if !ok || got != "v1.27.0" {
		t.Fatalf("Latest = %q, %v; want v1.27.0, true", got, ok)
	}
	if q := s.query.Load().(string); q != "" {
		t.Errorf("request carried a query string %q", q)
	}
	if ua := s.ua.Load().(string); !strings.HasPrefix(ua, "Go-http-client/") {
		t.Errorf("User-Agent %q, want Go's default", ua)
	}

	// Within the day: the cache answers, the site is not asked again.
	c.t = c.t.Add(23 * time.Hour)
	if got, ok := ch.Latest(context.Background(), "v1.26.0", dir); !ok || got != "v1.27.0" {
		t.Fatalf("cached Latest = %q, %v", got, ok)
	}
	if n := s.hits.Load(); n != 1 {
		t.Fatalf("%d requests within a day, want 1", n)
	}
	// After the day: one more request.
	c.t = c.t.Add(2 * time.Hour)
	ch.Latest(context.Background(), "v1.26.0", dir)
	if n := s.hits.Load(); n != 2 {
		t.Fatalf("%d requests after a day, want 2", n)
	}

	fi, err := os.Stat(filepath.Join(dir, CacheFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("cache mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestUpToDateOrNewerIsSilent(t *testing.T) {
	s := newSite(t, 200, `{"version":"v1.27.0"}`)
	c := &clock{time.Now()}
	for _, cur := range []string{"v1.27.0", "v1.27.1", "v2.0.0", "v1.100.0"} {
		if got, ok := checker(s, c).Latest(context.Background(), cur, t.TempDir()); ok {
			t.Errorf("current %s: reported %q", cur, got)
		}
	}
	// Numeric, not lexical: v1.10.0 is newer than v1.9.0.
	s2 := newSite(t, 200, `{"version":"v1.10.0"}`)
	if got, ok := checker(s2, c).Latest(context.Background(), "v1.9.0", t.TempDir()); !ok || got != "v1.10.0" {
		t.Errorf("v1.9.0 vs v1.10.0: %q, %v", got, ok)
	}
}

func TestNonReleaseBuildsNeverAsk(t *testing.T) {
	s := newSite(t, 200, `{"version":"v9.0.0"}`)
	c := &clock{time.Now()}
	for _, cur := range []string{"", "dev", "review-e87d42c", "v1.26.0-rc1", "1.26.0", "v1.26", "v1.26.0-3-gabc", "v01.2.3", "e87d42c"} {
		if got, ok := checker(s, c).Latest(context.Background(), cur, t.TempDir()); ok {
			t.Errorf("%q reported %q", cur, got)
		}
	}
	// Nor without a directory for the cache.
	checker(s, c).Latest(context.Background(), "v1.0.0", "")
	if n := s.hits.Load(); n != 0 {
		t.Fatalf("%d requests from non-release builds", n)
	}
}

func TestFailuresAreSilentAndNotRetriedWithinADay(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"404":         {404, `{"version":"v9.0.0"}`},
		"garbage":     {200, `<html>`},
		"empty":       {200, `{}`},
		"prerelease":  {200, `{"version":"v9.0.0-rc1"}`},
		"no v prefix": {200, `{"version":"9.0.0"}`},
		"oversized":   {200, `{"version":"v9.0.0","pad":"` + strings.Repeat("x", 8<<10) + `"}`},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSite(t, tc.status, tc.body)
			c := &clock{time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
			dir := t.TempDir()
			if got, ok := checker(s, c).Latest(context.Background(), "v1.0.0", dir); ok {
				t.Fatalf("reported %q", got)
			}
			// "At most once a day" is what the docs and the privacy pages
			// promise, so a failure is not retried any sooner than a success.
			c.t = c.t.Add(23 * time.Hour)
			checker(s, c).Latest(context.Background(), "v1.0.0", dir)
			if n := s.hits.Load(); n != 1 {
				t.Fatalf("%d requests within a day of a failure, want 1", n)
			}
			c.t = c.t.Add(2 * time.Hour)
			checker(s, c).Latest(context.Background(), "v1.0.0", dir)
			if n := s.hits.Load(); n != 2 {
				t.Fatalf("%d requests a day after a failure, want 2", n)
			}
		})
	}
}

func TestUnreachableAndSlowSitesAreSilent(t *testing.T) {
	// Unreachable: a closed server.
	s := newSite(t, 200, `{"version":"v9.0.0"}`)
	url := s.srv.URL
	s.srv.Close()
	ch := Checker{URL: url, Now: time.Now}
	if got, ok := ch.Latest(context.Background(), "v1.0.0", t.TempDir()); ok {
		t.Fatalf("unreachable site reported %q", got)
	}

	// Slow: the timeout ends the request.
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(block)
	start := time.Now()
	ch = Checker{URL: slow.URL, Client: slow.Client(), Timeout: 50 * time.Millisecond}
	if got, ok := ch.Latest(context.Background(), "v1.0.0", t.TempDir()); ok {
		t.Fatalf("slow site reported %q", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %v", d)
	}
}

func TestCacheEdgeCases(t *testing.T) {
	s := newSite(t, 200, `{"version":"v1.5.0"}`)
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	c := &clock{now}
	dir := t.TempDir()
	path := filepath.Join(dir, CacheFile)

	// A corrupt cache is ignored and replaced.
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := checker(s, c).Latest(context.Background(), "v1.0.0", dir); !ok || got != "v1.5.0" {
		t.Fatalf("corrupt cache: %q, %v", got, ok)
	}
	var e cacheEntry
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &e); err != nil || e.Latest != "v1.5.0" || !e.CheckedAt.Equal(now) {
		t.Fatalf("cache after fetch = %s (%v)", b, err)
	}

	// A timestamp from the future (wrong clock back then) is stale.
	future, _ := json.Marshal(cacheEntry{CheckedAt: now.Add(48 * time.Hour), Latest: "v1.1.0"})
	if err := os.WriteFile(path, future, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := checker(s, c).Latest(context.Background(), "v1.0.0", dir); got != "v1.5.0" {
		t.Fatalf("future-dated cache was trusted: %q", got)
	}

	// The cached answer is compared with the CURRENT version every time:
	// after an update the notice goes away without asking again.
	hits := s.hits.Load()
	if got, ok := checker(s, c).Latest(context.Background(), "v1.5.0", dir); ok {
		t.Fatalf("up to date after update, still reported %q", got)
	}
	if s.hits.Load() != hits {
		t.Fatal("asked the site again within the day")
	}

	// A config directory that does not exist yet is created.
	nested := filepath.Join(t.TempDir(), "a", "b")
	checker(s, c).Latest(context.Background(), "v1.0.0", nested)
	if _, err := os.Stat(filepath.Join(nested, CacheFile)); err != nil {
		t.Fatalf("cache not written into a new directory: %v", err)
	}
	// An unwritable location: still an answer, just no cache.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := checker(s, c).Latest(context.Background(), "v1.0.0", filepath.Join(blocker, "sub")); !ok || got != "v1.5.0" {
		t.Fatalf("unwritable cache dir: %q, %v", got, ok)
	}
}

func TestDefaults(t *testing.T) {
	var c Checker
	if c.url() != DefaultURL || c.client() != http.DefaultClient || c.ttl() != 24*time.Hour ||
		c.timeout() != 3*time.Second || c.now().IsZero() {
		t.Fatalf("zero Checker defaults are off")
	}
	// The package-level Latest is the zero Checker: a dev build returns at
	// once, without a request (no network in this test).
	if got, ok := Latest(context.Background(), "dev", t.TempDir()); ok {
		t.Fatalf("dev build reported %q", got)
	}
}
