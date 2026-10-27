// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// askpassPrompt is the TUI's promptFunc: ssh's question as a dialog. It runs
// on the askpass server's goroutine and waits for the operator; the dialog
// itself is built on the UI goroutine.
func (u *ui) askpassPrompt(req askpassRequest) (string, bool) {
	ch := make(chan askpassResponse, 1)
	reply := func(answer string, ok bool) {
		select {
		case ch <- askpassResponse{OK: ok, Answer: answer}:
		default:
		}
	}
	// In its own goroutine: QueueUpdateDraw blocks until the event loop takes
	// it, and an event loop that has just stopped never will.
	go u.app.QueueUpdateDraw(func() { u.openAskpass(req, reply) })
	select {
	case r := <-ch:
		return r.Answer, r.OK
	case <-u.runCtx.Done():
		return "", false
	}
}

// openAskpass shows one ssh question. UI goroutine.
func (u *ui) openAskpass(req askpassRequest, reply func(string, bool)) {
	app := u.app
	back := app.GetFocus()
	prompt := strings.TrimSpace(req.Prompt)
	switch req.Kind {
	case "none":
		// A notice ("confirm user presence for key …"): nothing to answer.
		u.flash(" [yellow]ssh:[white] " + tview.Escape(prompt))
		reply("", true)
		return
	case "confirm":
		m := tview.NewModal().SetText("ssh asks:\n\n" + prompt).AddButtons([]string{"Yes", "No"})
		ov := u.overlayFor(pageAskpass, back, "")
		m.SetDoneFunc(func(_ int, label string) {
			ov.Close()
			reply("", label == "Yes")
		})
		ov.show(newScrim(m), m)
		return
	}

	text := tview.NewTextView().SetText(prompt).SetWordWrap(true)
	in := tview.NewInputField()
	if !askpassEchoes(prompt) {
		in.SetMaskCharacter('•')
	}
	box := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(text, 0, 1, false).
		AddItem(in, 1, 0, true)
	box.SetBorder(true).SetTitle(" SSH sign-in ")
	ov := u.overlayFor(pageAskpass, back, footerKeys("Enter", "send", "Esc", "cancel"))
	in.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			ov.Close()
			reply(in.GetText(), true)
		case tcell.KeyEscape:
			ov.Close()
			reply("", false)
		}
	})
	ov.show(centered(box, 76, 7), in)
}

// offerSSHAdd handles a failed ssh sign-in where ssh cannot ask through
// swarmexec (OpenSSH before 8.4, Windows, switched off): it offers to run
// ssh-add right here, with the terminal handed back for the passphrase, and
// to retry afterwards. Once per cluster until ssh-add succeeds — the tree
// refreshes every ten seconds and must not bring the offer back each time.
// Reports whether it took the error.
func (u *ui) offerSSHAdd(c *clusterState, err error, retry func()) bool {
	if c == nil || c.sshAddOffered || askpassActive() || !isSSHAuthError(err) {
		return false
	}
	c.sshAddOffered = true
	// Off the UI goroutine: resolving the endpoint reads the Docker context
	// and `ssh -G` runs once per hop.
	go func() {
		ep := resolveEndpoint(c.ctxOverride)
		if !strings.HasPrefix(ep.Host, "ssh://") {
			return
		}
		argv := sshAddCommand(ep.Host, ep.ProxyJump)
		u.app.QueueUpdateDraw(func() { u.showSSHAddOffer(c, argv, retry) })
	}()
	return true
}

func (u *ui) showSSHAddOffer(c *clusterState, argv []string, retry func()) {
	why := askpassUnavailable()
	if why == "" {
		why = "swarmexec could not set up asking"
	}
	if !sshAgentRunning() {
		u.info(fmt.Sprintf("SSH sign-in to %q failed.\n\nIf your key has a passphrase: ssh cannot ask for it here (%s), "+
			"and no ssh-agent is running to hold it. Start one (eval \"$(ssh-agent)\"), load the key with ssh-add, "+
			"and start swarmexec again from that shell.", c.name, why))
		return
	}
	back := u.app.GetFocus()
	cmdline := strings.Join(argv, " ")
	m := tview.NewModal().
		SetText(fmt.Sprintf("SSH sign-in to %q failed.\n\nIf your key has a passphrase, ssh cannot ask for it here (%s). "+
			"Load it into your ssh-agent now?\n\n%s", c.name, why, cmdline)).
		AddButtons([]string{"Run ssh-add", "Not now"})
	ov := u.overlayFor(pageSSHAdd, back, "")
	m.SetDoneFunc(func(_ int, label string) {
		ov.Close()
		if label != "Run ssh-add" {
			return
		}
		ok := u.runSSHAdd(c.name, argv)
		if ok {
			c.sshAddOffered = false // a later failure is a new one
		}
		retry()
	})
	ov.show(newScrim(m), m)
}

// runSSHAdd hands the terminal to ssh-add, which asks for the passphrase the
// way it always does. UI goroutine (tview's Suspend is made for event
// handlers).
func (u *ui) runSSHAdd(name string, argv []string) bool {
	ok := false
	u.app.Suspend(func() {
		fmt.Printf("\nswarmexec: loading the SSH key for %q into your ssh-agent\n$ %s\n\n", name, strings.Join(argv, " "))
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Printf("\nssh-add failed: %v\nPress Enter to return to swarmexec. ", err)
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
			return
		}
		ok = true
	})
	return ok
}
