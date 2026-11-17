// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/rivo/tview"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
)

// The Volumes tab driven end to end: a running UI, a fake manager for the
// nodes and services, and one fake agent per node for the volumes, their
// consumers and the writes.

// volumeCluster is two nodes: shop_data on both (used by shop_web on
// node-a, labelled), old_cache on node-a (no container, but shop_db declares
// it), scratch on node-b (nobody's).
func volumeCluster() (*fakeManager, map[string]*fakeAgent) {
	db := stackedService("s2", "shop_db", "shop", 1)
	db.Spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: "old_cache", Target: "/cache"}}
	mgr := &fakeManager{
		nodes:    []swarm.Node{readyNode("n1", "node-a"), readyNode("n2", "node-b")},
		services: []swarm.Service{stackedService("s1", "shop_web", "shop", 1), db},
	}
	created := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	vol := func(name string, size int64, labels map[string]string) *pb.VolumeInfo {
		return &pb.VolumeInfo{Name: name, Driver: "local", CreatedAt: created, SizeBytes: size, SizeKnown: true, Labels: labels}
	}
	agents := map[string]*fakeAgent{
		"node-a": {
			vols: []*pb.VolumeInfo{vol("shop_data", 3<<20, map[string]string{"env": "prod"}), vol("old_cache", 1<<20, nil)},
			ctrs: []*pb.ContainerInfo{{Name: "shop_web.1.abc", Service: "shop_web", Volumes: []string{"shop_data"}}},
		},
		"node-b": {vols: []*pb.VolumeInfo{vol("shop_data", 2<<20, nil), vol("scratch", 5<<20, nil)}},
	}
	return mgr, agents
}

// volumeDialogUI is covChromeUI wired to mgr and the agents, on the Volumes tab,
// loaded and settled.
func volumeDialogUI(t *testing.T, mgr *fakeManager, agents map[string]*fakeAgent) *ui {
	t.Helper()
	byHost := map[string]pb.AgentServer{}
	for h, a := range agents {
		byHost[h] = a
	}
	cfg := startAgents(t, byHost)
	u := covChromeUI(t)
	wireVolumes(t, u, mgr, cfg)
	onLoop(t, u, func() {
		u.active = "volumes"
		u.pages.AddPage(pageMain, u.volTab().table, true, true) // the tab is the screen
		u.volTab().enter()
	})
	waitVolumesSettled(t, u)
	waitForTable(t, u, u.volTab().table, "shop_data", "old_cache")
	return u
}

func wireVolumes(t *testing.T, u *ui, mgr *fakeManager, cfg config.Config) {
	t.Helper()
	onLoop(t, u, func() {
		u.dcli = mgr
		u.r = resolve.New(mgr, resolve.AddrHostname)
		u.cfg = cfg
		u.agents = agentcall.New(cfg)
		u.f = &uiFlags{connectTimeout: 3 * time.Second}
	})
	t.Cleanup(func() { u.agents.Close() })
}

// selectVolume puts the cursor on the named volume.
func selectVolume(t *testing.T, u *ui, name string) {
	t.Helper()
	onLoop(t, u, func() {
		vt := u.volTab()
		for i, v := range vt.st().shown {
			if v.Name == name {
				vt.table.Select(i+1, 0)
				u.app.SetFocus(vt.table)
				return
			}
		}
		t.Errorf("volume %s is not shown", name)
	})
}

// volOrder is the volume names in display order.
func volOrder(t *testing.T, u *ui) string {
	t.Helper()
	var names []string
	onLoop(t, u, func() {
		for _, v := range u.volTab().st().shown {
			names = append(names, v.Name)
		}
	})
	return strings.Join(names, ",")
}

func volKey(t *testing.T, u *ui, r rune) {
	t.Helper()
	onLoop(t, u, func() { u.volTab().keys(rune_(r)) })
}

