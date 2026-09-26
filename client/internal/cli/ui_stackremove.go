// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"github.com/rivo/tview"
)

// openRemoveStack removes a whole stack the way `docker stack rm` does, after a
// confirm that names exactly what will go.
//
// The listing runs before the confirm rather than after it, because "remove
// this stack" is not a question an operator can answer without knowing what is
// in it: four services and a network is a different decision from four services
// and three secrets they did not know the stack owned.
func (u *ui) openRemoveStack(stack string, back tview.Primitive, onRemoved func()) {
	// The unstacked bucket is a display grouping, not a stack: nothing carries
	// that label, so a removal would either match nothing or — worse, if anyone
	// ever named a stack that — match the wrong thing.
	if stack == noStackLabel {
		u.flash(" [gray]these services carry no stack label — remove them individually[white]")
		return
	}

	app, dcli, ctx := u.app, u.dcli, u.ctx
	go func() {
		contents, err := stackContentsOf(ctx, dcli, stack)
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("could not read stack " + stack + ": " + err.Error())
				return
			}
			if contents.empty() {
				// Docker says "Nothing found in stack" here, and so do we: the
				// stack row exists because services are grouped under it, so an
				// empty answer means the label is gone, not that nothing ran.
				u.info(fmt.Sprintf("nothing labelled for stack %q — nothing to remove", stack))
				return
			}
			msg := fmt.Sprintf(
				"Remove stack %q?\n\nThis deletes everything `docker stack rm` would:\n\n    %s\n\nAll its tasks stop. It cannot be undone.",
				stack, contents.counts())
			u.confirm(msg, "Remove", back, func() {
				u.info(fmt.Sprintf("removing stack %q (%s)…", stack, contents.counts()))
				go func() {
					errs := removeStack(ctx, dcli, contents)
					app.QueueUpdateDraw(func() {
						if onRemoved != nil {
							onRemoved()
						}
						if len(errs) == 0 {
							u.info(fmt.Sprintf("removed stack %q (%s)", stack, contents.counts()))
							return
						}
						// Say what did NOT go, not just that something failed:
						// the rest is already gone, and the operator has to know
						// precisely what is left behind.
						var b strings.Builder
						fmt.Fprintf(&b, "stack %q removed, except:\n\n", stack)
						for _, e := range errs {
							fmt.Fprintf(&b, "  • %s\n", e.Error())
						}
						u.info(b.String())
					})
				}()
			})
		})
	}()
}
