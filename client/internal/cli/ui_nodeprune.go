// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/swarm"
	"github.com/rivo/tview"

	"swarmexec/client/internal/swarmops"
	"swarmexec/internal/pb"
	"swarmexec/internal/prunesched"
)

// --- per-node prune dialog --------------------------------------------------

// openNodePrune asks the node's agent what each category would free, then
// opens the prune dialog. The figures come from the agent's memory, so this is
// a round trip, not a scan. An agent that predates the RPC gets the old
// two-choice image menu instead — it can still do that much.
func (u *ui) openNodePrune(n swarmops.NodeInfo, back tview.Primitive, after func()) {
	u.flash(fmt.Sprintf(" [gray]asking %s's agent…[white]", n.Hostname))
	u.loadNodeReclaim(n.Hostname, false, func(resp *pb.ReclaimStatsResponse, err error) {
		switch {
		case errors.Is(err, errAgentTooOld):
			u.openNodeImagePrune(n, back, after)
		case err != nil:
			u.info("prune: " + err.Error())
		default:
			u.showPruneDialog(n, resp, back, after)
		}
	})
}

// pruneDialog is the per-node prune overlay: the category checklist, a
// summary of what the ticked ones free, the age prompt, and the prune behind a
// confirm.
//
// It was one 140-line function whose render, prompt, run and key handler were
// closures over the same locals (review Q-01). As a type the halves are
// separate methods — build (showPruneDialog), view (render), input (keys) and
// apply (run) — the same split scheduleEditor has.
type pruneDialog struct {
	u      *ui
	node   swarmops.NodeInfo
	stats  *pb.ReclaimStatsResponse
	after  func()
	choice pruneChoice

	table   *tview.Table
	summary *tview.TextView
	ov      *overlay
}

// showPruneDialog is the category checklist. Volumes sit last, unticked and
// red, and "all" (a) leaves them alone: they are data, and the only way to
// prune them is to tick them on purpose.
func (u *ui) showPruneDialog(n swarmops.NodeInfo, stats *pb.ReclaimStatsResponse, back tview.Primitive, after func()) {
	d := &pruneDialog{u: u, node: n, stats: stats, after: after, choice: defaultPruneChoice()}
	d.table = tview.NewTable().SetSelectable(true, false)
	d.table.SetBorder(true).SetTitle(fmt.Sprintf(" prune — %s ", n.Hostname))
	d.summary = tview.NewTextView().SetDynamicColors(true).SetWrap(true)
	layout := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(d.table, len(pruneCats)+2, 0, true).AddItem(d.summary, 0, 1, false)
	d.ov = u.overlayFor(pageNodePrune, back, footerKeys("j/k", "move", "space", "tick", "a", "all but volumes", "t", "keep stopped for", dangerKey("Enter"), "prune…", "Esc", "cancel"))
	d.render()
	d.table.SetInputCapture(d.keys)
	d.ov.show(centered(layout, 78, len(pruneCats)+8), d.table)
}

// render repaints the rows and the summary from the choice, keeping the
// cursor row.
func (d *pruneDialog) render() {
	row, _ := d.table.GetSelection()
	d.table.Clear()
	for i, pc := range pruneCats {
		d.renderRow(i, pc)
	}
	d.table.Select(row, 0)
	d.summary.SetText(d.summaryText(time.Now()))
}

// renderRow writes one category row: tick box, label (red when dangerous),
// and the figure the agent measured.
func (d *pruneDialog) renderRow(i int, pc pruneCat) {
	box := "[ ]"
	if d.choice.ticked[pc.cat] {
		box = "[x]"
	}
	label := pc.label
	switch pc.cat {
	case pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS:
		label = fmt.Sprintf("stopped containers (> %s)", d.choice.untilLabel())
	case pb.PruneCategory_PRUNE_CATEGORY_VOLUMES:
		label = "VOLUMES — data, never in all"
	}
	color := tcell.ColorWhite
	if pc.danger {
		color = tcell.ColorRed
	}
	rc, _ := reclaimFigure(d.stats, pc.cat)
	d.table.SetCell(i, 0, tview.NewTableCell(tview.Escape(box)).SetTextColor(color))
	d.table.SetCell(i, 1, tview.NewTableCell(label).SetTextColor(color).SetExpansion(1))
	d.table.SetCell(i, 2, tview.NewTableCell(figureText(rc)).SetAlign(tview.AlignRight))
}

