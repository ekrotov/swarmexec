// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package logfmt

import (
	"regexp"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"info": LevelInfo, "INFO": LevelInfo, "warning": LevelWarn, "warn": LevelWarn,
		"err": LevelError, "error": LevelError, "panic": LevelFatal, "debug": LevelDebug,
		"trace": LevelTrace, "": LevelUnknown, "nonsense": LevelUnknown,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestClassicFormat(t *testing.T) {
	e := Classic.Parse("2026-07-30 12:00:00 WARN disk almost full")
	if e.Level != LevelWarn {
		t.Errorf("level = %v, want WARN", e.Level)
	}
	if e.Message != "2026-07-30 12:00:00 WARN disk almost full" {
		t.Errorf("classic message should be the whole line, got %q", e.Message)
	}
	if lvl := Classic.Parse("just a line").Level; lvl != LevelUnknown {
		t.Errorf("no level word -> Unknown, got %v", lvl)
	}
}

func TestJSONFormat(t *testing.T) {
	e := JSON.Parse(`{"@timestamp":"t","level":"error","message":"boom"}`)
	if e.Level != LevelError || e.Message != "boom" {
		t.Errorf("json parse = %v/%q, want ERROR/boom", e.Level, e.Message)
	}
	// alternate field names
	e = JSON.Parse(`{"lvl":"warn","msg":"careful"}`)
	if e.Level != LevelWarn || e.Message != "careful" {
		t.Errorf("json alt fields = %v/%q, want WARN/careful", e.Level, e.Message)
	}
	// non-JSON falls back to the raw line, level unknown
	e = JSON.Parse("plain text line")
	if e.Level != LevelUnknown || e.Message != "plain text line" {
		t.Errorf("non-json fallback = %v/%q", e.Level, e.Message)
	}
}

func TestGELFFormat(t *testing.T) {
	// GELF: numeric syslog level (3 = err), short_message.
	e := GELF.Parse(`{"version":"1.1","host":"h","short_message":"down","level":3}`)
	if e.Level != LevelError || e.Message != "down" {
		t.Errorf("gelf parse = %v/%q, want ERROR/down", e.Level, e.Message)
	}
	if e := GELF.Parse(`{"short_message":"info msg","level":6}`); e.Level != LevelInfo {
		t.Errorf("gelf level 6 -> INFO, got %v", e.Level)
	}
}

func TestFilterMatch(t *testing.T) {
	f := Filter{MinLevel: LevelWarn}
	if f.Match(Entry{Level: LevelInfo}) {
		t.Error("INFO should be filtered out below WARN")
	}
	if !f.Match(Entry{Level: LevelError}) {
		t.Error("ERROR should pass a WARN filter")
	}
	if !f.Match(Entry{Level: LevelUnknown, Message: "trace-less line"}) {
		t.Error("unknown-level lines should pass the level filter")
	}

	g := Filter{Grep: regexp.MustCompile("(?i)timeout")}
	if !g.Match(Entry{Message: "connection Timeout"}) {
		t.Error("grep should match case-insensitively on the message")
	}
	if g.Match(Entry{Message: "all good"}) {
		t.Error("grep non-match should be filtered")
	}
}

func TestByNameAndStaticResolver(t *testing.T) {
	if _, ok := ByName("gelf"); !ok {
		t.Error("gelf should be a known format")
	}
	if _, ok := ByName("nope"); ok {
		t.Error("unknown format should not resolve")
	}
	r := Static(JSON)
	if r.Resolve(Hint{Service: "web"}).Name() != "json" {
		t.Error("static resolver should always return its format")
	}
}
