package server

import (
	"sync"
	"time"
)

// Output batching (PRF-02). stdcopy.StdCopy writes once per Docker frame, i.e.
// once per log line; sending a gRPC message per write cost ~8x the throughput
// and ~220x the allocations of the TTY path. outBatcher coalesces consecutive
// writes of the same stream (stdout or stderr) into one message.
//
// Latency bound: a byte that is buffered is sent after at most batchDelay (the
// timer starts with the first buffered byte and is not extended by later
// writes), or at once when the buffer reaches batchMax. A lone line therefore
// reaches the client within batchDelay (5 ms) plus the send itself; nothing
// ever waits for a full buffer. The wire format is unchanged: a chunk is just
// a larger stdout/stderr payload, which receivers already split by line.
const (
	batchMax   = 32 << 10
	batchDelay = 5 * time.Millisecond
)

// outBatcher is safe for concurrent use: Write comes from the copy goroutine,
// flushes from the timer goroutine. send is called with the lock held, so
// messages are never sent concurrently and keep their order. Switching between
// stdout and stderr flushes first, so interleaving is preserved.
type outBatcher struct {
	send  func(stderr bool, p []byte) error
	delay time.Duration

	mu     sync.Mutex
	buf    []byte
	stderr bool
	timer  *time.Timer
	err    error // first send error; sticky
	closed bool
}

func newOutBatcher(send func(stderr bool, p []byte) error) *outBatcher {
	return &outBatcher{send: send, delay: batchDelay}
}

// writer returns an io.Writer for one of the two streams.
func (b *outBatcher) writer(stderr bool) *batchStream { return &batchStream{b: b, stderr: stderr} }

type batchStream struct {
	b      *outBatcher
	stderr bool
}

func (s *batchStream) Write(p []byte) (int, error) { return s.b.write(s.stderr, p) }

func (b *outBatcher) write(stderr bool, p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return 0, b.err
	}
	if len(b.buf) > 0 && b.stderr != stderr {
		if err := b.flushLocked(); err != nil {
			return 0, err
		}
	}
	b.stderr = stderr
	// A write that alone fills a message skips the copy-and-wait.
	if len(b.buf) == 0 && len(p) >= batchMax {
		return b.sendLocked(p)
	}
	b.buf = append(b.buf, p...) // copies: p is the caller's reused buffer
	if len(b.buf) >= batchMax {
		if err := b.flushLocked(); err != nil {
			return 0, err
		}
	} else if b.timer == nil {
		b.timer = time.AfterFunc(b.delay, b.onTimer)
	}
	return len(p), nil
}

func (b *outBatcher) sendLocked(p []byte) (int, error) {
	// send owns its argument: copy out of the caller's buffer.
	cp := make([]byte, len(p))
	copy(cp, p)
	if err := b.send(b.stderr, cp); err != nil {
		b.err = err
		return 0, err
	}
	return len(p), nil
}

func (b *outBatcher) flushLocked() error {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	if len(b.buf) == 0 {
		return nil
	}
	out := b.buf
	b.buf = nil // ownership passes to the message
	if err := b.send(b.stderr, out); err != nil {
		b.err = err
		return err
	}
	return nil
}

func (b *outBatcher) onTimer() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.timer == nil {
		return
	}
	b.timer = nil
	_ = b.flushLocked()
}

// Close sends what is buffered and stops the timer; no send happens after it
// returns, so the handler may use the stream again.
func (b *outBatcher) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.err != nil {
		return b.err
	}
	return b.flushLocked()
}