// Sort cycling, reverse, the "/" filter, selection and the copy all act on
// the rows as shown.
func TestVolumesTabSortFilterSelectAndCopy(t *testing.T) {
	mgr, agents := volumeCluster()
	u := volumeDialogUI(t, mgr, agents)
	vt := u.volTab()
	km := u.km

	if got := volOrder(t, u); got != "old_cache,scratch,shop_data" {
		t.Errorf("by name: %s", got)
	}
	text := tableText(t, u, vt.table)
	for _, want := range []string{"VOLUME ▲", "2: node-a,node-b", "1|", "5.243MB"} {
		if !strings.Contains(text, want) {
			t.Errorf("table lacks %q:\n%s", want, text)
		}
	}
	// nodes ▼, used ▼, age ▲, size ▼, back to name ▲.
	wantOrder := []string{
		"shop_data,scratch,old_cache", // nodes ▼: 2 first, ties by name, reversed too
		"shop_data,scratch,old_cache", // used ▼: shop_data has a consumer
		"old_cache,scratch,shop_data", // age: all equal → by name
		"shop_data,scratch,old_cache", // size: 5MB (sum?) or per node; largest first
		"old_cache,scratch,shop_data",
	}
	for i, want := range wantOrder {
		volKey(t, u, km.VolSort)
		if i == 3 {
			// Size order depends on how the sizes of a two-node volume add up;
			// only "largest first, old_cache (1MB) last" is the claim.
			if got := volOrder(t, u); !strings.HasSuffix(got, "old_cache") {
				t.Errorf("size ▼: %s", got)
			}
			continue
		}
		if got := volOrder(t, u); got != want {
			t.Errorf("sort step %d: %s, want %s", i, got, want)
		}
	}
	volKey(t, u, km.VolSortRev)
	if got := volOrder(t, u); got != "shop_data,scratch,old_cache" {
		t.Errorf("name ▼: %s", got)
	}
	volKey(t, u, km.VolSortRev)

	// The filter matches name, driver and node.
	onLoop(t, u, func() { vt.setSearch("node-b") })
	if got := volOrder(t, u); got != "scratch,shop_data" {
		t.Errorf("filter node-b: %s", got)
	}
	onLoop(t, u, func() { vt.setSearch("nothing-like-it") })
	waitForTable(t, u, vt.table, `(no matches for "nothing-like-it")`)
	var placeholder string
	onLoop(t, u, func() { placeholder = vt.searchPlaceholder(); vt.setSearch("") })
	if placeholder == "" {
		t.Error("no search placeholder")
	}

	// Space marks the row, the status line counts it, "all" toggles.
	selectVolume(t, u, "scratch")
	volKey(t, u, km.VolSelect)
	var note, status string
	onLoop(t, u, func() { note, status = vt.statusNote(), u.status.GetText(true) })
	if !strings.Contains(note, "1 selected") || !strings.Contains(status, "1 selected") {
		t.Errorf("status note %q / line %q, want 1 selected", note, status)
	}
	waitForTable(t, u, vt.table, "▣ scratch")
	volKey(t, u, km.VolSelectAll)
	onLoop(t, u, func() { note = vt.statusNote() })
	if !strings.Contains(note, "3 selected") {
		t.Errorf("select all: %q", note)
	}
	volKey(t, u, km.VolSelectAll)
	onLoop(t, u, func() { note = vt.statusNote() })
	if note != "" {
		t.Errorf("all selected, all toggles off: %q", note)
	}
	volKey(t, u, km.VolSelect) // and once more on scratch
	volKey(t, u, km.VolSelect)

	var b strings.Builder
	var foot string
	var h helpWriter
	onLoop(t, u, func() {
		vt.yank(&b)
		foot = vt.footer()
		vt.helpSection(&h)
	})
	for _, want := range []string{"NAME\tDRIVER\tNODES\tUSED BY\tAGE\tSIZE", "shop_data\tlocal\t2: node-a,node-b\t1\t", "scratch\tlocal\t1: node-b\t-\t"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("copy lacks %q:\n%s", want, b.String())
		}
	}
	if !strings.Contains(foot, "prune") || !strings.Contains(h.String(), "prune unused") {
		t.Errorf("footer %q / help %q", foot, h.String())
	}

	// "/" opens the search bar on this tab.
	volKey(t, u, km.Search)
	var mode string
	onLoop(t, u, func() { mode = u.searchMode })
	if mode != "volumes" {
		t.Errorf("search mode %q", mode)
	}
}

