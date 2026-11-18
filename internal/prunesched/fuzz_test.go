// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package prunesched

import (
	"reflect"
	"testing"
)

// The schedule is read from container labels the operator (or anyone with
// access to the agent service spec) typed. A label that does not parse must be
// an error — the agent then runs no schedule — and never a panic; one that
// does parse must survive being written back and read again, because the TUI
// edits it that way.
func FuzzParseLabels(f *testing.F) {
	for _, s := range []string{"", "0", "24h", "1h30m", "mon-fri", "mon,wed,sun", "*", "03:30", "24:00", "03:5",
		"images,volumes", "containers,", "dangling=true,label=env", "x y", "-", ",,,", "mon-", "-1h"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		labels := map[string]string{}
		for _, p := range labelParsers {
			labels[p.label] = v
		}
		if s, err := Parse(labels); err == nil {
			again, err := Parse(Apply(nil, s))
			if err != nil {
				t.Fatalf("a parsed schedule wrote labels that do not parse: %v", err)
			}
			if !reflect.DeepEqual(s, again) {
				t.Fatalf("schedule changed on a round trip:\n  %+v\n  %+v", s, again)
			}
		}
		for _, one := range labelParsers {
			_, _ = Parse(map[string]string{one.label: v})
		}
		if m, err := ParseClock(v); err == nil {
			if back, err := ParseClock(FormatClock(m)); err != nil || back != m {
				t.Fatalf("clock %q -> %d -> %q -> %d, %v", v, m, FormatClock(m), back, err)
			}
		}
		if d, err := ParseDays(v); err == nil {
			if back, err := ParseDays(FormatDays(d)); err != nil || back != d {
				t.Fatalf("days %q -> %v -> %q -> %v, %v", v, d, FormatDays(d), back, err)
			}
		}
		if d, err := ParseUntil(v); err == nil {
			if back, err := ParseUntil(FormatUntil(d)); err != nil || back != d {
				t.Fatalf("until %q -> %v -> %q -> %v, %v", v, d, FormatUntil(d), back, err)
			}
		}
		_, _ = ParseCategories(v)
		_, _ = ParseLabelFilters(v)
	})
}
