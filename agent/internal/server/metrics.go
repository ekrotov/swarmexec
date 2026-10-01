// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

// Metrics is an optional observability sink. The default implementation is a
// no-op; a Prometheus-backed implementation lives in internal/metrics and is
// wired in by main when a metrics address is configured.
type Metrics interface {
	SessionStarted()
	SessionEnded()
	AuthDenied()
	BytesTransferred(in, out int64)
	// LimitRefused counts a request refused because a node cap was reached;
	// limit is "streams" or "forward_sidecars".
	LimitRefused(limit string)
}

// NopMetrics satisfies Metrics and does nothing.
type NopMetrics struct{}

func (NopMetrics) SessionStarted()             {}
func (NopMetrics) SessionEnded()               {}
func (NopMetrics) AuthDenied()                 {}
func (NopMetrics) BytesTransferred(_, _ int64) {}
func (NopMetrics) LimitRefused(string)         {}

var _ Metrics = NopMetrics{}
