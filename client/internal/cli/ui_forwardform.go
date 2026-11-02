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
	return strings.Join(parts, ", ")
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

// Port-forward form geometry: three fields, a spacer and up to four lines of
// hints (suggestions, address warning, and an error that may wrap), inside a
// border.
const (
	fwdFormWidth  = 64
	fwdFormHeight = 11
)

// portPrompt opens the port-forward form: container port, local address, local
// port — the same three things `swarmexec port-forward --address A svc L:R`
// takes.
//
// The container port is suggested from the container's real ports, which the
// form looks up in the background while it is already open: the service's
// endpoint ports from the manager and the EXPOSE ports from the agent on the
// container's node. Neither is complete on its own — a service that publishes
// nothing has no endpoint ports, and an agent that predates the field reports
// no EXPOSE ports — so both are asked and merged. Any port can still be typed.
func (u *ui) portPrompt(c resolve.Candidate) {
	app, ctree, cfg, dcli, ctx, f := u.app, u.ctree, u.cfg, u.dcli, u.ctx, u.f
	gen := u.generation()

	cport := tview.NewInputField().SetLabel(" container port ").SetFieldWidth(8).
		SetAcceptanceFunc(tview.InputFieldInteger)
	addr := tview.NewInputField().SetLabel(" local address  ").SetFieldWidth(28).
		SetText(defaultForwardAddress)
	lport := tview.NewInputField().SetLabel(" local port     ").SetFieldWidth(8).
		SetAcceptanceFunc(tview.InputFieldInteger)
	status := tview.NewTextView().SetDynamicColors(true).SetWrap(true).SetWordWrap(true)
	status.SetBorderPadding(0, 0, 1, 1)

	box := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(cport, 1, 0, true).
		AddItem(addr, 1, 0, false).
		AddItem(lport, 1, 0, false).
		AddItem(tview.NewBox(), 1, 0, false).
		AddItem(status, 0, 1, false)
	// No padding on the Flex itself: a Flex does not clear its background, so
	// padding cells would show the page beneath. Each item pads itself.
	box.SetBorder(true).
		SetTitle(fmt.Sprintf(" forward %s on %s ", orDash(c.Service), orDash(c.NodeName)))

	var (
		suggested []uint32
		loading   = true
		errMsg    string
		follow    portFollow
		showAll   bool
	)
	render := func() {
		var b strings.Builder
		switch {
		case loading:
			b.WriteString("[gray]looking up the container's ports…[-]")
		case len(suggested) == 0:
			b.WriteString("[gray]no ports advertised — type the port to forward[-]")
		default:
			fmt.Fprintf(&b, "[gray]suggested:[-] %s  [gray](↓ to pick)[-]", formatPorts(suggested))
		}
		if a := strings.TrimSpace(addr.GetText()); !isLoopbackAddr(a) {
			fmt.Fprintf(&b, "\n[yellow]⚠ %s is not loopback: reachable from your network[-]", tview.Escape(a))
		}
		if errMsg != "" {
			fmt.Fprintf(&b, "\n[red]%s[-]", tview.Escape(errMsg))
		}
		status.SetText(b.String())
	}

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
		footerKeys("Enter", "forward", "Tab", "next field", "↓", "suggested ports", "Esc", "cancel"))
	closeForm := ov.Close

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
		// cannot see from here. It wraps inside the form, so the cluster
		// name at its end is never clipped away.
		if msg, clash := u.portConflict(spec.local); clash {
			errMsg = msg
			render()
			app.SetFocus(lport)
			return
		}
		closeForm()
		u.startForward(c, spec.address, spec.local, spec.remote)
		u.flash(fmt.Sprintf(" [green]forwarding[white] %s → %s:%d",
			net.JoinHostPort(spec.address, strconv.FormatUint(uint64(spec.local), 10)), shortID(c.ContainerID), spec.remote))
	}
	fields := []*tview.InputField{cport, addr, lport}
	for i, fld := range fields {
		next, prev := fields[(i+1)%len(fields)], fields[(i+len(fields)-1)%len(fields)]
		fld.SetDoneFunc(func(key tcell.Key) {
			switch key {
			case tcell.KeyEnter:
				submit()
			case tcell.KeyTab:
				app.SetFocus(next)
			case tcell.KeyBacktab:
				app.SetFocus(prev)
			case tcell.KeyEscape:
				closeForm()
			}
		})
	}

	render()
	ov.show(centered(box, fwdFormWidth, fwdFormHeight), cport)

	go func() {
		// One budget for both lookups: a slow node must not keep the form
		// saying "looking up" for longer than a connect would take.
		tctx, cancel := context.WithTimeout(ctx, f.connectTimeout)
		ports := forwardPortSuggestions(tctx, dcli, cfg, c)
		cancel()
		u.onCluster(gen, func() {
			suggested, loading = ports, false
			// Prefill only an untouched field: the operator may have typed
			// a port while the lookup was running.
			if cport.GetText() == "" && len(ports) > 0 {
				cport.SetText(strconv.FormatUint(uint64(ports[0]), 10))
			}
			render()
		})
	}()
}
