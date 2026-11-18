// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package leakcheck fails a test binary that finishes with goroutines of its
// own still running — a pool whose idle timer was never stopped, a stream whose
// reader nobody cancelled. It is for packages that start goroutines and promise
// to stop them; use it from TestMain:
//
//	func TestMain(m *testing.M) { leakcheck.Main(m) }
//
// It looks once, after every test has finished, and gives late goroutines a
// moment to exit (they rarely need it): a package that cleans up pays
// nothing. Goroutines the runtime, the test framework and the net/http client
// keep for themselves are ignored.
package leakcheck

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// grace is how long a goroutine that was told to stop may take to do it.
const grace = 3 * time.Second

// Main runs the tests, then each cleanup (for state the tests share and cannot
// tear down one by one, such as servers built by a helper that has no *testing.T),
// then checks for leaks. It does not return.
func Main(m *testing.M, cleanup ...func()) {
	code := m.Run()
	for _, c := range cleanup {
		c()
	}
	if code == 0 {
		if leaked := wait(); len(leaked) > 0 {
			fmt.Fprintf(os.Stderr, "leakcheck: %d goroutine(s) outlived the tests:\n\n%s\n", len(leaked), strings.Join(leaked, "\n\n"))
			code = 1
		}
	}
	os.Exit(code)
}

func wait() []string {
	deadline := time.Now().Add(grace)
	for {
		leaked := leaks()
		if len(leaked) == 0 || time.Now().After(deadline) {
			return leaked
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ignorable are frames of goroutines that are not the code under test's.
var ignorable = []string{
	"testing.(*M).",    // the test main itself
	"testing.Main(",    //
	"leakcheck.leaks(", // the goroutine doing the checking
	"os/signal.signal_recv",
	"os/signal.loop",
	"runtime.ensureSigM",
	"runtime/trace.",
	"internal/poll.runtime_pollWait", // an idle net/http keep-alive reader, below
}

// leaks is the stacks of the goroutines that are still running and are not on
// the ignore list. Idle HTTP keep-alive connections show up as poll waits
// inside net/http transports; they belong to the client library and close on
// their own timeout.
func leaks() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if g = strings.TrimSpace(g); g == "" || ignored(g) {
			continue
		}
		out = append(out, g)
	}
	return out
}

func ignored(stack string) bool {
	for _, frame := range ignorable {
		if strings.Contains(stack, frame) {
			// The poll wait is only ignorable under net/http's own readers.
			if frame == "internal/poll.runtime_pollWait" && !strings.Contains(stack, "net/http.(*persistConn)") {
				continue
			}
			return true
		}
	}
	return false
}