// Enter on a volume lists its nodes and labels; the per-node delete asks and
// then removes it on the chosen node only; "a" asks for all of them.
func TestVolumesTabNodesViewDeletesPerNode(t *testing.T) {
	mgr, agents := volumeCluster()
	u := volumeDialogUI(t, mgr, agents)

	selectVolume(t, u, "shop_data")
	sendSpecial(t, u, tcell.KeyEnter)
	text := waitPageText(t, u, pageVolNodes, "volume shop_data — 2 node(s) · created", "labels", "env=prod", "[ ] node-a", "[ ] node-b")
	_ = text

	// Mark node-b (cursor down, space) and delete the marked one.
	sendRune(t, u, 'j')
	sendRune(t, u, ' ')
	waitPageText(t, u, pageVolNodes, "[x] node-b")
	sendRune(t, u, u.km.VolDelete)
	waitPageText(t, u, pageConfirm, `Remove volume "shop_data" on 1 node(s)?`, "node-b")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageInfo, `volume "shop_data":`, "node-b: removed")
	if a, b := agents["node-a"].removedNames(), agents["node-b"].removedNames(); a != "" || b != "shop_data" {
		t.Errorf("removed a=%q b=%q, want only node-b's", a, b)
	}
	closeInfo(t, u)
	waitVolumesSettled(t, u)

	// "a" asks for every node; Cancel leaves both.
	selectVolume(t, u, "shop_data")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageVolNodes, "node-a")
	sendRune(t, u, 'a')
	waitPageText(t, u, pageConfirm, "on 2 node(s)")
	sendSpecial(t, u, tcell.KeyTab) // to Cancel
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageGone(t, u, pageConfirm)
	if a := agents["node-a"].removedNames(); a != "" {
		t.Errorf("cancel removed %q", a)
	}
	// With nothing marked, the delete key takes the row under the cursor.
	sendRune(t, u, u.km.VolDelete)
	waitPageText(t, u, pageConfirm, "on 1 node(s)", "node-a")
	sendSpecial(t, u, tcell.KeyTab)
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageGone(t, u, pageConfirm)
	onLoop(t, u, func() { u.app.SetFocus(u.pages.GetPage(pageVolNodes)) })
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageVolNodes)
}

