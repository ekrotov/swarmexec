// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/mount"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (u *ui) editList(cfg editListConfig) {
	app := u.app
	title, applyVerb, items := cfg.title, cfg.applyVerb, cfg.items
	validate, onApply, suggest := cfg.validate, cfg.onApply, cfg.suggest
	allowEdit, multiline := cfg.allowEdit, cfg.multiline
	confirmNote, entryAction := cfg.confirmNote, cfg.entryAction
	formPrompt, back, after := cfg.formPrompt, cfg.back, cfg.after
	cur := append([]string{}, items...)
	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" %s ", title))
	keyPairs := []string{"a", "add"}
	if allowEdit {
		keyPairs = append(keyPairs, "e", "edit")
	}
	keyPairs = append(keyPairs, "d", "delete", keyLabel(u.km.Copy), "copy")
	if entryAction != nil {
		keyPairs = append(keyPairs, string(entryAction.key), entryAction.label)
	}
	keyPairs = append(keyPairs, "u", "undo", "w", "apply", "j/k", "move", "Esc", "cancel")
	// history stacks snapshots of cur before each staged change, so `u` undoes
	// one step (add/edit/delete) as long as nothing has been applied yet.
	var history [][]string
	render := func() {
		idx := list.GetCurrentItem()
		list.Clear()
		if len(cur) == 0 && len(items) == 0 {
			list.AddItem("(none)", "", 0, nil)
		} else {
			// Highlight staged (not-yet-applied) changes: an entry not present in
			// the original set is added/edited (green); originals missing from cur
			// are shown as dim-red "removed" ghosts. Diff is a multiset so it
			// survives undo without any parallel bookkeeping.
			origCount := map[string]int{}
			for _, it := range items {
				origCount[it]++
			}
			for _, it := range cur {
				label := tview.Escape(it)
				if origCount[it] > 0 {
					origCount[it]-- // unchanged — matches an original
				} else {
					label = "[green::b]" + label + " (new)[-:-:-]"
				}
				list.AddItem(label, "", 0, nil)
			}
			// origCount now holds the leftovers = removed items; show them (in the
			// original order) after the live rows. They sit beyond len(cur), so the
			// e/d/y handlers (which guard i<len(cur)) treat them as inert.
			shown := map[string]int{}
			for _, it := range items {
				if shown[it] < origCount[it] {
					shown[it]++
					list.AddItem("[red::d]- "+tview.Escape(it)+" (removed)[-:-:-]", "", 0, nil)
				}
			}
		}
		if idx >= 0 && idx < list.GetItemCount() {
			list.SetCurrentItem(idx)
		}
	}
	render()
	snapshot := func() { history = append(history, append([]string{}, cur...)) }
	undo := func() {
		if len(history) == 0 {
			return
		}
		cur = history[len(history)-1]
		history = history[:len(history)-1]
		render()
	}
	ov := u.overlayFor(pageListEdit, back, footerKeys(keyPairs...))
	closeEd := ov.Close
	setHelp := ov.setHelp
	// commit validates a typed entry and applies it via done.
	commit := func(raw string, done func(string)) {
		txt := strings.TrimSpace(raw)
		if txt == "" {
			return
		}
		norm, err := validate(txt)
		if err != nil {
			u.info(err.Error())
			return
		}
		done(norm)
		render()
	}
	prompt := func(label, initial string, done func(string)) {
		// One prompt overlay per invocation, whichever of the three shapes
		// below it takes; all of them go back to the list.
		pov := u.overlayFor(pageListEditPrompt, list, "")
		if formPrompt != nil {
			// Custom form: submit validates+stages+closes on success; the form
			// keeps focus on failure (it surfaces the error itself).
			cancel := pov.Close
			submit := func(raw string) error {
				norm, err := validate(strings.TrimSpace(raw))
				if err != nil {
					return err
				}
				done(norm)
				render()
				pov.Close()
				return nil
			}
			form := formPrompt(initial, submit, cancel)
			pov.show(centered(form, 78, 15), form)
			return
		}
		if multiline {
			// Long / multi-line values (e.g. GITLAB_OMNIBUS_CONFIG) are painful
			// in a one-line field, so edit them in a scrollable TextArea. Place
			// the cursor at the START (not the end): cursor-at-end scrolls a
			// freshly-built, not-yet-sized TextArea to its bottom, so the operator
			// only sees the tail of the value with the cursor pinned to the last
			// row. Start-of-text shows the value from the top.
			ta := tview.NewTextArea().SetText(initial, false)
			ta.SetBorder(true).SetTitle(" " + label + " — Ctrl-S save · Esc cancel ")
			ta.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
				switch ev.Key() {
				case tcell.KeyEscape:
					pov.Close()
					return nil
				case tcell.KeyCtrlS:
					txt := ta.GetText()
					pov.Close()
					commit(txt, done)
					return nil
				}
				return ev
			})
			pov.show(centered(ta, 96, 22), ta)
			return
		}
		in := tview.NewInputField().SetLabel(label).SetText(initial).SetFieldWidth(40)
		if suggest != nil {
			in.SetAutocompleteFunc(suggest)
		}
		in.SetDoneFunc(func(k tcell.Key) {
			pov.Close()
			if k != tcell.KeyEnter {
				return
			}
			commit(in.GetText(), done)
		})
		in.SetBorder(true)
		pov.show(centeredPrompt(in, 60), in)
	}
	// hasChanges reports whether anything is staged but not yet applied (cur
	// differs from the original set — order included; j/k only navigate here).
	hasChanges := func() bool {
		if len(cur) != len(items) {
			return true
		}
		for i := range cur {
			if cur[i] != items[i] {
				return true
			}
		}
		return false
	}
	// applyFlow runs the rolling-update confirmation and, on Apply, commits cur.
	// Shared by `w` and the "unapplied changes" reminder shown when leaving.
	applyFlow := func() {
		text := fmt.Sprintf("%s?\n\nThis triggers a rolling update of the service.", applyVerb)
		if confirmNote != nil {
			if note := confirmNote(cur); note != "" {
				text = note + "\n\n" + text
			}
		}
		u.confirm(text, "Apply", list, func() {
			go func() {
				err := onApply(cur)
				app.QueueUpdateDraw(func() {
					if err != nil {
						u.info("update failed: " + err.Error())
						app.SetFocus(list)
						return
					}
					ov.Close()
					u.info("service updated — rolling update started")
					if after != nil {
						after()
					}
				})
			}()
		})
	}
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape:
			if hasChanges() {
				// Don't silently drop staged edits — make the operator choose.
				lov := u.overlayFor(pageListEditLeave, nil, "")
				leave := tview.NewModal().
					SetText("You have unapplied changes.\n\nApply them now, discard them, or keep editing?\n(Esc discards and leaves.)").
					AddButtons([]string{"Apply", "Discard", "Keep editing"}).
					SetDoneFunc(func(_ int, lbl string) {
						lov.Close()
						switch lbl {
						case "Apply":
							applyFlow()
						case "Keep editing":
							app.SetFocus(list)
						default:
							// "Discard" button or Esc (empty label): leave the
							// editor, dropping the staged (never-applied) edits.
							// Esc must be an exit here — mapping it to "keep
							// editing" traps the operator, since Esc on the list
							// only re-opens this same guard, forever.
							closeEd()
						}
					})
				lov.show(newScrim(leave), leave)
				return nil
			}
			closeEd()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
			prompt("add: ", "", func(n string) { snapshot(); cur = append(cur, n) })
			return nil
		case allowEdit && ev.Key() == tcell.KeyRune && ev.Rune() == 'e':
			if i := list.GetCurrentItem(); i >= 0 && i < len(cur) {
				prompt("edit: ", cur[i], func(n string) { snapshot(); cur[i] = n })
			}
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
			if i := list.GetCurrentItem(); i >= 0 && i < len(cur) {
				snapshot()
				cur = append(cur[:i], cur[i+1:]...)
				render()
			}
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == u.km.Copy:
			if i := list.GetCurrentItem(); i >= 0 && i < len(cur) && u.screen != nil {
				u.screen.SetClipboard([]byte(cur[i]))
				setHelp(" [green]✓ copied[white] · " + footerKeys(keyPairs...))
			}
			return nil
		case entryAction != nil && ev.Key() == tcell.KeyRune && ev.Rune() == entryAction.key:
			if i := list.GetCurrentItem(); i >= 0 && i < len(cur) {
				entry := cur[i]
				closeEd()
				entryAction.run(entry)
			}
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'u':
			undo()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'w':
			applyFlow()
			return nil
		}
		return vimListKeys(ev)
	})
	ov.show(centered(list, 72, 18), list)
}