// summaryText is the block under the table: the expected gain, and a warning
// for each dangerous category ticked.
func (d *pruneDialog) summaryText(now time.Time) string {
	stats, choice := d.stats, d.choice
	var b strings.Builder
	if stats.GetMeasuredAt() != "" {
		fmt.Fprintf(&b, " expected: frees about [green]%s[white]  [gray](measured %s)[-]\n",
			diskSize(choice.expected(stats)), ago(stats.GetMeasuredAt(), now))
		if mu := untilText(stats.GetContainersUntil()); mu != "" && mu != choice.untilLabel() {
			fmt.Fprintf(&b, " [gray]the containers figure was measured for stopped > %s[-]\n", mu)
		}
	} else {
		b.WriteString(" [gray]not measured yet — no estimate[-]\n")
	}
	if choice.ticked[pb.PruneCategory_PRUNE_CATEGORY_IMAGES] {
		b.WriteString(" [red]every unused image[white] also removes images of services scaled to zero.\n")
	}
	if choice.ticked[pb.PruneCategory_PRUNE_CATEGORY_VOLUMES] {
		b.WriteString(" [red]VOLUMES ticked: unused volumes are deleted WITH THEIR DATA.[white]\n")
	}
	return b.String()
}

// untilPrompt asks how long stopped containers are kept (t).
func (d *pruneDialog) untilPrompt() {
	u := d.u
	in := tview.NewInputField().SetLabel("keep stopped containers for (e.g. 24h, 2h, 0): ").SetText(d.choice.untilLabel()).SetFieldWidth(10)
	pov := u.overlayFor(pageNodePrunePrompt, d.table, "")
	in.SetDoneFunc(func(k tcell.Key) {
		pov.Close()
		if k != tcell.KeyEnter {
			return
		}
		txt := strings.TrimSpace(in.GetText())
		if _, err := prunesched.ParseUntil(txt); err != nil {
			u.info(err.Error())
			return
		}
		d.choice.until = txt
		d.render()
	})
	in.SetBorder(true)
	pov.show(centeredPrompt(in, 66), in)
}

// run prunes the ticked categories on the node, behind a confirm.
func (d *pruneDialog) run() {
	u := d.u
	if len(d.choice.categories()) == 0 {
		u.info("Nothing ticked.")
		return
	}
	r, ac, f := u.r, u.agents, u.f
	host, after := d.node.Hostname, d.after
	u.confirm(pruneConfirmBody(host, d.choice, d.stats), "Prune", d.table, func() {
		d.ov.Close()
		req := d.choice.request()
		u.flash(fmt.Sprintf(" [gray]pruning on %s…[white]", host))
		async(u, "ui.nodes.prune", func(ctx context.Context) (*pb.PruneResponse, error) {
			target, err := nodeByHostname(ctx, r, host)
			if err != nil {
				return nil, err
			}
			return ac.Prune(ctx, target, req, f.connectTimeout)
		}, func(resp *pb.PruneResponse, err error) {
			if err != nil {
				u.info("prune: " + err.Error())
				return
			}
			u.info(pruneResultText(host, resp))
			if after != nil {
				after()
			}
		})
	})
}

