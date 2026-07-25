// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package dockerctx

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// isolate (in dockerctx_test.go) points DOCKER_CONFIG at dir and clears the
// docker env so these tests never touch the host's real contexts.

func TestCreateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	isolate(t, dir)

	if err := Create("prod", "ssh://ops@manager.example.com", "the prod swarm"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The directory name must be sha256(name), so docker reads it too.
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("prod")))
	metaPath := filepath.Join(dir, "contexts", "meta", digest, "meta.json")
	b, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("meta.json not written at the docker path: %v", err)
	}

	// It must decode as docker's format.
	var meta struct {
		Name      string            `json:"Name"`
		Metadata  map[string]string `json:"Metadata"`
		Endpoints map[string]struct {
			Host          string `json:"Host"`
			SkipTLSVerify bool   `json:"SkipTLSVerify"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatalf("meta.json is not valid docker format: %v", err)
	}
	if meta.Name != "prod" || meta.Endpoints["docker"].Host != "ssh://ops@manager.example.com" {
		t.Fatalf("meta mismatch: %+v", meta)
	}
	if meta.Metadata["Description"] != "the prod swarm" {
		t.Errorf("description = %q, want %q", meta.Metadata["Description"], "the prod swarm")
	}

	// And swarmexec's own resolver must round-trip it.
	host, err := ResolveHost("prod")
	if err != nil {
		t.Fatalf("ResolveHost: %v", err)
	}
	if host != "ssh://ops@manager.example.com" {
		t.Errorf("ResolveHost = %q", host)
	}

	// List includes it alongside the built-in default.
	names := map[string]bool{}
	ctxs, _ := List()
	for _, c := range ctxs {
		names[c.Name] = true
	}
	if !names["prod"] || !names["default"] {
		t.Errorf("List missing entries: %v", names)
	}
}

func TestCreateRejects(t *testing.T) {
	isolate(t, t.TempDir())
	tests := []struct {
		name, host, why string
	}{
		{"default", "ssh://h", "reserved name"},
		{"bad name", "ssh://h", "space in name"},
		{"-lead", "ssh://h", "leading dash"},
		{"ok", "http://h", "unsupported scheme"},
		{"ok2", "", "empty host"},
	}
	for _, tc := range tests {
		if err := Create(tc.name, tc.host, ""); err == nil {
			t.Errorf("Create(%q,%q) accepted, want rejection (%s)", tc.name, tc.host, tc.why)
		}
	}

	// Duplicate.
	if err := Create("dup", "tcp://h:2376", ""); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := Create("dup", "tcp://h:2376", ""); err == nil {
		t.Error("second Create of the same name accepted, want rejection")
	}
}

func TestUsePreservesConfig(t *testing.T) {
	dir := t.TempDir()
	isolate(t, dir)
	// A config.json that already holds unrelated data (credentials, etc.).
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"auths":{"reg.example.com":{"auth":"secret"}},"psFormat":"table"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Create("staging", "ssh://stg", ""); err != nil {
		t.Fatal(err)
	}
	if err := Use("staging"); err != nil {
		t.Fatalf("Use: %v", err)
	}

	b, _ := os.ReadFile(cfgPath)
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("config.json corrupted: %v", err)
	}
	if string(cfg["currentContext"]) != `"staging"` {
		t.Errorf("currentContext = %s, want \"staging\"", cfg["currentContext"])
	}
	// The pre-existing keys must survive untouched.
	if _, ok := cfg["auths"]; !ok {
		t.Error("Use dropped the existing auths key")
	}
	if string(cfg["psFormat"]) != `"table"` {
		t.Error("Use altered an unrelated key")
	}

	// Current() and List() reflect the selection.
	if Current() != "staging" {
		t.Errorf("Current() = %q, want staging", Current())
	}
	ctxs, _ := List()
	for _, c := range ctxs {
		if c.Name == "staging" && !c.Current {
			t.Error("List does not mark staging current")
		}
	}
}

func TestUseUnknownFails(t *testing.T) {
	isolate(t, t.TempDir())
	if err := Use("nope"); err == nil {
		t.Error("Use of a non-existent context accepted, want error")
	}
	// "default" is always usable.
	if err := Use("default"); err != nil {
		t.Errorf("Use(default): %v", err)
	}
}

func TestRemove(t *testing.T) {
	isolate(t, t.TempDir())
	if err := Create("gone", "tcp://h:2376", ""); err != nil {
		t.Fatal(err)
	}
	if err := Remove("gone", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if Exists("gone") {
		t.Error("context still exists after Remove")
	}
	// Removing the built-in default is refused.
	if err := Remove("default", true); err == nil {
		t.Error("Remove(default) accepted, want refusal")
	}
	// Removing a missing context errors.
	if err := Remove("missing", false); err == nil {
		t.Error("Remove(missing) accepted, want error")
	}
}

func TestRemoveCurrentNeedsForce(t *testing.T) {
	isolate(t, t.TempDir())
	if err := Create("live", "ssh://h", ""); err != nil {
		t.Fatal(err)
	}
	if err := Use("live"); err != nil {
		t.Fatal(err)
	}
	if err := Remove("live", false); err == nil {
		t.Error("removing the current context without --force was accepted")
	}
	if !Exists("live") {
		t.Fatal("context was removed despite the error")
	}
	if err := Remove("live", true); err != nil {
		t.Fatalf("Remove --force: %v", err)
	}
	if Exists("live") {
		t.Error("context survived a forced remove")
	}
	// The selection must fall back to default.
	if Current() != "default" {
		t.Errorf("Current() = %q after forced remove, want default", Current())
	}
}
