// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmfake

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/system"
)

// Characterization of the fake's routing at the HTTP level: every route,
// the version prefix, injected failures, refusals and unknown paths.
func TestSwarmRoutes(t *testing.T) {
	var svc swarm.Service
	svc.ID, svc.Spec.Name, svc.Version.Index = "s1", "web", 4
	var node swarm.Node
	node.ID = "n1"
	f := &Swarm{
		Services: []swarm.Service{svc},
		Nodes:    []swarm.Node{node},
		Secrets:  []swarm.Secret{{ID: "x", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "taken"}}}},
		Configs:  []swarm.Config{{ID: "y", Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "taken"}}}},
		Fail:     map[string]bool{"GET /networks": true},
	}
	do := func(method, target, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		f.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader(body)))
		return rec
	}
	cases := []struct {
		method, target, body string
		code                 int
		contains             string
	}{
		{"GET", "/v1.47/_ping", "", 200, "OK"},
		{"HEAD", "/_ping", "", 200, ""},
		{"GET", "/info", "", 200, `"Name":"manager-1"`},
		{"GET", "/swarm", "", 200, `"ID":""`},
		{"GET", "/v1.47/services", "", 200, `"ID":"s1"`},
		{"GET", `/services?filters={"name":{"nope":true}}`, "", 200, "null"},
		{"GET", "/services/web", "", 200, `"ID":"s1"`},
		{"GET", "/services/s9", "", 404, "not found: service s9"},
		{"GET", "/tasks", "", 200, "null"},
		{"GET", "/nodes", "", 200, `"ID":"n1"`},
		{"GET", "/nodes/n1", "", 200, `"ID":"n1"`},
		{"GET", "/nodes/n9", "", 404, "not found: node n9"},
		{"GET", "/networks", "", 500, "injected failure: GET /networks"},
		{"GET", `/secrets?filters={"name":{"tak":true}}`, "", 200, `"ID":"x"`},
		{"GET", "/configs", "", 200, `"ID":"y"`},
		{"GET", "/volumes", "", 404, "not found: /volumes"},
		{"DELETE", "/services/s1", "", 200, `"ID":"s1"`}, // anything but POST reads
		{"POST", "/secrets/create", `{"Name":"taken"}`, 409, "secret taken already exists"},
		{"POST", "/secrets/create", `{`, 400, "bad request body"},
		{"POST", "/configs/create", `{"Name":"taken"}`, 409, "config taken already exists"},
		{"POST", "/configs/create", `nope`, 400, "bad request body"},
		{"POST", "/configs/create", `{"Name":"new"}`, 201, `"ID":"config-new"`},
		{"POST", "/services/create", `{"Name":"web"}`, 409, "service web already exists"},
		{"POST", "/services/create", `[`, 400, "bad request body"},
		{"POST", "/services/s1/update?version=3", `{"Name":"web"}`, 500, "update out of sequence"},
		{"POST", "/services/s9/update?version=1", `{"Name":"x"}`, 404, "not found: service s9"},
		{"POST", "/services/s1/update", `{`, 400, "bad request body"},
		{"POST", "/services/web/update?version=4", `{"Name":"web"}`, 200, "{"},
		{"POST", "/services/s1/scale", `{}`, 404, "not found: /services/s1/scale"},
	}
	for _, c := range cases {
		rec := do(c.method, c.target, c.body)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.contains) {
			t.Errorf("%s %s = %d %q, want %d containing %q", c.method, c.target, rec.Code, rec.Body.String(), c.code, c.contains)
		}
	}
	if got := do("GET", "/_ping", "").Header().Get("Ostype"); got != "linux" {
		t.Errorf("ping Ostype = %q", got)
	}
	if f.Services[0].Version.Index != 5 {
		t.Errorf("update did not bump the version: %d", f.Services[0].Version.Index)
	}
	want := []string{"GET /volumes", "POST /services/s1/scale"}
	if !reflect.DeepEqual(f.Unknown, want) {
		t.Errorf("Unknown = %q, want %q", f.Unknown, want)
	}

	// Info and SwarmInfo answer when set.
	f.Info = &system.Info{Name: "boss"}
	f.SwarmInfo = &swarm.Swarm{}
	f.SwarmInfo.ID = "sw1"
	if rec := do("GET", "/info", ""); !strings.Contains(rec.Body.String(), `"Name":"boss"`) {
		t.Errorf("info = %s", rec.Body.String())
	}
	if rec := do("GET", "/swarm", ""); !strings.Contains(rec.Body.String(), `"ID":"sw1"`) {
		t.Errorf("swarm = %s", rec.Body.String())
	}
	if rec := do(http.MethodGet, "/info", ""); rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("content type = %q", rec.Header().Get("Content-Type"))
	}
}
