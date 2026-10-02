// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package docsi18n keeps the translated docs pages derived from the English
// one, section by section.
//
// site/docs.html is the source. Each translation (docs.<lang>.html) is cut at
// the same section boundaries, and site/i18n/<lang>.json records, per
// section, the English text it was translated from. A section whose English
// has changed since is stale: the CI check names it, and tools/docs-i18n
// re-translates exactly those sections — showing the model the old English,
// the new English and the current translation, so what did not change stays
// word for word. Nothing here talks to a network; the tool does, on a
// developer's machine.
package docsi18n

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// HeadID names the part of a page before its first section: <head>, the
// title block, the intro and the table of contents.
const HeadID = "_head"

// Section is one cut of a page: from a heading with an id up to the next one.
type Section struct {
	ID   string
	Body string
}

var sectionStart = regexp.MustCompile(`<h[23][^>]*\bid="([^"]+)"[^>]*>`)

// Split cuts a page into its head and its sections. Join(Split(p)) == p.
func Split(page string) []Section {
	starts := sectionStart.FindAllStringSubmatchIndex(page, -1)
	if len(starts) == 0 {
		return []Section{{ID: HeadID, Body: page}}
	}
	out := []Section{{ID: HeadID, Body: page[:starts[0][0]]}}
	for i, m := range starts {
		end := len(page)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		out = append(out, Section{ID: page[m[2]:m[3]], Body: page[m[0]:end]})
	}
	return out
}

// Join puts sections back together.
func Join(secs []Section) string {
	var b strings.Builder
	for _, s := range secs {
		b.WriteString(s.Body)
	}
	return b.String()
}

// Hash identifies a section's English text.
func Hash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])[:16]
}

// Memory is one language's record: section id → hash of the English that
// section was translated from.
type Memory map[string]string

// Sources holds the English text behind every hash some language still
// refers to, once, so a re-translation can show the model what changed.
type Sources map[string]string

func LoadMemory(path string) (Memory, error) {
	m := Memory{}
	return m, loadJSON(path, &m)
}

func LoadSources(path string) (Sources, error) {
	s := Sources{}
	return s, loadJSON(path, &s)
}

func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// SaveJSON writes with sorted keys (encoding/json sorts map keys), so a
// re-run without changes leaves the file byte-identical.
func SaveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Prune drops sources no language refers to any more.
func (s Sources) Prune(mems ...Memory) {
	used := map[string]bool{}
	for _, m := range mems {
		for _, h := range m {
			used[h] = true
		}
	}
	for h := range s {
		if !used[h] {
			delete(s, h)
		}
	}
}

// Stale lists the problems between the English page and one translation:
// sections missing, extra or out of order, and sections whose English changed
// since they were translated. Empty means the translation is current.
func Stale(en, tr []Section, mem Memory) []string {
	var out []string
	trIdx := map[string]int{}
	for i, s := range tr {
		trIdx[s.ID] = i
	}
	enIDs := map[string]bool{}
	for i, s := range en {
		enIDs[s.ID] = true
		j, ok := trIdx[s.ID]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("#%s: missing", s.ID))
			continue
		case j != i:
			out = append(out, fmt.Sprintf("#%s: at position %d, English has it at %d", s.ID, j, i))
		}
		if h, ok := mem[s.ID]; !ok || h != Hash(s.Body) {
			out = append(out, fmt.Sprintf("#%s: the English changed since it was translated", s.ID))
		}
	}
	for _, s := range tr {
		if !enIDs[s.ID] {
			out = append(out, fmt.Sprintf("#%s: not in the English page any more", s.ID))
		}
	}
	return out
}

// --- structure: what a translation must keep from its English section ----

var (
	tableRow     = regexp.MustCompile(`<tr>`)
	commandBlock = regexp.MustCompile(`class="(?:cmd|prompt)"`)
	flagName     = regexp.MustCompile(`--[a-z][a-z0-9-]+`)
	keyName      = regexp.MustCompile(`<kbd>([^<]+)</kbd>`)
	anyTag       = regexp.MustCompile(`<[^>]+>`)
)

// Shape is the part of a section a translation may not change: its tables,
// command blocks, the flags it names and the keys it shows.
type Shape struct {
	Rows, Blocks int
	Flags        map[string]bool
	Keys         []string
}

func ShapeOf(body string) Shape {
	s := Shape{
		Rows:   len(tableRow.FindAllString(body, -1)),
		Blocks: len(commandBlock.FindAllString(body, -1)),
		Flags:  map[string]bool{},
	}
	for _, f := range flagName.FindAllString(anyTag.ReplaceAllString(body, " "), -1) {
		s.Flags[f] = true
	}
	for _, k := range keyName.FindAllStringSubmatch(body, -1) {
		s.Keys = append(s.Keys, k[1])
	}
	sort.Strings(s.Keys)
	return s
}

// Mismatch lists how a translated section's shape differs from its English.
// Prose is free to differ; a row, a command block, a flag or a key is not.
func Mismatch(en, tr Shape) []string {
	var out []string
	if en.Rows != tr.Rows {
		out = append(out, fmt.Sprintf("%d table rows, English has %d", tr.Rows, en.Rows))
	}
	if en.Blocks != tr.Blocks {
		out = append(out, fmt.Sprintf("%d command blocks, English has %d", tr.Blocks, en.Blocks))
	}
	var missing []string
	for f := range en.Flags {
		if !tr.Flags[f] {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		out = append(out, "does not mention "+strings.Join(missing, ", "))
	}
	if strings.Join(en.Keys, "\x00") != strings.Join(tr.Keys, "\x00") {
		out = append(out, fmt.Sprintf("keys differ — English %v, here %v", en.Keys, tr.Keys))
	}
	return out
}

// headFacts are the tags of a page head that are per-language facts rather
// than prose — the page's language, its canonical URL, its og:url and
// og:locale, its hreflang alternates. They must survive any re-translation
// exactly. Matched as tags, not lines, so prose sharing a line with one of
// them can still change.
var headFacts = regexp.MustCompile(`<html[^>]*>|<link[^>]*\brel="canonical"[^>]*>|<meta[^>]*"og:(?:url|locale(?::alternate)?)"[^>]*>|<link[^>]*\bhreflang=[^>]*>`)

// HeadFacts returns those tags, in order.
func HeadFacts(head string) []string { return headFacts.FindAllString(head, -1) }
