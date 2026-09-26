// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"regexp"
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

// quit and copy apply inside dialogs, so they may not take a key a dialog
// answers itself — with quit=t, inspect's view toggle would have won and the
// remap would have done nothing there.
func TestLoadKeybinds_DialogKeysAreRefusedForQuitAndCopy(t *testing.T) {
	km, warns := loadKeybinds(writeKeys(t, "quit: t\ncopy: i\n"))
	if km.Quit != 'q' || km.Copy != 'y' {
		t.Errorf("dialog keys must keep the defaults, got quit=%q copy=%q", km.Quit, km.Copy)
	}
	if len(warns) != 2 || !strings.Contains(warns[0], "dialogs use themselves") {
		t.Errorf("want two dialog-key warnings, got %v", warns)
	}

	km, warns = loadKeybinds(writeKeys(t, "quit: x\ncopy: c\ncontext_focus: C\n"))
	if km.Quit != 'x' || km.Copy != 'c' || len(warns) != 0 {
		t.Errorf("free keys must be accepted, got quit=%q copy=%q warnings=%v", km.Quit, km.Copy, warns)
	}
}

var runeLiteral = regexp.MustCompile(`ev\.Rune\(\) == '(.)'|case '(.)'|ev\.Rune\(\) >= '(.)' && ev\.Rune\(\) <= '(.)'`)

// dialogRunes is a claim about the source — "these are the keys the UI
// handles itself" — so it is checked against the source. A fixed key added to
// a dialog without being listed would let quit or copy be remapped onto it;
// a listed key nothing uses any more only narrows the choice for nothing.
func TestDialogRunesMatchTheSource(t *testing.T) {
	files, _ := filepath.Glob("ui*.go")
	used := map[rune]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range runeLiteral.FindAllStringSubmatch(string(b), -1) {
			if m[3] != "" { // a range such as '1'..'9'
				for r := []rune(m[3])[0]; r <= []rune(m[4])[0]; r++ {
					used[r] = true
				}
				continue
			}
			used[[]rune(m[1] + m[2])[0]] = true
		}
	}
	for r := range used {
		if r == 'q' || r == 'y' {
			t.Errorf("a dialog checks %q literally; use u.km.Quit / u.km.Copy so a remap reaches it", r)
			continue
		}
		if !dialogRunes[r] && !reservedRunes[r] {
			t.Errorf("fixed key %q is handled in the UI but missing from dialogRunes", r)
		}
	}
	for r := range dialogRunes {
		if !used[r] {
			t.Errorf("dialogRunes lists %q, which no dialog handles any more", r)
		}
	}
}