func (u *ui) openPortsEditor(svcName string, back tview.Primitive, after func()) {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	go func() {
		items, err := currentServicePorts(ctx, dcli, svcName)
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("cannot load ports: " + err.Error())
				return
			}
			u.editList(editListConfig{
				title:     "ports of " + svcName,
				applyVerb: "Update the published ports",
				items:     items,
				validate: func(s string) (string, error) {
					p, e := parseServicePort(s)
					if e != nil {
						return "", e
					}
					return formatServicePort(p), nil
				},
				onApply: func(list []string) error {
					ports, e := portsFromStrings(list)
					if e != nil {
						return e
					}
					return setServicePorts(ctx, dcli, svcName, ports)
				},
				allowEdit: true,
				back:      back,
				after:     after,
			})
		})
	}()
}

func (u *ui) openLabelsEditor(svcName string, back tview.Primitive, after func()) {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	go func() {
		items, err := currentServiceLabels(ctx, dcli, svcName)
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("cannot load labels: " + err.Error())
				return
			}
			u.editList(editListConfig{
				title:     "labels of " + svcName,
				applyVerb: "Update the labels",
				items:     items,
				validate: func(s string) (string, error) {
					k, v, e := parseLabel(s)
					if e != nil {
						return "", e
					}
					return k + "=" + v, nil
				},
				onApply: func(list []string) error {
					labels, e := labelsFromStrings(list)
					if e != nil {
						return e
					}
					return setServiceLabels(ctx, dcli, svcName, labels)
				},
				allowEdit: true,
				back:      back,
				after:     after,
			})
		})
	}()
}

