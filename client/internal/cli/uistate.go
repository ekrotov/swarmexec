// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Before a list can show anything it is in one of three states: loading, failed,
// or empty. Each tab used to say them in its own words, in the place it needed
// them — and most empties were a dead end ("(no services)" — now what?) while
// exactly one taught the next step: "(no forwards — press p on a container)".
//
// These are those three sentences, said once. The empty one follows the forwards
// tab's example, because an empty list is the moment an operator most needs to
// be told what would fill it.

// loadingText is the single loading token, so the wait reads the same on every
// tab and in every overlay.
const loadingText = "loading…"

// emptyText renders "(no volumes — n to create one)": what is not there, and
// what to do about it. hint is omitted when there is nothing to do from here —
// an empty nodes table is a statement about the cluster, not an invitation.
func emptyText(what, hint string) string {
	if hint == "" {
		return "(no " + what + ")"
	}
	return "(no " + what + " — " + hint + ")"
}

// keyHint spells a live keybinding into an empty state's advice, so a remapped
// key is not quietly wrong — which is what "press p on a container" became the
// moment anyone remapped the forward key.
func keyHint(key rune, rest string) string {
	return keyLabel(key) + " " + rest
}

func loadingCell() *tview.TableCell {
	return stateCell(loadingText, tcell.ColorGray)
}

// errorText is the failed state in words, for the surfaces that are not tables
// (the containers tree).
func errorText(err error) string { return "error: " + err.Error() }

func errorCell(err error) *tview.TableCell {
	return stateCell(errorText(err), tcell.ColorRed)
}

func emptyCell(text string) *tview.TableCell {
	return stateCell(text, tcell.ColorGray)
}

// stateCell is never selectable: none of the three states is a row anyone can
// act on, and a cursor that lands on "loading…" invites a keystroke that then
// does nothing.
func stateCell(text string, color tcell.Color) *tview.TableCell {
	return tview.NewTableCell(text).SetTextColor(color).SetSelectable(false)
}

// tableHeaders clears a table and lays its header row down again — the opening
// of every load and every render, which each tab spelled out for itself.
func tableHeaders(t *tview.Table, headers []string) {
	t.Clear()
	for c, h := range headers {
		t.SetCell(0, c, headerCell(h))
	}
}

// tableState shows one of the three states in an otherwise empty table.
func tableState(t *tview.Table, headers []string, cell *tview.TableCell) {
	tableHeaders(t, headers)
	t.SetCell(1, 0, cell)
}
