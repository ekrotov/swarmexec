// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (u *ui) renderSecrets() {
	sectable := u.sectable
	selName := ""
	if row, _ := sectable.GetSelection(); row >= 1 {
		if c := sectable.GetCell(row, 0); c != nil {
			selName = c.Text
		}
	}
	tableHeaders(sectable, sHeaders)
	if len(u.secs) == 0 {
		sectable.SetCell(1, 0, emptyCell(emptyText("secrets", keyHint(u.km.SecNew, "to create one"))))
		return
	}
	selRow := 1
	for i, s := range u.secs {
		usedCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
		if len(s.Services) > 0 {
			usedCell = tview.NewTableCell(fmt.Sprintf("%d", len(s.Services))).SetTextColor(tcell.ColorGreen).SetExpansion(1)
		}
		labels := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
		if len(s.Labels) > 0 {
			labels = tview.NewTableCell(fmt.Sprintf("%d", len(s.Labels))).SetExpansion(1)
		}
		sectable.SetCell(i+1, 0, tview.NewTableCell(s.Name).SetExpansion(1))
		sectable.SetCell(i+1, 1, usedCell)
		sectable.SetCell(i+1, 2, tview.NewTableCell(volumeAge(s.Created)).SetExpansion(1))
		sectable.SetCell(i+1, 3, tview.NewTableCell(volumeAge(s.Updated)).SetExpansion(1))
		sectable.SetCell(i+1, 4, labels)
		if s.Name == selName {
			selRow = i + 1
		}
	}
	if len(u.secs) > 0 {
		sectable.Select(selRow, 0)
	}
}

func (u *ui) loadSecrets() {
	dcli, sectable := u.dcli, u.sectable
	tableState(sectable, sHeaders, loadingCell())
	async(u, "ui.loadSecrets", func(ctx context.Context) ([]swarmSecret, error) {
		return listSecrets(ctx, dcli)
	}, func(list []swarmSecret, err error) {
		if err != nil {
			tableState(sectable, sHeaders, errorCell(err))
			return
		}
		u.secs = list
		u.renderSecrets()
	})
}

func (u *ui) selectedSecret() (swarmSecret, bool) {
	sectable := u.sectable
	return selectedRow(sectable, u.secs)
}

