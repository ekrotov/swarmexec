// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package site holds no Go code; these tests keep the four translations of
// the docs page derived from the English one (see internal/docsi18n).
package site

import (
	"os"
	"strings"
	"testing"

	"swarmexec/internal/docsi18n"
)

var langs = []string{"de", "es", "fr", "pl"}

func sectionsOf(t *testing.T, path string) []docsi18n.Section {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return docsi18n.Split(string(b))
}

// Every section of every translation was translated from the English that is
// there now. A docs.html change without `make docs-i18n` fails here, naming
// the sections — the language that gets forgotten cannot be forgotten.
func TestTranslationsAreCurrent(t *testing.T) {
	en := sectionsOf(t, "docs.html")
	for _, lang := range langs {
		t.Run(lang, func(t *testing.T) {
			mem, err := docsi18n.LoadMemory("i18n/" + lang + ".json")
			if err != nil {
				t.Fatal(err)
			}
			if probs := docsi18n.Stale(en, sectionsOf(t, "docs."+lang+".html"), mem); len(probs) > 0 {
				t.Errorf("docs.%s.html is out of date with docs.html — run `make docs-i18n` "+
					"(or translate by hand and run `make docs-i18n-accept`):\n  %s", lang, strings.Join(probs, "\n  "))
			}
		})
	}
}

// Prose may differ — that is what a translation is; a table row, a command
// block, a flag or a key that exists in one language and not another may not.
func TestTranslationsMatchTheEnglishStructure(t *testing.T) {
	en := sectionsOf(t, "docs.html")
	if len(en) < 10 {
		t.Fatalf("found only %d sections in docs.html — the section pattern no longer matches the page", len(en))
	}
	for _, lang := range langs {
		tr := sectionsOf(t, "docs."+lang+".html")
		t.Run(lang, func(t *testing.T) {
			for i := 0; i < len(en) && i < len(tr); i++ {
				if en[i].ID != tr[i].ID {
					t.Fatalf("section %d is #%s, English has #%s", i, tr[i].ID, en[i].ID)
				}
				for _, p := range docsi18n.Mismatch(docsi18n.ShapeOf(en[i].Body), docsi18n.ShapeOf(tr[i].Body)) {
					t.Errorf("#%s: %s", en[i].ID, p)
				}
			}
		})
	}
}
