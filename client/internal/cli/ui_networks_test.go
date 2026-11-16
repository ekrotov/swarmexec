// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/rivo/tview"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/swarmops"
)

func (m *listManager) TaskList(context.Context, client.TaskListOptions) (client.TaskListResult, error) {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	return client.TaskListResult{Items: m.tasks}, nil
}

func (m *listManager) SwarmInspect(context.Context, client.SwarmInspectOptions) (client.SwarmInspectResult, error) {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	return client.SwarmInspectResult{Swarm: m.swarmInf}, nil
}

func (m *listManager) NetworkCreate(_ context.Context, name string, o client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	m.created = append(m.created, o)
	m.netNames = append(m.netNames, name)
	return client.NetworkCreateResult{}, nil
}

func (m *listManager) createdNames() string {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	return strings.Join(m.netNames, ",")
}

func subnetNet(name, subnet string) network.Summary {
	n := netSummary(name)
	n.IPAM.Config = []network.IPAMConfig{{Subnet: netip.MustParsePrefix(subnet)}}
	return n
}

// overlapManager is the operator's cluster: the hosts on 10.0.1.0/24 of a
// 10.0.0.0/16 underlay, dbnet on top of them, webnet inside the underlay.
func overlapManager() *listManager {
	m := &listManager{networks: []network.Summary{
		subnetNet("dbnet", "10.0.1.0/24"), subnetNet("webnet", "10.0.2.0/24"), subnetNet("appnet", "10.100.1.0/24"),
	}}
	m.nodes = []swarm.Node{readyNode("n2", "docker2v2"), readyNode("n3", "docker3")}
	m.nodes[0].Status.Addr, m.nodes[1].Status.Addr = "10.0.1.2", "10.0.1.3"
	var tk swarm.Task
	tk.Status.State = swarm.TaskStateRunning
	tk.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: "c-db"}
	tk.NetworksAttachments = []swarm.NetworkAttachment{{Network: swarm.Network{ID: "id-dbnet"}}}
	m.tasks = []swarm.Task{tk, tk}
	return m
}

func withUnderlay(t *testing.T, u *ui, cidrs ...string) {
	t.Helper()
	onLoop(t, u, func() {
		u.cfg.Contexts = map[string]config.ContextConfig{u.name: {Underlay: cidrs}}
	})
}

// The table shows subnet, a red/yellow marker and the running containers;
// the copy carries the same columns.
func TestNetworksTabShowsSubnetOverlapAndContainers(t *testing.T) {
	m := overlapManager()
	u := listUI(t, m)
	withUnderlay(t, u, "10.0.0.0/16")
	nt := u.netTab()
	onLoop(t, u, func() { nt.enter() })
	waitForTable(t, u, nt.table, "SUBNET", "CONTAINERS", "⚠ 10.0.1.0/24", "⚠ 10.0.2.0/24", "10.100.1.0/24")

	var db, web, app tcell.Color
	var dbCtr string
	onLoop(t, u, func() {
		for r := 1; r < nt.table.GetRowCount(); r++ {
			fg, _, _ := nt.table.GetCell(r, 4).Style.Decompose()
			switch nt.table.GetCell(r, 0).Text {
			case "dbnet":
				db, dbCtr = fg, nt.table.GetCell(r, 7).Text
			case "webnet":
				web = fg
			case "appnet":
				app = fg
			}
		}
	})
	if db != tcell.ColorRed || web != tcell.ColorYellow || app == tcell.ColorRed || app == tcell.ColorYellow {
		t.Errorf("subnet colours db=%v web=%v app=%v, want red / yellow / neither", db, web, app)
	}
	if dbCtr != "2" {
		t.Errorf("dbnet CONTAINERS = %q, want 2", dbCtr)
	}

	var b strings.Builder
	onLoop(t, u, func() { nt.yank(&b) })
	for _, want := range []string{"SUBNET\tSERVICES\tCONTAINERS\tOVERLAP", "dbnet\toverlay\tswarm\toverlay\t10.0.1.0/24\t0\t2\thigh", "webnet\toverlay\tswarm\toverlay\t10.0.2.0/24\t0\t0\twarn", "appnet\toverlay\tswarm\toverlay\t10.100.1.0/24\t0\t0\t-"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("yank lacks %q:\n%s", want, b.String())
		}
	}

	// The endpoint seam: a later per-node count lands next to CONTAINERS.
	onLoop(t, u, func() {
		nt.endpointNote = func(swarmops.Network) string { return "(3 ep)" }
		nt.render()
	})
	waitForTable(t, u, nt.table, "2 (3 ep)")
}

