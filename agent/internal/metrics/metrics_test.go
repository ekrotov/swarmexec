// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPrometheusCounters(t *testing.T) {
	p := New("v1.2.3", "swarmexec/v1")

	p.SessionStarted()
	p.SessionStarted()
	p.SessionEnded()
	if got := testutil.ToFloat64(p.totalSessions); got != 2 {
		t.Errorf("total sessions = %v, want 2", got)
	}
	if got := testutil.ToFloat64(p.activeSessions); got != 1 {
		t.Errorf("active sessions = %v, want 1", got)
	}

	p.AuthDenied()
	p.AuthDenied()
	if got := testutil.ToFloat64(p.authDenials); got != 2 {
		t.Errorf("auth denials = %v, want 2", got)
	}

	p.BytesTransferred(100, 250)
	p.BytesTransferred(-5, 0) // non-positive values are ignored
	if got := testutil.ToFloat64(p.bytesIn); got != 100 {
		t.Errorf("bytes in = %v, want 100", got)
	}
	if got := testutil.ToFloat64(p.bytesOut); got != 250 {
		t.Errorf("bytes out = %v, want 250", got)
	}
}

// What a scrape actually returns: the info series carries the build, and both
// refusal series exist at 0 before anything was refused, so an alert on a rate
// has a series to work with from the first scrape.
func TestScrapeCarriesInfoAndRefusals(t *testing.T) {
	p := New("v1.2.3", "swarmexec/v1")
	p.LimitRefused("streams")
	p.LimitRefused("streams")

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{
		`swarmexec_agent_info{protocol="swarmexec/v1",version="v1.2.3"} 1`,
		`swarmexec_limit_refusals_total{limit="streams"} 2`,
		`swarmexec_limit_refusals_total{limit="forward_sidecars"} 0`,
		`swarmexec_auth_denials_total 0`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("scrape is missing %q:\n%s", want, body)
		}
	}
}
