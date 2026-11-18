// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package swarmfake is a fake Docker Engine API for tests: an http.Handler
// that answers the manager calls swarmexec makes from fixed lists of
// services, tasks, nodes, networks, secrets and configs. Point a real
// *client.Client at it through httptest, and every layer above the client
// runs as it does against a cluster.
//
// It also takes the writes `swarmexec init` and the TUI make — creating
// secrets, configs and services, updating a service or a node, removing a
// service — into the same lists, with the two refusals of a real manager that
// their behaviour depends on: a name that is taken, and an update computed
// from a stale version.
//
// Test-only: nothing outside _test.go files imports it.
package swarmfake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
)

type Swarm struct {
	Services []swarm.Service
	Tasks    []swarm.Task
	Nodes    []swarm.Node
	Networks []network.Summary
	Secrets  []swarm.Secret
	Configs  []swarm.Config
	// SwarmInfo answers GET /swarm (nil = an empty swarm.Swarm: no default
	// address pool set, i.e. swarm's built-in 10.0.0.0/8).
	SwarmInfo *swarm.Swarm

	// Info is what /info answers; nil answers a bare "manager-1", which is
	// not a manager as far as init can tell (no ControlAvailable).
	Info *system.Info
	// Fail makes the named calls ("POST /services/create") answer 500, for
	// the error paths of a write.
	Fail map[string]bool

	// mu covers every field: the handler holds it for the whole request, so a
	// test reading the lists after a write does not race the server.
	mu      sync.Mutex
	Unknown []string // API paths the fake does not serve, for diagnosis
	// RegistryAuth is the X-Registry-Auth header of each service create and
	// update, in order ("" when none was sent).
	RegistryAuth []string
	// StrictNetworks makes a service create or update fail, as a real manager
	// does, when it names a network that does not exist.
	StrictNetworks bool
	// Warnings is what service create and update answer in their Warnings
	// field.
	Warnings []string
}

var apiVersionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func (f *Swarm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := apiVersionPrefix.ReplaceAllString(r.URL.Path, "")
	if f.Fail[r.Method+" "+path] {
		fail(w, http.StatusInternalServerError, "injected failure: "+r.Method+" "+path)
		return
	}
	// POST writes, DELETE removes; any other method reads.
	table := readRoutes
	switch r.Method {
	case http.MethodPost:
		table = writeRoutes
	case http.MethodDelete:
		table = deleteRoutes
	}
	c := &call{f: f, w: w, r: r}
	for _, rt := range table {
		if arg, ok := rt.match(path); ok {
			rt.serve(f, c, arg)
			return
		}
	}
	f.unknown(w, r, path)
}

// call is one request being served.
type call struct {
	f *Swarm
	w http.ResponseWriter
	r *http.Request
}

// reply answers v as JSON with 200.
func (c *call) reply(v any) {
	c.w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(c.w).Encode(v)
}

// filters is the request's list filters (name -> values). Only the names in
// allowed are served; any other filter, or a filters parameter that is not the
// engine's JSON, is refused with 400 as the engine does, and recorded in
// Unknown so a test that swallows the error still fails. ok is false after
// such a refusal: the caller must return without answering.
func (c *call) filters(allowed ...string) (filters map[string]map[string]bool, ok bool) {
	filters = map[string]map[string]bool{}
	raw := c.r.URL.Query().Get("filters")
	if raw == "" {
		return filters, true
	}
	if err := json.Unmarshal([]byte(raw), &filters); err != nil {
		c.f.Unknown = append(c.f.Unknown, c.r.Method+" "+c.r.URL.Path+" (unparsable filters)")
		fail(c.w, http.StatusBadRequest, "invalid filter: "+err.Error())
		return nil, false
	}
	for name := range filters {
		if !slices.Contains(allowed, name) {
			c.f.Unknown = append(c.f.Unknown, c.r.Method+" "+c.r.URL.Path+" (filter "+name+")")
			fail(c.w, http.StatusBadRequest, fmt.Sprintf("Invalid filter '%s'", name))
			return nil, false
		}
	}
	return filters, true
}

