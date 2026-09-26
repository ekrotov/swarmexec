// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
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

// Every record names its event in the `event` field — that is the field a
// SIEM filter, an alert rule or `jq 'select(.event == ...)'` keys on, and the
// message text is not a substitute. image.prune once had none, so every rule
// written against `event` silently skipped the one call that deletes images.
//
// The table lists every Logger method; a method missing from it is a record
// nobody checked, which is why TestEveryMethodIsInTheEventTable exists.
var eventTable = map[string]func(*Logger){
	"session_start":     func(l *Logger) { l.SessionStart("a", "c", "s", []string{"sh"}, true, "addr") },
	"session_end":       func(l *Logger) { l.SessionEnd("a", "c", 0, time.Second, 1, 2) },
	"logs_start":        func(l *Logger) { l.LogsStart("a", "c", "s", true, "addr") },
	"logs_end":          func(l *Logger) { l.LogsEnd("a", "c", time.Second, 3) },
	"forward_start":     func(l *Logger) { l.ForwardStart("a", "c", "s", 80, true, "addr") },
	"forward_end":       func(l *Logger) { l.ForwardEnd("a", "c", 80, time.Second, 1, 2) },
	"volume_remove":     func(l *Logger) { l.VolumeRemove("a", "v", true, "") },
	"volume_create":     func(l *Logger) { l.VolumeCreate("a", "v", true, "") },
	"image_prune":       func(l *Logger) { l.ImagePrune("a", true, 10, 1, true, "") },
	"event_watch_start": func(l *Logger) { l.EventWatchStart("a", "c", "s", "addr") },
	"event_watch_end":   func(l *Logger) { l.EventWatchEnd("a", "c", time.Second, 4) },
	"container_list":    func(l *Logger) { l.ContainerList("a", "web", 1) },
	"auth_decision":     func(l *Logger) { l.AuthDecision("a", "c", "s", false, "no") },
}

func TestEveryRecordNamesItsEvent(t *testing.T) {
	for event, emit := range eventTable {
		t.Run(event, func(t *testing.T) {
			l, buf := newCapture()
			emit(l)
			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("not one JSON record: %v\n%s", err, buf.String())
			}
			if rec["event"] != event {
				t.Errorf("event = %v, want %q", rec["event"], event)
			}
			if rec["msg"] != event {
				t.Errorf("msg = %v, want %q: message and event name the same thing", rec["msg"], event)
			}
			if rec["component"] != "audit" || rec["identity"] != "a" {
				t.Errorf("component/identity missing: %v", rec)
			}
			if _, ok := rec["identity_verified"]; !ok {
				t.Errorf("identity_verified missing: %v", rec)
			}
		})
	}
}

func TestEveryMethodIsInTheEventTable(t *testing.T) {
	methods := reflect.TypeOf(&Logger{}).NumMethod()
	if methods != len(eventTable) {
		t.Errorf("Logger has %d methods, eventTable lists %d — add the new record to the table",
			methods, len(eventTable))
	}
}
