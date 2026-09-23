// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package logfmt

import "testing"

// The line that started this: a level word can appear in a line that has no
// level at all. A node called node-error-2 turned every line that mentioned it
// into a failure, and "restarting after an error" read as an outage — which is
// worse than reading nothing, because --min-level then keeps the wrong lines
// and colours the tab red.
func TestClassicLevelIgnoresProseAndNames(t *testing.T) {
	for _, line := range []string{
		"scheduling task on node-error-2",
		"restarting after an error",
		"connection lost, will warn the operator later",
		"tailing /var/log/error.log",
		"calling error_handler for request 42",
		"GET /api/v1/info HTTP/1.1 200",
		"see the error above (an error, really)",
		"com.acme.service.Error thrown at Service.java:42",
	} {
		if got := classicLevel(line); got != LevelUnknown {
			t.Errorf("classicLevel(%q) = %v, want no level", line, got)
		}
	}
}

// What real plain-text loggers actually emit. Every one of these is a level in
// a position the parser can point at, which is the whole difference.
func TestClassicLevelReadsRealLoggers(t *testing.T) {
	cases := []struct {
		name string
		line string
		want Level
	}{
		{"nginx", `2026/09/22 10:00:00 [error] 123#123: *1 connect() failed`, LevelError},
		{"apache", `[Mon Sep 22 10:00:00 2026] [error] [client 10.0.0.5] File does not exist`, LevelError},
		{"mysql", `2026-09-22T10:00:00.1Z 0 [Warning] [MY-010068] [Server] CA certificate is self signed`, LevelWarn},
		{"fluent bit", `[2026/09/22 10:00:00] [ info] [fluent bit] version=2.0.0`, LevelInfo},
		{"spdlog", `[2026-09-22 10:00:00.123] [mylogger] [critical] disk is gone`, LevelFatal},
		{"logrus text", `INFO[0000] starting server                    addr=":8080"`, LevelInfo},
		{"python logging", `WARNING:root:be careful`, LevelWarn},
		{"postgres", `2026-09-22 10:00:00.123 UTC [123] ERROR:  relation "x" does not exist`, LevelError},
		{"plain upper", `2026-09-22 10:00:00 INFO  listening on :8080`, LevelInfo},
		{"java-style", `2026-09-22 10:00:00.123 [pool-1-thread-3] DEBUG c.a.Service - loaded 12 rows`, LevelDebug},
		{"lowercase with colon", `error: could not reach the registry`, LevelError},
		{"angle brackets", `<warn> the certificate expires in 3 days`, LevelWarn},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classicLevel(c.line); got != c.want {
				t.Errorf("classicLevel(%q) = %v, want %v", c.line, got, c.want)
			}
		})
	}
}

// A bracketed field beats a bare word: the brackets say where the field ends,
// so they are the stronger evidence — and the bare word may well be in the
// message.
func TestClassicLevelPrefersTheBracketedField(t *testing.T) {
	line := `[2026-09-22] [warn] the ERROR count is rising`
	if got := classicLevel(line); got != LevelWarn {
		t.Errorf("classicLevel(%q) = %v, want warn", line, got)
	}
}

// The trade this makes, stated out loud: a bare lowercase level with nothing
// around it now reads as levelless. It is the price of not calling every line
// that mentions trouble an error, and a logger that does this is rare.
func TestClassicLevelSkipsABareLowercaseWord(t *testing.T) {
	if got := classicLevel("2026-09-22 10:00:00 info listening"); got != LevelUnknown {
		t.Errorf("got %v, want no level — the documented trade", got)
	}
}

// The parser and the detector have to agree on what a level is, or `auto` picks
// classic for a stream classic cannot read (or refuses one it can).
func TestClassicDetectorAgreesWithTheParser(t *testing.T) {
	for _, line := range []string{
		`[2026/09/22 10:00:00] [ info] [fluent bit] version=2.0.0`,
		`2026-09-22 10:00:00 INFO  listening`,
		"scheduling task on node-error-2",
		"restarting after an error",
	} {
		parsed := Classic.Parse(line).Level != LevelUnknown
		if claimed := claimsClassic(line); claimed != parsed {
			t.Errorf("%q: detector says %v, parser says %v", line, claimed, parsed)
		}
	}
}

// A sample of lines that only mention trouble is not a classic log, and saying
// so is better than claiming a format that yields no levels.
func TestDetectDoesNotCallProseClassic(t *testing.T) {
	l := []string{
		"scheduling task on node-error-2",
		"moved task to node-error-2",
		"node-error-2 reports 4 free slots",
	}
	if got, ok := Detect(l); ok {
		t.Errorf("Detect = %s/%v, want no verdict", got.Name(), ok)
	}
}