// created answers a create with the new object's id.
func (c *call) created(id string) {
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(c.w).Encode(map[string]string{"ID": id})
}

// taken is the manager's refusal of a name in use.
func (c *call) taken(kind, name string) {
	fail(c.w, http.StatusConflict, kind+" "+name+" already exists")
}

// route serves the paths match accepts; arg is what match extracted (an id).
type route struct {
	match func(path string) (arg string, ok bool)
	serve func(f *Swarm, c *call, arg string)
}

// exact matches one path.
func exact(p string) func(string) (string, bool) {
	return func(path string) (string, bool) { return "", path == p }
}

// under matches prefix<arg>suffix and extracts arg.
func under(prefix, suffix string) func(string) (string, bool) {
	return func(path string) (string, bool) {
		if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
			return "", false
		}
		return strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix), true
	}
}

// readRoutes, writeRoutes and deleteRoutes are tried in order; the first
// match serves.
var readRoutes = []route{
	{exact("/_ping"), (*Swarm).ping},
	{exact("/info"), (*Swarm).info},
	{exact("/swarm"), (*Swarm).swarmInfo},
	{exact("/services"), (*Swarm).listServices},
	{under("/services/", ""), (*Swarm).getService},
	{exact("/tasks"), (*Swarm).listTasks},
	{exact("/nodes"), (*Swarm).listNodes},
	{under("/nodes/", ""), (*Swarm).getNode},
	{under("/networks/", ""), (*Swarm).getNetwork},
	// Digest resolution of an image reference: the fake has no registry, so
	// the answer is the engine's "not found" and the caller keeps the tag.
	{under("/distribution/", "/json"), func(_ *Swarm, c *call, ref string) { notFound(c.w, "image "+ref) }},
	{exact("/networks"), (*Swarm).listNetworks},
	{exact("/secrets"), (*Swarm).listSecrets},
	{exact("/configs"), (*Swarm).listConfigs},
}

var writeRoutes = []route{
	{exact("/secrets/create"), (*Swarm).createSecret},
	{exact("/configs/create"), (*Swarm).createConfig},
	{exact("/services/create"), (*Swarm).createService},
	{exact("/networks/create"), (*Swarm).createNetwork},
	{under("/services/", "/update"), (*Swarm).updateService},
	{under("/nodes/", "/update"), (*Swarm).updateNode},
}

var deleteRoutes = []route{
	{under("/services/", ""), (*Swarm).removeService},
	{under("/secrets/", ""), func(f *Swarm, c *call, id string) {
		f.Secrets = removeByID(c, f.Secrets, "secret "+id, func(s swarm.Secret) bool { return s.ID == id || s.Spec.Name == id })
	}},
	{under("/configs/", ""), func(f *Swarm, c *call, id string) {
		f.Configs = removeByID(c, f.Configs, "config "+id, func(cf swarm.Config) bool { return cf.ID == id || cf.Spec.Name == id })
	}},
	{under("/networks/", ""), func(f *Swarm, c *call, id string) {
		f.Networks = removeByID(c, f.Networks, "network "+id, func(n network.Summary) bool { return n.ID == id || n.Name == id })
	}},
}

// removeByID drops the first item that matches and answers 204, or answers 404.
func removeByID[T any](c *call, items []T, what string, is func(T) bool) []T {
	for i, it := range items {
		if is(it) {
			c.w.WriteHeader(http.StatusNoContent)
			return append(items[:i:i], items[i+1:]...)
		}
	}
	notFound(c.w, what)
	return items
}

func (f *Swarm) ping(c *call, _ string) {
	c.w.Header().Set("Api-Version", client.MaxAPIVersion)
	c.w.Header().Set("Ostype", "linux")
	_, _ = io.WriteString(c.w, "OK")
}

