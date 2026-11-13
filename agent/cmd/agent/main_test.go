// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"testing"
)

// Every zero timeout on an http.Server means "wait forever"; the metrics
// endpoint must set all of them.
func TestMetricsServerHasTimeouts(t *testing.T) {
	s := newMetricsServer(":0", http.NewServeMux())
	for name, d := range map[string]int64{
		"ReadHeaderTimeout": int64(s.ReadHeaderTimeout),
		"ReadTimeout":       int64(s.ReadTimeout),
		"WriteTimeout":      int64(s.WriteTimeout),
		"IdleTimeout":       int64(s.IdleTimeout),
	} {
		if d <= 0 {
			t.Errorf("%s is unset", name)
		}
	}
}
