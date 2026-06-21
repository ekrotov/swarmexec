// Package metrics provides an optional Prometheus-backed implementation of the
// server.Metrics interface (REQUIREMENTS §6, nice-to-have).
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus implements server.Metrics with Prometheus collectors.
type Prometheus struct {
	activeSessions prometheus.Gauge
	totalSessions  prometheus.Counter
	authDenials    prometheus.Counter
	bytesIn        prometheus.Counter
	bytesOut       prometheus.Counter
	registry       *prometheus.Registry
}

// New constructs a Prometheus metrics sink backed by a private registry.
func New() *Prometheus {
	reg := prometheus.NewRegistry()
	p := &Prometheus{
		registry: reg,
		activeSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "swarmexec_active_sessions",
			Help: "Number of currently active exec sessions.",
		}),
		totalSessions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "swarmexec_sessions_total",
			Help: "Total number of exec sessions started.",
		}),
		authDenials: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "swarmexec_auth_denials_total",
			Help: "Total number of authorization denials.",
		}),
		bytesIn: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "swarmexec_bytes_in_total",
			Help: "Total stdin bytes forwarded to containers.",
		}),
		bytesOut: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "swarmexec_bytes_out_total",
			Help: "Total output bytes forwarded to clients.",
		}),
	}
	reg.MustRegister(p.activeSessions, p.totalSessions, p.authDenials, p.bytesIn, p.bytesOut)
	return p
}

func (p *Prometheus) SessionStarted() {
	p.totalSessions.Inc()
	p.activeSessions.Inc()
}

func (p *Prometheus) SessionEnded() { p.activeSessions.Dec() }

func (p *Prometheus) AuthDenied() { p.authDenials.Inc() }

func (p *Prometheus) BytesTransferred(in, out int64) {
	if in > 0 {
		p.bytesIn.Add(float64(in))
	}
	if out > 0 {
		p.bytesOut.Add(float64(out))
	}
}

// Handler returns an HTTP handler exposing the registered metrics.
func (p *Prometheus) Handler() http.Handler {
	return promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{})
}
