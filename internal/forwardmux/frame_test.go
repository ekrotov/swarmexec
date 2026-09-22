// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package forwardmux

import (
	"bytes"
	"io"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := []Frame{
		{Conn: 1, Kind: KindOpen},
		// Binary payload including NUL and 0xff: the whole point of this path is
		// that it is byte-transparent.
		{Conn: 1, Kind: KindData, Payload: []byte{0x00, 0x01, 'H', 'T', 'T', 'P', 0xff}},
		{Conn: 4294967295, Kind: KindCloseWrite},
		{Conn: 2, Kind: KindOpenErr, Payload: []byte("connection refused")},
		{Conn: 2, Kind: KindClose},
	}
	for _, f := range in {
		if err := Write(&buf, f); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range in {
		got, err := Read(&buf)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if got.Conn != want.Conn || got.Kind != want.Kind || !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("frame %d: got %+v, want %+v", i, got, want)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("%d bytes left over", buf.Len())
	}
}

// A payload must not alias the reader's buffer: it travels on to another
// goroutine, where reuse shows up as corrupted bytes rather than as a crash.
func TestReadAllocatesItsPayload(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("first")
	if err := Write(&buf, Frame{Conn: 1, Kind: KindData, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := Write(&buf, Frame{Conn: 1, Kind: KindData, Payload: []byte("SECOND")}); err != nil {
		t.Fatal(err)
	}
	first, _ := Read(&buf)
	second, _ := Read(&buf)
	if string(first.Payload) != "first" {
		t.Errorf("the first payload was overwritten by the second: %q", first.Payload)
	}
	if string(second.Payload) != "SECOND" {
		t.Errorf("second payload = %q", second.Payload)
	}
}

// A length is attacker-influenced in the sense that matters here: a desynced or
// corrupted stream yields an arbitrary number, and sizing a buffer from it
// would turn a framing bug into an allocation the node cannot survive.
func TestReadRefusesAnOversizedFrame(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 1, KindData, 0xff, 0xff, 0xff, 0xff})
	if _, err := Read(&buf); err != ErrTooLarge {
		t.Errorf("want ErrTooLarge, got %v", err)
	}
	if err := Write(io.Discard, Frame{Kind: KindData, Payload: make([]byte, MaxPayload+1)}); err != ErrTooLarge {
		t.Errorf("Write: want ErrTooLarge, got %v", err)
	}
}

// A truncated stream must report EOF, not a half-read frame — the reader is
// what decides a sidecar has gone away.
func TestReadOnTruncatedStream(t *testing.T) {
	var buf bytes.Buffer
	_ = Write(&buf, Frame{Conn: 1, Kind: KindData, Payload: []byte("abcdef")})
	truncated := bytes.NewReader(buf.Bytes()[:HeaderLen+2])
	if _, err := Read(truncated); err == nil {
		t.Error("a truncated payload must be an error")
	}
	if _, err := Read(bytes.NewReader(nil)); err != io.EOF {
		t.Errorf("empty stream: want EOF, got %v", err)
	}
}

// The header goes out with its payload in one Write. The stdio is shared by
// every connection on the sidecar, so a torn header desynchronises all of them.
func TestWriteIsASingleCall(t *testing.T) {
	c := &countingWriter{}
	if err := Write(c, Frame{Conn: 7, Kind: KindData, Payload: bytes.Repeat([]byte("x"), 4096)}); err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 {
		t.Errorf("Write made %d calls, want 1", c.calls)
	}
}

type countingWriter struct{ calls int }

func (c *countingWriter) Write(p []byte) (int, error) { c.calls++; return len(p), nil }

func TestKindName(t *testing.T) {
	if KindName(KindOpen) != "open" || KindName(KindClose) != "close" {
		t.Error("known kinds should be named")
	}
	if KindName(200) != "kind(200)" {
		t.Errorf("unknown kind = %q", KindName(200))
	}
}
