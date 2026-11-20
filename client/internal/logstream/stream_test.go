// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package logstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestFollowTargetFromTarget(t *testing.T) {
	cases := []struct {
		in      string
		service string
		slot    int
	}{
		{"web", "web", 0},
		{"web.3", "web", 3},
		{" web.3 ", "web", 3},
		{"a.b.2", "a.b", 2},                 // only the trailing numeric segment is a slot
		{"1a2b3c4d5e6f", "1a2b3c4d5e6f", 0}, // a container id: no slot, resolves to nothing later
	}
	for _, c := range cases {
		ft := FollowTargetOf(c.in)
		if ft.Service != c.service || ft.Slot != c.slot {
			t.Errorf("followTargetFromTarget(%q) = {%q,%d}, want {%q,%d}", c.in, ft.Service, ft.Slot, c.service, c.slot)
		}
	}
}

// An open stream's error is an end when the caller stopped — and a stopped
// caller is recognised by the error alone, not only by the context, because
// the deadline's two timers (the context's and the one gRPC derives from it)
// can fire in either order. A live context with a DeadlineExceeded stream is
// exactly the state CI failed in.
func TestStreamEndedIsTheCallerStopping(t *testing.T) {
	live := context.Background()
	done, cancel := context.WithCancel(context.Background())
	cancel()

	// The transport's own wording for a deadline that cancelled the stream.
	rstDeadline := status.Error(codes.DeadlineExceeded, "stream terminated by RST_STREAM with error code: CANCEL")

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"deadline on a live context", live, rstDeadline, true},
		{"cancelled stream", live, status.Error(codes.Canceled, "context canceled"), true},
		{"plain context deadline", live, fmt.Errorf("recv: %w", context.DeadlineExceeded), true},
		{"plain context cancel", live, fmt.Errorf("recv: %w", context.Canceled), true},
		{"end of stream", live, io.EOF, true},
		{"the context ended first", done, errors.New("anything at all"), true},
		{"the agent went away", live, status.Error(codes.Unavailable, "connection refused"), false},
		{"the agent refused", live, status.Error(codes.PermissionDenied, "policy denies logs"), false},
		{"a plain failure", live, errors.New("container logs: no such container"), false},
	}
	for _, c := range cases {
		if got := streamEnded(c.ctx, c.err); got != c.want {
			t.Errorf("%s: streamEnded(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}
