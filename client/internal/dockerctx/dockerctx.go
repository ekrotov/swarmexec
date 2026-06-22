// Package dockerctx resolves the Docker daemon host the operator's machine
// should talk to for swarm discovery, honoring Docker CLI contexts (including
// ssh:// endpoints) the same way the `docker` CLI does — something the bare
// Docker SDK's client.FromEnv does not do.
package dockerctx

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/docker/client"
)

// ResolveHost returns the Docker daemon URL to dial. Precedence (matching the
// docker CLI): an explicit context override (--context) or $DOCKER_CONTEXT wins;
// otherwise $DOCKER_HOST; otherwise the config's current context; otherwise the
// platform default socket.
func ResolveHost(override string) (string, error) {
	name := override
	if name == "" {
		name = os.Getenv("DOCKER_CONTEXT")
	}
	if name == "" {
		// No explicit context selected: DOCKER_HOST takes over, else fall back
		// to whatever context the config has marked current.
		if h := os.Getenv("DOCKER_HOST"); h != "" {
			return h, nil
		}
		name = currentContextName()
	}
	if name == "" || name == "default" {
		if h := os.Getenv("DOCKER_HOST"); h != "" {
			return h, nil
		}
		return client.DefaultDockerHost, nil
	}
	return contextHost(name)
}

// configDir is the Docker config directory ($DOCKER_CONFIG or ~/.docker).
func configDir() string {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".docker"
	}
	return filepath.Join(home, ".docker")
}

// currentContextName reads "currentContext" from config.json (or "" if unset).
func currentContextName() string {
	b, err := os.ReadFile(filepath.Join(configDir(), "config.json"))
	if err != nil {
		return ""
	}
	var c struct {
		CurrentContext string `json:"currentContext"`
	}
	_ = json.Unmarshal(b, &c)
	return c.CurrentContext
}

// contextHost reads the docker endpoint host from a context's stored metadata.
func contextHost(name string) (string, error) {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	path := filepath.Join(configDir(), "contexts", "meta", digest, "meta.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("docker context %q not found (%s): %w", name, path, err)
	}
	var meta struct {
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return "", fmt.Errorf("parse docker context %q metadata: %w", name, err)
	}
	ep, ok := meta.Endpoints["docker"]
	if !ok || ep.Host == "" {
		return "", fmt.Errorf("docker context %q has no docker endpoint", name)
	}
	return ep.Host, nil
}
