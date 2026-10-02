// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package docsi18n

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Cutting and joining must be lossless on the real pages — the translations
// are rebuilt from their sections, so a lost byte here is a lost byte on the
// site.
func TestSplitJoinIsLosslessOnTheRealPages(t *testing.T) {
	pages, _ := filepath.Glob("../../site/docs*.html")
	if len(pages) != 5 {
		t.Fatalf("found %d docs pages, want 5", len(pages))
	}
	for _, p := range pages {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		secs := Split(string(b))
		if Join(secs) != string(b) {
			t.Errorf("%s: split/join changed the page", p)
		}
		if secs[0].ID != HeadID || len(secs) < 10 {
			t.Errorf("%s: %d sections, first %q", p, len(secs), secs[0].ID)
		}
	}
}

func TestStale(t *testing.T) {
	en := Split(`<p>head</p><h2 id="a">A</h2>one<h2 id="b">B</h2>two`)
	mem := Memory{HeadID: Hash(en[0].Body), "a": Hash(en[1].Body), "b": Hash(en[2].Body)}
	tr := Split(`<p>Kopf</p><h2 id="a">A</h2>eins<h2 id="b">B</h2>zwei`)
	if s := Stale(en, tr, mem); len(s) != 0 {
		t.Fatalf("current translation reported stale: %v", s)
	}

	changed := Split(`<p>head</p><h2 id="a">A</h2>one, revised<h2 id="b">B</h2>two`)
	if s := Stale(changed, tr, mem); len(s) != 1 || !strings.Contains(s[0], "#a") {
		t.Errorf("changed English: %v", s)
	}
	reordered := Split(`<p>Kopf</p><h2 id="b">B</h2>zwei<h2 id="a">A</h2>eins`)
	if s := Stale(en, reordered, mem); len(s) == 0 {
		t.Error("out-of-order sections must be reported")
	}
	extra := Split(`<p>Kopf</p><h2 id="a">A</h2>eins<h2 id="b">B</h2>zwei<h2 id="c">C</h2>drei`)
	if s := Stale(en, extra, mem); len(s) != 1 || !strings.Contains(s[0], "#c") {
		t.Errorf("a section English dropped must be reported: %v", s)
	}
}

func TestMismatch(t *testing.T) {
	en := ShapeOf(`<tr><td><kbd>q</kbd></td></tr><div class="cmd">x --stack s</div>`)
	if p := Mismatch(en, ShapeOf(`<tr><td><kbd>q</kbd></td></tr><div class="cmd">x --stack s</div>`)); len(p) != 0 {
		t.Errorf("identical shape: %v", p)
	}
	if p := Mismatch(en, ShapeOf(`<div class="cmd">x</div>`)); len(p) != 3 {
		t.Errorf("lost a row, a flag and a key: %v", p)
	}
}

func TestSourcesPrune(t *testing.T) {
	s := Sources{"h1": "a", "h2": "b", "h3": "c"}
	s.Prune(Memory{"x": "h1"}, Memory{"y": "h3"})
	if len(s) != 2 || s["h2"] != "" {
		t.Errorf("sources = %v", s)
	}
}
