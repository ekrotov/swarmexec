// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/swarm"
	"github.com/rivo/tview"

	"swarmexec/internal/pb"
	"swarmexec/internal/prunesched"
)

// --- per-node prune dialog --------------------------------------------------

// openNodePrune asks the node's agent what each category would free, then
// opens the prune dialog. The figures come from the agent's memory, so this is
// a round trip, not a scan. An agent that predates the RPC gets the old
// two-choice image menu instead — it can still do that much.
func (u *ui) openNodePrune(n swarmNodeInfo, back tview.Primitive, after func()) {
	app := u.app
	u.flash(fmt.Sprintf(" [gray]asking %s's agent…[white]", n.Hostname))
	go func() {
		target, err := nodeByHostname(u.ctx, u.r, n.Hostname)
		var resp *pb.ReclaimStatsResponse
		if err == nil {
			resp, err = fetchReclaim(u.ctx, u.cfg, target, false, u.f.connectTimeout)
		}
		app.QueueUpdateDraw(func() {
			switch {
			case errors.Is(err, errAgentTooOld):
				u.openNodeImagePrune(n, back, after)
			case err != nil:
				u.info("prune: " + err.Error())
			default:
				u.showPruneDialog(n, resp, back, after)
			}
		})
	}()
}

// showPruneDialog is the category checklist. Volumes sit last, unticked and
// red, and "all" (a) leaves them alone: they are data, and the only way to
// prune them is to tick them on purpose.
func (u *ui) showPruneDialog(n swarmNodeInfo, stats *pb.ReclaimStatsResponse, back tview.Primitive, after func()) {
	app := u.app
	choice := defaultPruneChoice()

	table := tview.NewTable().SetSelectable(true, false)
	table.SetBorder(true).SetTitle(fmt.Sprintf(" prune — %s ", n.Hostname))
	summary := tview.NewTextView().SetDynamicColors(true).SetWrap(true)
	layout := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(table, len(pruneCats)+2, 0, true).AddItem(summary, 0, 1, false)

	ov := u.overlayFor(pageNodePrune, back, footerKeys("j/k", "move", "space", "tick", "a", "all but volumes", "t", "keep stopped for", dangerKey("Enter"), "prune…", "Esc", "cancel"))

	render := func() {
		row, _ := table.GetSelection()
		table.Clear()
		for i, pc := range pruneCats {
			box := "[ ]"
			if choice.ticked[pc.cat] {
				box = "[x]"
			}
			label := pc.label
			switch pc.cat {
			case pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS:
				label = fmt.Sprintf("stopped containers (> %s)", choice.untilLabel())
			case pb.PruneCategory_PRUNE_CATEGORY_VOLUMES:
				label = "VOLUMES — data, never in all"
			}
			color := tcell.ColorWhite
			if pc.danger {
				color = tcell.ColorRed
			}
			rc, _ := reclaimFigure(stats, pc.cat)
			table.SetCell(i, 0, tview.NewTableCell(tview.Escape(box)).SetTextColor(color))
			table.SetCell(i, 1, tview.NewTableCell(label).SetTextColor(color).SetExpansion(1))
			table.SetCell(i, 2, tview.NewTableCell(figureText(rc)).SetAlign(tview.AlignRight))
		}
		table.Select(row, 0)

		var b strings.Builder
		if stats.GetMeasuredAt() != "" {
			fmt.Fprintf(&b, " expected: frees about [green]%s[white]  [gray](measured %s)[-]\n",
				diskSize(choice.expected(stats)), ago(stats.GetMeasuredAt(), time.Now()))
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
		summary.SetText(b.String())
	}
	render()

	untilPrompt := func() {
		in := tview.NewInputField().SetLabel("keep stopped containers for (e.g. 24h, 2h, 0): ").SetText(choice.untilLabel()).SetFieldWidth(10)
		pov := u.overlayFor(pageNodePrunePrompt, table, "")
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
			choice.until = txt
			render()
		})
		in.SetBorder(true)
		pov.show(centeredPrompt(in, 66), in)
	}

	run := func() {
		if len(choice.categories()) == 0 {
			u.info("Nothing ticked.")
			return
		}
		u.confirm(pruneConfirmBody(n.Hostname, choice, stats), "Prune", table, func() {
			ov.Close()
			req := choice.request()
			u.flash(fmt.Sprintf(" [gray]pruning on %s…[white]", n.Hostname))
			go func() {
				target, err := nodeByHostname(u.ctx, u.r, n.Hostname)
				var resp *pb.PruneResponse
				if err == nil {
					resp, err = pruneNode(u.ctx, u.cfg, target, req, u.f.connectTimeout)
				}
				app.QueueUpdateDraw(func() {
					if err != nil {
						u.info("prune: " + err.Error())
						return
					}
					u.info(pruneResultText(n.Hostname, resp))
					if after != nil {
						after()
					}
				})
			}()
		})
	}

	table.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		row, _ := table.GetSelection()
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == u.km.Quit):
			ov.Close()
			return nil
		case ev.Key() == tcell.KeyEnter || (ev.Key() == tcell.KeyRune && ev.Rune() == 'w'):
			run()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == ' ':
			if row >= 0 && row < len(pruneCats) {
				choice.toggle(pruneCats[row].cat)
				render()
			}
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
			choice.selectAll()
			render()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 't':
			untilPrompt()
			return nil
		}
		return ev
	})
	ov.show(centered(layout, 78, len(pruneCats)+8), table)
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
	go func() {
		target, err := nodeByHostname(u.ctx, u.r, hostname)
		var resp *pb.ReclaimStatsResponse
		if err == nil {
			resp, err = fetchReclaim(u.ctx, u.cfg, target, measureNow, u.f.connectTimeout)
		}
		u.app.QueueUpdateDraw(func() { apply(resp, err) })
	}()
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
	fmt.Fprintf(&b, " runs at %s %s on %s\n", strings.Join(runTimes(s), ", "), s.TZ, orDash(prunesched.FormatDays(s.Days)))
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
	app := u.app
	go func() {
		svc, err := findAgentService(u.ctx, u.dcli)
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("prune schedule: " + err.Error())
				return
			}
			u.showScheduleEditor(svc, back)
		})
	}()
}

