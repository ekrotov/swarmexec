// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package term

import "os"

// Windows has no SIGWINCH. We return a channel that never fires; live resize is
// a documented best-effort limitation (REQUIREMENTS §5, §10). Operators on
// Windows should re-launch the session if they resize their terminal.
func notifyResize() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal)
	return ch, func() {}
}
