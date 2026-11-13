// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dockerStatus is the one place a Docker Engine error becomes the gRPC status a
// client sees. Every handler that hands an Engine failure back uses it, so the
// code says what went wrong — the container is gone, it is in the wrong state,
// the daemon is down — instead of every failure arriving as Internal, "the
// agent is broken", which is what the handlers used to send.
//
// The Engine's HTTP status is the signal: the moby client turns it into a
// containerd errdefs class (404 → NotFound, 409 → Conflict, …), which is what
// the switch reads. Matching on the message text, as two handlers once did
// ("in use", "no such container"), breaks on the next rewording upstream.
//
// verb prefixes the message ("list volumes: …"); empty leaves it as is. An
// error that already carries a gRPC status (one the setup chose, such as the
// sidecar cap's ResourceExhausted) passes through unchanged. The table is
// CONTRACT.md §3.8; change both together.
func dockerStatus(err error, verb string) error {
	if err == nil {
		return nil
	}
	var se interface{ GRPCStatus() *status.Status }
	if errors.As(err, &se) && se.GRPCStatus().Code() != codes.Unknown {
		return err
	}
	msg := err.Error()
	if verb != "" {
		msg = verb + ": " + msg
	}
	return status.Error(dockerCode(err), msg)
}

// dockerCode classifies an Engine error. Order matters where classes overlap:
// the specific ones (NotFound, Conflict) come before InvalidArgument, which the
// moby client also uses as the catch-all for an unexpected 4xx.
//
// Two classes are deliberately NOT passed through as their namesakes:
//
//   - Engine NotImplemented maps to Internal, not Unimplemented. A client reads
//     Unimplemented as "this agent predates the RPC" and tells the operator to
//     upgrade it, which would be wrong advice for a daemon feature.
//   - Engine Unauthorized (401) maps to PermissionDenied, not Unauthenticated.
//     It is the agent that the daemon refused, not the client; Unauthenticated
//     would send the operator off to check a secret or certificate that is fine.
func dockerCode(err error) codes.Code {
	switch {
	case errors.Is(err, context.Canceled) || cerrdefs.IsCanceled(err):
		return codes.Canceled
	case cerrdefs.IsDeadlineExceeded(err):
		return codes.DeadlineExceeded
	case cerrdefs.IsNotFound(err):
		return codes.NotFound
	case cerrdefs.IsAlreadyExists(err):
		return codes.AlreadyExists
	case cerrdefs.IsConflict(err), cerrdefs.IsFailedPrecondition(err), cerrdefs.IsNotModified(err):
		return codes.FailedPrecondition
	case cerrdefs.IsUnauthorized(err), cerrdefs.IsPermissionDenied(err):
		return codes.PermissionDenied
	case cerrdefs.IsResourceExhausted(err):
		return codes.ResourceExhausted
	case cerrdefs.IsUnavailable(err), client.IsErrConnectionFailed(err):
		return codes.Unavailable
	case cerrdefs.IsInvalidArgument(err):
		return codes.InvalidArgument
	default:
		return codes.Internal
	}
}
