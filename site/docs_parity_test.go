// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package site holds no Go code; this test keeps the five language versions of
// the docs page from drifting apart.
//
// Every feature lands in docs.html and in four translations, and the one that
// gets forgotten is the one nobody reads in review. That is not a discipline
// problem to be solved with a reminder: it is checkable. English is the
// reference; for every section (an element with an id) each translation must
// have the same sections in the same order, the same number of table rows and
// command blocks, every --flag the English text names, and the same keys.
// Prose is free to differ — that is what a translation is — but a row, an
// example or a key that exists in one language and not another fails here.
package site

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

var (
	sectionStart = regexp.MustCompile(`<h[23][^>]*\bid="([^"]+)"[^>]*>`)
	tableRow     = regexp.MustCompile(`<tr>`)
	commandBlock = regexp.MustCompile(`class="(?:cmd|prompt)"`)
	flagName     = regexp.MustCompile(`--[a-z][a-z0-9-]+`)
	keyName      = regexp.MustCompile(`<kbd>([^<]+)</kbd>`)
	anyTag       = regexp.MustCompile(`<[^>]+>`)
)

type section struct {
	id           string
	rows, blocks int
	flags        map[string]bool
	keys         []string
}

func sectionsOf(t *testing.T, path string) []section {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	starts := sectionStart.FindAllStringSubmatchIndex(s, -1)
	var out []section
	for i, m := range starts {
		end := len(s)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		body := s[m[1]:end]
		sec := section{
			id:     s[m[2]:m[3]],
			rows:   len(tableRow.FindAllString(body, -1)),
			blocks: len(commandBlock.FindAllString(body, -1)),
			flags:  map[string]bool{},
		}
		for _, f := range flagName.FindAllString(anyTag.ReplaceAllString(body, " "), -1) {
			sec.flags[f] = true
		}
		for _, k := range keyName.FindAllStringSubmatch(body, -1) {
			sec.keys = append(sec.keys, k[1])
		}
		sort.Strings(sec.keys)
		out = append(out, sec)
	}
	return out
}

func TestTranslationsMatchTheEnglishDocs(t *testing.T) {
	en := sectionsOf(t, "docs.html")
	if len(en) < 10 {
		t.Fatalf("found only %d sections in docs.html — the section pattern no longer matches the page", len(en))
	}
	for _, lang := range []string{"de", "es", "fr", "pl"} {
		tr := sectionsOf(t, "docs."+lang+".html")
		t.Run(lang, func(t *testing.T) {
			if len(tr) != len(en) {
				t.Errorf("%d sections, English has %d", len(tr), len(en))
			}
			for i := 0; i < len(en) && i < len(tr); i++ {
				e, x := en[i], tr[i]
				if e.id != x.id {
					t.Errorf("section %d is #%s, English has #%s — missing or out of order", i+1, x.id, e.id)
					return // everything after a missing section would mismatch
				}
				if e.rows != x.rows {
					t.Errorf("#%s: %d table rows, English has %d", e.id, x.rows, e.rows)
				}
				if e.blocks != x.blocks {
					t.Errorf("#%s: %d command blocks, English has %d", e.id, x.blocks, e.blocks)
				}
				for f := range e.flags {
					if !x.flags[f] {
						t.Errorf("#%s: does not mention %s", e.id, f)
					}
				}
				if !equal(e.keys, x.keys) {
					t.Errorf("#%s: keys differ — English %v, here %v", e.id, diff(e.keys, x.keys), diff(x.keys, e.keys))
				}
			}
		})
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// diff is what a has and b lacks, counting repeats.
func diff(a, b []string) []string {
	left := map[string]int{}
	for _, x := range b {
		left[x]++
	}
	var out []string
	for _, x := range a {
		if left[x] > 0 {
			left[x]--
			continue
		}
		out = append(out, x)
	}
	return out
}
