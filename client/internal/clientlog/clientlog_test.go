// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package clientlog

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRingCap(t *testing.T) {
	r := newRing(3)
	for i := 0; i < 5; i++ {
		_, _ = r.Write([]byte(fmt.Sprintf("line%d\n", i)))
	}
	got := r.Lines()
	if len(got) != 3 || got[0] != "line2" || got[2] != "line4" {
		t.Errorf("ring kept %v, want [line2 line3 line4]", got)
	}
}

func TestInitLevelFilter(t *testing.T) {
	ring, err := Init("info", "")
	if err != nil {
		t.Fatal(err)
	}
	L().Debug("should-be-filtered")
	L().Info("should-appear")
	joined := strings.Join(ring.Lines(), "\n")
	if strings.Contains(joined, "should-be-filtered") {
		t.Errorf("debug record leaked at info level: %q", joined)
	}
	if !strings.Contains(joined, "should-appear") {
		t.Errorf("info record missing: %q", joined)
	}
}

func TestTimedLevels(t *testing.T) {
	ring, err := Init("debug", "")
	if err != nil {
		t.Fatal(err)
	}
	Timed("slowop", time.Now().Add(-time.Second), nil) // slow success → warn
	Timed("fastop", time.Now(), nil)                   // fast success → debug
	Timed("errop", time.Now(), errors.New("boom"))     // error → error
	joined := strings.Join(ring.Lines(), "\n")
	for _, want := range []string{
		"level=WARN",
		"op=slowop",
		"level=DEBUG",
		"op=fastop",
		"level=ERROR",
		"op=errop",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("log missing %q in:\n%s", want, joined)
		}
	}
}
