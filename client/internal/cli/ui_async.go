// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/clientlog"
)

// Background work and the UI loop meet in exactly one shape: do something slow
// off the loop, then show the outcome on it. Written by hand that shape is
//
//	go func() {
//		v, err := slowCall(ctx, …)
//		app.QueueUpdateDraw(func() { … })
//	}()
//
// and everything that makes it correct is opt-in at every site: the
// generation check that keeps a result for cluster A off cluster B's screen,
// the context that lets a switch stop the work, the log line, the error
// explanation. async is that shape with the guards built in, so they are
// structure rather than something each of ninety call sites must remember.
//
// The split between the two functions is deliberate too. work runs off the
// loop and must not touch tview or *ui — it gets a context and returns data,
// which keeps it movable into the data-layer packages the review proposes
// (A-01: agentcall for agent dialling, swarmops for manager-API logic). apply
// runs on the loop and owns every widget.

// async runs work off the UI goroutine against the visible cluster and hands
// its outcome to apply on the UI goroutine.
//
//   - It captures the cluster's generation, context and manager client at the
//     call, on the UI goroutine (the only place reading u.clusterState is
//     safe). work receives that context, so a switch away cancels it.
//   - op names the operation in the client log: one Timed record per call —
//     error, slow (>= clientlog.SlowThreshold) or done. A failure caused by
//     the context ending (a switch, the UI closing) is logged at debug as
//     cancelled instead of as an error.
//   - A non-nil error goes through explainErr, the single place a background
//     error is shaped for the operator, before apply sees it.
//   - apply is dropped — not run at all — when the operator has switched
//     cluster since the call: its result describes a cluster no longer on
//     screen, and the modal or table it would open would sit over the wrong
//     one. The drop is logged at debug; the outcome itself is already in the
//     log through the Timed record, so a mutation that finished (or failed)
//     after a switch still leaves a trace.
//
// async must be called on the UI goroutine (a key handler, an apply, a
// confirm's callback). It never blocks there: the only QueueUpdateDraw is on
// the worker goroutine, which is what keeps it clear of the deadlock that
// calling QueueUpdateDraw from the loop itself causes.
func async[T any](u *ui, op string, work func(ctx context.Context) (T, error), apply func(T, error)) {
	gen, cs := u.generation(), u.clusterState
	ctx, name := cs.ctx, cs.name
	if ctx == nil { // a cluster not connected yet has no context; nothing to bound by
		ctx = context.Background()
	}
	var dcli serviceLister // stays a nil interface, not a nil *client.Client
	if cs.dcli != nil {
		dcli = cs.dcli
	}
	go func() {
		start := time.Now()
		v, err := work(ctx)
		logOp(ctx, op, start, err, "cluster", name)
		if err != nil {
			err = explainErr(ctx, dcli, err)
		}
		u.deliver(gen, op, func() { apply(v, err) })
	}()
}

// asyncDo is async for work that produces nothing but an error — the shape of
// every mutation (update, scale, remove).
func asyncDo(u *ui, op string, work func(ctx context.Context) error, apply func(error)) {
	async(u, op, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, work(ctx)
	}, func(_ struct{}, err error) { apply(err) })
}

// deliver runs fn on the UI goroutine unless the generation has moved since
// gen was captured, in which case the result is stale and dropped with a debug
// record naming op (empty for the loaders that still call onCluster
// directly).
func (u *ui) deliver(gen uint64, op string, fn func()) {
	u.app.QueueUpdateDraw(func() {
		if now := u.gen.Load(); now != gen {
			clientlog.L().Debug("stale result dropped", "op", op, "gen", gen, "now", now)
			return
		}
		fn()
	})
}

// logOp records one background operation. A failure that is only the context
// ending — the operator switched away, the UI is closing — is not an error
// worth a red line in the log viewer, so it goes out at debug.
func logOp(ctx context.Context, op string, start time.Time, err error, attrs ...any) {
	if err != nil && ctx.Err() != nil && (errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.Canceled) {
		clientlog.L().Debug("op cancelled", append([]any{"op", op, "ms", time.Since(start).Milliseconds()}, attrs...)...)
		return
	}
	clientlog.Timed(op, start, err, attrs...)
}

// explainErr is the one place a background error is shaped before the
// operator sees it. Every async apply receives its error through here.
//
// Today it applies what enrichAgentError did for the exec terminal — and
// only to errors that came from an agent (a gRPC status), because the
// "is an agent deployed at all" probe is meaningless for a manager-API or
// local error and would misreport one as "no agent". Manager errors pass
// through unchanged. This is the seam the review's A-05 error mapping
// (agenterr.Explain) plugs into; keep it the only one.
//
// The explained error still wraps the original, so errors.Is / status.Code on
// what apply receives keep answering for the original cause.
func explainErr(ctx context.Context, dcli serviceLister, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); !ok {
		return err // not from an agent
	}
	var shown error
	switch {
	case agentTooOld(err):
		shown = errAgentTooOld
	case status.Code(err) == codes.Unauthenticated:
		shown = errSecretRejected
	case status.Code(err) == codes.Unavailable && dcli != nil && !agentDeployed(ctx, dcli):
		shown = errNoAgent
	}
	if shown == nil || errors.Is(err, shown) {
		return err
	}
	return explained{shown: shown, cause: err}
}

// explained shows one error and keeps another as its cause.
type explained struct{ shown, cause error }

func (e explained) Error() string   { return e.shown.Error() }
func (e explained) Unwrap() []error { return []error{e.shown, e.cause} }
