// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package dockerctx

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// These write the same on-disk layout the `docker` CLI uses, so a context
// created here is readable by `docker` and vice versa:
//
//	<config>/contexts/meta/<sha256(name)>/meta.json   the context definition
//	<config>/config.json  { "currentContext": "<name>" }   the active context
//
// This is what lets an operator manage contexts without the docker CLI at all.

// contextMeta mirrors docker's meta.json. Fields and JSON keys match exactly.
type contextMeta struct {
	Name      string                  `json:"Name"`
	Metadata  map[string]string       `json:"Metadata"`
	Endpoints map[string]endpointMeta `json:"Endpoints"`
}

type endpointMeta struct {
	Host          string `json:"Host"`
	SkipTLSVerify bool   `json:"SkipTLSVerify"`
}

// nameRE matches docker's allowed context names: start alphanumeric, then
// alphanumerics and a few punctuation marks. The name is never used as a path
// component (the directory is its sha256), so this is compatibility, not safety.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.+-]*$`)

// ValidateName rejects names docker would reject, plus the reserved "default".
func ValidateName(name string) error {
	if name == "default" {
		return fmt.Errorf("%q is the reserved built-in context and cannot be created or removed", name)
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid context name %q: must start with a letter or digit and contain only [a-zA-Z0-9_.+-]", name)
	}
	return nil
}

// validHostSchemes are the docker daemon endpoint schemes we accept.
var validHostSchemes = []string{"ssh://", "tcp://", "unix://", "npipe://"}

func validateHost(host string) error {
	for _, s := range validHostSchemes {
		if strings.HasPrefix(host, s) {
			return nil
		}
	}
	return fmt.Errorf("unsupported docker host %q: expected one of %s", host, strings.Join(validHostSchemes, " "))
}

func metaDir(name string) string {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	return filepath.Join(configDir(), "contexts", "meta", digest)
}

// Exists reports whether a stored context (or the built-in "default") exists.
func Exists(name string) bool {
	if name == "default" {
		return true
	}
	_, err := os.Stat(filepath.Join(metaDir(name), "meta.json"))
	return err == nil
}

// ProxyJumpKey is the context-metadata key under which swarmexec stores an
// ssh ProxyJump (comma-separated jump hosts). Docker ignores unknown metadata
// keys, so this rides along in the standard context without breaking `docker`.
const ProxyJumpKey = "swarmexec.ssh.proxyjump"

// CreateOptions describes a context to create. Host must carry a supported
// scheme (ssh/tcp/unix/npipe). ProxyJump (ssh only) is stored so swarmexec can
// inject `-J` into both the Docker-API and agent-tunnel ssh connections.
type CreateOptions struct {
	Name        string
	Host        string
	Description string
	ProxyJump   string
}

// BuildSSHHost assembles an ssh:// docker host from its parts. Only host is
// required; user and port are optional.
func BuildSSHHost(user, host, port string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", fmt.Errorf("ssh host is required")
	}
	var b strings.Builder
	b.WriteString("ssh://")
	if u := strings.TrimSpace(user); u != "" {
		b.WriteString(u + "@")
	}
	b.WriteString(host)
	if p := strings.TrimSpace(port); p != "" {
		b.WriteString(":" + p)
	}
	return b.String(), nil
}

// Create writes a new docker context from o. Description and ProxyJump are
// optional. It errors if the name is invalid, the host scheme is unsupported, or
// a context of that name already exists.
func Create(o CreateOptions) error {
	if err := ValidateName(o.Name); err != nil {
		return err
	}
	if err := validateHost(o.Host); err != nil {
		return err
	}
	if Exists(o.Name) {
		return fmt.Errorf("context %q already exists", o.Name)
	}

	md := map[string]string{}
	if o.Description != "" {
		md["Description"] = o.Description
	}
	if pj := strings.TrimSpace(o.ProxyJump); pj != "" && strings.HasPrefix(o.Host, "ssh://") {
		md[ProxyJumpKey] = pj
	}
	meta := contextMeta{
		Name:      o.Name,
		Metadata:  md,
		Endpoints: map[string]endpointMeta{"docker": {Host: o.Host, SkipTLSVerify: false}},
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode context metadata: %w", err)
	}

	dir := metaDir(o.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create context dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644); err != nil {
		return fmt.Errorf("write context metadata: %w", err)
	}
	return nil
}

// Use sets the active docker context (config.json "currentContext"), preserving
// every other key in config.json. The context must exist.
func Use(name string) error {
	if name != "default" && !Exists(name) {
		return fmt.Errorf("context %q not found", name)
	}
	return setCurrentContext(name)
}

// Remove deletes a stored context. The built-in "default" cannot be removed.
// Removing the active context requires force; on force its currentContext is
// reset to "default".
func Remove(name string, force bool) error {
	if name == "default" {
		return fmt.Errorf("cannot remove the built-in %q context", name)
	}
	if !Exists(name) {
		return fmt.Errorf("context %q not found", name)
	}
	if currentContextName() == name {
		if !force {
			return fmt.Errorf("context %q is in use; pass --force to remove it (its selection resets to default)", name)
		}
		if err := setCurrentContext("default"); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(metaDir(name)); err != nil {
		return fmt.Errorf("remove context: %w", err)
	}
	// Best-effort: drop any TLS material docker may have stored for it.
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	_ = os.RemoveAll(filepath.Join(configDir(), "contexts", "tls", digest))
	return nil
}

// setCurrentContext rewrites config.json's currentContext key while leaving all
// other keys byte-for-byte intact (they may hold credentials).
func setCurrentContext(name string) error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create docker config dir: %w", err)
	}
	path := filepath.Join(dir, "config.json")

	fields := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &fields); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	cur, _ := json.Marshal(name)
	fields["currentContext"] = cur
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return fmt.Errorf("encode docker config: %w", err)
	}
	// config.json may hold registry credentials, so keep it private.
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