func (u *ui) openAliasEditorForNet(svcName, netName, target string, back tview.Primitive, after func()) {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	go func() {
		att, err := serviceAttachedNetworks(ctx, dcli, svcName)
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("cannot load aliases: " + err.Error())
				return
			}
			var aliases []string
			found := false
			for _, a := range att {
				if a.Target == target || a.Name == netName {
					aliases = a.Aliases
					found = true
				}
			}
			if !found {
				u.info(fmt.Sprintf("service %q is not attached to network %q yet — apply the network first, then set aliases", svcName, netName))
				return
			}
			u.editList(editListConfig{
				title:     "aliases of " + svcName + " on " + netName,
				applyVerb: "Update the aliases",
				items:     aliases,
				validate: func(s string) (string, error) {
					s = strings.TrimSpace(s)
					if s == "" {
						return "", fmt.Errorf("alias must not be empty")
					}
					return s, nil
				},
				onApply:   func(list []string) error { return setNetworkAliases(ctx, dcli, svcName, target, list) },
				allowEdit: true,
				back:      back,
				after:     after,
			})
		})
	}()
}

func (u *ui) openNetworksEditor(svcName string, back tview.Primitive, after func()) {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	go func() {
		idByName, idToName, allNames := listNetworkRefs(ctx, dcli)
		current, err := currentServiceNetworks(ctx, dcli, svcName, idToName)
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("cannot load networks: " + err.Error())
				return
			}
			attached := map[string]bool{}
			for _, c := range current {
				attached[c] = true
			}
			u.editList(editListConfig{
				title:     "networks of " + svcName,
				applyVerb: "Update the networks",
				items:     current,
				validate: func(s string) (string, error) {
					if _, ok := idByName[s]; ok {
						return s, nil // known name
					}
					if n := idToName[s]; n != "" {
						return n, nil // an id → show its name
					}
					return "", fmt.Errorf("unknown network %q", s)
				},
				onApply: func(list []string) error {
					ids := make([]string, 0, len(list))
					for _, name := range list {
						id := idByName[name]
						if id == "" {
							id = name // already an id
						}
						ids = append(ids, id)
					}
					return setServiceNetworks(ctx, dcli, svcName, ids)
				},
				suggest: func(text string) []string {
					text = strings.ToLower(strings.TrimSpace(text))
					var out []string
					for _, n := range allNames {
						if attached[n] {
							continue
						}
						if text == "" || strings.Contains(strings.ToLower(n), text) {
							out = append(out, n)
						}
					}
					return out
				},
				entryAction: &listEntryAction{
					key:   'A',
					label: "aliases",
					run:   func(net string) { u.openAliasEditorForNet(svcName, net, idByName[net], back, after) },
				},
				back:  back,
				after: after,
			})
		})
	}()
}

func (u *ui) openSecretsEditor(svcName string, back tview.Primitive, after func()) {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	go func() {
		all := secretNames(ctx, dcli)
		current, err := currentServiceSecrets(ctx, dcli, svcName)
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("cannot load secrets: " + err.Error())
				return
			}
			known := map[string]bool{}
			for _, n := range all {
				known[n] = true
			}
			attached := map[string]bool{}
			for _, c := range current {
				attached[c] = true
			}
			u.editList(editListConfig{
				title:     "secrets of " + svcName,
				applyVerb: "Update the secrets",
				items:     current,
				validate: func(s string) (string, error) {
					if known[s] {
						return s, nil
					}
					return "", fmt.Errorf("unknown secret %q", s)
				},
				onApply: func(list []string) error { return setServiceSecrets(ctx, dcli, svcName, list) },
				suggest: func(text string) []string {
					text = strings.ToLower(strings.TrimSpace(text))
					var out []string
					for _, n := range all {
						if attached[n] {
							continue
						}
						if text == "" || strings.Contains(strings.ToLower(n), text) {
							out = append(out, n)
						}
					}
					return out
				},
				back:  back,
				after: after,
			})
		})
	}()
}

