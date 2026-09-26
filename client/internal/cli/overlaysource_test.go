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

// An overlay is opened through the overlay type, and nowhere else.
//
// This is a grep, and a grep is a blunt instrument — but the thing it protects
// cannot be checked any other way: there is no type that a raw pages.AddPage
// violates, only a convention, and the convention is exactly what drifted the
// first time. Fifty-odd call sites each wrote their own open and close, roughly
// twenty of them forgot to register the overlay, and the background refresh ran
// underneath them.
//
// A new overlay that goes around the type fails here, at the moment it is
// written, rather than in a bug report about focus months later.
func TestOverlaysGoThroughTheOverlayType(t *testing.T) {
	// The base page is not an overlay: it is what overlays float over.
	allowed := map[string]bool{
		"ui.go":         true, // pageMain
		"ui_overlay.go": true, // the type itself
	}
	calls := regexp.MustCompile(`\bpages\.(AddPage|RemovePage)\(`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || allowed[name] {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if calls.MatchString(line) {
				t.Errorf("%s:%d opens or closes a page directly:\n    %s\n"+
					"use u.overlayFor(...) and ov.Close() — see ui_overlay.go for why",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// ui.go is allowed one raw AddPage — the base page — and no more. Spelled out
// separately so the exemption above cannot quietly cover a second one.
func TestUIGoOnlyAddsTheBasePage(t *testing.T) {
	src, err := os.ReadFile("ui.go")
	if err != nil {
		t.Fatal(err)
	}
	var raw []string
	for i, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "pages.AddPage(") || strings.Contains(line, "pages.RemovePage(") {
			raw = append(raw, strings.TrimSpace(line)+" (line "+itoa(i+1)+")")
		}
	}
	if len(raw) != 1 || !strings.Contains(raw[0], "pageMain") {
		t.Errorf("ui.go should hold exactly one raw page call, the base page; got:\n  %s",
			strings.Join(raw, "\n  "))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
