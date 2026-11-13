// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package dockerctx

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"swarmexec/client/internal/clientlog"
)

func TestList(t *testing.T) {
	dir := t.TempDir()
	isolate(t, dir)
	writeContext(t, dir, "pk", "ssh://root@docker1")
	writeContext(t, dir, "staging", "ssh://root@staging")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"pk"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cs, err := List()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cs {
		names = append(names, c.Name)
		if c.Name == "pk" && (!c.Current || c.Host != "ssh://root@docker1") {
			t.Errorf("pk context wrong: %+v", c)
		}
	}
	want := []string{"default", "pk", "staging"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	if Current() != "pk" {
		t.Errorf("Current = %q, want pk", Current())
	}
}

func TestList_OnlyDefault(t *testing.T) {
	dir := t.TempDir()
	isolate(t, dir)
	cs, _ := List()
	if len(cs) != 1 || cs[0].Name != "default" || !cs[0].Current {
		t.Fatalf("expected a single current default context, got %+v", cs)
	}
	if Current() != "default" {
		t.Errorf("Current = %q, want default", Current())
	}
}

func writeContext(t *testing.T, dir, name, host string) {
	t.Helper()
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	metaDir := filepath.Join(dir, "contexts", "meta", digest)
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"Name":%q,"Endpoints":{"docker":{"Host":%q}}}`, name, host)
	if err := os.WriteFile(filepath.Join(metaDir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

// isolate clears the relevant env so tests are deterministic.
func isolate(t *testing.T, dir string) {
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
}

func TestResolveHost_ContextOverride(t *testing.T) {
	dir := t.TempDir()
	isolate(t, dir)
	writeContext(t, dir, "pk", "ssh://root@docker1")
	got, err := ResolveHost("pk")
	if err != nil || got != "ssh://root@docker1" {
		t.Fatalf("got %q, err %v", got, err)
	}
}

func TestResolveHost_CurrentContextFromConfig(t *testing.T) {
	dir := t.TempDir()
	isolate(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"pk"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeContext(t, dir, "pk", "ssh://root@docker1")
	got, err := ResolveHost("")
	if err != nil || got != "ssh://root@docker1" {
		t.Fatalf("got %q, err %v", got, err)
	}
}

func TestResolveHost_DockerHostWinsWithoutExplicitContext(t *testing.T) {
	dir := t.TempDir()
	isolate(t, dir)
	_ = os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"pk"}`), 0o644)
	writeContext(t, dir, "pk", "ssh://root@docker1")
	t.Setenv("DOCKER_HOST", "tcp://1.2.3.4:2376")
	got, _ := ResolveHost("")
	if got != "tcp://1.2.3.4:2376" {
		t.Fatalf("DOCKER_HOST should win without an explicit context, got %q", got)
	}
}

func TestResolveHost_ExplicitContextBeatsDockerHost(t *testing.T) {
	dir := t.TempDir()
	isolate(t, dir)
	writeContext(t, dir, "pk", "ssh://root@docker1")
	t.Setenv("DOCKER_HOST", "tcp://1.2.3.4:2376")
	got, _ := ResolveHost("pk")
	if got != "ssh://root@docker1" {
		t.Fatalf("explicit --context should win over DOCKER_HOST, got %q", got)
	}
}

func TestResolveHost_Default(t *testing.T) {
	dir := t.TempDir()
	isolate(t, dir)
	got, err := ResolveHost("default")
	if err != nil || got != client.DefaultDockerHost {
		t.Fatalf("got %q, err %v, want %q", got, err, client.DefaultDockerHost)
	}
}

func TestResolveHost_MissingContext(t *testing.T) {
	dir := t.TempDir()
	isolate(t, dir)
	if _, err := ResolveHost("does-not-exist"); err == nil {
		t.Fatal("expected error for missing context")
	}
}

// ResolveName must name the same context ResolveHost dials, for every rung of
// the precedence ladder. The rung that broke: DOCKER_HOST set, no context named
// — the name was the stored current context (production) while the host was
// DOCKER_HOST.
func TestResolveName_MatchesResolveHost(t *testing.T) {
	const local = "unix:///tmp/test-docker.sock"
	cases := []struct {
		name                 string
		override, envCtx, dh string
		want, wantHost       string
	}{
		{"stored current", "", "", "", "pk", "ssh://root@docker1"},
		{"DOCKER_HOST beats stored current", "", "", local, "default", local},
		{"DOCKER_CONTEXT beats DOCKER_HOST", "", "other", local, "other", "ssh://root@docker2"},
		{"--context beats everything", "other", "pk", local, "other", "ssh://root@docker2"},
		{"explicit default is DOCKER_HOST", "default", "", local, "default", local},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			isolate(t, dir)
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"pk"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			writeContext(t, dir, "pk", "ssh://root@docker1")
			writeContext(t, dir, "other", "ssh://root@docker2")
			t.Setenv("DOCKER_CONTEXT", c.envCtx)
			t.Setenv("DOCKER_HOST", c.dh)
			if got := ResolveName(c.override); got != c.want {
				t.Errorf("ResolveName(%q) = %q, want %q", c.override, got, c.want)
			}
			host, err := ResolveHost(c.override)
			if err != nil || host != c.wantHost {
				t.Errorf("ResolveHost(%q) = %q, %v; want %q", c.override, host, err, c.wantHost)
			}
			// Current is the stored selection and stays so: List marks it, and
			// Remove's in-use check relies on it.
			if c.envCtx == "" && Current() != "pk" {
				t.Errorf("Current() = %q, want the stored pk", Current())
			}
		})
	}
}

// A corrupt meta.json still drops that context from the list (as docker does),
// but leaves a warning in the client log instead of vanishing silently.
func TestList_CorruptMetaIsWarned(t *testing.T) {
	ring, err := clientlog.Init("warn", "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	isolate(t, dir)
	writeContext(t, dir, "pk", "ssh://root@docker1")
	bad := filepath.Join(dir, "contexts", "meta", "broken")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "meta.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	cs, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("contexts = %+v, want default + pk", cs)
	}
	logged := strings.Join(ring.Lines(), "\n")
	if !strings.Contains(logged, "docker context metadata unreadable") || !strings.Contains(logged, bad) {
		t.Errorf("no warning for the corrupt context; log = %q", logged)
	}
}