func (u *ui) openEnvEditor(svcName string, back tview.Primitive, after func()) {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	go func() {
		items, err := currentServiceEnv(ctx, dcli, svcName)
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("cannot load env: " + err.Error())
				return
			}
			u.editList(editListConfig{
				title:     "env of " + svcName,
				applyVerb: "Update the environment",
				items:     items,
				validate: func(s string) (string, error) {
					k, v, e := parseEnv(s)
					if e != nil {
						return "", e
					}
					return k + "=" + v, nil
				},
				onApply: func(list []string) error {
					env, e := envFromStrings(list)
					if e != nil {
						return e
					}
					return setServiceEnv(ctx, dcli, svcName, env)
				},
				allowEdit: true,
				multiline: true,
				back:      back,
				after:     after,
			})
		})
	}()
}

func (u *ui) placementListEditor(title, applyVerb string, items, cand []string, validate func(string) (string, error), apply func([]string) error, back tview.Primitive, after func()) {
	have := map[string]bool{}
	for _, it := range items {
		have[it] = true
	}
	u.editList(editListConfig{
		title:     title,
		applyVerb: applyVerb,
		items:     items,
		validate:  validate,
		onApply:   apply,
		suggest: func(text string) []string {
			text = strings.ToLower(strings.TrimSpace(text))
			var out []string
			for _, c := range cand {
				if have[c] {
					continue
				}
				if text == "" || strings.Contains(strings.ToLower(c), text) {
					out = append(out, c)
				}
			}
			return out
		},
		allowEdit: true,
		back:      back,
		after:     after,
	})
}

func (u *ui) openPlacementConstraintsEditor(svcName string, back tview.Primitive, after func()) {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	go func() {
		items, err := currentServicePlacement(ctx, dcli, svcName)
		cand, cerr := placementSuggestions(ctx, dcli)
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("cannot load placement: " + err.Error())
				return
			}
			if cerr != nil {
				cand = nil // autocomplete is best-effort
			}
			u.placementListEditor("constraints of "+svcName, "Update the placement constraints",
				items, cand, validatePlacementConstraint,
				func(list []string) error { return setServicePlacement(ctx, dcli, svcName, list) },
				back, after)
		})
	}()
}

func (u *ui) openSpreadEditor(svcName string, back tview.Primitive, after func()) {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	go func() {
		items, err := currentServiceSpread(ctx, dcli, svcName)
		cand, cerr := spreadSuggestions(ctx, dcli)
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("cannot load spread preferences: " + err.Error())
				return
			}
			if cerr != nil {
				cand = nil
			}
			u.placementListEditor("spread of "+svcName, "Update the spread preferences",
				items, cand, validateSpreadDescriptor,
				func(list []string) error { return setServiceSpread(ctx, dcli, svcName, list) },
				back, after)
		})
	}()
}

func (u *ui) openPlacementMenu(svcName string, back tview.Primitive, after func()) {
	app := u.app
	// Every branch opens the next editor, which takes focus itself; only the
	// default branch hands it back, and it does that explicitly.
	mov := u.overlayFor(pagePlacementMenu, nil, "")
	m := tview.NewModal().
		SetText("Edit placement for " + svcName).
		AddButtons([]string{"Constraints", "Spread preferences", "Cancel"}).
		SetDoneFunc(func(_ int, lbl string) {
			mov.Close()
			switch lbl {
			case "Constraints":
				u.openPlacementConstraintsEditor(svcName, back, after)
			case "Spread preferences":
				u.openSpreadEditor(svcName, back, after)
			default:
				app.SetFocus(back)
			}
		})
	mov.show(newScrim(m), m)
}

