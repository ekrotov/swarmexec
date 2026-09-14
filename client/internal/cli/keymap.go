// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"

	"swarmexec/client/internal/config"
)

// keybinds holds the rune for every configurable TUI shortcut. Structural keys —
// Enter, Esc, Tab, the arrows, the tab-number keys 1-6 and the vim aliases
// j/k/g/G — are fixed and not remappable.
type keybinds struct {
	// global (every tab)
	Quit, Refresh, Copy, ToggleMouse rune
	// containers + volumes
	Search rune
	// containers
	Fold, Unfold, Forward, ContainerInspect, Logs, SecurityRisks, StackGroup rune
	// volumes
	VolSelect, VolSelectAll, VolDelete, VolPrune, VolUsedBy, VolSort, VolSortRev, VolAttach, VolNew rune
	// networks
	NetAttached, NetNew rune
	// nodes
	NodeLabels, NodeAvail, NodeImages rune
	// secrets
	SecDelete, SecNew rune
	// contexts
	CtxUse, CtxNew, CtxDelete rune
	// forwards
	FwdStop, FwdCopyURL rune
}

// keyAction ties a yaml action name to its default rune, the tabs where it is
// active (for conflict detection), and the field it sets. "*" means every tab.
type keyAction struct {
	name  string
	tabs  []string
	def   rune
	field func(*keybinds) *rune
}

var keyActions = []keyAction{
	{"quit", []string{"*"}, 'q', func(k *keybinds) *rune { return &k.Quit }},
	{"refresh", []string{"*"}, 'r', func(k *keybinds) *rune { return &k.Refresh }},
	{"copy", []string{"*"}, 'y', func(k *keybinds) *rune { return &k.Copy }},
	{"toggle_mouse", []string{"*"}, 'm', func(k *keybinds) *rune { return &k.ToggleMouse }},
	{"search", []string{"containers", "volumes"}, '/', func(k *keybinds) *rune { return &k.Search }},
	{"fold", []string{"containers"}, 'h', func(k *keybinds) *rune { return &k.Fold }},
	{"unfold", []string{"containers"}, 'l', func(k *keybinds) *rune { return &k.Unfold }},
	{"forward", []string{"containers"}, 'p', func(k *keybinds) *rune { return &k.Forward }},
	{"container_inspect", []string{"containers"}, 'i', func(k *keybinds) *rune { return &k.ContainerInspect }},
	{"logs", []string{"containers"}, 'L', func(k *keybinds) *rune { return &k.Logs }},
	{"security_risks", []string{"containers"}, '!', func(k *keybinds) *rune { return &k.SecurityRisks }},
	{"stack_group", []string{"containers"}, 's', func(k *keybinds) *rune { return &k.StackGroup }},
	{"volume_select", []string{"volumes"}, ' ', func(k *keybinds) *rune { return &k.VolSelect }},
	{"volume_select_all", []string{"volumes"}, 'a', func(k *keybinds) *rune { return &k.VolSelectAll }},
	{"volume_delete", []string{"volumes"}, 'd', func(k *keybinds) *rune { return &k.VolDelete }},
	{"volume_prune", []string{"volumes"}, 'P', func(k *keybinds) *rune { return &k.VolPrune }},
	{"volume_used_by", []string{"volumes"}, 'i', func(k *keybinds) *rune { return &k.VolUsedBy }},
	{"volume_sort", []string{"volumes"}, 's', func(k *keybinds) *rune { return &k.VolSort }},
	{"volume_sort_reverse", []string{"volumes"}, 'S', func(k *keybinds) *rune { return &k.VolSortRev }},
	{"volume_attach", []string{"volumes"}, 'A', func(k *keybinds) *rune { return &k.VolAttach }},
	{"volume_new", []string{"volumes"}, 'n', func(k *keybinds) *rune { return &k.VolNew }},
	{"network_attached", []string{"networks"}, 'i', func(k *keybinds) *rune { return &k.NetAttached }},
	{"network_new", []string{"networks"}, 'n', func(k *keybinds) *rune { return &k.NetNew }},
	{"node_edit_labels", []string{"nodes"}, 'l', func(k *keybinds) *rune { return &k.NodeLabels }},
	{"node_availability", []string{"nodes"}, 'a', func(k *keybinds) *rune { return &k.NodeAvail }},
	// Capital P, the same gesture the volumes tab uses for its prune — same verb,
	// same shape, different scope.
	{"node_prune_images", []string{"nodes"}, 'P', func(k *keybinds) *rune { return &k.NodeImages }},
	{"secret_delete", []string{"secrets"}, 'd', func(k *keybinds) *rune { return &k.SecDelete }},
	{"secret_new", []string{"secrets"}, 'n', func(k *keybinds) *rune { return &k.SecNew }},
	{"context_use", []string{"contexts"}, 'u', func(k *keybinds) *rune { return &k.CtxUse }},
	{"context_new", []string{"contexts"}, 'n', func(k *keybinds) *rune { return &k.CtxNew }},
	{"context_delete", []string{"contexts"}, 'd', func(k *keybinds) *rune { return &k.CtxDelete }},
	{"forward_stop", []string{"forwards"}, 'd', func(k *keybinds) *rune { return &k.FwdStop }},
	{"forward_copy_url", []string{"forwards"}, 'o', func(k *keybinds) *rune { return &k.FwdCopyURL }},
}

