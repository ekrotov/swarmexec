// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"swarmexec/internal/docsi18n"
)

const enPage = `<html lang="en"><head><link rel="canonical" href="https://x/docs"></head><body><p>Intro.</p>
<h2 id="install">Install</h2>
<p>Run the installer.</p>
<div class="cmd">curl x | sh</div>
<h2 id="logs">Logs</h2>
<table><tr><td><kbd>L</kbd></td><td>open logs</td></tr></table>
`

const dePage = `<html lang="de"><head><link rel="canonical" href="https://x/docs.de.html"></head><body><p>Einleitung.</p>
<h2 id="install">Installation</h2>
<p>Den Installer ausführen.</p>
<div class="cmd">curl x | sh</div>
<h2 id="logs">Logs</h2>
<table><tr><td><kbd>L</kbd></td><td>Logs öffnen</td></tr></table>
`

// fakeTranslator records what it was given and answers with a function of it.
type fakeTranslator struct {
	calls  []call
	answer func(sec docsi18n.Section, oldEnglish, oldTranslation string) string
}

type call struct{ id, oldEnglish, oldTranslation, newEnglish string }

func (f *fakeTranslator) translate(_ string, sec docsi18n.Section, oldEnglish, oldTranslation string) (string, error) {
	f.calls = append(f.calls, call{sec.ID, oldEnglish, oldTranslation, sec.Body})
	return f.answer(sec, oldEnglish, oldTranslation), nil
}

// setup writes a site directory with an English page and a current German
// translation, seeded as up to date.
func setup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "docs.html", enPage)
	write(t, dir, "docs.de.html", dePage)
	if err := run(dir, []string{"de"}, false, true, nil); err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, dir, name, s string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func stale(t *testing.T, dir string) []string {
	t.Helper()
	mem, err := docsi18n.LoadMemory(filepath.Join(dir, "i18n", "de.json"))
	if err != nil {
		t.Fatal(err)
	}
	return docsi18n.Stale(docsi18n.Split(read(t, dir, "docs.html")), docsi18n.Split(read(t, dir, "docs.de.html")), mem)
}

// The point of the whole design: one changed English section is the one
// section sent, with what it was translated from, and every other section of
// the translation stays byte for byte as it was.
func TestOnlyTheChangedSectionIsRetranslated(t *testing.T) {
	dir := setup(t)
	write(t, dir, "docs.html", strings.Replace(enPage, "Run the installer.", "Run the installer, then check its checksum.", 1))
	if s := stale(t, dir); len(s) != 1 || !strings.Contains(s[0], "#install") {
		t.Fatalf("stale = %v, want only #install", s)
	}

	f := &fakeTranslator{answer: func(sec docsi18n.Section, _, old string) string {
		return strings.Replace(old, "Den Installer ausführen.", "Den Installer ausführen, dann die Prüfsumme prüfen.", 1)
	}}
	if err := run(dir, []string{"de"}, false, false, func() translator { return f }); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].id != "install" {
		t.Fatalf("calls = %+v", f.calls)
	}
	c := f.calls[0]
	if !strings.Contains(c.oldEnglish, "Run the installer.") || !strings.Contains(c.newEnglish, "checksum") ||
		!strings.Contains(c.oldTranslation, "Den Installer ausführen.") {
		t.Errorf("the model must see old English, new English and the current translation: %+v", c)
	}
	want := strings.Replace(dePage, "Den Installer ausführen.", "Den Installer ausführen, dann die Prüfsumme prüfen.", 1)
	if got := read(t, dir, "docs.de.html"); got != want {
		t.Errorf("page =\n%s\nwant\n%s", got, want)
	}
	if s := stale(t, dir); len(s) != 0 {
		t.Errorf("still stale after the run: %v", s)
	}
}