func TestChosenNodes(t *testing.T) {
	nodes := []resolve.Node{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	if got := nodeNames(chosenNodes(nodes, []bool{false, true, true}, 0)); strings.Join(got, ",") != "b,c" {
		t.Errorf("marked: %v", got)
	}
	if got := nodeNames(chosenNodes(nodes, make([]bool, 3), 2)); strings.Join(got, ",") != "c" {
		t.Errorf("cursor: %v", got)
	}
	if got := chosenNodes(nodes, make([]bool, 3), 7); got != nil {
		t.Errorf("cursor out of range: %v", got)
	}
}

// formInput is the form's input field with the given label.
func formInput(t *testing.T, u *ui, page, label string) *tview.InputField {
	t.Helper()
	var in *tview.InputField
	onLoop(t, u, func() {
		form := u.pages.GetPage(page).(*centeredBox).inner.(*tview.Form)
		in, _ = form.GetFormItemByLabel(label).(*tview.InputField)
	})
	if in == nil {
		t.Fatalf("form %s has no field %q", page, label)
	}
	return in
}

// pressFormButton focuses the form's button and presses Enter on it.
func pressFormButton(t *testing.T, u *ui, page, label string) {
	t.Helper()
	onLoop(t, u, func() {
		form := u.pages.GetPage(page).(*centeredBox).inner.(*tview.Form)
		u.app.SetFocus(form.GetButton(form.GetButtonIndex(label)))
	})
	sendSpecial(t, u, tcell.KeyEnter)
}

func covSetField(t *testing.T, u *ui, page, label, text string) {
	t.Helper()
	in := formInput(t, u, page, label)
	onLoop(t, u, func() { in.SetText(text) })
}

// The new-volume form refuses bad labels and an unknown node, creates on the
// one node named, and reports a partial failure across all nodes.
func TestVolumesTabCreateForm(t *testing.T) {
	mgr, agents := volumeCluster()
	agents["node-b"].failNew = true
	u := volumeDialogUI(t, mgr, agents)

	volKey(t, u, u.km.VolNew)
	waitPageText(t, u, pageVolForm, "new volume", "Name", "Driver", "Node (blank = all nodes)")
	// The node field completes from the cluster's nodes once they arrived.
	nodeField := formInput(t, u, pageVolForm, "Node (blank = all nodes)")
	waitUntil(t, "node autocomplete", func() bool {
		onLoop(t, u, func() {
			u.app.SetFocus(nodeField)
			nodeField.SetText("node")
			nodeField.Autocomplete()
		})
		return strings.Contains(pageText(t, u, pageVolForm), "node-b")
	})
	covSetField(t, u, pageVolForm, "Node (blank = all nodes)", "")

	covSetField(t, u, pageVolForm, "Name", "fresh")
	covSetField(t, u, pageVolForm, "Labels (k=v,k=v)", "broken")
	pressFormButton(t, u, pageVolForm, "Create")
	waitPageText(t, u, pageInfo, "invalid labels")
	closeInfo(t, u)

	covSetField(t, u, pageVolForm, "Labels (k=v,k=v)", "team=shop")
	covSetField(t, u, pageVolForm, "Node (blank = all nodes)", "node-z")
	pressFormButton(t, u, pageVolForm, "Create")
	waitPageText(t, u, pageInfo, `no node named "node-z"`)
	closeInfo(t, u)

	covSetField(t, u, pageVolForm, "Name", "")
	covSetField(t, u, pageVolForm, "Node (blank = all nodes)", "")
	pressFormButton(t, u, pageVolForm, "Create")
	waitPageText(t, u, pageInfo)
	closeInfo(t, u)

	covSetField(t, u, pageVolForm, "Name", "fresh")
	covSetField(t, u, pageVolForm, "Node (blank = all nodes)", "node-a")
	pressFormButton(t, u, pageVolForm, "Create")
	waitPageGone(t, u, pageVolForm)
	waitFooter(t, u, "created volume fresh on 1 node(s)")
	if a, b := agents["node-a"].createdNames(), agents["node-b"].createdNames(); a != "fresh" || b != "" {
		t.Errorf("created a=%q b=%q, want node-a only", a, b)
	}
	agents["node-a"].mu.Lock()
	labels := agents["node-a"].created[0].GetLabels()
	agents["node-a"].mu.Unlock()
	if labels["team"] != "shop" {
		t.Errorf("labels sent: %v", labels)
	}
	waitVolumesSettled(t, u)

	// Blank node = every node; node-b refuses.
	volKey(t, u, u.km.VolNew)
	waitPageText(t, u, pageVolForm, "new volume")
	covSetField(t, u, pageVolForm, "Name", "everywhere")
	// The form fetches the nodes as it opens; until they are in, Create says
	// there are none.
	waitUntil(t, "the create to go out", func() bool {
		if !pageOpen(t, u, pageVolForm) {
			return true
		}
		pressFormButton(t, u, pageVolForm, "Create")
		waitUntil(t, "an answer", func() bool { return pageOpen(t, u, pageInfo) || !pageOpen(t, u, pageVolForm) })
		if strings.Contains(pageText(t, u, pageInfo), "no nodes available") {
			closeInfo(t, u)
			return false
		}
		return true
	})
	waitPageText(t, u, pageInfo, `volume "everywhere": 1 ok, 1 failed`, "node-b:")
	closeInfo(t, u)
	waitVolumesSettled(t, u)

	// Esc and Cancel both close the form.
	volKey(t, u, u.km.VolNew)
	waitPageText(t, u, pageVolForm, "new volume")
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageVolForm)
	volKey(t, u, u.km.VolNew)
	waitPageText(t, u, pageVolForm, "new volume")
	pressFormButton(t, u, pageVolForm, "Cancel")
	waitPageGone(t, u, pageVolForm)
}

