// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/rivo/tview"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dial"
	"swarmexec/client/internal/resolve"
	pb "swarmexec/internal/pb"
)

// defaultForwardAddress is where a forward listens unless the operator says
// otherwise: loopback, so an internal port is not re-exposed to the network the
// operator's machine sits on. The CLI's --address defaults to the same.
const defaultForwardAddress = "127.0.0.1"

// isLoopbackAddr reports whether a local bind address keeps the forward on this
// machine: 127.0.0.0/8, ::1 or the name localhost. Empty counts as loopback,
// because empty means the default.
func isLoopbackAddr(addr string) bool {
	addr = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(addr), "["), "]")
	if addr == "" || strings.EqualFold(addr, "localhost") {
		return true
	}
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}

// normalizeForwardAddress trims an address typed into the form, drops the
// brackets an IPv6 literal is often written with, and fills in the default.
// It refuses anything that is neither an IP literal nor a plausible hostname,
// so a typo is reported in the form rather than as a failed forward.
func normalizeForwardAddress(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if strings.HasPrefix(addr, "[") && strings.HasSuffix(addr, "]") {
		addr = addr[1 : len(addr)-1]
	}
	if addr == "" {
		return defaultForwardAddress, nil
	}
	if net.ParseIP(addr) != nil {
		return addr, nil
	}
	for _, r := range addr {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return "", fmt.Errorf("invalid local address %q: expected an IP or a hostname", addr)
		}
	}
	return addr, nil
}

// browseHost is the host to put in a URL for a forward bound to addr. An
// unspecified bind address listens everywhere but is not something to connect
// to, so the URL goes through loopback, which it also listens on.
func browseHost(addr string) string {
	if addr == "" {
		return defaultForwardAddress
	}
	if ip := net.ParseIP(addr); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			return "127.0.0.1"
		}
		return "::1"
	}
	return addr
}