// Without an underlay only node IPs count: webnet is clean then.
func TestNetworksTabWithoutUnderlayFlagsOnlyHostIPs(t *testing.T) {
	m := overlapManager()
	u := listUI(t, m)
	nt := u.netTab()
	onLoop(t, u, func() { nt.enter() })
	text := waitForTable(t, u, nt.table, "⚠ 10.0.1.0/24", "10.0.2.0/24")
	if strings.Contains(text, "⚠ 10.0.2.0/24") {
		t.Errorf("webnet flagged without an underlay:\n%s", text)
	}
}

func TestNetHeaderExplainsAndGivesTheFix(t *testing.T) {
	n := swarmops.Network{
		Name: "dbnet", Subnets: []netip.Prefix{netip.MustParsePrefix("10.0.1.0/24")},
		Gateways: []netip.Addr{netip.MustParseAddr("10.0.1.1")}, Containers: 4,
		Overlaps: []swarmops.OverlapFinding{
			{Severity: swarmops.OverlapHigh, Message: "10.0.1.0/24 contains host IPs: 10.0.1.2 (docker2v2)"},
			{Severity: swarmops.OverlapWarn, Message: "10.0.1.0/24 overlaps the underlay 10.0.0.0/16"},
		},
	}
	text, lines := netHeader(n, 90)
	for _, want := range []string{
		"[gray]subnet[-] 10.0.1.0/24  [gray]gateway[-] 10.0.1.1  [gray]containers[-] 4",
		"[red]⚠ 10.0.1.0/24 contains host IPs: 10.0.1.2 (docker2v2)[-]",
		"[yellow]⚠ 10.0.1.0/24 overlaps the underlay 10.0.0.0/16[-]",
		"--subnet 10.100.1.0/24",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("header lacks %q:\n%s", want, text)
		}
	}
	if lines < 6 { // 4 lines, the remedy wraps, one blank
		t.Errorf("header height %d, too small for the remedy", lines)
	}
	if _, h := netHeader(swarmops.Network{Name: "host"}, 90); h != 0 {
		t.Errorf("a network without addressing gets a %d-line header", h)
	}
}

func TestMembersOverlayCarriesTheHeader(t *testing.T) {
	m := overlapManager()
	u := listUI(t, m)
	nt := u.netTab()
	onLoop(t, u, func() { nt.enter() })
	waitForTable(t, u, nt.table, "⚠ 10.0.1.0/24")
	var header string
	onLoop(t, u, func() {
		for _, n := range nt.rows() {
			if n.Name == "dbnet" {
				nt.showMembers(n)
			}
		}
		box, _ := u.pages.GetPage(pageNetMembers).(*centeredBox)
		if box == nil {
			return
		}
		if tv, ok := box.inner.(*tview.Flex).GetItem(0).(*tview.TextView); ok {
			header = tv.GetText(true)
		}
	})
	if !strings.Contains(header, "contains host IPs: 10.0.1.2 (docker2v2), 10.0.1.3 (docker3)") || !strings.Contains(header, "fix:") {
		t.Errorf("members header = %q", header)
	}
}