// reservedRunes are the fixed structural keys a configurable binding must not
// steal (the tab-number keys and the vim movement aliases).
// Every digit a tab can occupy is reserved, not just the tabs that exist today:
// tabKeys maps 1-9 to tab positions, so binding an action to a digit would be
// shadowed the moment a tab is added.
var reservedRunes = map[rune]bool{
	'j': true, 'k': true, 'g': true, 'G': true,
	'1': true, '2': true, '3': true, '4': true, '5': true,
	'6': true, '7': true, '8': true, '9': true,
}

// defaultKeybinds returns the built-in bindings.
func defaultKeybinds() keybinds {
	var k keybinds
	for _, a := range keyActions {
		*a.field(&k) = a.def
	}
	return k
}

// defaultKeysPath is keys.yaml next to config.yaml (or $SWARMEXEC_KEYS).
func defaultKeysPath() string {
	if p := os.Getenv("SWARMEXEC_KEYS"); p != "" {
		return p
	}
	cfg := config.DefaultFilePath()
	if cfg == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg), "keys.yaml")
}

// parseKey turns a yaml value into a single rune: "space" -> ' ', otherwise the
// sole rune of the string. Returns false for anything else.
func parseKey(s string) (rune, bool) {
	if s == "space" {
		return ' ', true
	}
	rs := []rune(s)
	if len(rs) != 1 {
		return 0, false
	}
	return rs[0], true
}

// keyLabel renders a rune for help text: ' ' -> "space".
func keyLabel(r rune) string {
	if r == ' ' {
		return "space"
	}
	return string(r)
}

// loadKeybinds returns the bindings (defaults overlaid with keys.yaml) and any
// warnings. It never fails: an unreadable/invalid file, an unknown action, an
// invalid or reserved key, or a same-tab conflict each fall back to the default
// with a warning, so the UI always starts.
func loadKeybinds(path string) (keybinds, []string) {
	k := defaultKeybinds()
	if path == "" {
		path = defaultKeysPath()
	}
	var warnings []string

	raw := map[string]string{}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			if err := yaml.Unmarshal(b, &raw); err != nil {
				return k, []string{fmt.Sprintf("keys.yaml: could not parse (%v) — using defaults", err)}
			}
		}
		// A missing file is fine: defaults, no warning.
	}

	byName := map[string]keyAction{}
	for _, a := range keyActions {
		byName[a.name] = a
	}

	// Apply overrides in a stable order for deterministic warnings.
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		a, ok := byName[name]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("keys.yaml: unknown action %q ignored", name))
			continue
		}
		r, ok := parseKey(raw[name])
		if !ok {
			warnings = append(warnings, fmt.Sprintf("keys.yaml: %s=%q is not a single key; kept default %q", name, raw[name], keyLabel(a.def)))
			continue
		}
		if reservedRunes[r] {
			warnings = append(warnings, fmt.Sprintf("keys.yaml: %s=%q is a reserved key; kept default %q", name, raw[name], keyLabel(a.def)))
			continue
		}
		// Defaults are collision-free per tab, so a conflict can only come from an
		// override. Check against the bindings applied so far; the override loses.
		if other, bad := conflictWith(&k, a, r); bad {
			warnings = append(warnings, fmt.Sprintf("keys.yaml: %s=%q clashes with %q on a shared tab; kept default %q", name, raw[name], other, keyLabel(a.def)))
			continue
		}
		*a.field(&k) = r
	}
	return k, warnings
}

// conflictWith reports whether binding action a to rune r would collide with
// another action that shares a tab with a (global actions share every tab).
func conflictWith(k *keybinds, a keyAction, r rune) (string, bool) {
	for _, other := range keyActions {
		if other.name == a.name {
			continue
		}
		if *other.field(k) == r && sharesTab(a, other) {
			return other.name, true
		}
	}
	return "", false
}

func sharesTab(a, b keyAction) bool {
	for _, ta := range a.tabs {
		for _, tb := range b.tabs {
			if ta == "*" || tb == "*" || ta == tb {
				return true
			}
		}
	}
	return false
}
