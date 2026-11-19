// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package hostlink

import (
	"os"
	"path/filepath"
	"testing"
)

func fakeSysfs(t *testing.T, links map[string]map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, files := range links {
		dir := filepath.Join(root, "class", "net", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f, v := range files {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func TestReadReportsWhatSysfsSays(t *testing.T) {
	root := fakeSysfs(t, map[string]map[string]string{
		"enp7s0": {"mtu": "1450", "operstate": "up", "type": "1"},
		"eth0":   {"mtu": "1500", "operstate": "up", "type": "1"},
		"lo":     {"mtu": "65536", "operstate": "unknown", "type": "772"},
	})
	got := Read(root)
	if len(got) != 3 {
		t.Fatalf("got %d links, want 3: %+v", len(got), got)
	}
	// Sorted, so a report reads the same every time.
	if got[0].Name != "enp7s0" || got[1].Name != "eth0" || got[2].Name != "lo" {
		t.Errorf("not sorted by name: %+v", got)
	}
	if got[0].MTU != 1450 || got[0].Operstate != "up" || got[0].ARPType != 1 {
		t.Errorf("enp7s0 = %+v", got[0])
	}
	if got[2].ARPType != 772 {
		t.Errorf("loopback type not carried through: %+v", got[2])
	}
}

// The mount is optional. A node without it must report nothing rather than
// something invented — "unknown" is a fact the client already handles, a
// guessed 1500 is the very number that causes the problem.
func TestReadWithoutTheMountIsQuiet(t *testing.T) {
	for _, root := range []string{"", "/nonexistent-sysfs-root", t.TempDir()} {
		if got := Read(root); len(got) != 0 {
			t.Errorf("Read(%q) = %+v, want nothing", root, got)
		}
	}
}

// An entry without a readable MTU is left out: a zero would be read as a
// measurement by whoever gets it.
func TestReadSkipsLinksWithoutAnMTU(t *testing.T) {
	root := fakeSysfs(t, map[string]map[string]string{
		"good":     {"mtu": "1400", "type": "1"},
		"dangling": {"operstate": "down"},
	})
	got := Read(root)
	if len(got) != 1 || got[0].Name != "good" {
		t.Errorf("got %+v, want only the link with an MTU", got)
	}
}