func TestCreateOverlapMessage(t *testing.T) {
	fs := []swarmops.OverlapFinding{{Message: "10.0.1.0/24 contains host IPs: 10.0.1.2 (a)"}}
	msg := createOverlapMessage(swarmops.NewNetworkOpts{Name: " db ", Subnet: "10.0.1.0/24"}, fs)
	for _, want := range []string{`Network "db"`, "⚠ 10.0.1.0/24 contains host IPs", "10.100.1.0/24", "Create anyway?"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

// openCreateForm opens the create form and fills name and subnet.
func openCreateForm(t *testing.T, u *ui, name, subnet string) *tview.Form {
	t.Helper()
	var form *tview.Form
	onLoop(t, u, func() {
		u.netTab().showCreate()
		box := u.pages.GetPage(pageNetForm).(*centeredBox)
		form = box.inner.(*tview.Form)
		form.GetFormItemByLabel("Name").(*tview.InputField).SetText(name)
		if subnet != "" {
			form.GetFormItemByLabel("Subnet (optional, e.g. 10.100.1.0/24)").(*tview.InputField).SetText(subnet)
		}
		u.app.SetFocus(form.GetButton(form.GetButtonIndex("Create")))
	})
	return form
}

func pressEnter(u *ui) { u.app.QueueEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone)) }

func waitCreated(t *testing.T, m *listManager, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for m.createdNames() != want {
		if time.Now().After(deadline) {
			t.Fatalf("created %q, want %q", m.createdNames(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitPage(t *testing.T, u *ui, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !pageOpen(t, u, name) {
		if time.Now().After(deadline) {
			t.Fatalf("page %s never opened", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A subnet on the hosts' range asks before creating; confirming creates it.
func TestCreateNetworkOnHostRangeAsksFirst(t *testing.T) {
	m := overlapManager()
	u := listUI(t, m)
	openCreateForm(t, u, "db2", "10.0.1.0/24")
	pressEnter(u)
	waitPage(t, u, pageConfirm)
	if got := m.createdNames(); got != "" {
		t.Fatalf("created %q before the confirm", got)
	}
	pressEnter(u) // the focused button: Create anyway
	waitCreated(t, m, "db2")
}

// A clean subnet creates directly.
func TestCreateNetworkCleanSubnetCreatesDirectly(t *testing.T) {
	m := overlapManager()
	u := listUI(t, m)
	withUnderlay(t, u, "10.0.0.0/16")
	openCreateForm(t, u, "ok", "10.200.1.0/24")
	pressEnter(u)
	waitCreated(t, m, "ok")
	if pageOpen(t, u, pageConfirm) {
		t.Error("a clean subnet must not ask")
	}
}

// No subnet: the default pool (unset = 10.0.0.0/8) holds the hosts, so it asks;
// with a pool set clear of them at swarm init it does not.
func TestCreateNetworkWithoutSubnetChecksTheDefaultPool(t *testing.T) {
	m := overlapManager()
	u := listUI(t, m)
	openCreateForm(t, u, "auto", "")
	pressEnter(u)
	waitPage(t, u, pageConfirm)
	onLoop(t, u, func() { u.app.SetFocus(u.pages.GetPage(pageConfirm)) })
	u.app.QueueEvent(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone)) // cancel
	deadline := time.Now().Add(10 * time.Second)
	for pageOpen(t, u, pageConfirm) {
		if time.Now().After(deadline) {
			t.Fatal("cancel did not close the confirm")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := m.createdNames(); got != "" {
		t.Fatalf("cancel created %q", got)
	}
	onLoop(t, u, func() { u.pages.RemovePage(pageNetForm) })

	m.lmu.Lock()
	m.swarmInf.DefaultAddrPool = []netip.Prefix{netip.MustParsePrefix("10.200.0.0/16")}
	m.lmu.Unlock()
	openCreateForm(t, u, "auto2", "")
	pressEnter(u)
	waitCreated(t, m, "auto2")
}
