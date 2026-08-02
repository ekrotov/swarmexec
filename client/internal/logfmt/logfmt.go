// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package logfmt parses container log lines into a normalized Entry so the
// client can filter and colour them independently of how an app logs. Formats
// are selected explicitly today; the Resolver seam lets a future online service
// auto-pick the format per detected service without changing any caller.
package logfmt

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Level is a normalized log severity.
type Level int

const (
	LevelUnknown Level = iota
	LevelTrace
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

func (l Level) String() string {
	switch l {
	case LevelTrace:
		return "TRACE"
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	case LevelFatal:
		return "FATAL"
	default:
		return ""
	}
}

// ParseLevel maps a textual level to a Level (case-insensitive); "" / unknown
// text yields LevelUnknown. Also used to parse the --min-level flag.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace":
		return LevelTrace
	case "debug":
		return LevelDebug
	case "info", "information", "notice":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "error", "err":
		return LevelError
	case "fatal", "critical", "crit", "panic", "emerg", "emergency", "alert":
		return LevelFatal
	}
	return LevelUnknown
}

// parseSyslogLevel maps a GELF/syslog numeric level (0-7) to a Level.
func parseSyslogLevel(n int) Level {
	switch {
	case n <= 2: // emerg, alert, crit
		return LevelFatal
	case n == 3:
		return LevelError
	case n == 4:
		return LevelWarn
	case n == 5, n == 6: // notice, info
		return LevelInfo
	default: // 7 debug (and anything higher)
		return LevelDebug
	}
}

// Entry is a normalized log line.
type Entry struct {
	Level   Level
	Message string // the human-readable message (the whole line for raw/classic)
	Raw     string // the original line, unmodified
}

// Format parses a raw log line into an Entry.
type Format interface {
	Name() string
	Parse(line string) Entry
	// Structured reports whether the raw line is a machine format (JSON/GELF)
	// whose message should be shown reformatted as "LEVEL  message" rather than
	// dumped verbatim. Plain formats (classic/raw) render the line unchanged.
	Structured() bool
}

type format struct {
	name       string
	structured bool
	parse      func(string) Entry
}

func (f format) Name() string         { return f.name }
func (f format) Parse(l string) Entry { return f.parse(l) }
func (f format) Structured() bool     { return f.structured }

// Raw does no parsing: the whole line is the message, level unknown.
var Raw Format = format{name: "raw", parse: func(line string) Entry {
	return Entry{Message: line, Raw: line}
}}

var classicLevelRe = regexp.MustCompile(`(?i)\b(trace|debug|info|warn(?:ing)?|error|err|fatal|critical|crit|panic)\b`)

// Classic reads a plain-text line and extracts a level from a common word
// (INFO/WARN/ERROR/…); the whole line stays as the message.
var Classic Format = format{name: "classic", parse: func(line string) Entry {
	e := Entry{Message: line, Raw: line}
	if m := classicLevelRe.FindString(line); m != "" {
		e.Level = ParseLevel(m)
	}
	return e
}}

// JSON is the logstash-style structured format: a JSON object whose message is
// message/msg/@message/log and level is level/severity/loglevel/lvl/@level.
var JSON Format = format{name: "json", structured: true, parse: func(line string) Entry {
	e := Entry{Message: line, Raw: line}
	m, ok := decodeObject(line)
	if !ok {
		return e // not JSON — keep the raw line so nothing is lost
	}
	if msg, ok := firstString(m, "message", "msg", "@message", "log"); ok {
		e.Message = msg
	}
	if lvl, ok := firstString(m, "level", "severity", "loglevel", "lvl", "@level"); ok {
		e.Level = ParseLevel(lvl)
	}
	return e
}}

// GELF is graylog's native wire format: message is short_message/message and
// level is a numeric syslog severity.
var GELF Format = format{name: "gelf", structured: true, parse: func(line string) Entry {
	e := Entry{Message: line, Raw: line}
	m, ok := decodeObject(line)
	if !ok {
		return e
	}
	if msg, ok := firstString(m, "short_message", "message"); ok {
		e.Message = msg
	}
	if raw, ok := m["level"]; ok {
		var n int
		if json.Unmarshal(raw, &n) == nil {
			e.Level = parseSyslogLevel(n)
		}
	}
	return e
}}

func decodeObject(line string) (map[string]json.RawMessage, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "{") {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &m) != nil {
		return nil, false
	}
	return m, true
}

func firstString(m map[string]json.RawMessage, keys ...string) (string, bool) {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s, true
		}
	}
	return "", false
}

// formats is the built-in set, in menu order.
var formats = []Format{Classic, JSON, GELF, Raw}

// Formats returns the built-in formats (menu order).
func Formats() []Format { return formats }

// ByName returns the built-in format with the given name.
func ByName(name string) (Format, bool) {
	for _, f := range formats {
		if f.Name() == name {
			return f, true
		}
	}
	return nil, false
}

// DefaultFormat is used when nothing is configured: classic keeps plain logs
// readable while still surfacing a level.
func DefaultFormat() Format { return Classic }

// Filter decides which parsed entries to show.
type Filter struct {
	MinLevel Level          // LevelUnknown = no level filter
	Grep     *regexp.Regexp // nil = no text filter
}

// Match reports whether e passes the filter. A line whose level could not be
// detected always passes the level filter (so multiline traces are not lost).
func (f Filter) Match(e Entry) bool {
	if f.MinLevel != LevelUnknown && e.Level != LevelUnknown && e.Level < f.MinLevel {
		return false
	}
	if f.Grep != nil && !f.Grep.MatchString(e.Message) {
		return false
	}
	return true
}

// Resolver picks the Format for a log stream. Today it is static; the Hint lets
// a future RemoteResolver query a service that maps a detected service/image to
// its optimal format. Callers depend only on this interface, so wiring the
// online service later needs no changes here.
type Resolver interface {
	Resolve(h Hint) Format
}

// Hint describes a log stream for a Resolver. The static resolver ignores it;
// the future auto-detection service will key off Service/Image.
type Hint struct {
	Service string
	Image   string
	Node    string
}

type staticResolver struct{ f Format }

func (s staticResolver) Resolve(Hint) Format { return s.f }

// Static returns a Resolver that always yields f.
func Static(f Format) Resolver { return staticResolver{f} }
