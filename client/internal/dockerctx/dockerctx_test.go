package dockerctx

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/client"
)

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
