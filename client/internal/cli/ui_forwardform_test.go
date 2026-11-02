// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/swarm"
	"github.com/rivo/tview"

	pb "swarmexec/internal/pb"
)

// Both sources are merged into one list: ascending, each port once, and a zero
// (a PortConfig without a target) never offered.
func TestMergePortsDedupesAndSorts(t *testing.T) {
	got := mergePorts([]uint32{8080, 80, 0}, nil, []uint32{443, 80, 8080})
	if want := []uint32{80, 443, 8080}; !slices.Equal(got, want) {
		t.Errorf("mergePorts = %v, want %v", got, want)
	}
	if got := mergePorts(nil, nil); len(got) != 0 {
		t.Errorf("no sources must give no suggestions, got %v", got)
	}
}

// The service's endpoint lists a port in the spec and again once allocated; UDP
// targets cannot be forwarded and must not be suggested.
func TestServiceTargetPortsTCPOnly(t *testing.T) {
	svc := swarm.Service{
		Spec: swarm.ServiceSpec{EndpointSpec: &swarm.EndpointSpec{Ports: []swarm.PortConfig{
			{TargetPort: 8080, PublishedPort: 80, Protocol: "tcp"},
			{TargetPort: 53, Protocol: "udp"},
			{TargetPort: 9000}, // protocol omitted means tcp
		}}},
		Endpoint: swarm.Endpoint{Ports: []swarm.PortConfig{{TargetPort: 8080, PublishedPort: 80, Protocol: "tcp"}}},
	}
	if got, want := serviceTargetPorts(svc), []uint32{8080, 9000}; !slices.Equal(got, want) {
		t.Errorf("serviceTargetPorts = %v, want %v", got, want)
	}
	if got := serviceTargetPorts(swarm.Service{}); len(got) != 0 {
		t.Errorf("a service without an endpoint spec has no target ports, got %v", got)
	}
}