// keys is the table's input capture.
func (d *pruneDialog) keys(ev *tcell.EventKey) *tcell.EventKey {
	row, _ := d.table.GetSelection()
	switch {
	case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == d.u.km.Quit):
		d.ov.Close()
	case ev.Key() == tcell.KeyEnter || (ev.Key() == tcell.KeyRune && ev.Rune() == 'w'):
		d.run()
	case ev.Key() != tcell.KeyRune:
		return ev
	case ev.Rune() == ' ':
		if row >= 0 && row < len(pruneCats) {
			d.choice.toggle(pruneCats[row].cat)
			d.render()
		}
	case ev.Rune() == 'a':
		d.choice.selectAll()
		d.render()
	case ev.Rune() == 't':
		d.untilPrompt()
	default:
		return ev
	}
	return nil
}

// --- node detail: reclaimable block ------------------------------------------

// nodeReclaimView is the detail's live reclaim block: the last answer, the
// last error, and whether a request is in flight. Touched only on the UI
// goroutine.
type nodeReclaimView struct {
	resp    *pb.ReclaimStatsResponse
	err     error
	loading bool
}

func (v *nodeReclaimView) text(now time.Time) string {
	return reclaimSection(v.resp, v.err, v.loading, now)
}

// loadNodeReclaim fetches the node's reclaim stats in the background and hands
// them to apply on the UI goroutine. measureNow forces a fresh measurement.
func (u *ui) loadNodeReclaim(hostname string, measureNow bool, apply func(*pb.ReclaimStatsResponse, error)) {
	r, ac, f := u.r, u.agents, u.f
	async(u, "ui.nodes.reclaim", func(ctx context.Context) (*pb.ReclaimStatsResponse, error) {
		target, err := nodeByHostname(ctx, r, hostname)
		if err != nil {
			return nil, err
		}
		return ac.ReclaimStats(ctx, target, measureNow, f.connectTimeout)
	}, apply)
}

// --- cluster-wide schedule editor --------------------------------------------

// schedRowKind is what one row of the schedule editor edits.
type schedRowKind int

const (
	rowEnabled schedRowKind = iota
	rowDay
	rowStart
	rowTimes
	rowTZ
	rowCat
	rowImages
	rowUntil
	rowFilter
)

type schedRow struct {
	kind schedRowKind
	day  time.Weekday
	cat  string
}

// schedRows is the editor's layout, top to bottom.
func schedRows() []schedRow {
	rows := []schedRow{{kind: rowEnabled}}
	for _, wd := range prunesched.WeekOrder() {
		rows = append(rows, schedRow{kind: rowDay, day: wd})
	}
	rows = append(rows,
		schedRow{kind: rowStart}, schedRow{kind: rowTimes}, schedRow{kind: rowTZ},
		schedRow{kind: rowCat, cat: prunesched.Containers},
		schedRow{kind: rowImages},
		schedRow{kind: rowCat, cat: prunesched.BuildCache},
		schedRow{kind: rowCat, cat: prunesched.Networks},
		schedRow{kind: rowCat, cat: prunesched.Volumes},
		schedRow{kind: rowUntil}, schedRow{kind: rowFilter},
	)
	return rows
}

// imagesMode is the schedule's image choice as the editor shows it.
func imagesMode(s prunesched.Schedule) string {
	switch {
	case slices.Contains(s.Categories, prunesched.Images):
		return "every unused"
	case slices.Contains(s.Categories, prunesched.DanglingImages):
		return "untagged only"
	default:
		return "off"
	}
}

// cycleImages steps off → untagged only → every unused → off.
func cycleImages(s *prunesched.Schedule) {
	mode := imagesMode(*s)
	setCat(s, prunesched.Images, false)
	setCat(s, prunesched.DanglingImages, false)
	switch mode {
	case "off":
		setCat(s, prunesched.DanglingImages, true)
	case "untagged only":
		setCat(s, prunesched.Images, true)
	}
}

// setCat adds or removes a category, keeping the canonical order so the label
// does not churn with the order things were clicked in.
func setCat(s *prunesched.Schedule, cat string, on bool) {
	have := map[string]bool{}
	for _, c := range s.Categories {
		have[c] = true
	}
	have[cat] = on
	var out []string
	for _, c := range prunesched.Categories {
		if have[c] {
			out = append(out, c)
		}
	}
	s.Categories = out
}

