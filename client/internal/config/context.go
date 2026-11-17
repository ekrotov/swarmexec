// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Keys whose provenance is tracked, as `config show` names them. The two
// secret keys share one entry because they are one setting.
const (
	KeyCA          = "ca"
	KeyCert        = "cert"
	KeyKey         = "key"
	KeyPort        = "port"
	KeyAddrMode    = "addr_mode"
	KeyServerName  = "server_name"
	KeyAgentSecret = "agent_secret"
	KeyInsecure    = "insecure"
)

var trackedKeys = []string{KeyCA, KeyCert, KeyKey, KeyPort, KeyAddrMode, KeyServerName, KeyAgentSecret, KeyInsecure}

// Provenance labels.
const (
	SourceDefault = "default"
	SourceFile    = "config file"
)

// SourceEnv labels a value taken from environment variable name.
func SourceEnv(name string) string { return "env " + name }

// envNames maps a tracked key to the variables that can set it.
var envNames = map[string][]string{
	KeyCA:          {"SWARMEXEC_CA"},
	KeyCert:        {"SWARMEXEC_CERT"},
	KeyKey:         {"SWARMEXEC_KEY"},
	KeyPort:        {"SWARMEXEC_PORT"},
	KeyAddrMode:    {"SWARMEXEC_ADDR_MODE"},
	KeyServerName:  {"SWARMEXEC_SERVER_NAME"},
	KeyAgentSecret: {"SWARMEXEC_AGENT_SECRET", "SWARMEXEC_AGENT_SECRET_FILE"},
	KeyInsecure:    {"SWARMEXEC_INSECURE"},
}

// loadSources works out, for a freshly loaded config, which tracked keys came
// from the file and which from the environment. Anything else is a default.
func loadSources(path string) map[string]string {
	src := make(map[string]string, len(trackedKeys))
	for _, k := range trackedKeys {
		src[k] = SourceDefault
	}
	if path == "" {
		path = DefaultFilePath()
	}
	if b, err := os.ReadFile(path); err == nil {
		var top map[string]any
		if yaml.Unmarshal(b, &top) == nil {
			for _, k := range trackedKeys {
				_, ok := top[k]
				if k == KeyAgentSecret {
					_, f := top["agent_secret_file"]
					ok = ok || f
				}
				if ok {
					src[k] = SourceFile
				}
			}
		}
	}
	for _, k := range trackedKeys {
		for _, name := range envNames[k] {
			if os.Getenv(name) != "" {
				src[k] = SourceEnv(name)
			}
		}
	}
	return src
}

// Source says where the effective value of a tracked key came from: "default",
// "config file", "contexts.<name>.<key>", "env NAME" or a flag. A hand-built
// Config reports "default".
func (c Config) Source(key string) string {
	if s, ok := c.src[key]; ok {
		return s
	}
	return SourceDefault
}

// SetSource records that key now holds a value from src (the cli uses it for
// flags, which it applies after Load).
func (c *Config) SetSource(key, src string) {
	m := make(map[string]string, len(c.src)+1)
	for k, v := range c.src {
		m[k] = v
	}
	m[key] = src
	c.src = m
}

func fromEnv(src string) bool { return strings.HasPrefix(src, "env ") }

