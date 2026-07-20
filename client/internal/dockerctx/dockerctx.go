// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

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
	"sort"

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

// Context is a Docker CLI context the cli can target.
type Context struct {
	Name    string
	Host    string
	Current bool // whether this is the active context
}

// Current returns the active context name ($DOCKER_CONTEXT, else the config's
// current context, else "default").
func Current() string {
	if n := os.Getenv("DOCKER_CONTEXT"); n != "" {
		return n
	}
	if n := currentContextName(); n != "" {
		return n
	}
	return "default"
}

// List returns all available Docker contexts (the built-in "default" plus any
// stored under the config dir), marking the active one.
func List() ([]Context, error) {
	current := Current()

	defHost := os.Getenv("DOCKER_HOST")
	if defHost == "" {
		defHost = client.DefaultDockerHost
	}
	out := []Context{{Name: "default", Host: defHost, Current: current == "default"}}

	entries, err := os.ReadDir(filepath.Join(configDir(), "contexts", "meta"))
	if err != nil {
		return out, nil // no stored contexts is fine
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(configDir(), "contexts", "meta", e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var meta struct {
			Name      string `json:"Name"`
			Endpoints map[string]struct {
				Host string `json:"Host"`
			} `json:"Endpoints"`
		}
		if json.Unmarshal(b, &meta) != nil || meta.Name == "" {
			continue
		}
		out = append(out, Context{
			Name:    meta.Name,
			Host:    meta.Endpoints["docker"].Host,
			Current: meta.Name == current,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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