func (u *ui) openMountsEditor(svcName string, back tview.Primitive, after func()) {
	app, r, cfg, dcli, f, ctx := u.app, u.r, u.cfg, u.dcli, u.f, u.ctx
	go func() {
		items, err := currentServiceMountSpecs(ctx, dcli, svcName)
		nodes, uneval, nerr := candidateNodesForService(ctx, dcli, svcName)
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("cannot load mounts: " + err.Error())
				return
			}
			confirmNote := func(list []string) string {
				var binds []string
				for _, it := range list {
					if m, e := parseServiceMount(it); e == nil && m.Type == mount.TypeBind {
						binds = append(binds, m.Source)
					}
				}
				if len(binds) == 0 {
					return ""
				}
				where := "(could not determine nodes)"
				if nerr == nil {
					where = fmt.Sprintf("%d node(s): %s", len(nodes), strings.Join(nodes, ", "))
				}
				msg := fmt.Sprintf("⚠ bind sources must ALREADY exist on every node this service can run on — swarmexec cannot verify this.\nnodes: %s\nbinds: %s", where, strings.Join(binds, ", "))
				if nerr == nil && len(uneval) > 0 {
					msg += fmt.Sprintf("\n(unevaluated constraints: %s — the real node set may be narrower)", strings.Join(uneval, ", "))
				}
				return msg
			}
			// volNames feeds the volume-mode autocomplete. Seed from whatever the
			// Volumes tab already loaded, then refresh in the background (volume
			// listing is a per-node agent fan-out, so we don't block opening).
			volNames := make([]string, 0, len(u.vols))
			for _, v := range u.vols {
				volNames = append(volNames, v.Name)
			}
			go func() {
				vnodes, e := r.Nodes(ctx)
				if e != nil {
					return
				}
				vs, _ := indexVolumes(ctx, cfg, vnodes, f.connectTimeout)
				names := make([]string, 0, len(vs))
				for _, v := range vs {
					names = append(names, v.Name)
				}
				sort.Strings(names)
				app.QueueUpdateDraw(func() { volNames = names })
			}()
			// mountForm splits a mount into separate inputs: a "bind mount" toggle,
			// a source (host path when bind, else a volume name with autocomplete),
			// the container path, and a read-only toggle. It replaces the old single
			// "type:source:target[:ro]" text entry.
			mountForm := func(initial string, submit func(raw string) error, cancel func()) tview.Primitive {
				isBind, src, tgt, ro := false, "", "", false
				if m, e := parseServiceMount(initial); e == nil {
					isBind, src, tgt, ro = m.Type == mount.TypeBind, m.Source, m.Target, m.ReadOnly
				}
				form := tview.NewForm()
				srcField := tview.NewInputField().SetText(src).SetFieldWidth(48)
				tgtField := tview.NewInputField().SetLabel("container path").SetText(tgt).SetFieldWidth(48).SetPlaceholder("/data")
				roCheck := tview.NewCheckbox().SetLabel("read-only").SetChecked(ro)
				volSuggest := func(text string) []string {
					text = strings.ToLower(strings.TrimSpace(text))
					var out []string
					for _, n := range volNames {
						if text == "" || strings.Contains(strings.ToLower(n), text) {
							out = append(out, n)
						}
					}
					return out
				}
				applyMode := func(bind bool) {
					if bind {
						srcField.SetLabel("host path").SetPlaceholder("/opt/app/config").SetAutocompleteFunc(nil)
					} else {
						srcField.SetLabel("volume").SetPlaceholder("volume name").SetAutocompleteFunc(volSuggest)
					}
				}
				bindCheck := tview.NewCheckbox().SetLabel("bind mount").SetChecked(isBind).
					SetChangedFunc(func(checked bool) { applyMode(checked) })
				applyMode(isBind)
				setTitle := func(t string) { form.SetTitle(tview.Escape(t)) }
				save := func() {
					typ := "volume"
					if bindCheck.IsChecked() {
						typ = "bind"
					}
					s := strings.TrimSpace(srcField.GetText())
					t := strings.TrimSpace(tgtField.GetText())
					if s == "" || t == "" {
						setTitle(" ⚠ source and container path are required ")
						return
					}
					raw := typ + ":" + s + ":" + t
					if roCheck.IsChecked() {
						raw += ":ro"
					}
					if e := submit(raw); e != nil {
						setTitle(" ⚠ " + e.Error() + " ")
					}
				}
				form.AddFormItem(bindCheck)
				form.AddFormItem(srcField)
				form.AddFormItem(tgtField)
				form.AddFormItem(roCheck)
				form.AddButton("Save", save)
				form.AddButton("Cancel", cancel)
				form.SetCancelFunc(cancel)
				form.SetBorder(true)
				if initial == "" {
					setTitle(" add mount · Tab moves · Enter on Save ")
				} else {
					setTitle(" edit mount · Tab moves · Enter on Save ")
				}
				return form
			}
			u.editList(editListConfig{
				title:     "mounts of " + svcName,
				applyVerb: "Update the mounts",
				items:     items,
				validate: func(s string) (string, error) {
					m, e := parseServiceMount(s)
					if e != nil {
						return "", e
					}
					return formatServiceMount(m), nil
				},
				onApply: func(list []string) error {
					ms, e := mountsFromStrings(list)
					if e != nil {
						return e
					}
					return setServiceMounts(ctx, dcli, svcName, ms)
				},
				allowEdit:   true,
				confirmNote: confirmNote,
				formPrompt:  mountForm,
				back:        back,
				after:       after,
			})
		})
	}()
}