func (f *Swarm) info(c *call, _ string) {
	if f.Info != nil {
		c.reply(f.Info)
		return
	}
	c.reply(system.Info{Name: "manager-1", ServerVersion: "28.0.0"})
}

func (f *Swarm) swarmInfo(c *call, _ string) {
	if f.SwarmInfo != nil {
		c.reply(f.SwarmInfo)
		return
	}
	c.reply(swarm.Swarm{})
}

func (f *Swarm) listServices(c *call, _ string) {
	filters, ok := c.filters("id", "name", "label")
	if !ok {
		return
	}
	var out []swarm.Service
	for _, s := range f.Services {
		if MatchService(s, filters) {
			out = append(out, s)
		}
	}
	c.reply(out)
}

func (f *Swarm) getService(c *call, id string) {
	for _, s := range f.Services {
		if s.ID == id || s.Spec.Name == id {
			c.reply(s)
			return
		}
	}
	notFound(c.w, "service "+id)
}

func (f *Swarm) listTasks(c *call, _ string) {
	filters, ok := c.filters("id", "service", "desired-state")
	if !ok {
		return
	}
	var out []swarm.Task
	for _, t := range f.Tasks {
		if matchTask(t, filters) {
			out = append(out, t)
		}
	}
	c.reply(out)
}

// The lists. Each serves the filters the engine documents for it that
// swarmexec uses; the others are refused (see call.filters), because a filter
// the fake drops makes every lookup "find" its target. As the manager does, id
// and name match as prefixes.

func (f *Swarm) listNodes(c *call, _ string) {
	if _, ok := c.filters(); ok {
		c.reply(f.Nodes)
	}
}

func (f *Swarm) listNetworks(c *call, _ string) {
	filters, ok := c.filters("id", "name", "label")
	if !ok {
		return
	}
	var out []network.Summary
	for _, n := range f.Networks {
		if hasPrefix(filters["id"], n.ID) && hasPrefix(filters["name"], n.Name) && hasLabels(n.Labels, filters["label"]) {
			out = append(out, n)
		}
	}
	c.reply(out)
}

func (f *Swarm) listSecrets(c *call, _ string) {
	filters, ok := c.filters("id", "name", "names", "label")
	if !ok {
		return
	}
	var out []swarm.Secret
	for _, s := range f.Secrets {
		if hasPrefix(filters["id"], s.ID) && hasPrefix(filters["name"], s.Spec.Name) &&
			isOneOf(filters["names"], s.Spec.Name) && hasLabels(s.Spec.Labels, filters["label"]) {
			out = append(out, s)
		}
	}
	c.reply(out)
}

func (f *Swarm) listConfigs(c *call, _ string) {
	filters, ok := c.filters("id", "name", "names", "label")
	if !ok {
		return
	}
	var out []swarm.Config
	for _, cf := range f.Configs {
		if hasPrefix(filters["id"], cf.ID) && hasPrefix(filters["name"], cf.Spec.Name) &&
			isOneOf(filters["names"], cf.Spec.Name) && hasLabels(cf.Spec.Labels, filters["label"]) {
			out = append(out, cf)
		}
	}
	c.reply(out)
}

func (f *Swarm) getNode(c *call, id string) {
	for _, n := range f.Nodes {
		if n.ID == id {
			c.reply(n)
			return
		}
	}
	notFound(c.w, "node "+id)
}

func (f *Swarm) unknown(w http.ResponseWriter, r *http.Request, path string) {
	f.Unknown = append(f.Unknown, r.Method+" "+path)
	notFound(w, path)
}

// The writes: the creates, the service and node updates, the service
// removal. A new object's id is
// "<kind>-<name>", so a test can lay down tasks for a service init has yet to
// create.