// mergePorts folds port lists from several sources into one ascending list
// without duplicates or zeros.
func mergePorts(lists ...[]uint32) []uint32 {
	var out []uint32
	for _, l := range lists {
		for _, p := range l {
			if p > 0 && p <= 65535 {
				out = append(out, p)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// serviceTargetPorts returns the container-side TCP ports a service declares in
// its endpoint: the spec's ports and the ones the manager actually allocated.
// Only ports declared for publishing appear here; EXPOSE comes from the agent.
func serviceTargetPorts(svc swarm.Service) []uint32 {
	var out []uint32
	add := func(ports []swarm.PortConfig) {
		for _, p := range ports {
			if p.Protocol == "" || strings.EqualFold(string(p.Protocol), "tcp") {
				out = append(out, p.TargetPort)
			}
		}
	}
	if svc.Spec.EndpointSpec != nil {
		add(svc.Spec.EndpointSpec.Ports)
	}
	add(svc.Endpoint.Ports)
	return mergePorts(out)
}

// formatPorts renders a port list for the form's hint line.
func formatPorts(ports []uint32) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.FormatUint(uint64(p), 10)
	}
	return strings.Join(parts, " · ")
}

// portCompletions is what the container-port field's drop-down offers for the
// text typed so far: every suggestion for an empty field (or when the operator
// asked for the list explicitly), otherwise those starting with the text.
//
// A lone exact match offers nothing. The drop-down takes Enter for itself, so
// an operator who has typed 8080 and presses Enter would otherwise be picking
// from a list instead of starting the forward.
func portCompletions(suggested []uint32, text string, all bool) []string {
	text = strings.TrimSpace(text)
	var out []string
	for _, p := range suggested {
		s := strconv.FormatUint(uint64(p), 10)
		if all || text == "" || strings.HasPrefix(s, text) {
			out = append(out, s)
		}
	}
	if !all && len(out) == 1 && out[0] == text {
		return nil
	}
	return out
}

// portFollow keeps the local port in step with the container port until the
// operator gives it a value of its own — the usual forward is the same port on
// both sides, and having to type it twice is the friction this removes.
//
// Following is derived, not flagged: the local port follows while it is empty
// or equal to the container port. Clearing it hands it back to following, and a
// programmatic update (which fires the field's changed event too) can never
// flip it to "edited" by accident.
type portFollow struct {
	container, local string
}

// setContainer records a new container port and reports the local port to show
// if it should follow.
func (p *portFollow) setContainer(text string) (local string, follow bool) {
	follow = p.local == "" || p.local == p.container
	p.container = text
	if follow {
		p.local = text
	}
	return p.local, follow
}

// setLocal records the local port field's text.
func (p *portFollow) setLocal(text string) { p.local = text }

// forwardSpec is what the form produces.
type forwardSpec struct {
	address       string
	local, remote uint32
}

// parseForwardForm validates the three fields. An empty local port means the
// same as the container port, matching `port-forward svc 8080`.
func parseForwardForm(container, address, local string) (forwardSpec, error) {
	parse := func(s, what string) (uint32, error) {
		n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
		if err != nil || n == 0 || n > 65535 {
			return 0, fmt.Errorf("invalid %s port %q: expected 1-65535", what, strings.TrimSpace(s))
		}
		return uint32(n), nil
	}
	var spec forwardSpec
	var err error
	if spec.remote, err = parse(container, "container"); err != nil {
		return forwardSpec{}, err
	}
	if spec.address, err = normalizeForwardAddress(address); err != nil {
		return forwardSpec{}, err
	}
	spec.local = spec.remote
	if strings.TrimSpace(local) != "" {
		if spec.local, err = parse(local, "local"); err != nil {
			return forwardSpec{}, err
		}
	}
	return spec, nil
}

// forwardPortSuggestions collects the ports worth offering for a container,
// from both places that know any: the manager (the service's endpoint ports,
// which works against any agent) and the container's own agent (its EXPOSE
// ports, which only agents that fill ContainerInfo.ports report). The two are
// asked concurrently and either may fail — what one returns is still used.
func forwardPortSuggestions(ctx context.Context, dcli *client.Client, cfg config.Config, c resolve.Candidate) []uint32 {
	var (
		wg              sync.WaitGroup
		svcPorts, agent []uint32
	)
	if dcli != nil && c.Service != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res, err := dcli.ServiceInspect(ctx, c.Service, client.ServiceInspectOptions{}); err == nil {
				svcPorts = serviceTargetPorts(res.Service)
			}
		}()
	}
	if c.DialHost != "" && c.ContainerID != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			agent = agentContainerPorts(ctx, cfg, c)
		}()
	}
	wg.Wait()
	return mergePorts(svcPorts, agent)
}

// agentContainerPorts asks the container's own agent which TCP ports the
// container exposes. Nil on any failure, and from an agent that predates the
// field: suggestions are a convenience, never a precondition.
func agentContainerPorts(ctx context.Context, cfg config.Config, c resolve.Candidate) []uint32 {
	conn, err := dial.Dial(ctx, c.DialHost, cfg.Port, cfg)
	if err != nil {
		return nil
	}
	defer conn.Close()
	resp, err := pb.NewAgentClient(conn).ListContainers(ctx, &pb.ListRequest{ServiceFilter: c.Service})
	if err != nil {
		return nil
	}
	return containerPortsFor(resp.GetContainers(), c.ContainerID)
}

// containerPortsFor picks one container's ports out of a ListContainers reply.
// Ids are compared by prefix both ways, since either side may hold a short id.
func containerPortsFor(list []*pb.ContainerInfo, containerID string) []uint32 {
	if containerID == "" {
		return nil
	}
	for _, ci := range list {
		id := ci.GetId()
		if id != "" && (strings.HasPrefix(id, containerID) || strings.HasPrefix(containerID, id)) {
			return mergePorts(ci.GetPorts())
		}
	}
	return nil
}