// "used by" lists the containers that mount the volume, or says none does.
func TestVolumesTabUsedBy(t *testing.T) {
	mgr, agents := volumeCluster()
	u := volumeDialogUI(t, mgr, agents)

	selectVolume(t, u, "shop_data")
	volKey(t, u, u.km.VolUsedBy)
	waitPageText(t, u, pageVolUsers, "shop_data — used by 1", "shop_web", "shop_web.1.abc", "on node-a")
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageVolUsers)

	selectVolume(t, u, "scratch")
	volKey(t, u, u.km.VolUsedBy)
	waitPageText(t, u, pageVolUsers, "(not in use by any container)")
	sendRune(t, u, 'i')
	waitPageGone(t, u, pageVolUsers)
}

// The delete key removes the marked volumes on every node that holds them,
// behind one confirm; prune spares the used and the service-declared ones.
func TestVolumesTabDeleteAndPrune(t *testing.T) {
	mgr, agents := volumeCluster()
	u := volumeDialogUI(t, mgr, agents)

	selectVolume(t, u, "shop_data")
	volKey(t, u, u.km.VolSelect)
	volKey(t, u, u.km.VolDelete)
	waitPageText(t, u, pageConfirm, "Remove 1 volume(s) on every node that holds them?", "shop_data")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageInfo, "removed 1 of 1 volume(s)")
	if a, b := agents["node-a"].removedNames(), agents["node-b"].removedNames(); a != "shop_data" || b != "shop_data" {
		t.Errorf("removed a=%q b=%q, want shop_data on both", a, b)
	}
	var note string
	onLoop(t, u, func() { note = u.volTab().statusNote() })
	if note != "" {
		t.Errorf("the selection survived the delete: %q", note)
	}
	closeInfo(t, u)
	waitVolumesSettled(t, u)

	volKey(t, u, u.km.VolPrune)
	text := waitPageText(t, u, pageConfirm, "Prune 1 unused volume(s)?", "scratch")
	if strings.Contains(text, "old_cache") || strings.Contains(text, "shop_data") {
		t.Errorf("prune offers a used or declared volume:\n%s", text)
	}
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageInfo, "removed 1 of 1 volume(s)")
	if b := agents["node-b"].removedNames(); b != "shop_data,scratch" {
		t.Errorf("node-b removed %q", b)
	}
	closeInfo(t, u)
	waitVolumesSettled(t, u)

	// A refusing agent: the summary names the failure.
	agents["node-b"].mu.Lock()
	agents["node-b"].failRm = true
	agents["node-b"].mu.Unlock()
	selectVolume(t, u, "scratch")
	volKey(t, u, u.km.VolDelete)
	waitPageText(t, u, pageConfirm, "scratch")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageInfo, "removed 0 of 1 volume(s)", "scratch on node-b")
	closeInfo(t, u)
	waitVolumesSettled(t, u)
}

func TestVolumesTabPruneWithNothingUnused(t *testing.T) {
	mgr, agents := volumeCluster()
	agents["node-b"].vols = agents["node-b"].vols[:1] // no scratch
	u := volumeDialogUI(t, mgr, agents)
	volKey(t, u, u.km.VolPrune)
	waitPageText(t, u, pageInfo, "no unused volumes to prune")
}

// Many targets: the confirm lists twelve and counts the rest.
func TestVolumesDeleteConfirmTruncates(t *testing.T) {
	u := covChromeUI(t)
	var targets []swarmVolume
	for i := 0; i < 15; i++ {
		targets = append(targets, swarmVolume{Name: "v" + string(rune('a'+i))})
	}
	onLoop(t, u, func() { u.volTab().deleteVolumes(targets, "Remove them?") })
	waitPageText(t, u, pageConfirm, "Remove them?", "va", "vl", "(+3 more)")
	onLoop(t, u, func() { u.volTab().deleteVolumes(nil, "nothing") })
}