// showSecretDetail shows a secret's metadata and the services/containers that
// use it. The value is deliberately absent: the Docker API never returns it,
// and the overlay says so. Usage (services with their running containers)
// needs a task lookup, so it fills in lazily after the overlay is up.
func (u *ui) showSecretDetail(s swarmSecret) {
	pages, dcli, sectable := u.pages, u.dcli, u.sectable
	ts := func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.Local().Format("2006-01-02 15:04:05")
	}
	tv := tview.NewTextView().SetDynamicColors(true)
	tv.SetBorder(true).SetTitle(fmt.Sprintf(" secret %s ", s.Name))
	render := func(members []netService, loading bool) {
		var b strings.Builder
		fmt.Fprintf(&b, "Name:     %s\n", s.Name)
		fmt.Fprintf(&b, "ID:       %s\n", s.ID)
		fmt.Fprintf(&b, "Created:  %s\n", ts(s.Created))
		fmt.Fprintf(&b, "Updated:  %s\n", ts(s.Updated))
		if len(s.Labels) == 0 {
			fmt.Fprintf(&b, "Labels:   -\n")
		} else {
			fmt.Fprintf(&b, "Labels:\n")
			keys := make([]string, 0, len(s.Labels))
			for k := range s.Labels {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&b, "  %s=%s\n", k, s.Labels[k])
			}
		}
		fmt.Fprintf(&b, "\nused by:\n")
		if len(members) == 0 {
			if loading {
				fmt.Fprintf(&b, "  …\n")
			} else {
				fmt.Fprintf(&b, "  (no services)\n")
			}
		}
		for _, m := range members {
			head := m.Name + " …"
			if !loading {
				head = fmt.Sprintf("%s (%d)", m.Name, len(m.Containers))
			}
			fmt.Fprintf(&b, "  %s\n", head)
			for _, c := range m.Containers {
				fmt.Fprintf(&b, "      %s  %s\n", c.ID, orDash(c.Node))
			}
		}
		fmt.Fprintf(&b, "\n[gray]the secret value is not retrievable via the Docker API[white]")
		tv.SetText(b.String())
	}
	// Seed with the service names already known from the list; containers pending.
	init := make([]netService, 0, len(s.Services))
	for _, name := range s.Services {
		init = append(init, netService{Name: name})
	}
	render(init, true)
	ov := u.overlayFor(pageSecDetail, sectable, footerKeys("a", "attach", "d", "detach", "r", "rotate", "j/k", "scroll", "Esc", "back"))
	closeSecret := ov.Close
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == u.km.Quit || ev.Rune() == 'i')):
			closeSecret()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
			suggestions := servicesExcluding(u.serviceNamesFromCache(), s.Services)
			u.servicePrompt(
				fmt.Sprintf("attach secret %q to a service", s.Name),
				fmt.Sprintf("Attach secret %q to service", s.Name), "Attach",
				suggestions, tv,
				func(ctx context.Context, svc string) error {
					return attachSecretToService(ctx, dcli, svc, s.ID, s.Name)
				},
				func() { closeSecret(); u.loadSecrets() },
			)
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
			if len(s.Services) == 0 {
				u.info("no services use this secret")
				return nil
			}
			u.servicePrompt(
				fmt.Sprintf("detach secret %q from a service", s.Name),
				fmt.Sprintf("Detach secret %q from service", s.Name), "Detach",
				s.Services, tv,
				func(ctx context.Context, svc string) error {
					return detachSecretFromService(ctx, dcli, svc, s.ID, s.Name)
				},
				func() { closeSecret(); u.loadSecrets() },
			)
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'r':
			u.openRotateSecret(s, tv, closeSecret)
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		}
		return ev
	})
	height := 11 + len(s.Labels) + len(s.Services)
	if height > 24 {
		height = 24
	}
	ov.show(centered(tv, 72, height), tv)
	async(u, "ui.secrets.members", func(ctx context.Context) ([]netService, error) {
		return secretMembers(ctx, dcli, s), nil
	}, func(members []netService, _ error) {
		if pages.HasPage(pageSecDetail) {
			render(members, false)
		}
	})
}

// openRotateSecret opens the rotate editor for s from its detail view. The
// stand-in of an interrupted rotation (NAME_rotating, carrying the marker
// label) rotates — that is, resumes — the secret it stands in for: after a
// failure in step 4 that is the only row left to press r on.
func (u *ui) openRotateSecret(s swarmSecret, back tview.Primitive, closeDetail func()) {
	name, labels := s.Name, s.Labels
	if of := s.Labels[rotateMarkerLabel]; of != "" {
		name, labels = of, map[string]string{}
		for k, v := range s.Labels {
			if k != rotateMarkerLabel {
				labels[k] = v
			}
		}
	}
	if u.rotating[name] {
		u.info("secret " + name + " is already being rotated — wait for it to finish")
		return
	}
	dcli, ctx, gen := u.dcli, u.ctx, u.generation()
	u.openRotateEditor(back, name, labels, rotateHooks{
		plan: func() (rotatePlan, error) { return planRotation(ctx, dcli, name) },
		// The editor belongs to the cluster it was opened on: a plan read
		// after a switch away is dropped like any other stale result.
		queue: func(fn func()) { u.onCluster(gen, fn) },
		confirm: func(msg string, back tview.Primitive, ok func()) {
			u.confirm(msg, "Rotate", back, ok)
		},
		start: func(data []byte) {
			closeDetail()
			u.runRotation(name, data)
		},
	})
}