// Port-forward dialog geometry. The width is fixed so the hint, warning and
// error lines wrap at a known column instead of running past the border. The
// height is not: the dialog is as tall as what it currently says (see
// fwdDialog.height), growing when a warning or an error appears and shrinking
// when it clears.
const (
	fwdFormWidth = 60
	// fwdLabelGap is the space between the label column and the fields.
	fwdLabelGap = 2
	// fwdLabelWidth is the label column including that gap. The service row
	// uses it too, so its value lines up with the fields.
	fwdLabelWidth = len("Container Port") + fwdLabelGap
	// fwdPadX is the blank columns each row keeps inside the border, per side.
	fwdPadX = 2
	// fwdFixedRows is every row but the status lines: border (2), top gap,
	// service row, its gap, three fields with a gap between each, the gap
	// before the status, the gap before the buttons, the buttons.
	fwdFixedRows = 2 + 1 + 1 + 1 + 5 + 1 + 1 + 1
)

// fwdField is an InputField that keeps Enter for itself. Inside a tview.Form,
// Enter on a field moves to the next one; here Enter starts the forward from
// whichever field it is pressed in, as it did before the form had buttons.
//
// The form installs its navigation handler through SetFinishedFunc each time
// it takes focus, so wrapping that method is the one place the override can
// live without fighting the form: finish sees every key first and returns true
// for the ones it handled, everything else reaches the form's own handler.
type fwdField struct {
	*tview.InputField
	finish func(tcell.Key) bool
	// marker, when it reports true, draws a ▾ in the field's last cell: there
	// is a list behind ↓. Needs the label width, which only the form knows.
	marker     func() bool
	labelWidth int
}

func newFwdField(label string, width int) *fwdField {
	return &fwdField{InputField: tview.NewInputField().SetLabel(label).SetFieldWidth(width)}
}

func (f *fwdField) SetFinishedFunc(handler func(tcell.Key)) tview.FormItem {
	f.InputField.SetFinishedFunc(func(key tcell.Key) {
		if f.finish != nil && f.finish(key) {
			return
		}
		if handler != nil {
			handler(key)
		}
	})
	return f
}

// SetFormAttributes widens the form's label column by one: tview.Form leaves a
// single space after the longest label, which crowds the fields.
func (f *fwdField) SetFormAttributes(labelWidth int, labelColor, bgColor, fieldTextColor, fieldBgColor tcell.Color) tview.FormItem {
	f.labelWidth = labelWidth + fwdLabelGap - 1
	f.InputField.SetFormAttributes(f.labelWidth, labelColor, bgColor, fieldTextColor, fieldBgColor)
	return f
}

func (f *fwdField) Draw(screen tcell.Screen) {
	f.InputField.Draw(screen)
	if f.marker == nil || !f.marker() {
		return
	}
	x, y, w, _ := f.GetInnerRect()
	if mx := x + f.labelWidth + f.GetFieldWidth() - 1; mx < x+w {
		screen.SetContent(mx, y, '▾', nil, tcell.StyleDefault.Background(palette.panel).Foreground(palette.textDim))
	}
}

// fwdDialog is the port-forward dialog, k9s's PortForward in this app's theme:
//
//	╭──────────────────── Port Forward ────────────────────╮
//	│                                                      │
//	│  Service         api · 3f2a91c41b2e · node n1        │
//	│                                                      │
//	│  Container Port  80         ▾                        │
//	│                                                      │
//	│  Local Port      80                                  │
//	│                                                      │
//	│  Address         127.0.0.1                           │
//	│                                                      │
//	│  exposed 80 · 8080  ↓ to pick                        │
//	│  ⚠ reachable from your network                       │
//	│                                                      │
//	│                 Forward    Cancel                    │
//	╰──────────────────────────────────────────────────────╯
//
// Two tview.Forms, so the fields and the buttons get the look every other form
// in the app has (label colour, field and button backgrounds, focus colours all
// come from the theme), with the status lines between them. Tab and Shift-Tab
// cross from one form to the other; see openForwardForm.
type fwdDialog struct {
	*tview.Flex
	header             *tview.TextView
	fields             *tview.Form
	cport, lport, addr *fwdField
	status             *tview.TextView
	buttons            *tview.Form
	top, afterHeader   *tview.Box
	afterFields        *tview.Box
	beforeButtons      *tview.Box
}