// A translation that loses a row, a command block, a flag or a key is not
// written — and the section stays stale, so the check keeps saying so.
func TestAnAnswerWithADifferentStructureIsRefused(t *testing.T) {
	dir := setup(t)
	write(t, dir, "docs.html", strings.Replace(enPage, "open logs", "open the logs", 1))
	f := &fakeTranslator{answer: func(sec docsi18n.Section, _, _ string) string {
		return "<h2 id=\"logs\">Logs</h2>\n<p>no table any more</p>\n"
	}}
	err := run(dir, []string{"de"}, false, false, func() translator { return f })
	if err == nil {
		t.Fatal("a structurally wrong answer must fail the run")
	}
	if got := read(t, dir, "docs.de.html"); got != dePage {
		t.Errorf("the page was changed by a refused answer:\n%s", got)
	}
	if s := stale(t, dir); len(s) != 1 {
		t.Errorf("the section must stay stale, got %v", s)
	}
}

// The head's per-language lines are facts, not prose: an answer that touches
// them — here, pointing the German page's canonical at the English one — is
// refused.
func TestTheHeadsLanguageFactsCannotChange(t *testing.T) {
	dir := setup(t)
	write(t, dir, "docs.html", strings.Replace(enPage, "<p>Intro.</p>", "<p>A new intro.</p>", 1))
	f := &fakeTranslator{answer: func(sec docsi18n.Section, _, old string) string {
		return strings.Replace(strings.Replace(old, "docs.de.html", "docs", 1), "Einleitung.", "Neue Einleitung.", 1)
	}}
	if err := run(dir, []string{"de"}, false, false, func() translator { return f }); err == nil {
		t.Fatal("changing the canonical URL must be refused")
	}
	f.answer = func(sec docsi18n.Section, _, old string) string {
		return strings.Replace(old, "Einleitung.", "Neue Einleitung.", 1)
	}
	if err := run(dir, []string{"de"}, false, false, func() translator { return f }); err != nil {
		t.Fatalf("a prose-only change to the head must pass: %v", err)
	}
	if !strings.Contains(read(t, dir, "docs.de.html"), `href="https://x/docs.de.html"`) {
		t.Error("the German canonical was lost")
	}
}

// A section new in English is translated fresh and lands where English has
// it; a section English dropped is dropped.
func TestNewAndRemovedSections(t *testing.T) {
	dir := setup(t)
	en := strings.Replace(enPage, `<h2 id="logs">`, "<h2 id=\"wait\">Wait</h2>\n<p>Wait for it.</p>\n<h2 id=\"logs\">", 1)
	en = en[:strings.Index(en, `<h2 id="logs">`)] // and drop #logs
	write(t, dir, "docs.html", en)
	f := &fakeTranslator{answer: func(sec docsi18n.Section, oldEnglish, old string) string {
		if oldEnglish != "" || old != "" {
			t.Errorf("a new section has nothing to compare with, got %q / %q", oldEnglish, old)
		}
		return "<h2 id=\"wait\">Warten</h2>\n<p>Darauf warten.</p>\n"
	}}
	if err := run(dir, []string{"de"}, false, false, func() translator { return f }); err != nil {
		t.Fatal(err)
	}
	got := read(t, dir, "docs.de.html")
	if !strings.Contains(got, "Darauf warten.") || strings.Contains(got, `id="logs"`) {
		t.Errorf("page:\n%s", got)
	}
	if strings.Index(got, `id="install"`) > strings.Index(got, `id="wait"`) {
		t.Error("the new section must land where English has it")
	}
	if s := stale(t, dir); len(s) != 0 {
		t.Errorf("stale: %v", s)
	}
}

// A dry run reports and changes nothing.
func TestDryRunChangesNothing(t *testing.T) {
	dir := setup(t)
	write(t, dir, "docs.html", strings.Replace(enPage, "Run the installer.", "Run it.", 1))
	memBefore := read(t, dir, "i18n/de.json")
	if err := run(dir, []string{"de"}, true, false, func() translator { t.Fatal("dry run must not translate"); return nil }); err != nil {
		t.Fatal(err)
	}
	if read(t, dir, "docs.de.html") != dePage || read(t, dir, "i18n/de.json") != memBefore {
		t.Error("dry run changed files")
	}
}
