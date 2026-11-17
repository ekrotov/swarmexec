// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"context"
	"io"
	"testing"

	pb "swarmexec/internal/pb"
)

// Without stdin the stream is half-closed right after StartExec, so a command
// that reads stdin sees EOF instead of hanging until the timeout (NEB-07).
func TestRunWithoutStdinHalfClosesAfterStart(t *testing.T) {
	fs := &fakeStream{script: []*pb.ServerMessage{srvExit(0)}}
	var out, errb bytes.Buffer
	opts := baseOpts(&out, &errb)
	opts.Stdin = nil

	if _, err := Run(context.Background(), fs, opts); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !fs.closeSent {
		t.Error("CloseSend was not called although there is no stdin")
	}
	if len(fs.sent) != 1 {
		t.Errorf("sent %d messages, want only StartExec", len(fs.sent))
	}
}

// With stdin the half-close waits for the reader's EOF, not before.
func TestRunWithStdinDoesNotCloseEarly(t *testing.T) {
	fs := &fakeStream{script: []*pb.ServerMessage{srvExit(0)}}
	var out, errb bytes.Buffer
	opts := baseOpts(&out, &errb)
	pr, pw := io.Pipe()
	opts.Stdin = pr
	defer pw.Close()

	if _, err := Run(context.Background(), fs, opts); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.closeSent {
		t.Error("CloseSend called while stdin is still open")
	}
}
