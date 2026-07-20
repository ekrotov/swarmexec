// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newCapture() (*Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	l := New(slog.New(slog.NewJSONHandler(&buf, nil)))
	return l, &buf
}

func TestSessionStart_NoPayload(t *testing.T) {
	l, buf := newCapture()
	l.SessionStart("alice", "c123", "web", []string{"bash", "-l"}, true, "10.0.0.5:5000")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	if rec["event"] != "session_start" || rec["identity"] != "alice" || rec["component"] != "audit" {
		t.Errorf("unexpected record: %v", rec)
	}
	if rec["container_id"] != "c123" || rec["service"] != "web" || rec["tty"] != true {
		t.Errorf("missing fields: %v", rec)
	}
}

func TestVolumeRemove(t *testing.T) {
	l, buf := newCapture()
	l.VolumeRemove("bob", "data", false, "volume is in use")
	s := buf.String()
	for _, want := range []string{`"event":"volume_remove"`, `"identity":"bob"`, `"volume":"data"`, `"ok":false`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
}

func TestSessionEnd(t *testing.T) {
	l, buf := newCapture()
	l.SessionEnd("alice", "c123", 0, 2*time.Second, 10, 20)
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["event"] != "session_end" {
		t.Errorf("event = %v", rec["event"])
	}
}
