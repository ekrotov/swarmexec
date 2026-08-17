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
	"strconv"
	"strings"
	"time"
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
	Level     Level
	Message   string    // the human-readable message (the whole line for raw/classic)
	Raw       string    // the original line, unmodified
	Timestamp time.Time // the line's time, if one was found; zero otherwise
}

// stripLeadingTimestamp splits off a leading RFC3339(Nano) token — what
// `docker logs --timestamps` prepends to every line — returning the parsed time
// and the remainder. Without it, a structured format's `{` / key=value payload
// hides behind the timestamp and parsing silently falls back to raw. If the line
// has no such prefix it is returned unchanged with a zero time.
func stripLeadingTimestamp(line string) (time.Time, string) {
	i := strings.IndexByte(line, ' ')
	if i <= 0 {
		return time.Time{}, line
	}
	if t, ok := parseTimeString(line[:i]); ok {
		return t, line[i+1:]
	}
	return time.Time{}, line
}

// parseTimeString parses a timestamp as RFC3339(Nano) or a unix epoch (seconds,
// possibly fractional) — the forms container logs use.
func parseTimeString(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
		sec := int64(f)
		return time.Unix(sec, int64((f-float64(sec))*1e9)), true
	}
	return time.Time{}, false
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
// (INFO/WARN/ERROR/…); the whole line stays as the message. A leading
// --timestamps prefix, if present, is captured but left in the displayed line.
var Classic Format = format{name: "classic", parse: func(line string) Entry {
	e := Entry{Message: line, Raw: line}
	if ts, _ := stripLeadingTimestamp(line); !ts.IsZero() {
		e.Timestamp = ts
	}
	if m := classicLevelRe.FindString(line); m != "" {
		e.Level = ParseLevel(m)
	}
	return e
}}

// JSON is the logstash-style structured format: a JSON object whose message is
// message/msg/@message/log and level is level/severity/loglevel/lvl/@level. A
// leading --timestamps prefix is stripped before decoding so `-t` and `json`
// work together.
var JSON Format = format{name: "json", structured: true, parse: func(line string) Entry {
	e := Entry{Message: line, Raw: line}
	ts, rest := stripLeadingTimestamp(line)
	if !ts.IsZero() {
		e.Timestamp = ts
	}
	m, ok := decodeObject(rest)
	if !ok {
		return e // not JSON — keep the raw line so nothing is lost
	}
	if msg, ok := firstString(m, "message", "msg", "@message", "log"); ok {
		e.Message = msg
	}
	if lvl, ok := firstString(m, "level", "severity", "loglevel", "lvl", "@level"); ok {
		e.Level = ParseLevel(lvl)
	}
	if t, ok := firstTime(m, "ts", "time", "timestamp", "@timestamp", "@time"); ok {
		e.Timestamp = t
	}
	return e
}}

// GELF is graylog's native wire format: message is short_message/message and
// level is a numeric syslog severity.
var GELF Format = format{name: "gelf", structured: true, parse: func(line string) Entry {
	e := Entry{Message: line, Raw: line}
	ts, rest := stripLeadingTimestamp(line)
	if !ts.IsZero() {
		e.Timestamp = ts
	}
	m, ok := decodeObject(rest)
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
	// GELF's timestamp field is a unix epoch (seconds, fractional allowed).
	if t, ok := firstTime(m, "timestamp"); ok {
		e.Timestamp = t
	}
	return e
}}

// Logfmt parses the key=value line format (level=info msg="…" ts=…) used by many
// Go apps, the Docker daemon, HashiCorp tools and others. message is msg/message,
// level is level/lvl/severity/loglevel, ts is ts/time/timestamp.
var Logfmt Format = format{name: "logfmt", structured: true, parse: func(line string) Entry {
	e := Entry{Message: line, Raw: line}
	ts, rest := stripLeadingTimestamp(line)
	if !ts.IsZero() {
		e.Timestamp = ts
	}
	kv := parseLogfmtPairs(rest)
	if len(kv) == 0 {
		return e // no key=value pairs — keep the raw line
	}
	if msg, ok := firstKV(kv, "msg", "message"); ok {
		e.Message = msg
	}
	if lvl, ok := firstKV(kv, "level", "lvl", "severity", "loglevel"); ok {
		e.Level = ParseLevel(lvl)
	}
	if s, ok := firstKV(kv, "ts", "time", "timestamp"); ok {
		if t, ok := parseTimeString(s); ok {
			e.Timestamp = t
		}
	}
	return e
}}

// parseLogfmtPairs scans a line of key=value pairs, honoring double-quoted values
// (with backslash escapes) so `msg="hello world"` stays one value. Bare tokens
// without an '=' are ignored. Best-effort: it never errors.
func parseLogfmtPairs(s string) map[string]string {
	out := map[string]string{}
	i, n := 0, len(s)
	for i < n {
		for i < n && s[i] == ' ' {
			i++
		}
		if i >= n {
			break
		}
		ks := i
		for i < n && s[i] != '=' && s[i] != ' ' {
			i++
		}
		key := s[ks:i]
		if i >= n || s[i] == ' ' {
			continue // bare token, no value
		}
		i++ // consume '='
		var val string
		if i < n && s[i] == '"' {
			i++
			var b strings.Builder
			for i < n && s[i] != '"' {
				if s[i] == '\\' && i+1 < n {
					i++
				}
				b.WriteByte(s[i])
				i++
			}
			if i < n {
				i++ // closing quote
			}
			val = b.String()
		} else {
			vs := i
			for i < n && s[i] != ' ' {
				i++
			}
			val = s[vs:i]
		}
		if key != "" {
			out[key] = val
		}
	}
	return out
}

// firstKV returns the first non-empty value among keys.
func firstKV(m map[string]string, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// firstTime returns the first parseable timestamp among keys. A value may be an
// RFC3339 string or a numeric unix epoch.
func firstTime(m map[string]json.RawMessage, keys ...string) (time.Time, bool) {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			if t, ok := parseTimeString(s); ok {
				return t, true
			}
			continue
		}
		var f float64
		if json.Unmarshal(raw, &f) == nil && f > 0 {
			sec := int64(f)
			return time.Unix(sec, int64((f-float64(sec))*1e9)), true
		}
	}
	return time.Time{}, false
}

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
var formats = []Format{Classic, JSON, Logfmt, GELF, Raw}

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
