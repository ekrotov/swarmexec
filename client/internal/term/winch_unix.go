// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package term

import (
	"os"
	"os/signal"
	"syscall"
)

func notifyResize() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	return ch, func() { signal.Stop(ch) }
}
