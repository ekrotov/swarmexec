// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"io"
	"strings"
	"testing"
	"time"
)

// SEC-08: a set-but-invalid environment value aborts the start, like an invalid
// flag. It used to fall back to the default silently — for
// SWARMEXEC_ALLOW_LEGACY_SECRET (default true) that left the unsafe path open.
func TestParse_InvalidEnvIsAnError(t *testing.T) {
	cases := []struct{ key, val string }{
		{"SWARMEXEC_ALLOW_LEGACY_SECRET", "flase"},
		{"SWARMEXEC_SELF_SIGNED", "maybe"},
		{"SWARMEXEC_MAX_STREAMS", "ten"},
		{"SWARMEXEC_PORT", "94x3"},
		{"SWARMEXEC_DRAIN_TIMEOUT", "5"}, // a duration needs a unit
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			t.Setenv(c.key, c.val)
			_, err := Parse(nil, io.Discard)
			if err == nil {
				t.Fatalf("%s=%q was accepted", c.key, c.val)
			}
			if !strings.Contains(err.Error(), c.key) {
				t.Errorf("the error does not name the variable: %v", err)
			}
		})
	}
}

func TestParse_ValidEnvStillWorks(t *testing.T) {
	t.Setenv("SWARMEXEC_ALLOW_LEGACY_SECRET", " No ")
	t.Setenv("SWARMEXEC_DRAIN_TIMEOUT", "7s")
	t.Setenv("SWARMEXEC_MAX_STREAMS", "12")
	c, err := Parse(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.AllowLegacySecret || c.DrainTimeout != 7*time.Second || c.MaxStreams != 12 {
		t.Errorf("env not applied: %+v", c)
	}
}

func TestParse_InvalidEnvDoesNotBlockVersion(t *testing.T) {
	t.Setenv("SWARMEXEC_ALLOW_LEGACY_SECRET", "flase")
	c, err := Parse([]string{"-version"}, io.Discard)
	if err != nil || !c.ShowVersion {
		t.Errorf("-version with a bad env: %v %+v", err, c)
	}
}

func TestWeakSecret(t *testing.T) {
	for secret, want := range map[string]bool{
		"": false, "short": true, "fifteen-chars-x": true, "sixteen-chars-xx": false,
	} {
		if got := WeakSecret(secret); got != want {
			t.Errorf("WeakSecret(%q) = %v, want %v", secret, got, want)
		}
	}
}
