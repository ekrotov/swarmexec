// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// ssh's questions in the TUI. Nothing here starts ssh: the dialogs are fed
// requests as the askpass server would, and the ssh-add offer is answered
// "Not now".

type askAnswer struct {
	answer string
	ok     bool
}

// ask runs askpassPrompt off the UI goroutine, as the askpass server does,
// and hands back its answer.
func ask(u *ui, req askpassRequest) <-chan askAnswer {
	ch := make(chan askAnswer, 1)
	go func() {
		a, ok := u.askpassPrompt(req)
		ch <- askAnswer{a, ok}
	}()
	return ch
}

func answerOf(t *testing.T, ch <-chan askAnswer) askAnswer {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(10 * time.Second):
		t.Fatal("askpassPrompt never returned")
		return askAnswer{}
	}
}

func askpassUI(t *testing.T) (*ui, context.CancelFunc) {
	t.Helper()
	u := covChromeUI(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	onLoop(t, u, func() { u.runCtx = ctx })
	return u, cancel
}

func TestAskpassPassphraseDialog(t *testing.T) {
	u, _ := askpassUI(t)

	ch := ask(u, askpassRequest{Prompt: "Enter passphrase for key '/home/op/.ssh/id_ed25519': "})
	waitPageText(t, u, pageAskpass, "SSH sign-in", "Enter passphrase for key")
	typeText(t, u, "s3cret")
	if text := pageText(t, u, pageAskpass); !strings.Contains(text, "••••••") || strings.Contains(text, "s3cret") {
		t.Errorf("the passphrase is not masked:\n%s", text)
	}
	sendSpecial(t, u, tcell.KeyEnter)
	if a := answerOf(t, ch); !a.ok || a.answer != "s3cret" {
		t.Errorf("answer %+v", a)
	}
	waitPageGone(t, u, pageAskpass)

	// A host-key question echoes; Esc declines.
	ch = ask(u, askpassRequest{Prompt: "Are you sure you want to continue connecting (yes/no/[fingerprint])?"})
	waitPageText(t, u, pageAskpass, "continue connecting")
	typeText(t, u, "yes")
	waitPageText(t, u, pageAskpass, "yes")
	sendSpecial(t, u, tcell.KeyEscape)
	if a := answerOf(t, ch); a.ok {
		t.Errorf("Esc answered %+v", a)
	}
}

func TestAskpassConfirmAndNotice(t *testing.T) {
	u, cancel := askpassUI(t)

	ch := ask(u, askpassRequest{Prompt: "Allow use of key id_ed25519?", Kind: "confirm"})
	waitPageText(t, u, pageAskpass, "ssh asks:", "Allow use of key", "Yes", "No")
	sendSpecial(t, u, tcell.KeyEnter)
	if a := answerOf(t, ch); !a.ok {
		t.Errorf("Yes answered %+v", a)
	}

	ch = ask(u, askpassRequest{Prompt: "Allow use of key id_ed25519?", Kind: "confirm"})
	waitPageText(t, u, pageAskpass, "ssh asks:")
	sendSpecial(t, u, tcell.KeyTab)
	sendSpecial(t, u, tcell.KeyEnter)
	if a := answerOf(t, ch); a.ok {
		t.Errorf("No answered %+v", a)
	}

	ch = ask(u, askpassRequest{Prompt: "Confirm user presence for key ED25519-SK", Kind: "none"})
	if a := answerOf(t, ch); !a.ok {
		t.Errorf("a notice answered %+v", a)
	}
	waitFooter(t, u, "ssh: Confirm user presence")

	// The UI ending stops the wait.
	ch = ask(u, askpassRequest{Prompt: "Enter passphrase: "})
	waitPageText(t, u, pageAskpass, "Enter passphrase")
	cancel()
	if a := answerOf(t, ch); a.ok {
		t.Errorf("a closed UI answered %+v", a)
	}
}

func TestOfferSSHAddGuards(t *testing.T) {
	isolatedDockerConfig(t) // the "default" context: a unix socket, no ssh
	// With ssh able to ask through swarmexec there is nothing to offer; an
	// earlier test of this binary may have left the server up.
	prev := askpassSrv.Swap(nil)
	t.Cleanup(func() { askpassSrv.Store(prev) })
	u, _ := askpassUI(t)
	authErr := errors.New("ssh: Permission denied (publickey)")
	var c *clusterState
	onLoop(t, u, func() { c = newClusterState("prod", "prod") })

	var took bool
	onLoop(t, u, func() { took = u.offerSSHAdd(nil, authErr, func() {}) })
	if took {
		t.Error("no cluster, no offer")
	}
	onLoop(t, u, func() { took = u.offerSSHAdd(c, errors.New("connection refused"), func() {}) })
	if took {
		t.Error("not an ssh sign-in failure, no offer")
	}
	onLoop(t, u, func() { took = u.offerSSHAdd(c, authErr, func() {}) })
	if !took || !c.sshAddOffered {
		t.Error("an ssh sign-in failure is taken, once")
	}
	onLoop(t, u, func() { took = u.offerSSHAdd(c, authErr, func() {}) })
	if took {
		t.Error("offered twice")
	}
	// The default context is not ssh: nothing is shown.
	time.Sleep(50 * time.Millisecond)
	if pageOpen(t, u, pageSSHAdd) {
		t.Error("an offer for a non-ssh endpoint")
	}
}

func TestSSHAddOffer(t *testing.T) {
	t.Setenv(askpassEnv, "off") // and so no `ssh -V` either
	u, _ := askpassUI(t)
	var c *clusterState
	onLoop(t, u, func() { c = newClusterState("prod", "prod") })
	argv := []string{"ssh-add", "/home/op/.ssh/id_ed25519"}

	t.Setenv("SSH_AUTH_SOCK", "")
	onLoop(t, u, func() { u.showSSHAddOffer(c, argv, func() {}) })
	waitPageText(t, u, pageInfo, `SSH sign-in to "prod" failed`, "no ssh-agent is running", askpassEnv+" is off")
	closeInfo(t, u)

	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	retried := make(chan struct{}, 1)
	onLoop(t, u, func() { u.showSSHAddOffer(c, argv, func() { retried <- struct{}{} }) })
	waitPageText(t, u, pageSSHAdd, "Load it into your ssh-agent now?", "ssh-add /home/op/.ssh/id_ed25519", "Run ssh-add", "Not now")
	sendSpecial(t, u, tcell.KeyTab) // Not now
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageGone(t, u, pageSSHAdd)
	select {
	case <-retried:
		t.Error("Not now retried")
	default:
	}
}