func tick(on bool) string {
	if on {
		return "[x]"
	}
	return "[ ]"
}

// schedRowText renders one editor row as label and value; danger rows are red.
func schedRowText(s prunesched.Schedule, r schedRow) (label, value string, danger bool) {
	switch r.kind {
	case rowEnabled:
		return "schedule", tick(s.Enabled) + " enabled", false
	case rowDay:
		return "  " + prunesched.DayName(r.day), tick(s.Days[r.day]), false
	case rowStart:
		return "start", prunesched.FormatClock(s.Start), false
	case rowTimes:
		return "runs per day", fmt.Sprintf("%d   (+/-)", s.Times), false
	case rowTZ:
		return "time zone", s.TZ, false
	case rowImages:
		return "images", imagesMode(s) + "   (space cycles)", imagesMode(s) == "every unused"
	case rowCat:
		on := slices.Contains(s.Categories, r.cat)
		switch r.cat {
		case prunesched.Volumes:
			return "VOLUMES (data)", tick(on) + " unused volumes incl. named — option, off by default", true
		case prunesched.BuildCache:
			return "build cache", tick(on), false
		case prunesched.Networks:
			return "networks", tick(on) + " unused local networks (never overlays)", false
		default:
			return "containers", tick(on) + " stopped containers", false
		}
	case rowUntil:
		return "keep stopped", prunesched.FormatUntil(s.ContainersUntil) + " after they stop", false
	case rowFilter:
		if len(s.LabelFilters) == 0 {
			return "label filter", "—", false
		}
		return "label filter", strings.Join(s.LabelFilters, ","), false
	}
	return "", "", false
}

// schedPreview is the live preview under the editor: the run times the split
// produces and the next few runs — or why the schedule cannot be saved.
func schedPreview(s prunesched.Schedule, now time.Time) string {
	var b strings.Builder
	if err := s.Validate(); err != nil {
		fmt.Fprintf(&b, " [red]cannot save:[-] %s\n", escapeTags(err.Error()))
		return b.String()
	}
	fmt.Fprintf(&b, " runs at %s %s on %s\n", strings.Join(swarmops.RunTimes(s), ", "), s.TZ, orDash(prunesched.FormatDays(s.Days)))
	if !s.Enabled {
		b.WriteString(" [gray]disabled — saving keeps these settings but runs nothing[-]\n")
		return b.String()
	}
	next := s.Upcoming(now, 3)
	if len(next) == 0 {
		b.WriteString(" [orange]no day selected — nothing will run[-]\n")
		return b.String()
	}
	var parts []string
	for _, t := range next {
		parts = append(parts, t.Format("Mon 15:04"))
	}
	fmt.Fprintf(&b, " next: %s\n", strings.Join(parts, " · "))
	return b.String()
}

// openPruneSchedule loads the agent service and opens the editor. The
// schedule is the cluster's, so this works from any node row.
func (u *ui) openPruneSchedule(back tview.Primitive) {
	dcli := u.dcli
	async(u, "ui.nodes.schedule.load", func(ctx context.Context) (*swarm.Service, error) {
		return swarmops.FindAgentService(ctx, dcli)
	}, func(svc *swarm.Service, err error) {
		if err != nil {
			u.info("prune schedule: " + err.Error())
			return
		}
		u.showScheduleEditor(svc, back)
	})
}

// scheduleEditor is the cluster-wide prune schedule overlay: one row per
// setting, a preview of the next runs under it, and save behind a confirm.
//
// It was one 170-line function whose render, prompt, toggle, edit, save and
// key handler were closures over the same locals (review Q-01). As a type the
// halves are separate methods — build (show), view (render), input (keys) and
// apply (save) — the same split inspectView and netMembersView have.
type scheduleEditor struct {
	u        *ui
	svc      *swarm.Service
	s        prunesched.Schedule // the schedule being edited
	parseErr error               // the service's labels did not parse; s holds the defaults
	rows     []schedRow

	table   *tview.Table
	preview *tview.TextView
	ov      *overlay
}