func (u *ui) openScalePrompt(svcName string, back tview.Primitive, after func()) {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	go func() {
		cur, replicated, err := currentServiceReplicas(ctx, dcli, svcName)
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("cannot load service: " + err.Error())
				return
			}
			if !replicated {
				u.info(fmt.Sprintf("service %q is not replicated and cannot be scaled", svcName))
				return
			}
			in := tview.NewInputField().SetLabel("replicas: ").SetText(fmt.Sprintf("%d", cur)).
				SetFieldWidth(8).SetAcceptanceFunc(tview.InputFieldInteger)
			sov := u.overlayFor(pageScalePrompt, back, "")
			in.SetDoneFunc(func(k tcell.Key) {
				sov.Close()
				if k != tcell.KeyEnter {
					return
				}
				n, perr := strconv.ParseUint(strings.TrimSpace(in.GetText()), 10, 64)
				if perr != nil {
					u.info("invalid replica count")
					return
				}
				go func() {
					serr := scaleService(ctx, dcli, svcName, n)
					app.QueueUpdateDraw(func() {
						if serr != nil {
							u.info("scale failed: " + serr.Error())
							return
						}
						u.info(fmt.Sprintf("scaled %q to %d — reconciling", svcName, n))
						if after != nil {
							after()
						}
					})
				}()
			})
			in.SetBorder(true).SetTitle(" scale service ")
			sov.show(centeredPrompt(in, 50), in)
		})
	}()
}

func (u *ui) openForceUpdate(svcName string, back tview.Primitive, after func()) {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	u.confirm(fmt.Sprintf("Force-update %q?\n\nRedeploys the service (like docker service update --force): every task is restarted / rescheduled. Handy to unstick a service in an incomplete state (e.g. 1/2).", svcName), "Force update", back, func() {
		app.SetFocus(back)
		go func() {
			err := forceUpdateService(ctx, dcli, svcName)
			app.QueueUpdateDraw(func() {
				if err != nil {
					u.info("force update failed: " + err.Error())
					return
				}
				u.info(fmt.Sprintf("force-updating %q — reconciling", svcName))
				if after != nil {
					after()
				}
			})
		}()
	})
}

func (u *ui) promptDeleteOrphanSecrets(orphans []secretRef) {
	app, dcli, ctx, ctree := u.app, u.dcli, u.ctx, u.ctree
	names := make([]string, 0, len(orphans))
	for _, o := range orphans {
		names = append(names, o.Name)
	}
	oov := u.overlayFor(pageOrphanSecrets, ctree, "")
	m := tview.NewModal().
		SetText(fmt.Sprintf("The removed service used %d secret(s) that no other service references:\n\n%s\n\nDelete them too?", len(orphans), strings.Join(names, ", "))).
		AddButtons([]string{"Delete secrets", "Keep"}).
		SetDoneFunc(func(_ int, lbl string) {
			oov.Close()
			if lbl != "Delete secrets" {
				return
			}
			go func() {
				var failed []string
				for _, o := range orphans {
					if err := removeSecret(ctx, dcli, o.Name); err != nil {
						failed = append(failed, o.Name+": "+err.Error())
					}
				}
				app.QueueUpdateDraw(func() {
					if len(failed) == 0 {
						u.flash(fmt.Sprintf(" [green]deleted[white] %d orphaned secret(s)", len(orphans)))
						return
					}
					u.info("some secrets could not be deleted:\n" + joinLines(failed))
				})
			}()
		})
	oov.show(newScrim(m), m)
}

func (u *ui) openRemoveService(svcName string, back tview.Primitive, onRemoved func()) {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	u.confirm(fmt.Sprintf("Remove service %q?\n\nThis permanently deletes the service and stops all its tasks. It cannot be undone.", svcName), "Remove", back, func() {
		go func() {
			// Compute orphaned secrets BEFORE removal (we need the service's
			// spec and the other services' current usage).
			orphans, _ := secretsOnlyUsedBy(ctx, dcli, svcName)
			err := removeService(ctx, dcli, svcName)
			app.QueueUpdateDraw(func() {
				// Order matters: whatever is shown LAST owns the focus. Both
				// app.SetFocus(back) and onRemoved (which closes the inspect and
				// refocuses the tree) used to run *after* u.info, leaving the
				// notice on screen but unfocused — Esc/Enter went to the widget
				// behind it and the dialog looked stuck.
				if err != nil {
					app.SetFocus(back)
					u.info("remove failed: " + err.Error())
					return
				}
				if onRemoved != nil {
					onRemoved()
				}
				if len(orphans) > 0 {
					// The orphan prompt is the actionable follow-up and its text
					// already says the service was removed, so it replaces the
					// notice rather than stacking on top of it.
					u.promptDeleteOrphanSecrets(orphans)
					return
				}
				u.info(fmt.Sprintf("removed service %q", svcName))
			})
		}()
	})
}

