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

func TestLogfmtFormat(t *testing.T) {
	e := Logfmt.Parse(`level=info msg="started server" addr=:8080 ts=2026-08-17T10:00:00Z`)
	if e.Level != LevelInfo {
		t.Errorf("level = %v, want INFO", e.Level)
	}
	if e.Message != "started server" {
		t.Errorf("message = %q, want %q", e.Message, "started server")
	}
	if e.Timestamp.IsZero() {
		t.Error("ts=… should populate Timestamp")
	}
	// alternate keys + unquoted value
	if e := Logfmt.Parse(`lvl=warn message=careful`); e.Level != LevelWarn || e.Message != "careful" {
		t.Errorf("alt keys = %v/%q, want WARN/careful", e.Level, e.Message)
	}
	// a line with no key=value pairs keeps the raw line, level unknown
	if e := Logfmt.Parse("just a plain sentence"); e.Level != LevelUnknown || e.Message != "just a plain sentence" {
		t.Errorf("non-logfmt fallback = %v/%q", e.Level, e.Message)
	}
}

func TestLeadingTimestampStructured(t *testing.T) {
	// `docker logs --timestamps` prepends an RFC3339Nano stamp; structured
	// formats must still parse the payload behind it (the bug this fixes).
	line := `2026-08-17T10:00:00.123456789Z {"level":"error","msg":"boom"}`
	e := JSON.Parse(line)
	if e.Level != LevelError || e.Message != "boom" {
		t.Errorf("json behind a -t stamp = %v/%q, want ERROR/boom", e.Level, e.Message)
	}
	if e.Timestamp.IsZero() || e.Timestamp.Year() != 2026 {
		t.Errorf("leading -t stamp should populate Timestamp, got %v", e.Timestamp)
	}
	// logfmt behind a stamp, too
	le := Logfmt.Parse(`2026-08-17T10:00:00Z level=warn msg=careful`)
	if le.Level != LevelWarn || le.Message != "careful" || le.Timestamp.IsZero() {
		t.Errorf("logfmt behind a -t stamp = %v/%q/%v", le.Level, le.Message, le.Timestamp)
	}
	// Raw remains the full, unmodified line.
	if e.Raw != line {
		t.Errorf("Raw should keep the original line, got %q", e.Raw)
	}
}

func TestJSONTimestampField(t *testing.T) {
	// string ts field
	e := JSON.Parse(`{"ts":"2026-08-17T10:00:00Z","msg":"hi"}`)
	if e.Timestamp.IsZero() {
		t.Error("string ts field should populate Timestamp")
	}
	// numeric epoch (GELF-style / zerolog unix)
	g := GELF.Parse(`{"short_message":"x","level":6,"timestamp":1755424800}`)
	if g.Timestamp.IsZero() || g.Timestamp.Year() != 2025 && g.Timestamp.Year() != 2026 {
		t.Errorf("numeric epoch timestamp not parsed: %v", g.Timestamp)
	}
}

func TestLogfmtInFormats(t *testing.T) {
	f, ok := ByName("logfmt")
	if !ok || f.Name() != "logfmt" || !f.Structured() {
		t.Errorf("logfmt should be a known structured format, got ok=%v", ok)
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
