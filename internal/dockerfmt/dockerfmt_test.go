// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package dockerfmt

import "testing"

func TestShortID(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "",
		"abc":                               "abc",
		"abcdef012345":                      "abcdef012345", // exactly 12: unchanged
		"abcdef0123456789":                  "abcdef012345",
		"sha256:abcdef0123456789abcdef0123": "sha256:abcde",
	} {
		if got := ShortID(in); got != want {
			t.Errorf("ShortID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOrDash(t *testing.T) {
	if got := OrDash(""); got != "-" {
		t.Errorf("OrDash(\"\") = %q, want a dash", got)
	}
	if got := OrDash(" "); got != " " {
		t.Errorf("OrDash(%q) = %q; only an EMPTY value is unknown", " ", got)
	}
	if got := OrDash("web"); got != "web" {
		t.Errorf("OrDash(%q) = %q", "web", got)
	}
}

// Unknown is not zero: a size the daemon did not report must not read as an
// empty volume.
func TestBytesSeparatesUnknownFromZero(t *testing.T) {
	if got := Bytes(-1); got != "-" {
		t.Errorf("Bytes(-1) = %q, want a dash", got)
	}
	if got := Bytes(0); got == "-" {
		t.Error("Bytes(0) rendered as unknown")
	}
	if got := Bytes(1536); got == "" {
		t.Error("Bytes(1536) rendered nothing")
	}
}