// statusLines is how many rows the status text takes at the given inner width.
func (d *fwdDialog) statusLines(width int) int {
	width -= 2 * fwdPadX
	n := 0
	for _, line := range strings.Split(d.status.GetText(false), "\n") {
		n += max(1, len(tview.WordWrap(line, width)))
	}
	return n
}

// height is the dialog's full height for what it currently shows.
func (d *fwdDialog) height() int {
	return fwdFixedRows + d.statusLines(fwdFormWidth-2)
}

// Draw lays the rows out for the space centered() actually gave. On a terminal
// too short for the full dialog the blank rows give way — the top gap first,
// then the gaps between the fields, the one under the service row, the one
// above the status, and the one above the buttons last — so the fields, the
// messages and the buttons stay on screen and the footer stays free. Only when
// every gap is gone does the status area shrink.
func (d *fwdDialog) Draw(screen tcell.Screen) {
	_, _, w, h := d.GetInnerRect()
	deficit := fwdFixedRows - 2 + d.statusLines(w) - h
	give := func() int {
		if deficit > 0 {
			deficit--
			return 0
		}
		return 1
	}
	d.ResizeItem(d.top, give(), 0)
	pad := 1 // both gaps between the fields go together: it is one setting
	if deficit > 0 {
		pad, deficit = 0, deficit-2
	}
	d.fields.SetItemPadding(pad)
	d.ResizeItem(d.fields, 3+2*pad, 0)
	d.ResizeItem(d.afterHeader, give(), 0)
	d.ResizeItem(d.afterFields, give(), 0)
	d.ResizeItem(d.beforeButtons, give(), 0)
	d.Flex.Draw(screen)
}

func newFwdDialog(c resolve.Candidate) *fwdDialog {
	d := &fwdDialog{
		top: tview.NewBox(), afterHeader: tview.NewBox(),
		afterFields: tview.NewBox(), beforeButtons: tview.NewBox(),
	}

	d.header = tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	d.header.SetBorderPadding(0, 0, fwdPadX, fwdPadX)
	d.header.SetText(fmt.Sprintf("[%s]%-*s[-]%s", palette.textDim.CSS(), fwdLabelWidth, "Service",
		tview.Escape(fmt.Sprintf("%s · %s · node %s", orDash(c.Service), orDash(shortID(c.ContainerID)), orDash(c.NodeName)))))

	// k9s's order: what to reach, where to listen, on which address.
	d.cport = newFwdField("Container Port", 12)
	d.cport.SetAcceptanceFunc(tview.InputFieldInteger)
	d.lport = newFwdField("Local Port", 12)
	d.lport.SetAcceptanceFunc(tview.InputFieldInteger).
		SetPlaceholder("= container").SetPlaceholderTextColor(palette.textFade)
	d.addr = newFwdField("Address", 28)
	d.addr.SetText(defaultForwardAddress)

	// Item padding 1 is tview.Form's own default: a blank row between fields.
	// Draw takes it back to 0 on a short terminal.
	d.fields = tview.NewForm().SetItemPadding(1).
		AddFormItem(d.cport).AddFormItem(d.lport).AddFormItem(d.addr)
	d.fields.SetBorderPadding(0, 0, fwdPadX, fwdPadX)

	d.status = tview.NewTextView().SetDynamicColors(true).SetWrap(true).SetWordWrap(true)
	d.status.SetBorderPadding(0, 0, fwdPadX, fwdPadX)

	d.buttons = tview.NewForm().AddButton("Forward", nil).AddButton("Cancel", nil).
		SetButtonsAlign(tview.AlignCenter)
	d.buttons.SetBorderPadding(0, 0, fwdPadX, fwdPadX) // a Form pads by one all round; the row is one line

	d.Flex = tview.NewFlex().SetDirection(tview.FlexRow)
	// The status takes whatever rows are left: as many as its text needs when
	// the dialog has its full height, fewer only on a very short terminal.
	d.AddItem(d.top, 1, 0, false).
		AddItem(d.header, 1, 0, false).
		AddItem(d.afterHeader, 1, 0, false).
		AddItem(d.fields, 5, 0, true).
		AddItem(d.afterFields, 1, 0, false).
		AddItem(d.status, 0, 1, false).
		AddItem(d.beforeButtons, 1, 0, false).
		AddItem(d.buttons, 1, 0, false)
	// No padding on the Flex itself: a Flex does not clear its background, so
	// padding cells would show the page beneath. Each item pads itself.
	d.SetBorder(true).SetTitle(" Port Forward ")
	return d
}

