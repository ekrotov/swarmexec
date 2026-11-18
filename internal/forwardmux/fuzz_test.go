// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package forwardmux

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// The sidecar's stdout is the agent's input, so Read must survive any bytes
// without sizing a buffer from the header (MaxPayload), and a frame it accepts
// must survive a trip through Write.
func FuzzRead(f *testing.F) {
	var ok bytes.Buffer
	_ = Write(&ok, Frame{Conn: 7, Kind: KindData, Payload: []byte("hello")})
	f.Add(ok.Bytes())
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 1, KindOpen, 0, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 1, KindData, 0xff, 0xff, 0xff, 0xff}) // claims 4 GiB
	f.Add([]byte{0, 0, 0, 1, KindData, 0, 0, 0, 9, 'x'})        // truncated payload
	f.Fuzz(func(t *testing.T, data []byte) {
		fr, err := Read(bytes.NewReader(data))
		if err != nil {
			if len(fr.Payload) != 0 {
				t.Fatalf("an error must come with no payload, got %d bytes", len(fr.Payload))
			}
			return
		}
		var buf bytes.Buffer
		if err := Write(&buf, fr); err != nil {
			t.Fatalf("a frame Read accepted must be writable: %v", err)
		}
		back, err := Read(&buf)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("round trip: %v", err)
		}
		if back.Conn != fr.Conn || back.Kind != fr.Kind || !bytes.Equal(back.Payload, fr.Payload) {
			t.Fatalf("round trip changed the frame: %+v -> %+v", fr, back)
		}
	})
}