func (f *Swarm) createSecret(c *call, _ string) {
	var spec swarm.SecretSpec
	if !decode(c.w, c.r, &spec) {
		return
	}
	for _, s := range f.Secrets {
		if s.Spec.Name == spec.Name {
			c.taken("secret", spec.Name)
			return
		}
	}
	f.Secrets = append(f.Secrets, swarm.Secret{ID: "secret-" + spec.Name, Spec: spec})
	c.created("secret-" + spec.Name)
}

func (f *Swarm) createConfig(c *call, _ string) {
	var spec swarm.ConfigSpec
	if !decode(c.w, c.r, &spec) {
		return
	}
	for _, cf := range f.Configs {
		if cf.Spec.Name == spec.Name {
			c.taken("config", spec.Name)
			return
		}
	}
	f.Configs = append(f.Configs, swarm.Config{ID: "config-" + spec.Name, Spec: spec})
	c.created("config-" + spec.Name)
}

func (f *Swarm) createNetwork(c *call, _ string) {
	var req struct {
		Name   string
		Driver string
		Labels map[string]string
	}
	if !decode(c.w, c.r, &req) {
		return
	}
	for _, n := range f.Networks {
		if n.Name == req.Name {
			c.taken("network", req.Name)
			return
		}
	}
	n := network.Summary{Network: network.Network{ID: "network-" + req.Name, Name: req.Name, Driver: req.Driver, Labels: req.Labels}}
	f.Networks = append(f.Networks, n)
	c.created(n.ID)
}

func (f *Swarm) getNetwork(c *call, id string) {
	for _, n := range f.Networks {
		if n.ID == id || n.Name == id {
			c.reply(n)
			return
		}
	}
	notFound(c.w, "network "+id)
}

// missingNetwork names the first network of spec that does not exist, "" when
// all do (or StrictNetworks is off).
func (f *Swarm) missingNetwork(spec swarm.ServiceSpec) string {
	if !f.StrictNetworks {
		return ""
	}
	for _, att := range spec.TaskTemplate.Networks {
		found := false
		for _, n := range f.Networks {
			if n.ID == att.Target || n.Name == att.Target {
				found = true
			}
		}
		if !found {
			return att.Target
		}
	}
	return ""
}

func (f *Swarm) createService(c *call, _ string) {
	var spec swarm.ServiceSpec
	if !decode(c.w, c.r, &spec) {
		return
	}
	for _, s := range f.Services {
		if s.Spec.Name == spec.Name {
			c.taken("service", spec.Name)
			return
		}
	}
	if n := f.missingNetwork(spec); n != "" {
		fail(c.w, http.StatusBadRequest, "network "+n+" not found")
		return
	}
	f.RegistryAuth = append(f.RegistryAuth, c.r.Header.Get("X-Registry-Auth"))
	var s swarm.Service
	s.ID, s.Version.Index, s.Spec = "service-"+spec.Name, 1, spec
	f.Services = append(f.Services, s)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(c.w).Encode(swarm.ServiceCreateResponse{ID: s.ID, Warnings: f.Warnings})
}

func (f *Swarm) updateService(c *call, id string) {
	var spec swarm.ServiceSpec
	if !decode(c.w, c.r, &spec) {
		return
	}
	for i := range f.Services {
		s := &f.Services[i]
		if s.ID != id && s.Spec.Name != id {
			continue
		}
		// The manager's optimistic lock: an update names the version it
		// was computed from, and loses to any write since.
		if c.r.URL.Query().Get("version") != fmt.Sprint(s.Version.Index) {
			fail(c.w, http.StatusInternalServerError, "update out of sequence")
			return
		}
		if n := f.missingNetwork(spec); n != "" {
			fail(c.w, http.StatusBadRequest, "network "+n+" not found")
			return
		}
		f.RegistryAuth = append(f.RegistryAuth, c.r.Header.Get("X-Registry-Auth"))
		// As the manager does: the spec being replaced becomes the one a
		// rollback goes back to.
		prev := s.Spec
		s.PreviousSpec = &prev
		s.Spec = spec
		s.Version.Index++
		c.reply(swarm.ServiceUpdateResponse{Warnings: f.Warnings})
		return
	}
	notFound(c.w, "service "+id)
}

