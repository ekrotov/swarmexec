// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
)

// The confirm body is the last thing an operator reads before something
// irreversible, so the two modes must never describe each other.
func TestPruneConfirmTextSeparatesTheModes(t *testing.T) {
	ni := nodeImages{Total: 10 << 30, Dangling: 2 << 30, Unused: 5 << 30, Count: 40}

	safe := pruneConfirmText("host-a", ni, false)
	// Note the safe text does mention a pull — as a reassurance ("cannot ...
	// force a pull"), so the check is for the sweep's actual warnings, not for
	// the word.
	for _, mustNot := range []string{"every unused", "RIGHT NOW", "scaled to zero", "registry"} {
		if strings.Contains(safe, mustNot) {
			t.Errorf("the safe confirm must not carry the sweep's warning %q:\n%s", mustNot, safe)
		}
	}
	if !strings.Contains(safe, "cannot") || !strings.Contains(safe, "force a pull") {
		t.Errorf("safe confirm should say what it cannot do:\n%s", safe)
	}
	if !strings.Contains(safe, "untagged") || !strings.Contains(safe, "host-a") {
		t.Errorf("safe confirm should name what it removes and where:\n%s", safe)
	}
	all := pruneConfirmText("host-a", ni, true)
	for _, want := range []string{"RIGHT NOW", "scaled to zero", "pull", "registry"} {
		if !strings.Contains(all, want) {
			t.Errorf("the sweeping confirm must spell out %q:\n%s", want, all)
		}
	}
	// It must also be honest about covering both buckets, not just the risky one.
	if !strings.Contains(all, "untagged") {
		t.Errorf("the sweep includes the untagged leftovers too:\n%s", all)
	}
}

// The detail must not offer reclaimable space it cannot back up, and must stay
// absent when no agent answered rather than showing a confident zero.
func TestImageSection(t *testing.T) {
	if got := imageSection(nodeImages{}, false); got != "" {
		t.Errorf("no agent reading should render nothing, got %q", got)
	}

	withJunk := imageSection(nodeImages{Total: 10 << 30, Dangling: 2 << 30, Unused: 5 << 30, Count: 40}, true)
	for _, want := range []string{"images on this node", "untagged leftovers", "pulling those images again"} {
		if !strings.Contains(withJunk, want) {
			t.Errorf("section missing %q:\n%s", want, withJunk)
		}
	}

	// A clean node says so instead of offering a zero-byte reclaim.
	clean := imageSection(nodeImages{Total: 3 << 30, Count: 5}, true)
	if !strings.Contains(clean, "nothing untagged to reclaim") {
		t.Errorf("clean node should say so:\n%s", clean)
	}
	// With nothing unused there is no reason to mention pulling.
	if strings.Contains(clean, "pulling those images again") {
		t.Errorf("clean node should not warn about re-pulls:\n%s", clean)
	}
}

func TestNodeImagesReclaimable(t *testing.T) {
	ni := nodeImages{Dangling: 100, Unused: 900}
	// Reclaimable is the SAFE figure only: the unused bytes are not free space,
	// they are space something still needs.
	if got := ni.Reclaimable(); got != 100 {
		t.Errorf("Reclaimable() = %d, want only the dangling bytes", got)
	}
}
