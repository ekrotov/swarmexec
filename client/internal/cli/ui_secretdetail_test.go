// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/swarmops"
)

// The secret detail overlay (review Q-01 pinned it before showDetail became
// a type): what it shows before and after the usage lookup, and its keys.

var apiKeySecret = swarmops.Secret{
	ID: "sec-api", Name: "api_key", Services: []string{"web"},
	Labels:  map[string]string{"team": "core", "env": "prod"},
	Created: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
}

// openSecretDetail opens the detail of s and returns its text view.
func openSecretDetail(t *testing.T, u *ui, s swarmops.Secret) *tview.TextView {
	t.Helper()
	onLoop(t, u, func() { u.secTab().showDetail(s) })
	waitFront(t, u, pageSecDetail)
	tv := focusedAs[*tview.TextView](t, u)
	return tv
}

func TestSecretDetailShowsMetadataAndUsers(t *testing.T) {
	u, _, _ := editUI(t, editSwarm())
	tv := openSecretDetail(t, u, apiKeySecret)
	created := apiKeySecret.Created.Local().Format("2006-01-02 15:04:05")
	waitFor(t, "the usage lookup", func() bool { return strings.Contains(textOf(t, u, tv), "web (1)") })
	text := textOf(t, u, tv)
	want := []string{
		"Name:     api_key\n",
		"ID:       sec-api\n",
		"Created:  " + created + "\n",
		"Updated:  -\n",
		"Labels:\n  env=prod\n  team=core\n",
		"used by:\n  web (1)\n      web1aaaaaaaa  node-a\n",
		"the secret value is not retrievable via the Docker API",
	}
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("detail lacks %q:\n%s", w, text)
		}
	}
}

func TestSecretDetailWithoutLabelsOrUsers(t *testing.T) {
	u, _, _ := editUI(t, editSwarm())
	tv := openSecretDetail(t, u, swarmops.Secret{ID: "sec-db", Name: "db_pass"})
	waitFor(t, "the usage lookup", func() bool { return strings.Contains(textOf(t, u, tv), "(no services)") })
	if text := textOf(t, u, tv); !hasAll(text, "Labels:   -\n", "Created:  -\n") {
		t.Errorf("detail:\n%s", text)
	}
}

// Before the lookup answers, the services known from the list show with a
// pending marker. Rendered directly: the lookup is too quick to catch.
func TestSecretDetailTextWhileLoading(t *testing.T) {
	got := secretDetailText(apiKeySecret, []swarmops.NetService{{Name: "web"}}, true)
	if !strings.Contains(got, "used by:\n  web …\n") {
		t.Errorf("loading:\n%s", got)
	}
	if got := secretDetailText(swarmops.Secret{Name: "x"}, nil, true); !strings.Contains(got, "used by:\n  …\n") {
		t.Errorf("loading, no services:\n%s", got)
	}
}

func TestSecretDetailKeys(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	tv := openSecretDetail(t, u, apiKeySecret)
	send := func(ev *tcell.EventKey) *tcell.EventKey {
		var out *tcell.EventKey
		onLoop(t, u, func() { out = tv.GetInputCapture()(ev) })
		return out
	}
	if out := send(rune_('j')); out == nil || out.Key() != tcell.KeyDown {
		t.Errorf("j became %v", out)
	}
	if out := send(rune_('k')); out == nil || out.Key() != tcell.KeyUp {
		t.Errorf("k became %v", out)
	}
	if ev := rune_('Z'); send(ev) != ev {
		t.Error("an unbound key was not passed on")
	}

	// d: detach from one of the services using it.
	send(rune_('d'))
	waitFront(t, u, pageSvcPrompt)
	keyCode(t, u, tcell.KeyEscape)
	waitFront(t, u, pageSecDetail)

	// r and m keep their global meaning (refresh, mouse): the detail passes
	// them on instead of rotating (review T-10).
	for _, r := range []rune{'r', 'm'} {
		if ev := rune_(r); send(ev) != ev {
			t.Errorf("%c was not passed on", r)
		}
	}
	if pageOpen(t, u, pageSecRotate) {
		t.Fatal("r opened the rotate editor")
	}

	// R: the rotate editor.
	send(rune_('R'))
	waitFront(t, u, pageSecRotate)
	onLoop(t, u, func() { u.pages.RemovePage(pageSecRotate); u.app.SetFocus(tv) })

	// a: attach to a service, confirmed, applied; the detail closes.
	send(rune_('a'))
	waitFront(t, u, pageSvcPrompt)
	answer(t, u, "agent")
	confirmAndWait(t, u, ring, `\"agent\" updated`)
	var got []string
	for _, r := range ls.spec(t, "agent").TaskTemplate.ContainerSpec.Secrets {
		got = append(got, r.SecretName)
	}
	if !slices.Equal(got, []string{"api_key"}) {
		t.Errorf("agent's secrets = %q", got)
	}
	if pageOpen(t, u, pageSecDetail) {
		t.Error("the detail stayed open after the attach")
	}
}

func TestSecretDetailCloseAndDetachRefusal(t *testing.T) {
	for _, ev := range []*tcell.EventKey{
		tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone), rune_(defaultKeybinds().Quit), rune_('i'),
	} {
		u, _, _ := editUI(t, editSwarm())
		openSecretDetail(t, u, apiKeySecret)
		key(t, u, ev)
		waitFront(t, u, pageMain)
	}

	u, _, ring := editUI(t, editSwarm())
	openSecretDetail(t, u, swarmops.Secret{ID: "sec-db", Name: "db_pass"})
	keyRune(t, u, 'd')
	waitForLog(t, ring, "ui notice", "no services use this secret")
}
