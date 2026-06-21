// Package term wraps golang.org/x/term with an idempotent terminal-state
// restorer and terminal-size / resize helpers. The restorer is safe to call
// from defers, signal handlers, panic recovery, and — crucially — explicitly
// before os.Exit (which does not run defers). See REQUIREMENTS §5.
package term

import (
	"os"
	"sync/atomic"

	xterm "golang.org/x/term"
)

// Restorer restores terminal state at most once. The restore action is held as
// a func so it can be injected in tests without a real terminal.
type Restorer struct {
	fn   func()
	done atomic.Bool
}

// newRestorer wraps a restore action.
func newRestorer(fn func()) *Restorer { return &Restorer{fn: fn} }

// IsTerminal reports whether fd refers to a terminal.
func IsTerminal(fd uintptr) bool {
	return xterm.IsTerminal(int(fd))
}

// MakeRaw switches the terminal at fd into raw mode and returns a Restorer that
// puts it back. The Restorer is idempotent.
func MakeRaw(fd uintptr) (*Restorer, error) {
	ifd := int(fd)
	st, err := xterm.MakeRaw(ifd)
	if err != nil {
		return nil, err
	}
	return newRestorer(func() { _ = xterm.Restore(ifd, st) }), nil
}

// Restore returns the terminal to its saved (cooked) state. It runs at most
// once regardless of how many times — or from how many goroutines — it is
// called, so it is safe to wire into every exit path. A nil Restorer is a no-op.
func (r *Restorer) Restore() {
	if r == nil {
		return
	}
	if r.done.CompareAndSwap(false, true) {
		r.fn()
	}
}

// Size returns the current terminal width (columns) and height (rows) for fd.
func Size(fd uintptr) (width, height uint32, err error) {
	cols, rows, err := xterm.GetSize(int(fd))
	if err != nil {
		return 0, 0, err
	}
	return uint32(cols), uint32(rows), nil
}

// NotifyResize subscribes to terminal-resize events for the lifetime of the
// returned stop func. On platforms without SIGWINCH the channel simply never
// fires (best-effort; documented limitation). The channel is buffered and
// coalescing — callers should re-read the size on each tick rather than trust
// the count.
func NotifyResize() (events <-chan os.Signal, stop func()) {
	return notifyResize()
}