// runRotation rotates name in the background behind a progress overlay. Esc
// hides the overlay; the rotation carries on and still reports how it ended.
//
// Progress arrives on the rotating goroutine. It is collected under a lock
// and drawn by at most one queued update at a time, so a burst of lines costs
// one redraw, not one each.
func (u *ui) runRotation(name string, data []byte) {
	if u.rotating == nil {
		u.rotating = map[string]bool{}
	}
	rotating := u.rotating // this cluster's, even if the operator switches away
	rotating[name] = true
	app, dcli, ctx := u.app, u.dcli, u.ctx

	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true)
	tv.SetBorder(true).SetTitle(" rotating secret " + tview.Escape(name) + " ")
	ov := u.overlayFor(pageSecRotateRun, u.sectable, footerKeys("Esc", "hide (the rotation continues)"))
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape {
			ov.Close()
			return nil
		}
		return ev
	})

	var (
		mu      sync.Mutex
		lines   []string
		pending bool
	)
	draw := func() { // on the UI goroutine
		mu.Lock()
		text := strings.Join(lines, "\n")
		pending = false
		mu.Unlock()
		tv.SetText(text)
		tv.ScrollToEnd()
	}
	add := func(line string) { // on the rotating goroutine
		mu.Lock()
		lines = append(lines, line)
		queue := !pending
		pending = true
		mu.Unlock()
		if queue {
			app.QueueUpdateDraw(draw)
		}
	}
	lines = append(lines, "[gray]starting…[-]")
	draw()
	ov.show(centered(tv, 90, 18), tv)

	go func() {
		err := rotateSecret(ctx, dcli, name, data, rotateOptions{
			Progress: func(e rotateEvent) {
				add(fmt.Sprintf("[yellow]%d/%d[-] %s", e.Step, rotateSteps, tview.Escape(e.Msg)))
			},
		})
		app.QueueUpdateDraw(func() {
			delete(rotating, name)
			ov.Close()
			u.loadSecrets()
			if err != nil {
				msg := "rotation of " + name + " failed:\n\n" + err.Error()
				if rotateResumeHint(err, "") != "" {
					msg += "\n\nThe services keep running on " + rotateTempName(name) +
						", which holds the new content.\nRe-run r on " + name + " to resume."
				}
				u.info(msg)
				return
			}
			u.flash(" [green]rotated[white] secret " + name)
		})
	}()
}

// openDeleteSecret permanently removes a secret after a confirm. Docker refuses
// to remove a secret a service still references, so warn up front when in use.
func (u *ui) openDeleteSecret(s swarmSecret) {
	app, dcli, sectable := u.app, u.dcli, u.sectable
	msg := fmt.Sprintf("Remove secret %q?\n\nThis permanently deletes the secret; it cannot be undone.", s.Name)
	if len(s.Services) > 0 {
		msg += fmt.Sprintf("\n\n⚠ Still referenced by %d service(s): %s\nDocker will refuse to remove a secret in use — detach it from those services first.", len(s.Services), strings.Join(s.Services, ", "))
	}
	u.confirm(msg, "Delete", sectable, func() {
		asyncDo(u, "ui.secrets.remove", func(ctx context.Context) error {
			return removeSecret(ctx, dcli, s.Name)
		}, func(err error) {
			if err != nil {
				u.info("remove failed: " + err.Error())
				app.SetFocus(sectable)
				return
			}
			u.loadSecrets()
			u.flash(" [green]removed[white] secret " + s.Name)
		})
	})
}

// secretKeys is the key handler of the Secrets table.
func (u *ui) secretKeys(ev *tcell.EventKey) *tcell.EventKey {
	km := u.km
	if ev.Key() == tcell.KeyRune && ev.Rune() == km.SecNew {
		u.showCreateSecret()
		return nil
	}
	if ev.Key() == tcell.KeyRune && ev.Rune() == km.SecDelete {
		if s, ok := u.selectedSecret(); ok {
			u.openDeleteSecret(s)
		}
		return nil
	}
	// "i" opens the same detail as Enter — inspect means the same on every tab.
	if ev.Key() == tcell.KeyRune && ev.Rune() == 'i' {
		if s, ok := u.selectedSecret(); ok {
			u.showSecretDetail(s)
		}
		return nil
	}
	return u.tabKeys(ev)
}