func (u *ui) showScheduleEditor(svc *swarm.Service, back tview.Primitive) {
	app := u.app
	s, parseErr := agentSchedule(svc)
	if parseErr != nil {
		s = prunesched.Default()
	}
	rows := schedRows()

	table := tview.NewTable().SetSelectable(true, false)
	table.SetBorder(true).SetTitle(" prune schedule — ALL nodes ")
	preview := tview.NewTextView().SetDynamicColors(true).SetWrap(true)
	layout := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(table, len(rows)+2, 0, true).AddItem(preview, 0, 1, false)
	ov := u.overlayFor(pageNodeSchedule, back, footerKeys("j/k", "move", "space", "toggle", "Enter", "edit", "+/-", "runs/day", "w", "save (restarts agents)", "Esc", "cancel"))

	render := func() {
		row, _ := table.GetSelection()
		table.Clear()
		for i, r := range rows {
			label, value, danger := schedRowText(s, r)
			color := tcell.ColorWhite
			if danger {
				color = tcell.ColorRed
			}
			table.SetCell(i, 0, tview.NewTableCell(label).SetTextColor(tcell.ColorGray))
			table.SetCell(i, 1, tview.NewTableCell(tview.Escape(value)).SetTextColor(color).SetExpansion(1))
		}
		table.Select(row, 0)
		text := schedPreview(s, time.Now())
		if parseErr != nil {
			text = fmt.Sprintf(" [orange]the service's labels were invalid (%s); showing defaults[-]\n", escapeTags(parseErr.Error())) + text
		}
		preview.SetText(text)
	}
	render()

	prompt := func(label, initial string, apply func(string) error) {
		in := tview.NewInputField().SetLabel(label).SetText(initial).SetFieldWidth(32)
		pov := u.overlayFor(pageNodeSchedPrompt, table, "")
		in.SetDoneFunc(func(k tcell.Key) {
			pov.Close()
			if k != tcell.KeyEnter {
				return
			}
			if err := apply(strings.TrimSpace(in.GetText())); err != nil {
				u.info(err.Error())
				return
			}
			render()
		})
		in.SetBorder(true)
		pov.show(centeredPrompt(in, 66), in)
	}

	toggle := func(r schedRow) {
		switch r.kind {
		case rowEnabled:
			s.Enabled = !s.Enabled
		case rowDay:
			s.Days[r.day] = !s.Days[r.day]
		case rowCat:
			setCat(&s, r.cat, !slices.Contains(s.Categories, r.cat))
		case rowImages:
			cycleImages(&s)
		}
		render()
	}
	edit := func(r schedRow) {
		switch r.kind {
		case rowStart:
			prompt("first run of the day (HH:MM): ", prunesched.FormatClock(s.Start), func(v string) error {
				m, err := prunesched.ParseClock(v)
				if err == nil {
					s.Start = m
				}
				return err
			})
		case rowTimes:
			prompt(fmt.Sprintf("runs per day (1-%d): ", prunesched.MaxTimesPerDay), fmt.Sprint(s.Times), func(v string) error {
				var n int
				if _, err := fmt.Sscan(v, &n); err != nil || n < 1 || n > prunesched.MaxTimesPerDay {
					return fmt.Errorf("%q: want a number from 1 to %d", v, prunesched.MaxTimesPerDay)
				}
				s.Times = n
				return nil
			})
		case rowTZ:
			prompt("time zone (IANA, e.g. Europe/Berlin): ", s.TZ, func(v string) error {
				if _, err := time.LoadLocation(v); err != nil || v == "" {
					return fmt.Errorf("%q is not a time zone", v)
				}
				s.TZ = v
				return nil
			})
		case rowUntil:
			prompt("keep stopped containers for (24h, 2h, 0): ", prunesched.FormatUntil(s.ContainersUntil), func(v string) error {
				d, err := prunesched.ParseUntil(v)
				if err == nil {
					s.ContainersUntil = d
				}
				return err
			})
		case rowFilter:
			prompt("only objects labelled (k or k=v, comma-separated): ", strings.Join(s.LabelFilters, ","), func(v string) error {
				f, err := prunesched.ParseLabelFilters(v)
				if err == nil {
					s.LabelFilters = f
				}
				return err
			})
		default:
			toggle(r)
		}
	}
	save := func() {
		if err := s.Validate(); err != nil {
			u.info("cannot save: " + err.Error())
			return
		}
		msg := fmt.Sprintf("Save the prune schedule for ALL nodes?\n\n%s\n\nAgents older than this release ignore the schedule.", scheduleRestartWarning)
		u.confirm(msg, "Save", table, func() {
			ov.Close()
			saved := s
			u.flash(" [gray]saving the prune schedule…[white]")
			go func() {
				err := saveAgentSchedule(u.ctx, u.dcli, svc, saved)
				app.QueueUpdateDraw(func() {
					if err != nil {
						u.info("prune schedule: " + err.Error())
						return
					}
					u.flash(" [green]✓ prune schedule saved — the agents restart one by one[white]")
				})
			}()
		})
	}

	table.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		row, _ := table.GetSelection()
		if row < 0 || row >= len(rows) {
			row = 0
		}
		r := rows[row]
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == u.km.Quit):
			ov.Close()
			return nil
		case ev.Key() == tcell.KeyEnter:
			edit(r)
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == ' ':
			toggle(r)
			return nil
		case ev.Key() == tcell.KeyRune && (ev.Rune() == '+' || ev.Rune() == '-'):
			if ev.Rune() == '+' && s.Times < prunesched.MaxTimesPerDay {
				s.Times++
			}
			if ev.Rune() == '-' && s.Times > 1 {
				s.Times--
			}
			render()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'w':
			save()
			return nil
		}
		return ev
	})
	ov.show(centered(layout, 84, len(rows)+7), table)
}
