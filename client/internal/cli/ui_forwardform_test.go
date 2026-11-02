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

// forwardFormUI is a ui with just enough state for the forward dialog, and
// one forward already running on local port 8080 so a conflict can be hit.
func forwardFormUI() *ui {
	u := overlayUI()
	u.forwards = newForwardRegistry()
	u.clusterState = &clusterState{ctx: context.Background()}
	u.f = &uiFlags{connectTimeout: time.Second}
	u.ctree = tview.NewTreeView()
	// Long names, so the conflict sentence is longer than the dialog is wide.
	e := u.forwards.add("prod-eu-west-1", cand("other", "billing-postgres-primary", "n2"), 8080, 5432, func() {})
	u.forwards.markActive(e.id, "127.0.0.1:8080")
	return u
}

// openDialog returns the dialog the front page shows.
func openDialog(t *testing.T, u *ui) *fwdDialog {
	t.Helper()
	if !u.overlayOpen() {
		t.Fatal("the form must be open")
	}
	_, page := u.pages.GetFrontPage()
	cb, ok := page.(*centeredBox)
	if !ok {
		t.Fatalf("the form must float through centered(), got %T", page)
	}
	d, ok := cb.inner.(*fwdDialog)
	if !ok {
		t.Fatalf("unexpected form layout %T", cb.inner)
	}
	return d
}

// press sends a key the way the running app would: into the front page, which
// hands it down to whatever has focus — so the forms' own input captures see
// it too.
func press(u *ui, key tcell.Key) {
	_, page := u.pages.GetFrontPage()
	page.InputHandler()(tcell.NewEventKey(key, 0, tcell.ModNone), func(p tview.Primitive) { u.app.SetFocus(p) })
}

// The form itself, driven through its widgets: k9s's field order, the local
// port follows what is typed into the container port, a non-loopback address
// shows the warning, and a conflict keeps the form open with the reason.
func TestPortPromptForm(t *testing.T) {
	u := forwardFormUI()
	u.portPrompt(cand("abc123456789", "api", "n1"))
	d := openDialog(t, u)

	for i, want := range []string{"Container Port", "Local Port", "Address"} {
		if got := d.fields.GetFormItem(i).GetLabel(); got != want {
			t.Errorf("field %d = %q, want %q", i, got, want)
		}
	}
	if d.addr.GetText() != "127.0.0.1" {
		t.Errorf("address default = %q", d.addr.GetText())
	}
	if u.app.GetFocus() != d.cport {
		t.Errorf("the container port must have focus, got %T", u.app.GetFocus())
	}
	d.cport.SetText("8080")
	if d.lport.GetText() != "8080" {
		t.Errorf("local port did not follow: %q", d.lport.GetText())
	}
	if strings.Contains(d.status.GetText(true), "reachable") {
		t.Errorf("loopback must not warn: %q", d.status.GetText(true))
	}
	d.addr.SetText("0.0.0.0")
	if !strings.Contains(d.status.GetText(true), "reachable from your network") {
		t.Errorf("no warning for 0.0.0.0: %q", d.status.GetText(true))
	}

	// Enter on a field submits; 8080 is taken, so the form stays with why,
	// and the local port — the field to correct — gets the focus.
	press(u, tcell.KeyEnter)
	if !u.overlayOpen() {
		t.Fatal("a port conflict must keep the form open")
	}
	if txt := d.status.GetText(true); !strings.Contains(txt, "already forwarded") || !strings.Contains(txt, "prod") {
		t.Errorf("conflict reason missing: %q", txt)
	}
	if u.app.GetFocus() != d.lport {
		t.Errorf("a conflict must focus the local port, got %T", u.app.GetFocus())
	}

	// Esc cancels.
	press(u, tcell.KeyEscape)
	if u.overlayOpen() {
		t.Error("Esc must close the form")
	}
}

// Enter is "forward" in every field, the last one included — the form does
// not use tview.Form's Enter-moves-on.
func TestForwardFormEnterOnAddressSubmits(t *testing.T) {
	u := forwardFormUI()
	var got []forwardSpec
	u.openForwardForm(cand("abc123456789", "api", "n1"), func(s forwardSpec) { got = append(got, s) })
	d := openDialog(t, u)
	d.cport.SetText("9000")
	d.lport.SetText("9001")
	press(u, tcell.KeyTab)
	press(u, tcell.KeyTab)
	if u.app.GetFocus() != d.addr {
		t.Fatalf("two Tabs must reach the address, got %T", u.app.GetFocus())
	}
	press(u, tcell.KeyEnter)
	if u.overlayOpen() {
		t.Error("a valid form must close on Enter")
	}
	if want := (forwardSpec{address: "127.0.0.1", local: 9001, remote: 9000}); len(got) != 1 || got[0] != want {
		t.Errorf("started %+v, want one %+v", got, want)
	}
}

