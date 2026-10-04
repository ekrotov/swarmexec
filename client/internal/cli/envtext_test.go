// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestEnvTextRoundTrip(t *testing.T) {
	items := []string{
		"LOG_LEVEL=info",
		"EMPTY=",
		"WITH_EQ=a=b",
		"OMNIBUS=external_url 'https://x'\ngitlab_rails['a'] = true",
		"HAS_EOF=first\nEOF\nlast",
		"TRAILING=line\n",
	}
	text := formatEnvText(items)
	if !strings.Contains(text, "OMNIBUS<<EOF\n") || !strings.Contains(text, "HAS_EOF<<EOF2\n") {
		t.Fatalf("heredocs not as expected:\n%s", text)
	}
	if strings.HasSuffix(text, "\n") {
		t.Error("the text ends in a newline")
	}
	got, err := parseEnvText(text)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, items) {
		t.Fatalf("round trip:\n got %q\nwant %q", got, items)
	}
}

func TestParseEnvTextSkipsBlanksAndComments(t *testing.T) {
	got, err := parseEnvText("A=1\r\n\n  # B=2\n#C=3\nD=4\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A=1", "D=4"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseEnvTextErrorsNameTheLine(t *testing.T) {
	cases := []struct {
		name, text string
		line       int
		msg        string
	}{
		{"no equals", "A=1\njust text", 2, "KEY=VALUE"},
		{"empty key", "A=1\n\n=x", 3, "KEY=VALUE"},
		{"duplicate", "A=1\nB=2\nA=3", 3, "already set on line 1"},
		{"unclosed heredoc", "A=1\nB<<EOF\nx\ny", 2, "never closed"},
		{"duplicate after heredoc", "B<<EOF\nx\nEOF\nB=2", 4, "already set on line 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseEnvText(c.text)
			var le *lineError
			if !errors.As(err, &le) {
				t.Fatalf("got %v, want a line error", err)
			}
			if le.line != c.line || !strings.Contains(le.msg, c.msg) {
				t.Fatalf("got %v, want line %d with %q", err, c.line, c.msg)
			}
		})
	}
}

func TestEnvChangeSummary(t *testing.T) {
	got := envChangeSummary([]string{"A=1", "B=2", "C=3"}, []string{"A=1", "B=9", "D=4"})
	want := "added: D\nchanged: B\nremoved: C"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := envChangeSummary([]string{"A=1", "B=2"}, []string{"B=2", "A=1"}); got != "only the order changes" {
		t.Fatalf("reorder: %q", got)
	}
}
