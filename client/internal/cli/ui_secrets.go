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

	"swarmexec/client/internal/swarmops"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// secretsTab is the Secrets tab: the swarm's secrets with the services using
// each, and create / delete / attach / detach / rotate.
type secretsTab struct {
	*listTab[swarmops.Secret]

	// rotating holds, per cluster, the secrets with a rotation running in
	// the background, so a second one cannot be started on top of it. Only
	// touched on the UI goroutine.
	rotating perCluster[map[string]bool]
}

func newSecretsTab(u *ui) *secretsTab {
	t := &secretsTab{listTab: newListTab(u, sHeaders, func(s swarmops.Secret) string { return s.Name })}
	t.rotating.init = func() *map[string]bool { m := map[string]bool{}; return &m }
	t.row = t.renderRow
	t.empty = func() string { return emptyText("secrets", keyHint(u.km.SecNew, "to create one")) }
	t.table.SetSelectedFunc(func(int, int) {
		if s, ok := t.selected(); ok {
			t.showDetail(s)
		}
	})
	t.table.SetInputCapture(t.keys)
	return t
}

// secTab is the session's Secrets tab.
func (u *ui) secTab() *secretsTab { return u.tab("secrets").(*secretsTab) }

func (t *secretsTab) enter()   { t.reload() }
func (t *secretsTab) refresh() { t.reload() }

// reload fetches the secret list.
func (t *secretsTab) reload() {
	dcli := t.u.dcli
	t.load("ui.loadSecrets", func(ctx context.Context) ([]swarmops.Secret, error) { return swarmops.ListSecrets(ctx, dcli) })
}

// rotatingOn is the set of secrets being rotated on cluster c.
func (t *secretsTab) rotatingOn(c *clusterState) map[string]bool { return *t.rotating.of(c) }

func (t *secretsTab) footer() string {
	km := t.u.km
	return fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/i[white] details  [yellow]%s[white] new  %s delete", keyLabel(km.SecNew), keyTag(dangerKey(keyLabel(km.SecDelete))))
}

func (t *secretsTab) helpSection(h *helpWriter) {
	km := t.u.km
	h.sec("Secrets")
	h.line("Enter / i", "details")
	h.line("  a · d", "in details: attach to · detach from a service")
	h.line("  r", "in details: rotate — new content, same name (services restart twice)")
	h.line(keyLabel(km.SecNew), "new (vim editor, :r file)")
	h.danger(keyLabel(km.SecDelete), "delete")
}

func (t *secretsTab) yank(b *strings.Builder) {
	fmt.Fprintln(b, "SECRET\tUSED BY\tAGE\tUPDATED\tLABELS")
	for _, s := range t.rows() {
		fmt.Fprintf(b, "%s\t%d\t%s\t%s\t%d\n", s.Name, len(s.Services), volumeAge(s.Created), volumeAge(s.Updated), len(s.Labels))
	}
}

func (t *secretsTab) renderRow(r int, s swarmops.Secret) {
	usedCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
	if len(s.Services) > 0 {
		usedCell = tview.NewTableCell(fmt.Sprintf("%d", len(s.Services))).SetTextColor(tcell.ColorGreen).SetExpansion(1)
	}
	labels := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
	if len(s.Labels) > 0 {
		labels = tview.NewTableCell(fmt.Sprintf("%d", len(s.Labels))).SetExpansion(1)
	}
	t.table.SetCell(r, 0, tview.NewTableCell(s.Name).SetExpansion(1))
	t.table.SetCell(r, 1, usedCell)
	t.table.SetCell(r, 2, tview.NewTableCell(volumeAge(s.Created)).SetExpansion(1))
	t.table.SetCell(r, 3, tview.NewTableCell(volumeAge(s.Updated)).SetExpansion(1))
	t.table.SetCell(r, 4, labels)
}