func (u *ui) showScheduleEditor(svc *swarm.Service, back tview.Primitive) {
	e := &scheduleEditor{u: u, svc: svc, rows: schedRows()}
	e.s, e.parseErr = swarmops.AgentSchedule(svc)
	if e.parseErr != nil {
		e.s = prunesched.Default()
	}
	e.table = tview.NewTable().SetSelectable(true, false)
	e.table.SetBorder(true).SetTitle(" prune schedule — ALL nodes ")
	e.preview = tview.NewTextView().SetDynamicColors(true).SetWrap(true)
	layout := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(e.table, len(e.rows)+2, 0, true).AddItem(e.preview, 0, 1, false)
	e.ov = u.overlayFor(pageNodeSchedule, back, footerKeys("j/k", "move", "space", "toggle", "Enter", "edit", "+/-", "runs/day", "Ctrl-S", "apply (restarts agents)", "Esc", "cancel"))
	e.render()
	e.table.SetInputCapture(e.keys)
	e.ov.show(centered(layout, 84, len(e.rows)+7), e.table)
}

// render repaints the rows and the preview from e.s, keeping the cursor row.
func (e *scheduleEditor) render() {
	row, _ := e.table.GetSelection()
	e.table.Clear()
	for i, r := range e.rows {
		label, value, danger := schedRowText(e.s, r)
		color := tcell.ColorWhite
		if danger {
			color = tcell.ColorRed
		}
		e.table.SetCell(i, 0, tview.NewTableCell(label).SetTextColor(tcell.ColorGray))
		e.table.SetCell(i, 1, tview.NewTableCell(tview.Escape(value)).SetTextColor(color).SetExpansion(1))
	}
	e.table.Select(row, 0)
	text := schedPreview(e.s, time.Now())
	if e.parseErr != nil {
		text = fmt.Sprintf(" [orange]the service's labels were invalid (%s); showing defaults[-]\n", escapeTags(e.parseErr.Error())) + text
	}
	e.preview.SetText(text)
}

// prompt asks for one value; apply validates and stores it, and a refusal is
// shown and leaves the schedule as it was.
func (e *scheduleEditor) prompt(label, initial string, apply func(string) error) {
	u := e.u
	in := tview.NewInputField().SetLabel(label).SetText(initial).SetFieldWidth(32)
	pov := u.overlayFor(pageNodeSchedPrompt, e.table, "")
	in.SetDoneFunc(func(k tcell.Key) {
		pov.Close()
		if k != tcell.KeyEnter {
			return
		}
		if err := apply(strings.TrimSpace(in.GetText())); err != nil {
			u.info(err.Error())
			return
		}
		e.render()
	})
	in.SetBorder(true)
	pov.show(centeredPrompt(in, 66), in)
}

// toggle flips a yes/no row (space): enabled, a weekday, a category, or the
// images mode, which cycles.
func (e *scheduleEditor) toggle(r schedRow) {
	s := &e.s
	switch r.kind {
	case rowEnabled:
		s.Enabled = !s.Enabled
	case rowDay:
		s.Days[r.day] = !s.Days[r.day]
	case rowCat:
		setCat(s, r.cat, !slices.Contains(s.Categories, r.cat))
	case rowImages:
		cycleImages(s)
	}
	e.render()
}

