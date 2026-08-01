// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKeys(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "keys.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadKeybinds_DefaultsWhenNoFile(t *testing.T) {
	km, warns := loadKeybinds(filepath.Join(t.TempDir(), "nope.yaml"))
	if len(warns) != 0 {
		t.Errorf("missing file should not warn, got %v", warns)
	}
	if km != defaultKeybinds() {
		t.Errorf("expected defaults, got %+v", km)
	}
}

func TestLoadKeybinds_ValidOverride(t *testing.T) {
	km, warns := loadKeybinds(writeKeys(t, "quit: x\nvolume_delete: X\nvolume_select: space\n"))
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if km.Quit != 'x' {
		t.Errorf("quit = %q, want x", km.Quit)
	}
	if km.VolDelete != 'X' {
		t.Errorf("volume_delete = %q, want X", km.VolDelete)
	}
	if km.VolSelect != ' ' {
		t.Errorf("volume_select = %q, want space", km.VolSelect)
	}
}

func TestLoadKeybinds_InvalidAndReservedKeepDefault(t *testing.T) {
	km, warns := loadKeybinds(writeKeys(t, "quit: xy\nrefresh: j\n"))
	if km.Quit != 'q' || km.Refresh != 'r' {
		t.Errorf("invalid/reserved should keep defaults, got quit=%q refresh=%q", km.Quit, km.Refresh)
	}
	if len(warns) != 2 {
		t.Errorf("want 2 warnings (invalid + reserved), got %d: %v", len(warns), warns)
	}
}

func TestLoadKeybinds_UnknownActionWarns(t *testing.T) {
	_, warns := loadKeybinds(writeKeys(t, "frobnicate: z\n"))
	if len(warns) != 1 || !strings.Contains(warns[0], "unknown action") {
		t.Errorf("want one unknown-action warning, got %v", warns)
	}
}

func TestLoadKeybinds_ConflictKeepsDefault(t *testing.T) {
	// volume_delete=i collides with volume_used_by (default i) on the volumes tab.
	km, warns := loadKeybinds(writeKeys(t, "volume_delete: i\n"))
	if km.VolDelete != 'd' {
		t.Errorf("conflicting override should keep default d, got %q", km.VolDelete)
	}
	if km.VolUsedBy != 'i' {
		t.Errorf("volume_used_by should stay i, got %q", km.VolUsedBy)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "clashes") {
		t.Errorf("want one clash warning, got %v", warns)
	}
}

func TestLoadKeybinds_GlobalConflictAcrossTabs(t *testing.T) {
	// quit is global; setting it to d collides with volume_delete on the volumes tab.
	km, warns := loadKeybinds(writeKeys(t, "quit: d\n"))
	if km.Quit != 'q' {
		t.Errorf("global override clashing with a tab action should keep default, got %q", km.Quit)
	}
	if len(warns) != 1 {
		t.Errorf("want one clash warning, got %v", warns)
	}
}

// The built-in defaults must be collision-free within every tab, or the UI's
// per-tab switch would silently shadow a binding.
func TestDefaultKeybinds_NoPerTabCollision(t *testing.T) {
	k := defaultKeybinds()
	tabs := []string{"containers", "volumes", "networks", "secrets", "contexts", "forwards"}
	for _, tab := range tabs {
		seen := map[rune]string{}
		for _, a := range keyActions {
			if !sharesTab(a, keyAction{tabs: []string{tab}}) {
				continue
			}
			r := *a.field(&k)
			if prev, ok := seen[r]; ok {
				t.Errorf("tab %s: default %q and %q both bound to %q", tab, a.name, prev, keyLabel(r))
			}
			seen[r] = a.name
		}
	}
}

func TestParseKeyAndLabel(t *testing.T) {
	if r, ok := parseKey("space"); !ok || r != ' ' {
		t.Errorf("parseKey(space) = %q,%v", r, ok)
	}
	if r, ok := parseKey("/"); !ok || r != '/' {
		t.Errorf("parseKey(/) = %q,%v", r, ok)
	}
	if _, ok := parseKey("ab"); ok {
		t.Error("parseKey(ab) should fail")
	}
	if _, ok := parseKey(""); ok {
		t.Error("parseKey(empty) should fail")
	}
	if keyLabel(' ') != "space" || keyLabel('q') != "q" {
		t.Error("keyLabel wrong")
	}
}