// showDetail shows a secret's metadata and the services/containers that use
// it. The value is deliberately absent: the Docker API never returns it, and
// the overlay says so. Usage (services with their running containers) needs a
// task lookup, so it fills in lazily after the overlay is up.
func (t *secretsTab) showDetail(s swarmops.Secret) {
	u := t.u
	pages, dcli, sectable := u.pages, u.dcli, t.table
	ts := func(tm time.Time) string {
		if tm.IsZero() {
			return "-"
		}
		return tm.Local().Format("2006-01-02 15:04:05")
	}
	tv := tview.NewTextView().SetDynamicColors(true)
	tv.SetBorder(true).SetTitle(fmt.Sprintf(" secret %s ", s.Name))
	render := func(members []swarmops.NetService, loading bool) {
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
	init := make([]swarmops.NetService, 0, len(s.Services))
	for _, name := range s.Services {
		init = append(init, swarmops.NetService{Name: name})
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
			suggestions := swarmops.ServicesExcluding(u.serviceNamesFromCache(), s.Services)
			u.servicePrompt(
				fmt.Sprintf("attach secret %q to a service", s.Name),
				fmt.Sprintf("Attach secret %q to service", s.Name), "Attach",
				suggestions, tv,
				func(ctx context.Context, svc string) error {
					return swarmops.AttachSecretToService(ctx, dcli, svc, s.ID, s.Name)
				},
				func() { closeSecret(); t.reload() },
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
					return swarmops.DetachSecretFromService(ctx, dcli, svc, s.ID, s.Name)
				},
				func() { closeSecret(); t.reload() },
			)
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'r':
			t.openRotate(s, tv, closeSecret)
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
	async(u, "ui.secrets.members", func(ctx context.Context) ([]swarmops.NetService, error) {
		return swarmops.SecretMembers(ctx, dcli, s), nil
	}, func(members []swarmops.NetService, _ error) {
		if pages.HasPage(pageSecDetail) {
			render(members, false)
		}
	})
}

// openRotate opens the rotate editor for s from its detail view. The stand-in
// of an interrupted rotation (NAME_rotating, carrying the marker label)
// rotates — that is, resumes — the secret it stands in for: after a failure in
// step 4 that is the only row left to press r on.
func (t *secretsTab) openRotate(s swarmops.Secret, back tview.Primitive, closeDetail func()) {
	u := t.u
	name, labels := s.Name, s.Labels
	if of := s.Labels[swarmops.RotateMarkerLabel]; of != "" {
		name, labels = of, map[string]string{}
		for k, v := range s.Labels {
			if k != swarmops.RotateMarkerLabel {
				labels[k] = v
			}
		}
	}
	if t.rotatingOn(u.clusterState)[name] {
		u.info("secret " + name + " is already being rotated — wait for it to finish")
		return
	}
	dcli, ctx, gen := u.dcli, u.ctx, u.generation()
	u.openRotateEditor(back, name, labels, rotateHooks{
		plan: func() (swarmops.RotatePlan, error) { return swarmops.PlanRotation(ctx, dcli, name) },
		// The editor belongs to the cluster it was opened on: a plan read
		// after a switch away is dropped like any other stale result.
		queue: func(fn func()) { u.onCluster(gen, fn) },
		confirm: func(msg string, back tview.Primitive, ok func()) {
			u.confirm(msg, "Rotate", back, ok)
		},
		start: func(data []byte) {
			closeDetail()
			t.runRotation(name, data)
		},
	})
}

// rotateSecretFn is the rotation runRotation starts; a variable only so a test
// can stand in for the cluster.
var rotateSecretFn = swarmops.RotateSecret

// runRotation rotates name in the background behind a progress overlay.
//
// A rotation must finish once it has started, wherever the operator goes: it
// deletes the old secret object before it recreates it under the old name, so
// one stopped halfway leaves the services parked on the temporary secret. So
// it runs on the SESSION context (runCtx), not the cluster's — a switch to
// another cluster cancels the cluster's context, and must not cancel this. It
// keeps the manager client of the cluster it started on (a switch away keeps
// that connection open; only quitting the UI closes it).
//
// Esc only hides the progress overlay, and so does a switch (which closes the
// old cluster's overlays). Either way the rotation still reports how it
// ended; when another cluster is on screen by then, the report names the
// cluster the rotation ran on. Quitting the UI does stop it — runCtx ends.
//
// Progress arrives on the rotating goroutine. It is collected under a lock
// and drawn by at most one queued update at a time, so a burst of lines costs
// one redraw, not one each.
func (t *secretsTab) runRotation(name string, data []byte) {
	u := t.u
	cs := u.clusterState         // the cluster the rotation runs on, whatever is visible later
	rotating := t.rotatingOn(cs) // this cluster's, even if the operator switches away
	rotating[name] = true
	app, ctx := u.app, u.runCtx
	var dcli swarmops.SecretRotateAPI // a nil interface, not a nil *client.Client
	if cs.dcli != nil {
		dcli = cs.dcli
	}

	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true)
	tv.SetBorder(true).SetTitle(" rotating secret " + tview.Escape(name) + " ")
	ov := u.overlayFor(pageSecRotateRun, t.table, footerKeys("Esc", "hide (the rotation continues)"))
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

	// Deliberately not async: that would drop the report after a switch.
	go func() {
		start := time.Now()
		err := rotateSecretFn(ctx, dcli, name, data, swarmops.RotateOptions{
			Progress: func(e swarmops.RotateEvent) {
				add(fmt.Sprintf("[yellow]%d/%d[-] %s", e.Step, swarmops.RotateSteps, tview.Escape(e.Msg)))
			},
		})
		logOp(ctx, "ui.secrets.rotate", start, err, "cluster", cs.name, "secret", name)
		app.QueueUpdateDraw(func() {
			delete(rotating, name)
			ov.Close()
			here := u.clusterState == cs
			where := "" // nothing to say when the cluster on screen is the one it ran on
			if here {
				t.reload()
			} else {
				where = " on cluster " + cs.name
			}
			if err != nil {
				msg := "rotation of " + name + where + " failed:\n\n" + err.Error()
				if swarmops.RotateResumeHint(err, "") != "" {
					msg += "\n\nThe services keep running on " + swarmops.RotateTempName(name) +
						", which holds the new content.\nRe-run r on " + name + " to resume."
				}
				u.info(msg)
				return
			}
			u.flash(" [green]rotated[white] secret " + name + where)
		})
	}()
}

// openDelete permanently removes a secret after a confirm. Docker refuses to
// remove a secret a service still references, so warn up front when in use.
func (t *secretsTab) openDelete(s swarmops.Secret) {
	u := t.u
	app, dcli, sectable := u.app, u.dcli, t.table
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
			t.reload()
			u.flash(" [green]removed[white] secret " + s.Name)
		})
	})
}

// keys is the key handler of the Secrets table.
func (t *secretsTab) keys(ev *tcell.EventKey) *tcell.EventKey {
	u, km := t.u, t.u.km
	if ev.Key() == tcell.KeyRune && ev.Rune() == km.SecNew {
		u.showCreateSecret()
		return nil
	}
	if ev.Key() == tcell.KeyRune && ev.Rune() == km.SecDelete {
		if s, ok := t.selected(); ok {
			t.openDelete(s)
		}
		return nil
	}
	// "i" opens the same detail as Enter — inspect means the same on every tab.
	if ev.Key() == tcell.KeyRune && ev.Rune() == 'i' {
		if s, ok := t.selected(); ok {
			t.showDetail(s)
		}
		return nil
	}
	return u.tabKeys(ev)
}
