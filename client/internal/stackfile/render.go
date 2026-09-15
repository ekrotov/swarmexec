// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// YAML renders the stack as a compose file.
//
// Determinism is the requirement, not beauty: this output is one side of a
// diff, so the same stack must render byte-identically every time. Every map is
// emitted through an explicitly ordered node and every list is sorted upstream,
// because Go map iteration order is random and would otherwise turn a diff into
// noise that changes on each run.
func (s *Stack) YAML() (string, error) { return s.render(false) }

func (s *Stack) render(escapeDollars bool) (string, error) {
	root := mapping()
	put(root, "services", mapOf(s.Services, func(v *Service) (*yaml.Node, error) { return node(v) }))
	if len(s.Networks) > 0 {
		put(root, "networks", mapOf(s.Networks, func(v *Network) (*yaml.Node, error) { return node(v) }))
	}
	if len(s.Volumes) > 0 {
		put(root, "volumes", mapOf(s.Volumes, func(v *Resource) (*yaml.Node, error) { return node(v) }))
	}
	if len(s.Secrets) > 0 {
		put(root, "secrets", mapOf(s.Secrets, func(v *Resource) (*yaml.Node, error) { return node(v) }))
	}
	if len(s.Configs) > 0 {
		put(root, "configs", mapOf(s.Configs, func(v *Resource) (*yaml.Node, error) { return node(v) }))
	}

	if escapeDollars {
		escapeInterpolation(root, false)
	}

	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Document is the stack as a file, with the notes as a header comment.
//
// Unlike YAML it escapes "$" as "$$". Compose interpolates "$VAR" and "$(…)"
// when a file is LOADED, so a value carrying a literal dollar — an entrypoint
// that does `$(cat /run/secrets/…)`, say — comes back either mangled or as a
// hard error. An export that cannot be read back is not an export, and this was
// found by feeding a real exported stack straight back into the diff.
//
// The escaping belongs here and NOT in YAML(): the diff renders both sides
// through YAML(), and a file the operator wrote has already been interpolated
// by the loader, so escaping there would compare "$$" against "$". The
// notes ride in the file itself rather than only on the terminal, because the
// file is what gets kept, mailed and committed — and what it does not contain
// has to travel with it.
func (s *Stack) Document() (string, error) {
	body, err := s.render(true)
	if err != nil {
		return "", err
	}
	var h strings.Builder
	fmt.Fprintf(&h, "# stack: %s\n", s.Name)
	fmt.Fprintf(&h, "# exported by swarmexec from the deployed services\n")
	for _, n := range s.Notes {
		for _, line := range wrapComment(n, 76) {
			fmt.Fprintf(&h, "# %s\n", line)
		}
	}
	h.WriteString("\n")
	return h.String() + body, nil
}

func wrapComment(s string, width int) []string {
	words, line, out := strings.Fields(s), "", []string(nil)
	for _, w := range words {
		switch {
		case line == "":
			line = w
		case len(line)+1+len(w) <= width:
			line += " " + w
		default:
			out = append(out, line)
			line = w
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

func mapping() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

func scalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func put(m *yaml.Node, key string, val *yaml.Node) {
	if val == nil {
		return
	}
	m.Content = append(m.Content, scalar(key), val)
}

// mapOf renders a map in sorted key order. An entry that marshals to nothing
// becomes an empty mapping rather than being dropped: "this network exists and
// says nothing about itself" is a fact, and dropping it would hide a network
// that one side has and the other does not.
func mapOf[V any](m map[string]V, enc func(V) (*yaml.Node, error)) *yaml.Node {
	out := mapping()
	for _, k := range sortedKeys(m) {
		n, err := enc(m[k])
		if err != nil || n == nil || len(n.Content) == 0 {
			n = mapping()
		}
		out.Content = append(out.Content, scalar(k), n)
	}
	return out
}

// node encodes a value. yaml.v3 emits struct fields in declaration order and
// map keys in sorted order, which is exactly what a diff needs: the deliberate
// field order of Service survives, and an environment block cannot shuffle
// between two renderings of the same service. Verified rather than assumed.
func node(v any) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	return &n, nil
}

// escapeInterpolation doubles every "$" in a scalar VALUE, which is how compose
// spells a literal dollar. Keys are left alone: compose does not interpolate
// them, and doubling a dollar in an environment variable's NAME would rename it.
//
// isKey threads through the walk because a mapping's children alternate key,
// value, key, value — there is no other way to tell them apart in a yaml.Node.
func escapeInterpolation(n *yaml.Node, isKey bool) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if !isKey && n.Tag == "!!str" {
			n.Value = strings.ReplaceAll(n.Value, "$", "$$")
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			escapeInterpolation(n.Content[i], true)
			escapeInterpolation(n.Content[i+1], false)
		}
	default:
		for _, c := range n.Content {
			escapeInterpolation(c, false)
		}
	}
}
