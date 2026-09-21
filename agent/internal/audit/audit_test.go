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
	l := New(slog.New(slog.NewJSONHandler(&buf, nil)), true)
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

// Every record has to say what the identity field is worth. In shared-secret
// mode the operator name is whatever the client put in a header — believing it
// and printing it like a certificate CN invites an accountability claim the
// trail cannot support.
func TestNew_TagsIdentityVerification(t *testing.T) {
	for _, verified := range []bool{true, false} {
		var buf bytes.Buffer
		New(slog.New(slog.NewJSONHandler(&buf, nil)), verified).
			SessionStart("alice", "c1", "svc", []string{"sh"}, true, "1.2.3.4")

		var rec map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
			t.Fatal(err)
		}
		if rec["identity_verified"] != verified {
			t.Errorf("identity_verified = %v, want %v", rec["identity_verified"], verified)
		}
		if rec["identity"] != "alice" {
			t.Errorf("identity = %v", rec["identity"])
		}
	}
}
