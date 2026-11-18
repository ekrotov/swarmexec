// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import "sync/atomic"

// loadSeq orders the answers of one loader. The generation (ui.gen) only keeps
// an answer for another CLUSTER off this one's screen; within a cluster two
// loads can be in flight at once — the refresh key pressed twice, the poll
// overlapping an explicit reload — and whichever answers last used to win, so a
// slow older fetch could paint over a newer list.
//
// Each load takes a ticket when it starts (start, safe from any goroutine: the
// tree's poll starts off the loop) and its apply asks fresh on the loop. An
// answer is applied unless something that started LATER has already been; an
// older answer that arrives first is still shown, because it is the newest
// thing there is. fresh is UI goroutine only.
type loadSeq struct {
	n       atomic.Uint64
	applied uint64
}

func (s *loadSeq) start() uint64 { return s.n.Add(1) }

// fresh reports whether the answer for ticket id may be applied, and records it.
func (s *loadSeq) fresh(id uint64) bool {
	if id <= s.applied {
		return false
	}
	s.applied = id
	return true
}

// latest reports whether no later answer than id has been applied.
func (s *loadSeq) latest(id uint64) bool { return s.applied == id }
