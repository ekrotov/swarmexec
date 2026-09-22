// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package logfmt

import (
	"encoding/json"
	"strings"
)

// Content detection answers the question the format menu asks of the operator
// and that the operator usually cannot answer either: what does this container
// log like? Picking wrong is not cosmetic — a `--min-level warn` on a stream
// parsed as the wrong format filters nothing, because no line yields a level.
//
// The rule is deliberately dull: score every built-in against a sample of real
// lines and take the first one, in specificity order, that explains enough of
// them. No line is classified by the image it came from, by a guess at the
// language, or by a single lucky line.

// AutoName is the pseudo-format name that asks for detection instead of naming
// a format. It is accepted wherever a format name is (`--log-format auto`,
// `logs.format: auto` in the config) and is never the name of a parsed result:
// what the detector picks is one of the built-ins.
const AutoName = "auto"

const (
	// DetectSample is how many of the most recent lines Detect looks at. Recent
	// rather than first, because the first lines of a container are its startup
	// banner — routinely printed by a framework that logs nothing like the
	// application that follows it.
	DetectSample = 200
	// DetectMinLines is the fewest lines Detect will draw a conclusion from.
	// Below this a single unusual line decides the verdict.
	DetectMinLines = 3
	// detectThreshold is the fraction of the sample a format has to explain. Two
	// lines in three: low enough to survive the stack traces and bare
	// continuation lines that every structured logger emits, high enough that an
	// occasional JSON line in a plain log does not carry the vote.
	detectThreshold = 0.6
)

// sniffers are tried in this order, most specific first. Order is not a
// tiebreak detail: every GELF line is also valid JSON and carries a `level`, so
// JSON would swallow the format that is strictly more informative. Classic is
// last because its evidence — a level word anywhere in the line — is the
// weakest of the four and matches inside a message that quotes one.
var sniffers = []struct {
	format Format
	claims func(string) bool
}{
	{GELF, claimsGELF},
	{JSON, claimsJSON},
	{Logfmt, claimsLogfmt},
	{Classic, claimsClassic},
}

// Detect picks the format that best explains lines. The bool reports whether it
// reached a verdict at all: false means the sample was too small or too mixed
// to tell, and the returned format is the default — so a caller that ignores
// the bool still behaves exactly as it did before detection existed.
func Detect(lines []string) (Format, bool) {
	sample := sampleOf(lines)
	if len(sample) < DetectMinLines {
		return DefaultFormat(), false
	}
	need := detectThreshold * float64(len(sample))
	for _, s := range sniffers {
		hits := 0
		for _, l := range sample {
			if s.claims(l) {
				hits++
			}
		}
		if float64(hits) >= need {
			return s.format, true
		}
	}
	return DefaultFormat(), false
}

// sampleOf returns the last DetectSample non-blank lines. Blank lines are
// dropped rather than counted as failures: they are structure, not format, and
// counting them would punish exactly the loggers that separate their records.
func sampleOf(lines []string) []string {
	out := make([]string, 0, min(len(lines), DetectSample))
	for i := len(lines) - 1; i >= 0 && len(out) < DetectSample; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			out = append(out, lines[i])
		}
	}
	return out
}

// claimsGELF requires the fields the GELF spec makes mandatory. This is the
// only thing that separates GELF from any other JSON log: a short_message next
// to a version or a host.
func claimsGELF(line string) bool {
	m, ok := objectOf(line)
	if !ok {
		return false
	}
	if _, ok := m["short_message"]; !ok {
		return false
	}
	_, hasVersion := m["version"]
	_, hasHost := m["host"]
	return hasVersion || hasHost
}

// claimsJSON requires an object that carries a message or a level — the two
// fields the parser reads. A JSON object with neither is data the container
// happened to print, not a log record this format would improve.
func claimsJSON(line string) bool {
	m, ok := objectOf(line)
	if !ok {
		return false
	}
	if _, ok := firstString(m, "message", "msg", "@message", "log"); ok {
		return true
	}
	_, ok = firstString(m, "level", "severity", "loglevel", "lvl", "@level")
	return ok
}

// claimsLogfmt requires a msg or level key, not merely a `key=value` somewhere:
// a URL with a query string, a shell command line and a Java property dump all
// parse as key=value pairs, and none of them is a logfmt record.
func claimsLogfmt(line string) bool {
	_, rest := stripLeadingTimestamp(line)
	kv := parseLogfmtPairs(rest)
	if len(kv) == 0 {
		return false
	}
	for _, k := range []string{"msg", "message", "level", "lvl", "severity", "loglevel"} {
		if v, ok := kv[k]; ok && v != "" {
			return true
		}
	}
	return false
}

// claimsClassic is a level word in a plain line — the same evidence the classic
// parser itself uses, so a sample it claims is a sample it can actually read.
func claimsClassic(line string) bool {
	return classicLevelRe.MatchString(line)
}

// objectOf decodes a line as a JSON object, past a `docker logs --timestamps`
// prefix if one is there.
func objectOf(line string) (map[string]json.RawMessage, bool) {
	_, rest := stripLeadingTimestamp(line)
	return decodeObject(rest)
}