// edit is Enter on a row: a prompt for the rows that take a value, a toggle
// for the others.
func (e *scheduleEditor) edit(r schedRow) {
	s := &e.s
	switch r.kind {
	case rowStart:
		e.prompt("first run of the day (HH:MM): ", prunesched.FormatClock(s.Start), func(v string) error {
			m, err := prunesched.ParseClock(v)
			if err == nil {
				s.Start = m
			}
			return err
		})
	case rowTimes:
		e.prompt(fmt.Sprintf("runs per day (1-%d): ", prunesched.MaxTimesPerDay), fmt.Sprint(s.Times), func(v string) error {
			var n int
			if _, err := fmt.Sscan(v, &n); err != nil || n < 1 || n > prunesched.MaxTimesPerDay {
				return fmt.Errorf("%q: want a number from 1 to %d", v, prunesched.MaxTimesPerDay)
			}
			s.Times = n
			return nil
		})
	case rowTZ:
		e.prompt("time zone (IANA, e.g. Europe/Berlin): ", s.TZ, func(v string) error {
			if _, err := time.LoadLocation(v); err != nil || v == "" {
				return fmt.Errorf("%q is not a time zone", v)
			}
			s.TZ = v
			return nil
		})
	case rowUntil:
		e.prompt("keep stopped containers for (24h, 2h, 0): ", prunesched.FormatUntil(s.ContainersUntil), func(v string) error {
			d, err := prunesched.ParseUntil(v)
			if err == nil {
				s.ContainersUntil = d
			}
			return err
		})
	case rowFilter:
		e.prompt("only objects labelled (k or k=v, comma-separated): ", strings.Join(s.LabelFilters, ","), func(v string) error {
			f, err := prunesched.ParseLabelFilters(v)
			if err == nil {
				s.LabelFilters = f
			}
			return err
		})
	default:
		e.toggle(r)
	}
}

// save writes the schedule to the agent service, behind a confirm: saving
// restarts every agent.
func (e *scheduleEditor) save() {
	u := e.u
	if err := e.s.Validate(); err != nil {
		u.info("cannot save: " + err.Error())
		return
	}
	msg := fmt.Sprintf("Save the prune schedule for ALL nodes?\n\n%s\n\nAgents older than this release ignore the schedule.", swarmops.ScheduleRestartWarning)
	u.confirm(msg, "Save", e.table, func() {
		e.ov.Close()
		saved, svc := e.s, e.svc
		u.flash(" [gray]saving the prune schedule…[white]")
		dcli := u.dcli
		asyncDo(u, "ui.nodes.schedule.save", func(ctx context.Context) error {
			return swarmops.SaveAgentSchedule(ctx, dcli, svc, saved)
		}, func(err error) {
			if err != nil {
				u.info("prune schedule: " + err.Error())
				return
			}
			u.flash(" [green]✓ prune schedule saved — the agents restart one by one[white]")
		})
	})
}

// stepTimes is + (one more run a day, up to the maximum) or - (one fewer,
// at least one).
func (e *scheduleEditor) stepTimes(key rune) {
	if key == '+' && e.s.Times < prunesched.MaxTimesPerDay {
		e.s.Times++
	}
	if key == '-' && e.s.Times > 1 {
		e.s.Times--
	}
	e.render()
}

// keys is the table's input capture.
func (e *scheduleEditor) keys(ev *tcell.EventKey) *tcell.EventKey {
	row, _ := e.table.GetSelection()
	if row < 0 || row >= len(e.rows) {
		row = 0
	}
	r := e.rows[row]
	switch {
	case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == e.u.km.Quit):
		e.ov.Close()
	case ev.Key() == tcell.KeyEnter:
		e.edit(r)
	case ev.Key() == tcell.KeyRune && ev.Rune() == ' ':
		e.toggle(r)
	case ev.Key() == tcell.KeyRune && (ev.Rune() == '+' || ev.Rune() == '-'):
		e.stepTimes(ev.Rune())
	// Ctrl-S applies, as in every editor; w is its one-key alias.
	case ev.Key() == tcell.KeyCtrlS || (ev.Key() == tcell.KeyRune && ev.Rune() == 'w'):
		e.save()
	default:
		return ev
	}
	return nil
}