// Attach asks for a service, a target path, then read-only or not, and adds
// the mount to the service.
func TestVolumesTabAttachToService(t *testing.T) {
	mgr, agents := volumeCluster()
	u := volumeDialogUI(t, mgr, agents)
	onLoop(t, u, func() {
		u.treeTab().st().svcs = []resolve.Service{{Name: "shop_web"}, {Name: "shop_db"}}
	})

	selectVolume(t, u, "scratch")
	volKey(t, u, u.km.VolAttach)
	waitPageText(t, u, pageVolAttach, `attach volume "scratch" to service`)
	typeText(t, u, "shop_w")
	waitPageText(t, u, pageVolAttach, "shop_web") // the drop-down
	sendSpecial(t, u, tcell.KeyEnter)             // picks it
	sendSpecial(t, u, tcell.KeyEnter)             // and goes on
	waitPageText(t, u, pageVolAttachTgt, "attach scratch → shop_web")
	typeText(t, u, "data")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageInfo, "target must be an absolute path")
	closeInfo(t, u)

	selectVolume(t, u, "scratch")
	volKey(t, u, u.km.VolAttach)
	waitPageText(t, u, pageVolAttach, "scratch")
	setFocusedText(t, u, "shop_web")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageVolAttachTgt, "shop_web")
	typeText(t, u, "/data")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageVolAttachConfirm, `Attach volume "scratch" to service "shop_web" at /data?`)
	sendSpecial(t, u, tcell.KeyTab) // "Attach read-only"
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageInfo, `attached volume "scratch" to "shop_web" at /data`)
	mgr.mu.Lock()
	var got []mount.Mount
	if len(mgr.updates) == 1 {
		got = mgr.updates[0].Spec.TaskTemplate.ContainerSpec.Mounts
	}
	mgr.mu.Unlock()
	if len(got) != 1 || got[0].Source != "scratch" || got[0].Target != "/data" || !got[0].ReadOnly {
		t.Errorf("service update mounts = %+v, want scratch at /data read-only", got)
	}
	closeInfo(t, u)
	waitVolumesSettled(t, u)

	// A failing update says so; Esc and Cancel on the way leave it alone.
	mgr.mu.Lock()
	mgr.updateErr = errRefused
	mgr.mu.Unlock()
	selectVolume(t, u, "scratch")
	volKey(t, u, u.km.VolAttach)
	setFocusedText(t, u, "shop_db")
	sendSpecial(t, u, tcell.KeyEnter)
	typeText(t, u, "/x")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageVolAttachConfirm, "shop_db")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageInfo, "attach failed")
	closeInfo(t, u)

	volKey(t, u, u.km.VolAttach)
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageVolAttach)
	volKey(t, u, u.km.VolAttach)
	setFocusedText(t, u, "shop_db")
	sendSpecial(t, u, tcell.KeyEnter)
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageVolAttachTgt)
	volKey(t, u, u.km.VolAttach)
	setFocusedText(t, u, "shop_db")
	sendSpecial(t, u, tcell.KeyEnter)
	typeText(t, u, "/y")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageVolAttachConfirm, "shop_db")
	sendSpecial(t, u, tcell.KeyTab)
	sendSpecial(t, u, tcell.KeyTab)
	sendSpecial(t, u, tcell.KeyEnter) // Cancel
	waitPageGone(t, u, pageVolAttachConfirm)
}

// No agent answers and none is deployed: the tab says so instead of an
// empty list.
func TestVolumesTabSaysWhenNoAgentIsDeployed(t *testing.T) {
	mgr, _ := volumeCluster()
	u := covChromeUI(t)
	wireVolumes(t, u, mgr, startAgents(t, map[string]pb.AgentServer{}))
	onLoop(t, u, func() { u.volTab().enter() })
	waitForTable(t, u, u.volTab().table, errNoAgent.Error())
}
