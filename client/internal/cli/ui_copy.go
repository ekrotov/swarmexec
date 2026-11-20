// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/resolve"
	"swarmexec/internal/dockerfmt"
)

// The TUI half of `swarmexec cp`: two entries in the container menu, each a
// two-field form. The copy itself is the CLI's own core (copyOut / copyIn in
// cp.go), called with the container the operator is looking at — so the tar
// handling, the destination rules and the refusals are the same code, and
// there is no second implementation to keep in step.
//
// Container-to-container is deliberately absent here. It needs the operator to
// pick a SECOND container, and a two-container selection exists nowhere else
// in this UI; inventing one in a copy dialog would be the wrong place to try
// it. The command line does it today: `swarmexec cp web.1:/f db.2:/tmp/`.

// tuiCopyMaxBytes caps a copy started from the TUI, as --max-size does on the
// command line. The same 1 GiB: the point of the cap is to stop a mistyped
// path ("/"), not to argue with a deliberate copy, and the agent enforces its
// own anyway.
const tuiCopyMaxBytes = 1 << 30

// copyDirection is which way a copy from the container menu goes.
type copyDirection int

const (
	copyFromContainer copyDirection = iota
	copyIntoContainer
)

// copyFilePrompt opens the copy form for c and runs the copy it is filled in
// with. Both fields are plain paths: the container is already chosen, so there
// is nothing to resolve and nothing to type twice.
func (u *ui) copyFilePrompt(c resolve.Candidate, ep resolve.Endpoint, dir copyDirection) {
	ctree := u.ctree
	var remote, local string

	form := tview.NewForm()
	title := fmt.Sprintf(" copy out of %s on %s ", dockerfmt.OrDash(c.Service), dockerfmt.OrDash(c.NodeName))
	if dir == copyIntoContainer {
		title = fmt.Sprintf(" copy into %s on %s ", dockerfmt.OrDash(c.Service), dockerfmt.OrDash(c.NodeName))
	}
	form.SetBorder(true).SetTitle(title)

	if dir == copyFromContainer {
		form.AddInputField("Path in container", "", 44, nil, func(t string) { remote = t })
		form.AddInputField("Local destination", ".", 44, nil, func(t string) { local = t })
		local = "."
	} else {
		form.AddInputField("Local file or directory", "", 44, nil, func(t string) { local = t })
		form.AddInputField("Destination in container", "/tmp/", 44, nil, func(t string) { remote = t })
		remote = "/tmp/"
	}

	ov := u.overlayFor(pageCopyForm, ctree, footerKeys("Tab", "next field", "Enter", "confirm", "Esc", "cancel"))
	closeForm := ov.Close
	form.AddButton("Copy", func() {
		r, l := strings.TrimSpace(remote), strings.TrimSpace(local)
		if r == "" || l == "" {
			u.info("both paths are needed: what to copy, and where it goes")
			return
		}
		closeForm()
		u.app.SetFocus(ctree)
		u.runCopy(c, ep, dir, r, l)
	})
	form.AddButton("Cancel", closeForm)
	form.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape {
			closeForm()
			return nil
		}
		return ev
	})
	ov.show(centered(form, 72, 11), form)
}

// runCopy performs the copy in the background and reports it. It goes through
// mutate, like every other write from this UI: the copy runs on the session
// context, so switching cluster or closing the menu does not cut it, and the
// result lands back on the UI goroutine.
//
// There is no live byte counter and no cancel key. A copy is bounded by
// tuiCopyMaxBytes and by the agent's own cap, and a progress line that cannot
// be cancelled would only be decoration; the summary says what arrived.
func (u *ui) runCopy(c resolve.Candidate, ep resolve.Endpoint, dir copyDirection, remote, local string) {
	ac, timeout := u.agents, u.f.connectTimeout
	what := remote
	if dir == copyIntoContainer {
		what = local
	}
	u.flash(" copying " + what + "…")

	mutate(u, "ui.containers.copy", "file copy", func(ctx context.Context) (copyResult, error) {
		cr := &cpRun{
			ac: ac,
			// quiet: the progress callback writes carriage returns, which have
			// no meaning inside a tview screen.
			f:        &cpFlags{connectTimeout: timeout, quiet: true},
			maxBytes: tuiCopyMaxBytes,
			out:      io.Discard,
		}
		if dir == copyFromContainer {
			return cr.copyOut(ctx, ep, remote, local)
		}
		return cr.copyIn(ctx, ep, local, remote)
	}, func(res copyResult, err error) {
		if err != nil {
			u.info(copyFailure(c, dir, remote, local, err))
			return
		}
		u.flash(fmt.Sprintf(" [green]copied[white] %s — %s in %d file(s)",
			res.Name, dockerfmt.Bytes(res.Bytes), res.Files))
		if len(res.Refused) > 0 {
			// Not a flash: a refused entry is something the operator has to
			// see, and the copy is otherwise reported as a success.
			u.info("Copied, but some entries were refused:\n\n" + strings.Join(res.Refused, "\n"))
		}
	})
}

// copyFailure is the notice for a copy that did not happen, naming both ends.
func copyFailure(c resolve.Candidate, dir copyDirection, remote, local string, err error) string {
	from, to := remote, local
	if dir == copyIntoContainer {
		from, to = local, remote
	}
	return fmt.Sprintf("Copying %s to %s failed on %s:\n\n%v",
		from, to, dockerfmt.OrDash(c.NodeName), err)
}
