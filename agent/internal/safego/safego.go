// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package safego starts goroutines that cannot take the agent down.
//
// The gRPC recovery interceptors only see a handler's own goroutine; a panic in
// a goroutine the handler (or a background loop) starts ends the process, and
// with it every shell and forward open on the node. Go wraps such a goroutine
// in a recover that logs the panic with its stack instead.
package safego

import (
	"log/slog"
	"runtime/debug"
)

// Go runs fn in a new goroutine. A panic in fn is logged (name says which
// goroutine) and then each onPanic hook runs, so the owner can tear down what
// the dead goroutine was serving. A nil log falls back to slog.Default.
func Go(log *slog.Logger, name string, fn func(), onPanic ...func()) {
	go func() {
		defer Recover(log, name, onPanic...)
		fn()
	}()
}

// Recover is the deferred half of Go, for a goroutine that is already written
// as a closure: `go func() { defer safego.Recover(log, "name"); ... }()`. It
// must be deferred directly, because recover only works in the deferred call.
func Recover(log *slog.Logger, name string, onPanic ...func()) {
	r := recover()
	if r == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	log.Error("goroutine panicked; recovered", "goroutine", name, "panic", r, "stack", string(debug.Stack()))
	for _, h := range onPanic {
		h()
	}
}
