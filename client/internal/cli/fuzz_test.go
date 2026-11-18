// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"reflect"
	"testing"

	"swarmexec/client/internal/portfwd"
)

// The env editor's text is typed (or pasted) by the operator. Whatever parses
// must render back to text that parses to the same entries: the editor
// re-renders after every apply, and a value that changed on the way would be a
// silent edit of a running service.
func FuzzParseEnvText(f *testing.F) {
	for _, s := range []string{
		"",
		"A=1\nB=two words\n# comment\n\nC=",
		"KEY<<EOF\nline one\nline two\nEOF\nB=1",
		"KEY<<EOF\nEOF\nEOF",
		"KEY<<EOF\nnever closed",
		"A=1\nA=2",
		"=novalue",
		"A=1\r\nB=2\r\n",
		"K<<X\n\nX\nL<<X2\nX\nX2",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		items, err := parseEnvText(text)
		if err != nil {
			return
		}
		again, err := parseEnvText(formatEnvText(items))
		if err != nil {
			t.Fatalf("rendered text does not parse: %v\n%q", err, formatEnvText(items))
		}
		if !reflect.DeepEqual(items, again) {
			t.Fatalf("env changed on a round trip:\n  %q\n  %q", items, again)
		}
	})
}

// Port arguments come straight from the command line and from the forward
// form: a spec is either an error or a pair of real ports.
func FuzzPortSpecs(f *testing.F) {
	for _, s := range []string{"8080", "9090:8080", "0", "65535:1", "65536", ":", "1:", ":1", " 80 : 90 ", "-1", "1e3", "٣"} {
		f.Add(s, s, s)
	}
	f.Fuzz(func(t *testing.T, spec, address, local string) {
		l, r, err := portfwd.ParsePortSpec(spec)
		if err == nil && (l == 0 || l > 65535 || r == 0 || r > 65535) {
			t.Fatalf("portfwd.ParsePortSpec(%q) = %d, %d without an error", spec, l, r)
		}
		fs, err := parseForwardForm(spec, address, local)
		if err == nil && (fs.local == 0 || fs.local > 65535 || fs.remote == 0 || fs.remote > 65535) {
			t.Fatalf("parseForwardForm(%q, %q, %q) = %+v without an error", spec, address, local, fs)
		}
	})
}