// Tab runs through the fields into the buttons and round again; Shift-Tab
// goes the other way; the Forward button submits, by key and by mouse.
func TestForwardFormButtons(t *testing.T) {
	u := forwardFormUI()
	var got []forwardSpec
	u.openForwardForm(cand("abc123456789", "api", "n1"), func(s forwardSpec) { got = append(got, s) })
	d := openDialog(t, u)
	forward, cancel := d.buttons.GetButton(0), d.buttons.GetButton(1)

	order := []tview.Primitive{d.lport, d.addr, forward, cancel, d.cport}
	for i, want := range order {
		press(u, tcell.KeyTab)
		if u.app.GetFocus() != want {
			t.Fatalf("Tab %d: focus on %T, want %T", i+1, u.app.GetFocus(), want)
		}
	}
	press(u, tcell.KeyBacktab)
	if u.app.GetFocus() != cancel {
		t.Fatalf("Shift-Tab from the first field must wrap to Cancel, got %T", u.app.GetFocus())
	}
	press(u, tcell.KeyLeft)
	if u.app.GetFocus() != forward {
		t.Fatalf("← must move between the buttons, got %T", u.app.GetFocus())
	}

	// No container port yet: the button reports it the way Enter would.
	press(u, tcell.KeyEnter)
	if !u.overlayOpen() || !strings.Contains(d.status.GetText(true), "invalid container port") {
		t.Fatalf("an empty container port must keep the form open: %q", d.status.GetText(true))
	}

	// A click on Forward submits too.
	d.cport.SetText("8081")
	drawForm(u, 80, 24)
	bx, by, _, _ := forward.GetRect()
	click := func(action tview.MouseAction) {
		_, page := u.pages.GetFrontPage()
		page.MouseHandler()(action, tcell.NewEventMouse(bx+1, by, tcell.Button1, tcell.ModNone), func(p tview.Primitive) { u.app.SetFocus(p) })
	}
	click(tview.MouseLeftDown)
	click(tview.MouseLeftUp)
	click(tview.MouseLeftClick)
	if u.overlayOpen() {
		t.Error("clicking Forward must close the form")
	}
	if want := (forwardSpec{address: "127.0.0.1", local: 8081, remote: 8081}); len(got) != 1 || got[0] != want {
		t.Errorf("started %+v, want one %+v", got, want)
	}
}

// Cancel closes without starting anything.
func TestForwardFormCancelButton(t *testing.T) {
	u := forwardFormUI()
	started := false
	u.openForwardForm(cand("abc123456789", "api", "n1"), func(forwardSpec) { started = true })
	d := openDialog(t, u)
	d.cport.SetText("8081")
	press(u, tcell.KeyBacktab) // first field → last button
	press(u, tcell.KeyEnter)
	if u.overlayOpen() || started {
		t.Errorf("Cancel must close without forwarding (open=%v started=%v)", u.overlayOpen(), started)
	}
}

// drawForm renders the pages on a w×h screen and returns its rows.
func drawForm(u *ui, w, h int) []string {
	screen := tcell.NewSimulationScreen("UTF-8")
	_ = screen.Init()
	screen.SetSize(w, h)
	u.pages.SetRect(0, 0, w, h)
	u.pages.Draw(screen)
	rows := make([]string, h)
	for y := range h {
		var b strings.Builder
		for x := 0; x < w; {
			str, _, cw := screen.Get(x, y)
			if str == "" {
				str = " "
			}
			b.WriteString(str)
			x += max(cw, 1)
		}
		rows[y] = b.String()
	}
	return rows
}

// The dialog keeps its border around everything it says: the warning and a
// long error wrap inside it, on a roomy terminal and on a small one, where it
// also leaves the footer rows free and still shows its buttons.
func TestForwardFormLayout(t *testing.T) {
	applyTheme(backdropDim) // the app's rounded borders, as the real UI draws them
	for _, size := range [][2]int{{80, 24}, {90, 14}} {
		w, h := size[0], size[1]
		u := forwardFormUI()
		u.portPrompt(cand("abc123456789", "api", "n1"))
		d := openDialog(t, u)
		d.addr.SetText("0.0.0.0")
		d.cport.SetText("8080")
		press(u, tcell.KeyEnter) // conflict: the longest status there is

		rows := drawForm(u, w, h)
		screen := strings.Join(rows, "\n")
		for _, want := range []string{"Port Forward", "Container Port", "Local Port", "Address", "Forward ", "Cancel", "reachable from your network", "prod-eu-west-1"} {
			if !strings.Contains(screen, want) {
				t.Errorf("%dx%d: %q not on screen:\n%s", w, h, want, screen)
			}
		}
		top, bottom := -1, -1
		for y, r := range rows {
			if strings.Contains(r, "╭") {
				top = y
			}
			if strings.Contains(r, "╰") {
				bottom = y
			}
		}
		if top < 0 || bottom < 0 {
			t.Fatalf("%dx%d: no dialog border:\n%s", w, h, screen)
		}
		// The right border is one column for the whole dialog; nothing may
		// be drawn past it.
		col := strings.IndexRune(string([]rune(rows[top])), '╮')
		col = len([]rune(rows[top][:col]))
		for y := top; y <= bottom; y++ {
			r := []rune(rows[y])
			if c := r[col]; c != '│' && c != '╮' && c != '╯' {
				t.Errorf("%dx%d row %d: right border broken: %q", w, h, y, rows[y])
			}
			if rest := strings.TrimSpace(string(r[col+1:])); rest != "" {
				t.Errorf("%dx%d row %d: text past the border: %q", w, h, y, rows[y])
			}
		}
		if bottom >= h-overlayFooterReserve {
			t.Errorf("%dx%d: the dialog covers the footer (bottom border on row %d)", w, h, bottom)
		}
		t.Logf("%dx%d:\n%s", w, h, screen)
	}
}
