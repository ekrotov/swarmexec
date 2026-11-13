// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package metrics provides an optional Prometheus-backed implementation of the
// server.Metrics interface (REQUIREMENTS §6, nice-to-have).
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// StreamMethods are the "method" label values of the per-stream series: the
// agent's streaming RPCs. The server package checks it against its stream gate.
var StreamMethods = []string{"Exec", "Logs", "PortForward", "WatchContainerEvents"}

// Prometheus implements server.Metrics with Prometheus collectors.
type Prometheus struct {
	activeSessions prometheus.Gauge
	totalSessions  prometheus.Counter
	authDenials    prometheus.Counter
	bytesIn        prometheus.Counter
	bytesOut       prometheus.Counter
	limitRefusals  *prometheus.CounterVec
	streamsTotal   *prometheus.CounterVec
	activeStreams  *prometheus.GaugeVec
	registry       *prometheus.Registry
}

// New constructs a Prometheus metrics sink backed by a private registry.
// New builds the agent's metrics. version and protocol are published as
// swarmexec_agent_info, the one series a cluster-wide query can compare across
// nodes ("are all agents on the same build?").
func New(version, protocol string) *Prometheus {
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
			Help: "Total bytes received from clients and passed on (exec stdin, port-forward).",
		}),
		bytesOut: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "swarmexec_bytes_out_total",
			Help: "Total bytes sent to clients (exec output, logs, port-forward).",
		}),
	}
	p.limitRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "swarmexec_limit_refusals_total",
		Help: "Requests refused because a node cap was reached (limit = streams | forward_sidecars).",
	}, []string{"limit"})
	// Both label values exist from the start, so a dashboard sees 0 rather than
	// "no data" on a node that has never been refused.
	p.limitRefusals.WithLabelValues("streams")
	p.limitRefusals.WithLabelValues("forward_sidecars")
	p.streamsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "swarmexec_streams_total",
		Help: "Streaming RPCs admitted past the drain and the stream cap, by method.",
	}, []string{"method"})
	p.activeStreams = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "swarmexec_active_streams",
		Help: "Streaming RPCs currently open, by method.",
	}, []string{"method"})
	// Same reasoning as the refusals: every method has a series from the first
	// scrape.
	for _, m := range StreamMethods {
		p.streamsTotal.WithLabelValues(m)
		p.activeStreams.WithLabelValues(m)
	}
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "swarmexec_agent_info",
		Help: "Constant 1, labelled with the agent build and wire-protocol version.",
	}, []string{"version", "protocol"})
	info.WithLabelValues(version, protocol).Set(1)
	reg.MustRegister(p.activeSessions, p.totalSessions, p.authDenials, p.bytesIn, p.bytesOut, p.limitRefusals,
		p.streamsTotal, p.activeStreams, info)
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

func (p *Prometheus) LimitRefused(limit string) { p.limitRefusals.WithLabelValues(limit).Inc() }

func (p *Prometheus) StreamStarted(method string) {
	p.streamsTotal.WithLabelValues(method).Inc()
	p.activeStreams.WithLabelValues(method).Inc()
}

func (p *Prometheus) StreamEnded(method string) { p.activeStreams.WithLabelValues(method).Dec() }

// Handler returns an HTTP handler exposing the registered metrics.
func (p *Prometheus) Handler() http.Handler {
	return promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{})
}
