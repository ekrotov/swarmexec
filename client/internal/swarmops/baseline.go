// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/moby/moby/api/types/swarm"
)

// ErrConflict marks an edit refused because the object changed after the
// editor loaded it. errors.Is(err, ErrConflict) is the test; the message names
// the object and says what to do.
var ErrConflict = errors.New("changed since it was loaded")

// ConflictError is the refusal of a write based on stale data (KOR-05).
type ConflictError struct {
	Kind     string // "service" or "node"
	Name     string
	Loaded   uint64 // version the editor read
	Current  uint64 // version the cluster has now
	Detached bool   // the swarm itself refused the write ("update out of sequence")
}

func (e *ConflictError) Error() string {
	vers := ""
	if !e.Detached {
		vers = fmt.Sprintf(" (version %d, now %d)", e.Loaded, e.Current)
	}
	return fmt.Sprintf("%s %q was changed by someone else since you opened this editor%s — nothing was written. "+
		"Your input is still here: note it, close the editor and reopen it to load the current values, then apply again",
		e.Kind, e.Name, vers)
}

func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// Baseline remembers, per service, the version an editor read when it loaded
// the service's data. Every Current* reader records into it and every Set*
// writer checks it, so a rolling update or a second operator in between is
// refused rather than overwritten: swarm's own version check cannot catch it,
// because the version the writer sends is read fresh, right before the write.
//
// It rides on a context (WithBaseline) so the editors' many Current*/Set* pairs
// keep their signatures. A context without one behaves as before: read, mutate,
// write (the CLI, which has no editor open between the two).
type Baseline struct {
	mu sync.Mutex
	v  map[string]uint64
}

type baselineKey struct{}

// WithBaseline returns ctx with an empty Baseline attached.
func WithBaseline(ctx context.Context) context.Context {
	return context.WithValue(ctx, baselineKey{}, &Baseline{v: map[string]uint64{}})
}

func baselineOf(ctx context.Context) *Baseline {
	b, _ := ctx.Value(baselineKey{}).(*Baseline)
	return b
}

// recordLoaded notes the version an editor just read for service name.
func recordLoaded(ctx context.Context, name string, v swarm.Version) {
	if b := baselineOf(ctx); b != nil {
		b.mu.Lock()
		b.v[name] = v.Index
		b.mu.Unlock()
	}
}

// loadedVersion returns the version recorded for name, if an editor loaded it.
func loadedVersion(ctx context.Context, name string) (uint64, bool) {
	b := baselineOf(ctx)
	if b == nil {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.v[name]
	return v, ok
}

// forgetLoaded drops name's baseline once its edit is applied: the next editor
// loads (and records) afresh.
func forgetLoaded(ctx context.Context, name string) {
	if b := baselineOf(ctx); b != nil {
		b.mu.Lock()
		delete(b.v, name)
		b.mu.Unlock()
	}
}

// asConflict turns the swarm's own "update out of sequence" into a
// ConflictError, for a write that lost the race after our check.
func asConflict(kind, name string, err error) error {
	if err != nil && (isVersionConflict(err) || strings.Contains(err.Error(), "out of sequence")) {
		return &ConflictError{Kind: kind, Name: name, Detached: true}
	}
	return err
}
