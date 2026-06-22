package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPrometheusCounters(t *testing.T) {
	p := New()

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