// The agent's reply covers every container on the node matching the filter; only
// the one being forwarded counts. An old agent's reply carries no ports at all.
func TestContainerPortsFor(t *testing.T) {
	list := []*pb.ContainerInfo{
		{Id: "aaaa1111", Ports: []uint32{5432}},
		{Id: "bbbb2222", Ports: []uint32{443, 80}},
		{Id: "cccc3333"}, // agent that predates the field
	}
	if got := containerPortsFor(list, "bbbb2222"); !slices.Equal(got, []uint32{80, 443}) {
		t.Errorf("full id: got %v", got)
	}
	if got := containerPortsFor(list, "bbbb"); !slices.Equal(got, []uint32{80, 443}) {
		t.Errorf("short id: got %v", got)
	}
	if got := containerPortsFor(list, "cccc3333"); len(got) != 0 {
		t.Errorf("old agent: got %v", got)
	}
	if got := containerPortsFor(list, "dddd"); len(got) != 0 {
		t.Errorf("unknown container: got %v", got)
	}
	if got := containerPortsFor(list, ""); len(got) != 0 {
		t.Errorf("no id must match nothing, got %v", got)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"":            true, // empty means the default
		"127.0.0.1":   true,
		"127.1.2.3":   true, // all of 127.0.0.0/8
		"::1":         true,
		"[::1]":       true,
		"localhost":   true,
		"LocalHost":   true,
		" 127.0.0.1":  true,
		"0.0.0.0":     false,
		"::":          false,
		"192.168.1.5": false,
		"myhost":      false,
	} {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestBrowseHost(t *testing.T) {
	for addr, want := range map[string]string{
		"":          "127.0.0.1",
		"0.0.0.0":   "127.0.0.1",
		"::":        "::1",
		"127.0.0.1": "127.0.0.1",
		"10.0.0.5":  "10.0.0.5",
	} {
		if got := browseHost(addr); got != want {
			t.Errorf("browseHost(%q) = %q, want %q", addr, got, want)
		}
	}
}

// The address carries through to everything that used to hard-code 127.0.0.1.
func TestForwardEntryAddressPropagates(t *testing.T) {
	r := newForwardRegistry()
	e := r.addAt("prod", "0.0.0.0", cand("abc", "api", "n1"), 8080, 80, func() {})
	got, _ := r.get(e.id)
	if s := got.localEndpoint(); s != "0.0.0.0:8080" {
		t.Errorf("starting endpoint = %q", s)
	}
	// Go binds the 0.0.0.0 wildcard dual-stack, so the listener reports [::].
	// What the operator typed is what they should see.
	r.markActive(e.id, "[::]:8080")
	got, _ = r.get(e.id)
	if s := got.localEndpoint(); s != "0.0.0.0:8080" {
		t.Errorf("active endpoint = %q, want the requested address", s)
	}
	if s := got.url(); s != "http://127.0.0.1:8080" {
		t.Errorf("url for 0.0.0.0 = %q, want it through loopback", s)
	}

	v6 := r.addAt("prod", "::1", cand("abc", "api", "n1"), 9090, 80, func() {})
	r.markActive(v6.id, "[::1]:9090")
	got, _ = r.get(v6.id)
	if s := got.localEndpoint(); s != "[::1]:9090" {
		t.Errorf("v6 endpoint = %q", s)
	}
	if s := got.url(); s != "http://[::1]:9090" {
		t.Errorf("v6 url = %q", s)
	}

	// The old entry point stays loopback.
	old := r.add("prod", cand("abc", "api", "n1"), 7070, 80, func() {})
	got, _ = r.get(old.id)
	if s := got.url(); s != "http://127.0.0.1:7070" {
		t.Errorf("default url = %q", s)
	}
}

func TestParseForwardForm(t *testing.T) {
	spec, err := parseForwardForm("80", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if spec != (forwardSpec{address: "127.0.0.1", local: 80, remote: 80}) {
		t.Errorf("defaults: %+v", spec)
	}
	spec, err = parseForwardForm(" 8080 ", "[::1]", "9090")
	if err != nil {
		t.Fatal(err)
	}
	if spec != (forwardSpec{address: "::1", local: 9090, remote: 8080}) {
		t.Errorf("explicit: %+v", spec)
	}
	for _, bad := range [][3]string{
		{"", "127.0.0.1", "80"},      // no container port
		{"70000", "127.0.0.1", "80"}, // out of range
		{"80", "127.0.0.1", "0"},     // local 0
		{"80", "not an address", ""}, // spaces
		{"80", "127.0.0.1:80", ""},   // a port in the address field
	} {
		if _, err := parseForwardForm(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("parseForwardForm(%q, %q, %q) accepted", bad[0], bad[1], bad[2])
		}
	}
}

// The local port follows the container port until the operator types their own,
// and goes back to following once they clear it.
func TestPortFollow(t *testing.T) {
	var p portFollow
	if l, ok := p.setContainer("80"); !ok || l != "80" {
		t.Fatalf("untouched local port must follow: %q %v", l, ok)
	}
	p.setLocal("80") // the field echoes the programmatic update
	if l, ok := p.setContainer("8080"); !ok || l != "8080" {
		t.Fatalf("still following: %q %v", l, ok)
	}
	p.setLocal("8080")
	p.setLocal("9090") // operator's own value
	if _, ok := p.setContainer("443"); ok {
		t.Fatal("an edited local port must not be overwritten")
	}
	p.setLocal("") // cleared: following again
	if l, ok := p.setContainer("5432"); !ok || l != "5432" {
		t.Fatalf("a cleared local port follows again: %q %v", l, ok)
	}
}

func TestPortCompletions(t *testing.T) {
	sug := []uint32{80, 443, 8080}
	if got := portCompletions(sug, "", false); !slices.Equal(got, []string{"80", "443", "8080"}) {
		t.Errorf("empty field offers all: %v", got)
	}
	if got := portCompletions(sug, "80", false); !slices.Equal(got, []string{"80", "8080"}) {
		t.Errorf("prefix: %v", got)
	}
	// A lone exact match would swallow the Enter that should start the forward.
	if got := portCompletions(sug, "443", false); got != nil {
		t.Errorf("lone exact match must offer nothing, got %v", got)
	}
	// ↓ asks for the whole list whatever is typed.
	if got := portCompletions(sug, "443", true); len(got) != 3 {
		t.Errorf("explicit list: %v", got)
	}
	if got := portCompletions(nil, "", true); got != nil {
		t.Errorf("no suggestions, no list: %v", got)
	}
}

// The form itself, driven through its widgets: the local port follows what is
// typed into the container port, a non-loopback address shows the warning, and
// a conflict keeps the form open with the reason.
func TestPortPromptForm(t *testing.T) {
	u := overlayUI()
	u.forwards = newForwardRegistry()
	u.clusterState = &clusterState{ctx: context.Background()}
	u.f = &uiFlags{connectTimeout: time.Second}
	u.ctree = tview.NewTreeView()
	e := u.forwards.add("prod", cand("other", "db", "n2"), 8080, 5432, func() {})
	u.forwards.markActive(e.id, "127.0.0.1:8080")

	u.portPrompt(cand("abc123456789", "api", "n1"))
	if !u.overlayOpen() {
		t.Fatal("the form must be open")
	}
	_, page := u.pages.GetFrontPage()
	box, ok := page.(*centeredBox).inner.(*tview.Flex)
	if !ok {
		t.Fatalf("unexpected form layout %T", page)
	}
	cport := box.GetItem(0).(*tview.InputField)
	addr := box.GetItem(1).(*tview.InputField)
	lport := box.GetItem(2).(*tview.InputField)
	status := box.GetItem(4).(*tview.TextView)

	if addr.GetText() != "127.0.0.1" {
		t.Errorf("address default = %q", addr.GetText())
	}
	cport.SetText("8080")
	if lport.GetText() != "8080" {
		t.Errorf("local port did not follow: %q", lport.GetText())
	}
	addr.SetText("0.0.0.0")
	if !strings.Contains(status.GetText(true), "reachable from your network") {
		t.Errorf("no warning for 0.0.0.0: %q", status.GetText(true))
	}

	// Enter on a field submits; 8080 is taken, so the form stays with why.
	cport.InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), func(tview.Primitive) {})
	if !u.overlayOpen() {
		t.Fatal("a port conflict must keep the form open")
	}
	if txt := status.GetText(true); !strings.Contains(txt, "already forwarded") || !strings.Contains(txt, "prod") {
		t.Errorf("conflict reason missing: %q", txt)
	}

	// Esc cancels.
	lport.InputHandler()(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone), func(tview.Primitive) {})
	if u.overlayOpen() {
		t.Error("Esc must close the form")
	}
}
