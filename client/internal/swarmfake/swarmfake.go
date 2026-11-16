// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package swarmfake is a fake Docker Engine API for tests: an http.Handler
// that answers the manager calls swarmexec makes from fixed lists of
// services, tasks, nodes, networks, secrets and configs. Point a real
// *client.Client at it through httptest, and every layer above the client
// runs as it does against a cluster.
//
// It also takes the writes `swarmexec init` makes — creating secrets, configs
// and services, and updating a service — into the same lists, with the two
// refusals of a real manager that init's behaviour depends on: a name that is
// taken, and an update computed from a stale version.
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
	"regexp"
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
	// Every POST is a write; any other method reads.
	table := readRoutes
	if r.Method == http.MethodPost {
		table = writeRoutes
	}
	c := &call{w: w, r: r}
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
	w http.ResponseWriter
	r *http.Request
}

// reply answers v as JSON with 200.
func (c *call) reply(v any) {
	c.w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(c.w).Encode(v)
}

// filters is the request's list filters (name -> values), empty when none
// or unparsable.
func (c *call) filters() map[string]map[string]bool {
	filters := map[string]map[string]bool{}
	if raw := c.r.URL.Query().Get("filters"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &filters)
	}
	return filters
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

// readRoutes and writeRoutes are tried in order; the first match serves.
var readRoutes = []route{
	{exact("/_ping"), (*Swarm).ping},
	{exact("/info"), (*Swarm).info},
	{exact("/swarm"), (*Swarm).swarmInfo},
	{exact("/services"), (*Swarm).listServices},
	{under("/services/", ""), (*Swarm).getService},
	{exact("/tasks"), (*Swarm).listTasks},
	{exact("/nodes"), func(f *Swarm, c *call, _ string) { c.reply(f.Nodes) }},
	{under("/nodes/", ""), (*Swarm).getNode},
	{exact("/networks"), func(f *Swarm, c *call, _ string) { c.reply(f.Networks) }},
	{exact("/secrets"), func(f *Swarm, c *call, _ string) {
		c.reply(filterNamed(f.Secrets, c.filters(), func(s swarm.Secret) string { return s.Spec.Name }))
	}},
	{exact("/configs"), func(f *Swarm, c *call, _ string) {
		c.reply(filterNamed(f.Configs, c.filters(), func(cf swarm.Config) string { return cf.Spec.Name }))
	}},
}

var writeRoutes = []route{
	{exact("/secrets/create"), (*Swarm).createSecret},
	{exact("/configs/create"), (*Swarm).createConfig},
	{exact("/services/create"), (*Swarm).createService},
	{under("/services/", "/update"), (*Swarm).updateService},
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
	filters := c.filters()
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
	filters := c.filters()
	var out []swarm.Task
	for _, t := range f.Tasks {
		if matchTask(t, filters) {
			out = append(out, t)
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

// The writes: the creates and the service update. A new object's id is
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
	f.RegistryAuth = append(f.RegistryAuth, c.r.Header.Get("X-Registry-Auth"))
	var s swarm.Service
	s.ID, s.Version.Index, s.Spec = "service-"+spec.Name, 1, spec
	f.Services = append(f.Services, s)
	c.created(s.ID)
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
		f.RegistryAuth = append(f.RegistryAuth, c.r.Header.Get("X-Registry-Auth"))
		s.Spec = spec
		s.Version.Index++
		c.reply(swarm.ServiceUpdateResponse{})
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

func MatchService(s swarm.Service, f map[string]map[string]bool) bool {
	for name := range f["name"] {
		if !strings.Contains(s.Spec.Name, name) {
			return false
		}
	}
	for l := range f["label"] {
		k, v, hasV := strings.Cut(l, "=")
		got, ok := s.Spec.Labels[k]
		if !ok || (hasV && got != v) {
			return false
		}
	}
	return true
}

func matchTask(t swarm.Task, f map[string]map[string]bool) bool {
	if want := f["service"]; len(want) > 0 && !want[t.ServiceID] {
		return false
	}
	if want := f["desired-state"]; len(want) > 0 && !want[string(t.DesiredState)] {
		return false
	}
	return true
}

func filterNamed[T any](items []T, f map[string]map[string]bool, name func(T) string) []T {
	if len(f["name"]) == 0 {
		return items
	}
	var out []T
	for _, it := range items {
		for n := range f["name"] {
			if strings.Contains(name(it), n) {
				out = append(out, it)
				break
			}
		}
	}
	return out
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
