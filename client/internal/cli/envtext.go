// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"regexp"
	"strings"
)

// The env editor edits a service's whole environment as one text, a variable
// per line:
//
//	LOG_LEVEL=info
//	# DEBUG=1             (a comment: ignored, a quick way to drop a variable)
//	GITLAB_OMNIBUS_CONFIG<<EOF
//	external_url 'https://gitlab.example.com'
//	EOF
//
// A value with newlines is written as a heredoc, KEY<<DELIM followed by the
// value's lines and a line holding just DELIM — the form GitHub Actions uses
// for multi-line outputs, so it reads as familiar rather than invented.

// heredocStart matches a heredoc opener. The key may hold anything but '=' and
// blanks, the delimiter is a plain word, so a KEY=VALUE line never matches.
var heredocStart = regexp.MustCompile(`^([^=\s]+)<<([A-Za-z_][A-Za-z0-9_]*)$`)

// formatEnvText renders KEY=VALUE entries as the editor's text: one per line,
// multi-line values as heredocs. No trailing newline, so the cursor has no
// empty line to land on below the last variable.
func formatEnvText(items []string) string {
	var b strings.Builder
	for _, it := range items {
		k, v, err := parseEnv(it)
		if err != nil || !strings.Contains(v, "\n") {
			b.WriteString(it)
			b.WriteByte('\n')
			continue
		}
		delim := heredocDelim(v)
		fmt.Fprintf(&b, "%s<<%s\n%s\n%s\n", k, delim, v, delim)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// heredocDelim picks EOF, or EOF2, EOF3 … when the value has a line that
// would end the heredoc early.
func heredocDelim(v string) string {
	lines := map[string]bool{}
	for _, l := range strings.Split(v, "\n") {
		lines[strings.TrimSpace(l)] = true
	}
	d := "EOF"
	for i := 2; lines[d]; i++ {
		d = fmt.Sprintf("EOF%d", i)
	}
	return d
}

// parseEnvText reads the editor's text back into KEY=VALUE entries, in order.
// Blank lines and lines starting with # are skipped. Every error names its
// line, so the editor can put the cursor on it.
func parseEnvText(text string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out []string
	seen := map[string]int{} // key → the line that set it
	add := func(line int, k, v string) error {
		if first, dup := seen[k]; dup {
			return &lineError{line: line, msg: fmt.Sprintf("%s is already set on line %d", k, first)}
		}
		seen[k] = line
		out = append(out, k+"="+v)
		return nil
	}
	for i := 0; i < len(lines); i++ {
		n, l := i+1, lines[i]
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if m := heredocStart.FindStringSubmatch(trimmed); m != nil {
			key, delim := m[1], m[2]
			var body []string
			closed := false
			for i++; i < len(lines); i++ {
				if strings.TrimSpace(lines[i]) == delim {
					closed = true
					break
				}
				body = append(body, lines[i])
			}
			if !closed {
				return nil, &lineError{line: n, msg: fmt.Sprintf("%s<<%s is never closed — end it with a line holding just %s", key, delim, delim)}
			}
			if err := add(n, key, strings.Join(body, "\n")); err != nil {
				return nil, err
			}
			continue
		}
		k, v, err := parseEnv(l)
		if err != nil {
			return nil, &lineError{line: n, msg: "expected KEY=VALUE (or KEY<<EOF for a multi-line value)"}
		}
		if err := add(n, k, v); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// envChangeSummary describes, key by key, what applying after over before
// changes — for the confirmation before the rolling update.
func envChangeSummary(before, after []string) string {
	old := map[string]string{}
	for _, it := range before {
		if k, v, err := parseEnv(it); err == nil {
			old[k] = v
		}
	}
	var added, changed, removed []string
	now := map[string]bool{}
	for _, it := range after {
		k, v, err := parseEnv(it)
		if err != nil {
			continue
		}
		now[k] = true
		switch ov, ok := old[k]; {
		case !ok:
			added = append(added, k)
		case ov != v:
			changed = append(changed, k)
		}
	}
	for _, it := range before {
		if k, _, err := parseEnv(it); err == nil && !now[k] {
			removed = append(removed, k)
		}
	}
	var parts []string
	for _, p := range []struct {
		label string
		keys  []string
	}{{"added", added}, {"changed", changed}, {"removed", removed}} {
		if len(p.keys) > 0 {
			parts = append(parts, p.label+": "+strings.Join(p.keys, ", "))
		}
	}
	if len(parts) == 0 {
		return "only the order changes"
	}
	return strings.Join(parts, "\n")
}
