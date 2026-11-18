// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/swarmops"
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
//     one. The drop is logged at debug. async is for reads: a write goes
//     through mutate below, which neither cancels on a switch nor drops the
//     outcome.
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
	var dcli swarmops.ServiceLister // stays a nil interface, not a nil *client.Client
	if cs.dcli != nil {
		dcli = cs.dcli
	}
	auth := agenterr.AuthOf(cs.cfg.SecretMode())
	go func() {
		start := time.Now()
		v, err := work(ctx)
		logOp(ctx, op, start, err, "cluster", name)
		if err != nil {
			err = explainErr(ctx, auth, dcli, err)
		}
		u.deliver(gen, op, func() { apply(v, err) })
	}()
}

// mutateTimeout bounds one write. Generous — a stack removal or a deploy
// walks many objects — but finite, so a hung manager cannot pin a write (and
// the quit confirmation that waits for it) forever.
const mutateTimeout = 10 * time.Minute

// mutate is async's sibling for writes: update, scale, remove, deploy, prune.
// A read that a switch interrupts can be cancelled and forgotten; a write
// cannot, because the operator then no longer knows whether it happened. So
// where async ties the work to the cluster's context and drops the result,
// mutate:
//
//   - runs on the SESSION context (u.runCtx) with mutateTimeout, so switching
//     away does not stop it. Only quitting does — and quitting asks first
//     while one is running (see mutations).
//   - always reports. When the cluster it ran on is still the visible one,
//     apply runs exactly as for async. When the operator has switched away,
//     apply is not run (its widgets belong to a cluster no longer on screen),
//     and a notice names the cluster and says how it ended, success or error.
//   - counts as running from the call until work returns, which is what the
//     quit confirmation lists.
//
// label is the operator-facing name of the write ("stack removal"); op is the
// log key, as for async. Same calling rules as async: UI goroutine only.
func mutate[T any](u *ui, op, label string, work func(ctx context.Context) (T, error), apply func(T, error)) {
	gen, cs := u.generation(), u.clusterState
	name := cs.name
	base := u.runCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, mutateTimeout)
	var dcli swarmops.ServiceLister // stays a nil interface, not a nil *client.Client
	if cs.dcli != nil {
		dcli = cs.dcli
	}
	auth := agenterr.AuthOf(cs.cfg.SecretMode())
	end := u.muts.begin(label + " on " + name)
	go func() {
		defer cancel()
		start := time.Now()
		v, err := work(ctx)
		end()
		logOp(ctx, op, start, err, "cluster", name)
		if err != nil {
			err = explainErr(ctx, auth, dcli, err)
		}
		u.app.QueueUpdateDraw(func() {
			if u.gen.Load() == gen {
				apply(v, err)
				return
			}
			u.info(mutatedElsewhere(label, name, err))
		})
	}()
}

// mutateDo is mutate for a write that produces nothing but an error.
func mutateDo(u *ui, op, label string, work func(ctx context.Context) error, apply func(error)) {
	mutate(u, op, label, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, work(ctx)
	}, func(_ struct{}, err error) { apply(err) })
}

// mutatedElsewhere is the notice for a write that ended after the operator
// had switched to another cluster. It says which cluster and how it ended;
// what it cannot say is how far a failed multi-step write got, so it points at
// the cluster rather than guessing.
func mutatedElsewhere(label, cluster string, err error) string {
	if err == nil {
		return fmt.Sprintf("%s on cluster %q finished.\n\nYou had switched to another cluster while it ran.", label, cluster)
	}
	return fmt.Sprintf("%s on cluster %q failed:\n\n%v\n\nYou had switched to another cluster while it ran; look at %q to see what is left.", label, cluster, err, cluster)
}

// mutations is the set of writes in flight — begin/end from mutate, and the
// secret rotation, which has its own progress overlay but the same stake: ended
// halfway it leaves services on a temporary secret.
type mutations struct {
	mu   sync.Mutex
	next uint64
	live map[uint64]string
}

// begin registers a running write and returns the function that ends it.
func (m *mutations) begin(what string) (end func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.live == nil {
		m.live = map[uint64]string{}
	}
	m.next++
	id := m.next
	m.live[id] = what
	return func() {
		m.mu.Lock()
		delete(m.live, id)
		m.mu.Unlock()
	}
}

// running lists what is in flight, sorted so the dialog is stable.
func (m *mutations) running() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.live))
	for _, w := range m.live {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
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
// It is agenterr.Explain for a background load: only an error that came from
// an agent (a gRPC status) is explained, because the "is an agent deployed at
// all" probe is meaningless for a manager-API or local error and would
// misreport one as "no agent" — manager errors pass through unchanged. auth is
// the cluster's configured credential, so an Unauthenticated names the one the
// operator actually uses (secret or client certificate).
//
// The explained error still wraps the original, so errors.Is / status.Code on
// what apply receives keep answering for the original cause.
func explainErr(ctx context.Context, auth agenterr.Auth, dcli swarmops.ServiceLister, err error) error {
	return agenterr.Explain(ctx, err, agenterr.Options{Auth: auth, AgentDeployed: deployedProbe(dcli)})
}
