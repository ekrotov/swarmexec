// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/session"
)

// deadline fails the test when fn does not return in time: the regression
// these tests guard against is a hang, so it must show up as a failure.
func deadline(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked", what)
	}
}

func keyEvent(r rune) *tcell.EventKey { return tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone) }

// A key typed while the dial is still pending, and after it failed, must not
// block the caller (the UI loop), and the failure must reach onDone.
func TestTerminalDialTimeoutKeyDoesNotBlock(t *testing.T) {
	v := newTerminalView(tview.NewApplication())
	v.dial = func(ctx context.Context, _ string) (*agentcall.Conn, error) {
		<-ctx.Done() // a node that never answers
		return nil, ctx.Err()
	}
	type res struct {
		code int
		err  error
	}
	done := make(chan res, 1)
	v.run(context.Background(), nil, terminalSession{}, 50*time.Millisecond, func(c int, e error) { done <- res{c, e} })

	h := v.InputHandler()
	deadline(t, "typing during the dial", func() {
		for i := 0; i < 1000; i++ { // more than the queue holds
			h(keyEvent('a'), nil)
		}
	})
	select {
	case r := <-done:
		if r.err == nil || r.code != session.TransportFailure {
			t.Fatalf("onDone = %d, %v", r.code, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onDone never called")
	}
	deadline(t, "typing after the failure", func() {
		for i := 0; i < 1000; i++ {
			h(keyEvent('b'), nil)
		}
	})
}

// Keys arriving while the pane closes (the session context ends) never block,
// and the writer goroutine stops and closes the pipe.
func TestTerminalPaneCloseWhileKeysArrive(t *testing.T) {
	v := newTerminalView(tview.NewApplication())
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { v.pumpInput(ctx, pw); close(stopped) }()
	h := v.InputHandler()
	deadline(t, "typing while closing", func() {
		for i := 0; i < 2000; i++ {
			if i == 100 {
				cancel()
			}
			h(keyEvent('x'), nil)
		}
	})
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("writer goroutine did not stop")
	}
	deadline(t, "reader seeing the close", func() { _, _ = io.Copy(io.Discard, pr) })
}

// Queued keys reach the session reader in order once it is reading.
func TestTerminalInputReachesReader(t *testing.T) {
	v := newTerminalView(tview.NewApplication())
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go v.pumpInput(ctx, pw)
	h := v.InputHandler()
	h(keyEvent('h'), nil)
	h(keyEvent('i'), nil)
	buf := make([]byte, 2)
	deadline(t, "reading queued keys", func() { _, _ = io.ReadFull(pr, buf) })
	if string(buf) != "hi" {
		t.Fatalf("got %q", buf)
	}
}

// Draw with a changed pane size racing vt.Write output that makes the
// emulator reply (device status / cursor position): the two locks must not
// deadlock.
func TestTerminalDrawResizeVsDeviceStatusQuery(t *testing.T) {
	v := newTerminalView(tview.NewApplication())
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(120, 40)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = v.vt.Write([]byte("out\x1b[6n\x1b[5n"))
		}
	}()
	deadline(t, "Draw/Write race", func() {
		for i := 0; i < 300; i++ {
			v.SetRect(0, 0, 20+i%50, 10+i%20) // a new size every frame
			v.Draw(screen)
		}
	})
	close(stop)
	deadline(t, "writer stop", wg.Wait)
}

// The emulator's reply is queued, not written: no reader, no block.
func TestReplyWriterDoesNotBlock(t *testing.T) {
	v := newTerminalView(tview.NewApplication())
	deadline(t, "reply writer", func() {
		for i := 0; i < 1000; i++ {
			_, _ = v.vt.Write([]byte("\x1b[6n"))
		}
	})
	if len(v.keys) == 0 {
		t.Fatal("no reply queued")
	}
}
