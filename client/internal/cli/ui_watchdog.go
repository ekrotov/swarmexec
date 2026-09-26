// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"time"

	"swarmexec/client/internal/clientlog"
)

// watchdog times how long the event loop takes to service a no-op every two
// seconds and logs a busy or stalled loop, so "the UI does not react" leaves a
// trace in the log viewer and the log file. QueueUpdate runs on its own
// goroutine so a stuck loop cannot block the measurement.
func (u *ui) watchdog(ctx context.Context) {
	app := u.app
	tk := time.NewTicker(2 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			sent := time.Now()
			done := make(chan struct{})
			go func() { app.QueueUpdate(func() {}); close(done) }()
			select {
			case <-done:
				if d := time.Since(sent); d > uiStallWarn {
					clientlog.L().Warn("ui event loop was busy", "blocked_ms", d.Milliseconds())
				}
			case <-time.After(uiStallWarn):
				clientlog.L().Warn("ui event loop stalled", "over_ms", uiStallWarn.Milliseconds())
				<-done
				clientlog.L().Warn("ui event loop recovered", "after_ms", time.Since(sent).Milliseconds())
			case <-ctx.Done():
				return
			}
		}
	}
}