// upgradeInfo is the version-picker data captured from the inspect upgrade row:
// the repo, the running tag, the newer same-family tags (autocomplete
// suggestions) and every known repo tag (to validate a typed-in override). For a
// :latest digest update newer/all are empty and only target is set.
type upgradeInfo struct {
	repo    string
	current string
	target  string   // default update target ref (repo:highest, or repo:latest@digest)
	newer   []string // newer versions, highest first
	all     []string // every tag the repo lists
}

// openImageVersionPicker drives an image update. For a version-pinned service
// with discrete tags it opens a prompt whose autocomplete suggests the newer
// versions (highest first) but whose field is freely editable — the operator can
// type any existing tag, including an older one to pin a known-good release. A
// typed tag is validated against the repo's tags, and a downgrade is confirmed
// with a warning. For a :latest service (no discrete versions) it falls back to a
// single confirm on the current-digest target.
// versionSuggestionLimit caps the autocomplete dropdown: falling back to every
// known tag can mean hundreds of entries on a busy repo.
const versionSuggestionLimit = 25

func (u *ui) openImageVersionPicker(svcName string, info upgradeInfo, back tview.Primitive, after func()) {
	// A :latest service with an update available has a digest-pinned target and
	// no version to pick between — keep the one-shot confirm. Routing it through
	// the picker would invite typing "latest", which resolves to the bare tag and
	// silently drops the digest pinning the update is there to refresh.
	if len(info.newer) == 0 && strings.Contains(info.target, "@sha256:") {
		u.confirmImageUpdate(svcName, info.target, false, back, after)
		return
	}
	// Nothing to offer: no newer version, and the registry returned no tags
	// (private or unreachable registry, or a digest-only ref). Confirming the
	// empty target here would set the service to an empty image.
	if len(info.newer) == 0 && len(info.all) == 0 {
		if info.target == "" {
			u.info(fmt.Sprintf("No versions available for %q.\n\nThe registry did not return any tags for this image — it may be private, unreachable, or the service may be pinned by digest.", svcName))
			return
		}
		u.confirmImageUpdate(svcName, info.target, false, back, after)
		return
	}

	in := tview.NewInputField().SetLabel("version: ").SetFieldWidth(24)
	if len(info.newer) > 0 {
		in.SetText(info.newer[0]) // default to the highest newer version
	} else {
		in.SetText(info.current)
	}
	// Suggest the newer versions when there are any — that is the common intent
	// and keeps the list short. With none (already newest, or pinning
	// deliberately) fall back to every known tag, so the field is still
	// browsable instead of silently offering nothing. Either way it stays
	// free-text, so any existing tag can be typed in.
	suggestFrom := info.newer
	if len(suggestFrom) == 0 {
		suggestFrom = info.all
	}
	in.SetAutocompleteFunc(func(cur string) []string {
		cur = strings.TrimSpace(cur)
		var out []string
		for _, v := range suggestFrom {
			if cur == "" || strings.Contains(v, cur) {
				out = append(out, v)
			}
			if len(out) >= versionSuggestionLimit {
				break // a big repo can list hundreds of tags
			}
		}
		return out
	})
	iov := u.overlayFor(pageImageVersion, back, "")
	in.SetDoneFunc(func(k tcell.Key) {
		iov.Close()
		if k != tcell.KeyEnter {
			return
		}
		tag := strings.TrimSpace(in.GetText())
		if tag == "" {
			return
		}
		// Reject a tag the registry doesn't have, so a typo can't pin the service
		// to an unpullable image. (knownTags is always populated on this path.)
		if len(info.all) > 0 && !slices.Contains(info.all, tag) {
			u.info(fmt.Sprintf("no such tag in %s: %q", info.repo, tag))
			return
		}
		u.confirmImageUpdate(svcName, info.repo+":"+tag, isDowngrade(info.current, tag), back, after)
	})
	in.SetBorder(true).SetTitle(fmt.Sprintf(" update %s — running %s ", svcName, info.current))
	iov.show(centeredPrompt(in, 60), in)
}

