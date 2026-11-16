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
	if r.Method == http.MethodPost {
		f.write(w, r, path)
		return
	}
	filters := map[string]map[string]bool{}
	if raw := r.URL.Query().Get("filters"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &filters)
	}
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case path == "/_ping":
		w.Header().Set("Api-Version", client.MaxAPIVersion)
		w.Header().Set("Ostype", "linux")
		_, _ = io.WriteString(w, "OK")
	case path == "/info":
		if f.Info != nil {
			reply(f.Info)
			return
		}
		reply(system.Info{Name: "manager-1", ServerVersion: "28.0.0"})
	case path == "/swarm":
		if f.SwarmInfo != nil {
			reply(f.SwarmInfo)
			return
		}
		reply(swarm.Swarm{})
	case path == "/services":
		var out []swarm.Service
		for _, s := range f.Services {
			if MatchService(s, filters) {
				out = append(out, s)
			}
		}
		reply(out)
	case strings.HasPrefix(path, "/services/"):
		id := strings.TrimPrefix(path, "/services/")
		for _, s := range f.Services {
			if s.ID == id || s.Spec.Name == id {
				reply(s)
				return
			}
		}
		notFound(w, "service "+id)
	case path == "/tasks":
		var out []swarm.Task
		for _, t := range f.Tasks {
			if matchTask(t, filters) {
				out = append(out, t)
			}
		}
		reply(out)
	case path == "/nodes":
		reply(f.Nodes)
	case strings.HasPrefix(path, "/nodes/"):
		id := strings.TrimPrefix(path, "/nodes/")
		for _, n := range f.Nodes {
			if n.ID == id {
				reply(n)
				return
			}
		}
		notFound(w, "node "+id)
	case path == "/networks":
		reply(f.Networks)
	case path == "/secrets":
		reply(filterNamed(f.Secrets, filters, func(s swarm.Secret) string { return s.Spec.Name }))
	case path == "/configs":
		reply(filterNamed(f.Configs, filters, func(c swarm.Config) string { return c.Spec.Name }))
	default:
		f.unknown(w, r, path)
	}
}

func (f *Swarm) unknown(w http.ResponseWriter, r *http.Request, path string) {
	f.Unknown = append(f.Unknown, r.Method+" "+path)
	notFound(w, path)
}

// write serves the creates and the service update. A new object's id is
// "<kind>-<name>", so a test can lay down tasks for a service init has yet to
// create.
func (f *Swarm) write(w http.ResponseWriter, r *http.Request, path string) {
	created := func(id string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"ID": id})
	}
	taken := func(kind, name string) {
		fail(w, http.StatusConflict, kind+" "+name+" already exists")
	}
	switch {
	case path == "/secrets/create":
		var spec swarm.SecretSpec
		if !decode(w, r, &spec) {
			return
		}
		for _, s := range f.Secrets {
			if s.Spec.Name == spec.Name {
				taken("secret", spec.Name)
				return
			}
		}
		f.Secrets = append(f.Secrets, swarm.Secret{ID: "secret-" + spec.Name, Spec: spec})
		created("secret-" + spec.Name)
	case path == "/configs/create":
		var spec swarm.ConfigSpec
		if !decode(w, r, &spec) {
			return
		}
		for _, c := range f.Configs {
			if c.Spec.Name == spec.Name {
				taken("config", spec.Name)
				return
			}
		}
		f.Configs = append(f.Configs, swarm.Config{ID: "config-" + spec.Name, Spec: spec})
		created("config-" + spec.Name)
	case path == "/services/create":
		var spec swarm.ServiceSpec
		if !decode(w, r, &spec) {
			return
		}
		for _, s := range f.Services {
			if s.Spec.Name == spec.Name {
				taken("service", spec.Name)
				return
			}
		}
		f.RegistryAuth = append(f.RegistryAuth, r.Header.Get("X-Registry-Auth"))
		var s swarm.Service
		s.ID, s.Version.Index, s.Spec = "service-"+spec.Name, 1, spec
		f.Services = append(f.Services, s)
		created(s.ID)
	case strings.HasPrefix(path, "/services/") && strings.HasSuffix(path, "/update"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/services/"), "/update")
		var spec swarm.ServiceSpec
		if !decode(w, r, &spec) {
			return
		}
		for i := range f.Services {
			s := &f.Services[i]
			if s.ID != id && s.Spec.Name != id {
				continue
			}
			// The manager's optimistic lock: an update names the version it
			// was computed from, and loses to any write since.
			if r.URL.Query().Get("version") != fmt.Sprint(s.Version.Index) {
				fail(w, http.StatusInternalServerError, "update out of sequence")
				return
			}
			f.RegistryAuth = append(f.RegistryAuth, r.Header.Get("X-Registry-Auth"))
			s.Spec = spec
			s.Version.Index++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(swarm.ServiceUpdateResponse{})
			return
		}
		notFound(w, "service "+id)
	default:
		f.unknown(w, r, path)
	}
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
