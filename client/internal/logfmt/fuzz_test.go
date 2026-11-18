// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package logfmt

import (
	"strings"
	"testing"
)

// A container writes whatever it likes to stdout, so every format must take any
// line — invalid UTF-8, a huge number, a bracket that never closes — and keep
// the original text: the viewer shows Raw when it cannot do better.
func FuzzFormatsParse(f *testing.F) {
	for _, s := range []string{
		"",
		"plain text with ERROR in it",
		"2026-11-18T10:00:00.123456789Z [warn] disk almost full",
		`{"level":"info","msg":"listening","addr":":8080"}`,
		`{"version":"1.1","host":"n","short_message":"x","level":4,"_k":{"a":1}}`,
		`{"level":`,
		`time=2026-11-18T10:00:00Z level=error msg="quoted \" value" k=v k2="`,
		`1731924000.5 <3>sys message`,
		"\xff\xfe\x00 not utf-8",
		`{"level":1e999,"msg":null}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		for _, fm := range Formats() {
			e := fm.Parse(line)
			if e.Raw != line {
				t.Fatalf("%s: Raw = %q, want the input %q", fm.Name(), e.Raw, line)
			}
		}
		// Detection reads a sample of lines; a verdict must be a real format.
		if got, ok := Detect([]string{line, line, line, line, line, line, line, line, line, line}); ok && got == nil {
			t.Fatal("Detect reached a verdict without a format")
		}
		_ = ParseLevel(strings.TrimSpace(line))
	})
}
