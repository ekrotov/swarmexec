// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"

	"github.com/rivo/tview"
)

// listTab is the part the simple list tabs share — Networks, Secrets and
// Configs: one table, one list per cluster fetched in one call, and the
// cursor kept by name across a reload and across a cluster switch.
//
// Volumes does not use it: its "/" filter, sort and two-pass load make the
// displayed rows a function of more than the list, so it keeps its own
// (same-shaped) state. A tab embeds *listTab[T] and gets widget, selected,
// load and render; it supplies the headers, the row painter, the empty text
// and the row's name.
type listTab[T any] struct {
	u       *ui
	table   *tview.Table
	headers []string
	name    func(T) string           // the cursor's key: the row's name
	row     func(r int, item T)      // paint table row r
	empty   func() string            // the empty-state text, read at render time (it names live keys)
	per     perCluster[listState[T]] // what each cluster shows
	// shownFor is the cluster whose rows the table holds, nil while it shows
	// a loading or error state; see volumesTab.shownFor.
	shownFor *clusterState
}

// listState is what a list tab knows about one cluster.
type listState[T any] struct {
	rows   []T
	cursor string // the row the cursor stood on, by name
}

func newListTab[T any](u *ui, headers []string, name func(T) string) *listTab[T] {
	l := &listTab[T]{u: u, headers: headers, name: name}
	l.per.init = func() *listState[T] { return &listState[T]{} }
	l.table = tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	l.table.SetSelectedStyle(u.selStyle)
	return l
}

func (l *listTab[T]) widget() tview.Primitive { return l.table }

// st is the visible cluster's state.
func (l *listTab[T]) st() *listState[T] { return l.per.of(l.u.clusterState) }

// rows is the visible cluster's list as last loaded.
func (l *listTab[T]) rows() []T { return l.st().rows }

func (l *listTab[T]) selected() (T, bool) { return selectedRow(l.table, l.st().rows) }

// remember saves the cursor of the cluster the table currently shows.
func (l *listTab[T]) remember() {
	if l.shownFor == nil {
		return
	}
	prev := l.per.of(l.shownFor)
	if it, ok := selectedRow(l.table, prev.rows); ok {
		prev.cursor = l.name(it)
	}
}

// placeholder replaces the rows with a loading or error state.
func (l *listTab[T]) placeholder(cell *tview.TableCell) {
	l.remember()
	l.shownFor = nil
	tableState(l.table, l.headers, cell)
}

// render repaints the visible cluster's rows from cache, putting the cursor
// back on the row it stood on.
func (l *listTab[T]) render() {
	l.remember()
	st := l.st()
	l.shownFor = l.u.clusterState
	tableHeaders(l.table, l.headers)
	if len(st.rows) == 0 {
		l.table.SetCell(1, 0, emptyCell(l.empty()))
		return
	}
	selRow := 1
	for i, it := range st.rows {
		l.row(i+1, it)
		if l.name(it) == st.cursor {
			selRow = i + 1
		}
	}
	l.table.Select(selRow, 0)
}

// load fetches the list off the UI goroutine (op names it in the client log)
// and renders it; a failure shows in the table and keeps the previous list.
// The result lands in the state of the cluster the load started on, and is
// dropped by async if that cluster is no longer the visible one.
func (l *listTab[T]) load(op string, fetch func(ctx context.Context) ([]T, error)) {
	st := l.st()
	l.placeholder(loadingCell())
	async(l.u, op, fetch, func(list []T, err error) {
		if err != nil {
			l.placeholder(errorCell(err))
			return
		}
		st.rows = list
		l.render()
	})
}
