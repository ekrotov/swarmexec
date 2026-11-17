// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmfake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
)

// filtered is the list endpoint path with the engine's JSON filters.
func filtered(path string, f map[string][]string) string {
	m := map[string]map[string]bool{}
	for k, vs := range f {
		m[k] = map[string]bool{}
		for _, v := range vs {
			m[k][v] = true
		}
	}
	b, _ := json.Marshal(m)
	return path + "?filters=" + url.QueryEscape(string(b))
}

func named(name string, labels map[string]string) swarm.Annotations {
	return swarm.Annotations{Name: name, Labels: labels}
}

// A list answers only the filters it implements. One it drops would make a
// lookup "find" any target (the resolver filters tasks by id and takes the
// first), and a misspelt filter would pass unnoticed; the engine answers 400.
func TestListsRefuseFiltersTheyDoNotServe(t *testing.T) {
	f := &Swarm{}
	for _, c := range []struct{ path, filter string }{
		{"/services", "node"},
		{"/tasks", "label"},
		{"/tasks", "name"},
		{"/nodes", "role"},
		{"/networks", "driver"},
		{"/secrets", "service"},
		{"/configs", "service"},
		{"/services", "nmae"}, // a typo is not silently ignored
	} {
		rec := httptest.NewRecorder()
		f.ServeHTTP(rec, httptest.NewRequest("GET", filtered(c.path, map[string][]string{c.filter: {"x"}}), nil))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Invalid filter '"+c.filter+"'") {
			t.Errorf("%s with filter %s = %d %q, want 400 Invalid filter", c.path, c.filter, rec.Code, rec.Body.String())
		}
	}
	if len(f.Unknown) != 8 {
		t.Errorf("Unknown = %q, want every refusal recorded", f.Unknown)
	}

	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest("GET", "/services?filters=not-json", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unparsable filters = %d, want 400", rec.Code)
	}
}

// The filters that are served narrow the list as the manager does: id and
// name are prefixes, label is key or key=value, names is exact.
func TestListsApplyTheirFilters(t *testing.T) {
	f := &Swarm{
		Services: []swarm.Service{
			{ID: "s1abc", Spec: swarm.ServiceSpec{Annotations: named("shop_web", map[string]string{"tier": "front"})}},
			{ID: "s2def", Spec: swarm.ServiceSpec{Annotations: named("shop_db", nil)}},
		},
		Tasks: []swarm.Task{
			{ID: "t1aa", ServiceID: "s1abc", DesiredState: swarm.TaskStateRunning},
			{ID: "t1bb", ServiceID: "s2def", DesiredState: swarm.TaskStateShutdown},
		},
		Networks: []network.Summary{
			{Network: network.Network{ID: "n1", Name: "shop_net", Labels: map[string]string{"k": "v"}}},
			{Network: network.Network{ID: "n2", Name: "other"}},
		},
		Secrets: []swarm.Secret{{ID: "x1", Spec: swarm.SecretSpec{Annotations: named("db_pw", nil)}}, {ID: "x2", Spec: swarm.SecretSpec{Annotations: named("db_pw2", nil)}}},
		Configs: []swarm.Config{{ID: "c1", Spec: swarm.ConfigSpec{Annotations: named("app", nil)}}, {ID: "c2", Spec: swarm.ConfigSpec{Annotations: named("app2", nil)}}},
	}
	count := func(path string, fl map[string][]string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		f.ServeHTTP(rec, httptest.NewRequest("GET", filtered(path, fl), nil))
		if rec.Code != 200 {
			t.Fatalf("%s %v = %d %s", path, fl, rec.Code, rec.Body.String())
		}
		var items []json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
			t.Fatal(err)
		}
		return len(items)
	}
	for _, c := range []struct {
		path string
		fl   map[string][]string
		want int
	}{
		{"/services", nil, 2},
		{"/services", map[string][]string{"id": {"s1"}}, 1},
		{"/services", map[string][]string{"id": {"definitely-not-there"}}, 0},
		{"/services", map[string][]string{"name": {"shop_"}}, 2},
		{"/services", map[string][]string{"name": {"web"}}, 0}, // a prefix, not a substring
		{"/services", map[string][]string{"label": {"tier=front"}}, 1},
		{"/services", map[string][]string{"label": {"tier=back"}}, 0},
		{"/tasks", map[string][]string{"id": {"t1"}}, 2},
		{"/tasks", map[string][]string{"id": {"t1aa"}}, 1},
		{"/tasks", map[string][]string{"id": {"definitely-not-there"}}, 0},
		{"/tasks", map[string][]string{"service": {"s1abc"}}, 1},
		{"/tasks", map[string][]string{"desired-state": {"running"}}, 1},
		{"/tasks", map[string][]string{"service": {"s1abc"}, "desired-state": {"shutdown"}}, 0},
		{"/networks", map[string][]string{"id": {"n2"}}, 1},
		{"/networks", map[string][]string{"name": {"shop"}}, 1},
		{"/networks", map[string][]string{"label": {"k"}}, 1},
		{"/secrets", map[string][]string{"name": {"db_pw"}}, 2},
		{"/secrets", map[string][]string{"names": {"db_pw"}}, 1},
		{"/secrets", map[string][]string{"id": {"x2"}}, 1},
		{"/configs", map[string][]string{"name": {"app"}}, 2},
		{"/configs", map[string][]string{"names": {"app2"}}, 1},
	} {
		if got := count(c.path, c.fl); got != c.want {
			t.Errorf("%s %v: %d items, want %d", c.path, c.fl, got, c.want)
		}
	}
	if len(f.Unknown) != 0 {
		t.Errorf("Unknown = %q", f.Unknown)
	}
}

// failRecorder is a testing.TB whose Errorf is recorded, for checking what
// Serve reports at cleanup.
type failRecorder struct {
	testing.TB
	errs     []string
	cleanups []func()
}

func (r *failRecorder) Helper()                   {}
func (r *failRecorder) Cleanup(fn func())         { r.cleanups = append(r.cleanups, fn) }
func (r *failRecorder) Errorf(f string, a ...any) { r.errs = append(r.errs, f) }

// Serve fails the test at cleanup when the fake was asked for something it
// does not serve, and not otherwise.
func TestServeFailsOnUnknown(t *testing.T) {
	run := func(path string) *failRecorder {
		rec := &failRecorder{TB: t}
		srv := Serve(rec, &Swarm{})
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for i := len(rec.cleanups) - 1; i >= 0; i-- {
			rec.cleanups[i]()
		}
		return rec
	}
	if r := run("/services"); len(r.errs) != 0 {
		t.Errorf("a served path failed the test: %v", r.errs)
	}
	if r := run("/services?filters=" + url.QueryEscape(`{"bogus":{"x":true}}`)); len(r.errs) != 1 {
		t.Errorf("a refused filter did not fail the test: %v", r.errs)
	}
	if r := run("/volumes"); len(r.errs) != 1 {
		t.Errorf("an unknown path did not fail the test: %v", r.errs)
	}
}