// portPrompt opens the port-forward dialog and starts the forward it is
// filled in with.
func (u *ui) portPrompt(c resolve.Candidate) {
	u.openForwardForm(c, func(spec forwardSpec) {
		u.startForward(c, spec.address, spec.local, spec.remote)
		u.flash(fmt.Sprintf(" [green]forwarding[white] %s → %s:%d",
			net.JoinHostPort(spec.address, strconv.FormatUint(uint64(spec.local), 10)), shortID(c.ContainerID), spec.remote))
	})
}

// openForwardForm runs the dialog: container port, local port, address — the
// same three things `swarmexec port-forward --address A svc L:R` takes — and
// hands a valid, conflict-free spec to start once the form has closed.
//
// The container port is suggested from the container's real ports, which the
// form looks up in the background while it is already open: the service's
// endpoint ports from the manager and the EXPOSE ports from the agent on the
// container's node. Neither is complete on its own — a service that publishes
// nothing has no endpoint ports, and an agent that predates the field reports
// no EXPOSE ports — so both are asked and merged. Any port can still be typed.
func (u *ui) openForwardForm(c resolve.Candidate, start func(forwardSpec)) {
	app, ctree, cfg, dcli, f := u.app, u.ctree, u.cfg, u.dcli, u.f
	d := newFwdDialog(c)
	cport, lport, addr, status := d.cport, d.lport, d.addr, d.status

	var (
		suggested []uint32
		loading   = true
		errMsg    string
		follow    portFollow
		// relayout fits the dialog's height to what render just wrote; nil
		// until the dialog is on screen.
		relayout func()
		showAll  bool
	)
	render := func() {
		var b strings.Builder
		dim := palette.textDim.CSS()
		switch {
		case loading:
			fmt.Fprintf(&b, "[%s]looking up the container's ports…[-]", dim)
		case len(suggested) == 0:
			fmt.Fprintf(&b, "[%s]no ports found — type the port to forward[-]", dim)
		default:
			fmt.Fprintf(&b, "[%s]exposed[-] %s  [%s]↓ to pick[-]", dim, formatPorts(suggested), dim)
		}
		if !isLoopbackAddr(addr.GetText()) {
			b.WriteString("\n[yellow]⚠ reachable from your network[-]")
		}
		if errMsg != "" {
			fmt.Fprintf(&b, "\n[red]%s[-]", tview.Escape(errMsg))
		}
		status.SetText(b.String())
		if relayout != nil {
			relayout()
		}
	}

	cport.marker = func() bool { return len(suggested) > 0 }
	cport.SetAutocompleteFunc(func(text string) []string {
		all := showAll
		showAll = false
		return portCompletions(suggested, text, all)
	})
	cport.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyDown {
			showAll = true // ↓ is "show me the list", whatever is typed
		}
		return ev
	})
	cport.SetChangedFunc(func(text string) {
		if local, ok := follow.setContainer(text); ok && lport.GetText() != local {
			lport.SetText(local)
		}
		errMsg = ""
		render()
	})
	lport.SetChangedFunc(func(text string) {
		follow.setLocal(text)
		errMsg = ""
		render()
	})
	addr.SetChangedFunc(func(string) {
		errMsg = ""
		render()
	})

	ov := u.overlayFor(pageFwdPrompt, ctree,
		footerKeys("Enter", "forward", "Tab", "next", "↓", "ports", "Esc", "cancel"))
	closed := false
	closeForm := func() {
		closed = true
		ov.Close()
	}

	// focusIn moves focus to one element of one of the two forms. It goes
	// through the form, not straight to the element, because the form wires
	// its elements' navigation handlers when it takes focus.
	focusIn := func(form *tview.Form, index int) {
		form.SetFocus(index)
		app.SetFocus(form)
	}

	submit := func() {
		spec, err := parseForwardForm(cport.GetText(), addr.GetText(), lport.GetText())
		if err != nil {
			// Keep the form open so the operator corrects the field instead
			// of retyping all three.
			errMsg = err.Error()
			render()
			return
		}
		// An occupied local port is a correctable mistake too, and the
		// sentence names WHICH forward holds it — the part the operator
		// cannot see from here. It wraps inside the dialog, so the cluster
		// name at its end is never clipped away.
		if msg, clash := u.portConflict(spec.local); clash {
			errMsg = msg
			render()
			focusIn(d.fields, 1)
			return
		}
		closeForm()
		start(spec)
	}

	// Enter submits from any field; Tab and Shift-Tab run through the fields
	// and on into the buttons, and back round.
	fields := []*fwdField{cport, lport, addr}
	for i, fld := range fields {
		fld.finish = func(key tcell.Key) bool {
			switch {
			case key == tcell.KeyEnter:
				submit()
			case key == tcell.KeyTab && i == len(fields)-1:
				focusIn(d.buttons, 0)
			case key == tcell.KeyBacktab && i == 0:
				focusIn(d.buttons, d.buttons.GetButtonCount()-1)
			default:
				return false
			}
			return true
		}
	}
	d.buttons.GetButton(0).SetSelectedFunc(submit)
	d.buttons.GetButton(1).SetSelectedFunc(closeForm)
	d.buttons.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		_, btn := d.buttons.GetFocusedItemIndex()
		last := d.buttons.GetButtonCount() - 1
		// All of the navigation is done here rather than in the buttons' exit
		// handlers: those are only wired once the form itself takes focus, and
		// a button clicked with the mouse is focused without that.
		switch ev.Key() {
		case tcell.KeyTab:
			if btn == last {
				focusIn(d.fields, 0)
			} else {
				focusIn(d.buttons, btn+1)
			}
		case tcell.KeyBacktab:
			if btn == 0 {
				focusIn(d.fields, len(fields)-1)
			} else {
				focusIn(d.buttons, btn-1)
			}
		case tcell.KeyLeft, tcell.KeyRight:
			// The buttons sit side by side; the arrows move between them.
			focusIn(d.buttons, (btn+1)%(last+1))
		case tcell.KeyEscape:
			closeForm()
		default:
			return ev
		}
		return nil
	})
	d.fields.SetCancelFunc(closeForm)
	d.buttons.SetCancelFunc(closeForm)

	render()
	shownH := d.height()
	ov.show(centered(d, fwdFormWidth, shownH), d.fields)
	// The dialog is as tall as its content: a warning or an error grows it,
	// and it shrinks again once the message clears. Re-showing replaces the
	// page of the same name; focus stays where the operator has it (AddPage
	// would otherwise hand it to the fields form).
	relayout = func() {
		if h := d.height(); h != shownH && !closed && u.pages.HasPage(pageFwdPrompt) {
			shownH = h
			ov.show(centered(d, fwdFormWidth, h), app.GetFocus())
		}
	}

	async(u, "ui.forward.suggest", func(ctx context.Context) ([]uint32, error) {
		// One budget for both lookups: a slow node must not keep the form
		// saying "looking up" for longer than a connect would take.
		tctx, cancel := context.WithTimeout(ctx, f.connectTimeout)
		defer cancel()
		return forwardPortSuggestions(tctx, dcli, cfg, c), nil
	}, func(ports []uint32, _ error) {
		suggested, loading = ports, false
		// Prefill only an untouched field: the operator may have typed
		// a port while the lookup was running.
		if cport.GetText() == "" && len(ports) > 0 {
			cport.SetText(strconv.FormatUint(uint64(ports[0]), 10))
		}
		render()
	})
}
