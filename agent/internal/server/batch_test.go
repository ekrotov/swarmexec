// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

type sent struct {
	stderr bool
	p      string
}

type sink struct {
	mu   sync.Mutex
	got  []sent
	fail error
	ch   chan struct{}
}

func newSink() *sink { return &sink{ch: make(chan struct{}, 16)} }

func (s *sink) send(stderr bool, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.got = append(s.got, sent{stderr, string(p)})
	s.ch <- struct{}{}
	return nil
}

func (s *sink) all() []sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sent(nil), s.got...)
}

func TestBatcherFlushesOnTimer(t *testing.T) {
	s := newSink()
	b := newOutBatcher(s.send)
	b.delay = 10 * time.Millisecond
	w := b.writer(false)
	_, _ = w.Write([]byte("a\n"))
	_, _ = w.Write([]byte("b\n"))
	select {
	case <-s.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("buffered bytes never flushed")
	}
	if g := s.all(); len(g) != 1 || g[0].p != "a\nb\n" {
		t.Fatalf("got %v, want one coalesced message", g)
	}
	_ = b.Close()
}

func TestBatcherFlushesAtMaxAndCopiesInput(t *testing.T) {
	s := newSink()
	b := newOutBatcher(s.send)
	b.delay = time.Hour
	w := b.writer(false)
	buf := bytes.Repeat([]byte("x"), batchMax/2)
	_, _ = w.Write(buf)
	buf[0] = 'Y' // the caller reuses its buffer
	_, _ = w.Write(buf)
	g := s.all()
	if len(g) != 1 || len(g[0].p) != batchMax || g[0].p[0] != 'x' {
		t.Fatalf("got %d messages, want one full message with the original bytes", len(g))
	}
	// A single oversize write goes out whole and unbuffered.
	_, _ = w.Write(bytes.Repeat([]byte("z"), 2*batchMax))
	if g := s.all(); len(g) != 2 || len(g[1].p) != 2*batchMax {
		t.Fatalf("oversize write: %d messages", len(g))
	}
	_ = b.Close()
}

func TestBatcherKeepsStreamOrderAndCloseFlushes(t *testing.T) {
	s := newSink()
	b := newOutBatcher(s.send)
	b.delay = time.Hour
	_, _ = b.writer(false).Write([]byte("o1"))
	_, _ = b.writer(true).Write([]byte("e1"))
	_, _ = b.writer(false).Write([]byte("o2"))
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	want := []sent{{false, "o1"}, {true, "e1"}, {false, "o2"}}
	g := s.all()
	if len(g) != len(want) {
		t.Fatalf("got %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("got %v, want %v", g, want)
		}
	}
}

func TestBatcherSendErrorIsSticky(t *testing.T) {
	s := newSink()
	b := newOutBatcher(s.send)
	b.delay = time.Hour
	boom := errors.New("stream gone")
	s.fail = boom
	w := b.writer(false)
	_, _ = w.Write([]byte("a"))
	if err := b.Close(); !errors.Is(err, boom) {
		t.Fatalf("Close = %v, want the send error", err)
	}
	if _, err := w.Write([]byte("b")); !errors.Is(err, boom) {
		t.Fatalf("Write after failure = %v, want the send error", err)
	}
}

func TestBatcherNoSendAfterClose(t *testing.T) {
	s := newSink()
	b := newOutBatcher(s.send)
	b.delay = 5 * time.Millisecond
	_, _ = b.writer(false).Write([]byte("a"))
	_ = b.Close()
	time.Sleep(30 * time.Millisecond)
	if n := len(s.all()); n != 1 {
		t.Fatalf("%d messages, want exactly the Close flush", n)
	}
}