func (f *Swarm) updateNode(c *call, id string) {
	var spec swarm.NodeSpec
	if !decode(c.w, c.r, &spec) {
		return
	}
	for i := range f.Nodes {
		n := &f.Nodes[i]
		if n.ID != id && n.Description.Hostname != id {
			continue
		}
		// The same optimistic lock as a service update.
		if c.r.URL.Query().Get("version") != fmt.Sprint(n.Version.Index) {
			fail(c.w, http.StatusInternalServerError, "update out of sequence")
			return
		}
		n.Spec = spec
		n.Version.Index++
		c.w.WriteHeader(http.StatusOK)
		return
	}
	notFound(c.w, "node "+id)
}

// removeService drops the service and its tasks, as the manager does once
// the tasks have shut down.
func (f *Swarm) removeService(c *call, id string) {
	for i, s := range f.Services {
		if s.ID != id && s.Spec.Name != id {
			continue
		}
		f.Services = append(f.Services[:i:i], f.Services[i+1:]...)
		var tasks []swarm.Task
		for _, t := range f.Tasks {
			if t.ServiceID != s.ID {
				tasks = append(tasks, t)
			}
		}
		f.Tasks = tasks
		c.w.WriteHeader(http.StatusOK)
		return
	}
	notFound(c.w, "service "+id)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return false
	}
	return true
}

func notFound(w http.ResponseWriter, what string) {
	fail(w, http.StatusNotFound, "not found: "+what)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

// hasPrefix: no filter values, or one is a prefix of have.
func hasPrefix(want map[string]bool, have string) bool {
	if len(want) == 0 {
		return true
	}
	for w := range want {
		if strings.HasPrefix(have, w) {
			return true
		}
	}
	return false
}

// isOneOf: no filter values, or have is one of them.
func isOneOf(want map[string]bool, have string) bool {
	return len(want) == 0 || want[have]
}

// hasLabels: have carries every label filter (key or key=value).
func hasLabels(have map[string]string, want map[string]bool) bool {
	for l := range want {
		k, v, hasV := strings.Cut(l, "=")
		got, ok := have[k]
		if !ok || (hasV && got != v) {
			return false
		}
	}
	return true
}

// MatchService reports whether s passes the service filters id, name (both
// prefixes) and label.
func MatchService(s swarm.Service, f map[string]map[string]bool) bool {
	return hasPrefix(f["id"], s.ID) && hasPrefix(f["name"], s.Spec.Name) && hasLabels(s.Spec.Labels, f["label"])
}

// matchTask applies the task filters: id (prefix), service (id), desired-state.
func matchTask(t swarm.Task, f map[string]map[string]bool) bool {
	if !hasPrefix(f["id"], t.ID) {
		return false
	}
	if want := f["service"]; len(want) > 0 && !want[t.ServiceID] {
		return false
	}
	if want := f["desired-state"]; len(want) > 0 && !want[string(t.DesiredState)] {
		return false
	}
	return true
}

// selfSignedPair makes a throwaway server certificate for the fake agent.
func SelfSignedPair(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake-agent"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// Serve starts f as an HTTP server for the test and, at cleanup, fails the
// test if f was asked for something it does not serve: an unknown path or a
// refused filter (see Swarm.Unknown). It returns the server; close is
// registered.
func Serve(t testing.TB, f *Swarm) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(func() {
		srv.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.Unknown) > 0 {
			t.Errorf("swarmfake was asked for what it does not serve: %q", f.Unknown)
		}
	})
	return srv
}

// Client serves f and returns a Docker client pointed at it, with both closed
// at cleanup. Every package that tests against a fake swarm needs this and
// each had written its own copy.
func Client(t testing.TB, f *Swarm) *client.Client {
	t.Helper()
	api := Serve(t, f)
	c, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