// ForContext returns the config in effect for one Docker context.
//
// Precedence, highest first (the cli applies flags above all of this):
//
//	environment variable > contexts.<name>.<key> > global <key> in the file > default
//
// The environment stays on top because it always was: a user who exports
// SWARMEXEC_AGENT_SECRET gets exactly that for every cluster, as before. A
// context key only ever fills in where the environment is silent, and a
// context that sets no credential key sees the global values unchanged — so a
// config written before per-context credentials existed resolves identically.
//
// The secret is one setting spelled two ways: a context's agent_secret or
// agent_secret_file replaces BOTH global spellings, so a context secret is
// never shadowed by a global agent_secret_file.
func (c Config) ForContext(name string) Config {
	cc, ok := c.Contexts[name]
	if !ok {
		return c
	}
	out := c
	out.src = make(map[string]string, len(c.src))
	for k, v := range c.src {
		out.src[k] = v
	}
	set := func(key string, apply func()) {
		if fromEnv(out.Source(key)) {
			return
		}
		apply()
		out.src[key] = "contexts." + name + "." + key
	}
	if cc.CA != "" {
		set(KeyCA, func() { out.CA = cc.CA })
	}
	if cc.Cert != "" {
		set(KeyCert, func() { out.Cert = cc.Cert })
	}
	if cc.Key != "" {
		set(KeyKey, func() { out.Key = cc.Key })
	}
	if cc.Port != 0 {
		set(KeyPort, func() { out.Port = cc.Port })
	}
	if cc.AddrMode != "" {
		set(KeyAddrMode, func() { out.AddrMode = cc.AddrMode })
	}
	if cc.ServerName != "" {
		set(KeyServerName, func() { out.ServerName = cc.ServerName })
	}
	if cc.Insecure != nil {
		set(KeyInsecure, func() { out.Insecure = *cc.Insecure })
	}
	if cc.AgentSecret != "" || cc.AgentSecretFile != "" {
		set(KeyAgentSecret, func() {
			out.AgentSecret, out.AgentSecretFile = cc.AgentSecret, cc.AgentSecretFile
		})
		if cc.AgentSecretFile != "" && !fromEnv(c.Source(KeyAgentSecret)) {
			out.src[KeyAgentSecret] = "contexts." + name + ".agent_secret_file"
		}
	}
	return out
}

// ContextKeys are the values `init` persists for the context it deployed to.
type ContextKeys struct {
	AgentSecret string
	Insecure    bool
	AddrMode    string
	Port        int
}

// SetContextKeys writes ONLY the given keys under contexts.<name> in the
// config file at path and leaves everything else as it was — other contexts,
// global keys, comments, unknown keys. It edits the YAML tree rather than
// marshalling a Config, because a Config is defaults + file + environment and
// marshalling it persists all three (an exported SWARMEXEC_CA ended up in the
// file). A file that does not parse is an error and is not touched. The write
// is atomic and 0600.
//
// A context's agent_secret_file is dropped when agent_secret is written: the
// file would win and silently shadow the secret just stored.
func SetContextKeys(path, name string, k ContextKeys) (string, error) {
	if path == "" {
		path = DefaultFilePath()
	}
	if path == "" {
		return "", fmt.Errorf("cannot determine config file path")
	}
	if name == "" {
		return "", fmt.Errorf("no context name to store credentials under")
	}
	var root yaml.Node
	switch data, err := os.ReadFile(path); {
	case err == nil:
		if err := yaml.Unmarshal(data, &root); err != nil {
			return "", fmt.Errorf("parse config file %s: %w (not modified)", path, err)
		}
	case !os.IsNotExist(err):
		return "", fmt.Errorf("read config file %s: %w", path, err)
	}
	if root.Kind == 0 {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return "", fmt.Errorf("config file %s: top level is not a mapping (not modified)", path)
	}
	block := mapChild(mapChild(top, "contexts"), name)
	if k.AgentSecret != "" {
		setScalar(block, "agent_secret", k.AgentSecret, "!!str")
		dropKey(block, "agent_secret_file")
	}
	setScalar(block, "insecure", fmt.Sprint(k.Insecure), "!!bool")
	if k.AddrMode != "" {
		setScalar(block, "addr_mode", k.AddrMode, "!!str")
	}
	if k.Port != 0 {
		setScalar(block, "port", fmt.Sprint(k.Port), "!!int")
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, out); err != nil {
		return "", err
	}
	return path, nil
}

// mapChild returns the mapping stored under key in m, creating it (or
// replacing a non-mapping, e.g. an empty `contexts:`) when missing.
func mapChild(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			if v := m.Content[i+1]; v.Kind == yaml.MappingNode {
				return v
			}
			child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			m.Content[i+1] = child
			return child
		}
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, child)
	return child
}

func setScalar(m *yaml.Node, key, value, tag string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
}

func dropKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