// confirmImageUpdate confirms and applies an image change (rolling update). When
// downgrade is set the prompt warns that the target is older than what runs.
func (u *ui) confirmImageUpdate(svcName, target string, downgrade bool, back tview.Primitive, after func()) {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	msg := fmt.Sprintf("Update %q to:\n\n%s\n\nThis triggers a rolling update.", svcName, target)
	if downgrade {
		msg = fmt.Sprintf("[yellow]⚠ This is a DOWNGRADE.[white]\n\nUpdate %q to:\n\n%s\n\nThis triggers a rolling update onto an older image.", svcName, target)
	}
	u.confirm(msg, "Update", back, func() {
		app.SetFocus(back)
		go func() {
			err := updateServiceImage(ctx, dcli, svcName, target)
			app.QueueUpdateDraw(func() {
				if err != nil {
					u.info("update failed: " + err.Error())
					return
				}
				u.info(fmt.Sprintf("updating %q — rolling update started", svcName))
				if after != nil {
					after()
				}
			})
		}()
	})
}

func (u *ui) showPlacementDiagnosis(svcName string, back tview.Primitive) {
	app, pages, dcli, ctx := u.app, u.pages, u.dcli, u.ctx
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true)
	tv.SetBorder(true).SetTitle(fmt.Sprintf(" placement of %s ", svcName))
	tv.SetText("  [gray]diagnosing…[-]")
	ov := u.overlayFor(pagePlaceDiag, back, footerKeys("j/k", "scroll", "Esc", "close"))
	closeDiag := ov.Close
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == u.km.Quit || ev.Rune() == 'i')):
			closeDiag()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		}
		return ev
	})
	ov.show(centered(tv, 90, 24), tv)
	go func() {
		rep, err := diagnoseServicePlacement(ctx, dcli, svcName)
		app.QueueUpdateDraw(func() {
			if !pages.HasPage(pagePlaceDiag) {
				return
			}
			if err != nil {
				tv.SetText("  [red]diagnosis failed:[-] " + tview.Escape(err.Error()))
				return
			}
			tv.SetText(renderPlaceReport(rep))
		})
	}()
}

func (u *ui) openResourcesEditor(svcName string, back tview.Primitive, after func()) {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	go func() {
		rc, err := currentServiceResources(ctx, dcli, svcName)
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("cannot load resources: " + err.Error())
				return
			}
			form := tview.NewForm()
			cpuL := tview.NewInputField().SetLabel("CPU limit (cores)").SetText(rc.CPULimit).SetFieldWidth(16).SetPlaceholder("e.g. 0.5 — empty = none")
			memL := tview.NewInputField().SetLabel("Memory limit").SetText(rc.MemLimit).SetFieldWidth(16).SetPlaceholder("e.g. 512m — empty = none")
			cpuR := tview.NewInputField().SetLabel("CPU reservation").SetText(rc.CPUReservation).SetFieldWidth(16).SetPlaceholder("optional")
			memR := tview.NewInputField().SetLabel("Memory reservation").SetText(rc.MemReservation).SetFieldWidth(16).SetPlaceholder("optional")
			setTitle := func(t string) { form.SetTitle(tview.Escape(t)) }
			rov := u.overlayFor(pageResEdit, back, footerKeys("Tab", "move", "Enter", "button", "Esc", "cancel"))
			closeForm := rov.Close
			apply := func() {
				cl, e1 := parseCPUCores(cpuL.GetText())
				ml, e2 := parseMemBytes(memL.GetText())
				cr, e3 := parseCPUCores(cpuR.GetText())
				mr, e4 := parseMemBytes(memR.GetText())
				for _, e := range []error{e1, e2, e3, e4} {
					if e != nil {
						setTitle(" ⚠ " + e.Error() + " ")
						return
					}
				}
				if cl > 0 && cr > cl {
					setTitle(" ⚠ CPU reservation exceeds the limit ")
					return
				}
				if ml > 0 && mr > ml {
					setTitle(" ⚠ memory reservation exceeds the limit ")
					return
				}
				u.confirm("Update resource limits for "+svcName+"?\n\nThis triggers a rolling update of the service.", "Apply", form, func() {
					go func() {
						aerr := setServiceResources(ctx, dcli, svcName, cl, ml, cr, mr)
						app.QueueUpdateDraw(func() {
							if aerr != nil {
								u.info("update failed: " + aerr.Error())
								app.SetFocus(form)
								return
							}
							closeForm()
							u.info("service updated — rolling update started")
							if after != nil {
								after()
							}
						})
					}()
				})
			}
			form.AddFormItem(cpuL)
			form.AddFormItem(memL)
			form.AddFormItem(cpuR)
			form.AddFormItem(memR)
			form.AddButton("Apply", apply)
			form.AddButton("Cancel", closeForm)
			form.SetCancelFunc(closeForm)
			form.SetBorder(true)
			setTitle(" resource limits of " + svcName + " — empty clears a limit ")
			rov.show(centered(form, 70, 15), form)
		})
	}()
}
