// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"strings"
	"testing"

	"swarmexec/client/internal/logfmt"
)

func TestBuildLogFilter(t *testing.T) {
	f, flt, err := buildLogFilter("json", "warn", "timeout")
	if err != nil {
		t.Fatal(err)
	}
	if f.Name() != "json" || flt.MinLevel != logfmt.LevelWarn || flt.Grep == nil {
		t.Errorf("unexpected: format=%s level=%v grep=%v", f.Name(), flt.MinLevel, flt.Grep)
	}
	if _, _, err := buildLogFilter("nope", "", ""); err == nil {
		t.Error("unknown format should error")
	}
	if _, _, err := buildLogFilter("", "loud", ""); err == nil {
		t.Error("unknown level should error")
	}
	if _, _, err := buildLogFilter("", "", "("); err == nil {
		t.Error("invalid regexp should error")
	}
}

func TestFilterWriter_JSONFilterAndRender(t *testing.T) {
	var out bytes.Buffer
	_, flt, _ := buildLogFilter("json", "warn", "")
	w := newFilterWriter(&out, logfmt.JSON, flt, renderPlain)

	// Two lines: one INFO (dropped), one ERROR (kept, reformatted to LEVEL msg).
	w.Write([]byte(`{"level":"info","message":"starting"}` + "\n"))
	w.Write([]byte(`{"level":"error","message":"boom"}` + "\n"))
	got := out.String()
	if strings.Contains(got, "starting") {
		t.Errorf("INFO line should be filtered out, got %q", got)
	}
	if strings.TrimSpace(got) != "ERROR  boom" {
		t.Errorf("kept line render = %q, want %q", strings.TrimSpace(got), "ERROR  boom")
	}
}

func TestFilterWriter_PartialLineFlush(t *testing.T) {
	var out bytes.Buffer
	w := newFilterWriter(&out, logfmt.Raw, logfmt.Filter{}, renderPlain)
	w.Write([]byte("first\nno-newline-yet"))
	if got := out.String(); strings.TrimSpace(got) != "first" {
		t.Errorf("before flush = %q, want just 'first'", got)
	}
	w.Flush()
	if !strings.Contains(out.String(), "no-newline-yet") {
		t.Errorf("flush should emit the buffered partial line, got %q", out.String())
	}
}
