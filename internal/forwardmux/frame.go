// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package forwardmux is the framing between the agent and a port-forward
// sidecar, so several TCP connections can share one sidecar's stdio.
//
// It exists as a shared package rather than as two matching implementations
// because there are already two framing layers on this path and confusing them
// is the failure this design invites. Docker's stdcopy framing separates the
// sidecar's stdout from its stderr; THIS framing separates the connections
// multiplexed inside that stdout. A sender that wrote one and a reader that
// expected the other would produce a stream that looks plausible and corrupts
// payload — so both sides call the same code.
package forwardmux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Frame kinds. The direction gives each its meaning; the numbers are shared so
// a mismatched pair fails loudly rather than misreading a payload.
const (
	// KindOpen — agent to sidecar: dial the target for this connection id.
	KindOpen uint8 = 1
	// KindOpenOK — sidecar to agent: the dial succeeded.
	KindOpenOK uint8 = 2
	// KindOpenErr — sidecar to agent: the dial failed; payload is the reason.
	KindOpenErr uint8 = 3
	// KindData — either direction: payload bytes for this connection.
	KindData uint8 = 4
	// KindCloseWrite — either direction: no more data from this sender, but the
	// other direction stays open. Half-close needs its own frame now that
	// closing the stdio would end every connection, not one.
	KindCloseWrite uint8 = 5
	// KindClose — either direction: this connection is finished.
	KindClose uint8 = 6
)

// HeaderLen is connection id (4) + kind (1) + payload length (4).
const HeaderLen = 9

// MaxPayload bounds one frame. A reader must never size a buffer from an
// attacker-chosen length, and 1 MiB is far above the 32 KiB the copiers use.
const MaxPayload = 1 << 20

// ErrTooLarge is returned for a frame that claims more than MaxPayload.
var ErrTooLarge = errors.New("forwardmux: frame exceeds the maximum payload")

// Frame is one multiplexed message.
type Frame struct {
	Conn    uint32
	Kind    uint8
	Payload []byte
}

// KindName is for logs and errors; an unknown kind reports its number.
func KindName(k uint8) string {
	switch k {
	case KindOpen:
		return "open"
	case KindOpenOK:
		return "open-ok"
	case KindOpenErr:
		return "open-err"
	case KindData:
		return "data"
	case KindCloseWrite:
		return "close-write"
	case KindClose:
		return "close"
	}
	return fmt.Sprintf("kind(%d)", k)
}

// Write encodes one frame. The header and payload go out in a single Write so a
// concurrent writer cannot interleave halfway through — the stdio is shared by
// every connection on this sidecar, and a torn header desynchronises all of
// them, not just the sender.
func Write(w io.Writer, f Frame) error {
	if len(f.Payload) > MaxPayload {
		return ErrTooLarge
	}
	buf := make([]byte, HeaderLen+len(f.Payload))
	binary.BigEndian.PutUint32(buf[0:4], f.Conn)
	buf[4] = f.Kind
	binary.BigEndian.PutUint32(buf[5:9], uint32(len(f.Payload)))
	copy(buf[HeaderLen:], f.Payload)
	_, err := w.Write(buf)
	return err
}

// Read decodes one frame. The returned payload is freshly allocated, so a
// caller may keep it after the next Read — the reason being that these payloads
// travel on to another goroutine, where a reused buffer is a data race that
// shows up as corrupted bytes rather than as a crash.
func Read(r io.Reader) (Frame, error) {
	var hdr [HeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[5:9])
	if n > MaxPayload {
		return Frame{}, ErrTooLarge
	}
	f := Frame{
		Conn: binary.BigEndian.Uint32(hdr[0:4]),
		Kind: hdr[4],
	}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			return Frame{}, err
		}
	}
	return f, nil
}
