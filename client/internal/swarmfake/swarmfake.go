// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package swarmfake is a fake Docker Engine API for tests: an http.Handler
// that answers the manager calls swarmexec makes from fixed lists of
// services, tasks, nodes, networks, secrets and configs. Point a real
// *client.Client at it through httptest, and every layer above the client
// runs as it does against a cluster.
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

	mu      sync.Mutex
	Unknown []string // API paths the fake does not serve, for diagnosis
}

var apiVersionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func (f *Swarm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := apiVersionPrefix.ReplaceAllString(r.URL.Path, "")
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
		f.mu.Lock()
		f.Unknown = append(f.Unknown, r.Method+" "+path)
		f.mu.Unlock()
		notFound(w, path)
	}
}

func notFound(w http.ResponseWriter, what string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "not found: " + what})
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
