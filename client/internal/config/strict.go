// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// unknownKey is a key in the config file that no setting is called.
type unknownKey struct {
	path    string   // dotted path as the user wrote it, e.g. contexts.prod.confirmSwitch
	suggest []string // real keys at that level it is a near miss of (empty = not close to any)
}

// findUnknownKeys walks the parsed YAML against the Config struct (by its
// yaml tags) and reports every mapping key that is not a field. Context names
// under `contexts:` are free-form and are not checked, their contents are.
func findUnknownKeys(root *yaml.Node) []unknownKey {
	if root == nil || len(root.Content) == 0 {
		return nil
	}
	var out []unknownKey
	walkKeys(root.Content[0], reflect.TypeOf(Config{}), "", &out)
	return out
}

func walkKeys(n *yaml.Node, t reflect.Type, path string, out *[]unknownKey) {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if n.Kind != yaml.MappingNode {
		return
	}
	join := func(k string) string {
		if path == "" {
			return k
		}
		return path + "." + k
	}
	switch t.Kind() {
	case reflect.Map:
		for i := 0; i+1 < len(n.Content); i += 2 {
			walkKeys(n.Content[i+1], t.Elem(), join(n.Content[i].Value), out)
		}
	case reflect.Struct:
		fields := yamlFields(t)
		names := make([]string, 0, len(fields))
		for k := range fields {
			names = append(names, k)
		}
		sort.Strings(names)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			if ft, ok := fields[k]; ok {
				walkKeys(n.Content[i+1], ft, join(k), out)
				continue
			}
			*out = append(*out, unknownKey{path: join(k), suggest: nearKeys(k, names)})
		}
	}
}

// yamlFields maps the yaml key of each exported, non-skipped field to its type.
func yamlFields(t reflect.Type) map[string]reflect.Type {
	m := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "-" || !f.IsExported() {
			continue
		}
		if tag == "" {
			tag = strings.ToLower(f.Name)
		}
		m[tag] = f.Type
	}
	return m
}

// normKey folds the spellings people mistake for each other: case, "_" and "-".
func normKey(s string) string {
	return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(s))
}

// nearKeys returns the real keys that key is a near miss of: the same word
// spelled differently (confirmSwitch, confirm-switch) or within a small edit
// distance (agent_secrt). Short keys get a tighter bound so "ca" does not
// match every two-letter word.
func nearKeys(key string, real []string) []string {
	nk := normKey(key)
	var out []string
	for _, r := range real {
		nr := normKey(r)
		bound := 2
		if len(nr) <= 4 {
			bound = 1
		}
		if nk == nr || editDistance(nk, nr) <= bound {
			out = append(out, r)
		}
	}
	return out
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			c := 1
			if ra[i-1] == rb[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// checkUnknownKeys applies the policy for keys nobody recognises.
//
// A key that is a near miss of a real one is almost certainly a typo, and a
// typo here is not harmless: `confirmSwitch: true` that does nothing leaves the
// production-cluster prompt off while the file looks as if it were on. Those
// are an error naming the key and the one it probably meant. A key that is
// nothing like any setting is far more likely a newer version's setting (or
// another tool's) in a file shared across machines; refusing it would make an
// older client unusable, so it is reported once as a warning and ignored.
func checkUnknownKeys(path string, root *yaml.Node) (warnings []string, err error) {
	var typos []string
	for _, u := range findUnknownKeys(root) {
		if len(u.suggest) > 0 {
			prefix := ""
			if i := strings.LastIndex(u.path, "."); i >= 0 {
				prefix = u.path[:i+1]
			}
			var opts []string
			for _, s := range u.suggest {
				opts = append(opts, prefix+s)
			}
			typos = append(typos, fmt.Sprintf("unknown key %q (did you mean %s?)", u.path, strings.Join(opts, " or ")))
			continue
		}
		warnings = append(warnings, fmt.Sprintf("config file %s: unknown key %q ignored (not a setting of this version)", path, u.path))
	}
	if len(typos) > 0 {
		return warnings, fmt.Errorf("config file %s: %s", path, strings.Join(typos, "; "))
	}
	return warnings, nil
}
