// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/secscan"
)

// Writing the report from the ui is the same report the cli writes, gathered
// the same way. It is NOT a dump of what the risks overlay is showing: the
// overlay covers services, and the file covers the cluster. Reusing the screen
// would produce a file that claims a cluster-wide scope it never had.

// writeSecurityReportPrompt asks where to put the report, then writes it off
// the ui goroutine. keys is the overlay's own footer text, so the progress
// message can hand the footer back when it is done — a transient message that
// keeps the footer is how the shortcuts disappear.
func (u *ui) writeSecurityReportPrompt(back tview.Primitive, setHelp func(string), keys string) {
	in := tview.NewInputField().SetLabel("file: ").SetFieldWidth(52).
		SetText(defaultReportPath(u.contextName()))
	ov := u.overlayFor(pageSecurityReport, back, "")
	in.SetDoneFunc(func(key tcell.Key) {
		ov.Close()
		if key == tcell.KeyEscape {
			return
		}
		path := strings.TrimSpace(in.GetText())
		if path == "" {
			return
		}
		u.writeSecurityReport(path, setHelp, keys)
	})
	in.SetBorder(true).SetTitle(" write security report ")
	ov.show(centeredPrompt(in, 72), in)
}

// writeSecurityReport gathers and writes in the background.
//
// Everything here talks to the manager and to every node's agent, which on a
// cluster with a slow or unreachable node takes seconds. Doing that on the ui
// goroutine would freeze the whole application mid-keystroke, so the work runs
// on its own goroutine and only the result comes back through QueueUpdateDraw —
// which must never be called from the ui goroutine itself.
func (u *ui) writeSecurityReport(path string, setHelp func(string), keys string) {
	setHelp(" [yellow]writing security report…[white] " + tview.Escape(path))
	ctx, app := u.ctx, u.app
	go func() {
		report, err := gatherSecurityReport(ctx, u.dcli, u.cfg, u.g,
			&securityFlags{output: path, connectTimeout: u.f.connectTimeout}, u.contextName())
		if err == nil {
			err = writeReportFile(path, report.Markdown())
		}
		app.QueueUpdateDraw(func() {
			setHelp(keys) // the progress message was a borrower, not the owner
			if err != nil {
				u.info("security report failed: " + err.Error())
				return
			}
			// The count is the point of the message: a report that found
			// nothing and a report that found forty things are both "written",
			// and only one of them wants opening now.
			u.info("wrote " + path + " — " + reportSummaryLine(report))
		})
	}()
}

// defaultReportPath is a name that will not collide with the last one and that
// says which cluster it describes — two reports from two clusters land in the
// same directory often enough that an unqualified name is a trap.
func defaultReportPath(context string) string {
	stamp := time.Now().Format("20060102-150405")
	if context == "" {
		return "swarmexec-security-" + stamp + ".md"
	}
	return "swarmexec-security-" + safeFileName(context) + "-" + stamp + ".md"
}

// safeFileName reduces a context name to something that cannot escape the
// directory or confuse a shell: a docker context name is user-chosen and may
// contain a slash.
func safeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// contextName is the cluster the ui is actually pointed at — the switched-to
// context, not the --context flag, which stops being the answer the moment
// someone switches in the Contexts tab. Used for the export filename and for
// the report's own header, which exists to say which cluster it describes.
func (u *ui) contextName() string { return u.ctxOverride }

// reportSummaryLine is the one-line verdict used where a full report will not
// fit. Kept next to the report so the wording cannot drift from it.
func reportSummaryLine(r secscan.Report) string {
	act := r.Totals.High + r.Totals.Medium
	if act == 0 {
		return fmt.Sprintf("%d finding(s), none above low", r.Totals.Total())
	}
	return fmt.Sprintf("%d finding(s), %d above low", r.Totals.Total(), act)
}
