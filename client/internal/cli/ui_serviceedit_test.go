// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/rivo/tview"

	"swarmexec/client/internal/swarmops"
)

// The service editors end to end (review Q-01): each opens on the fake
// swarm's current spec, is driven through its real widgets, and the test
// reads back what the update wrote.

// openEditor runs open on the UI goroutine with the tree as "back" and an
// after hook that counts, and waits for want to be the front page.
func openEditor(t *testing.T, u *ui, want string, open func(back tview.Primitive, after func())) *int {
	t.Helper()
	afters := new(int)
	onLoop(t, u, func() { open(u.ctree, func() { *afters++ }) })
	waitFront(t, u, want)
	return afters
}

func afterCount(t *testing.T, u *ui, n *int) int {
	t.Helper()
	var v int
	onLoop(t, u, func() { v = *n })
	return v
}

func TestScalePromptScalesAReplicatedService(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	afters := openEditor(t, u, pageScalePrompt, func(b tview.Primitive, a func()) { u.openScalePrompt("web", b, a) })
	if in := focusedAs[*tview.InputField](t, u); in.GetText() != "2" {
		t.Errorf("the prompt starts at %q, want the current 2", in.GetText())
	}
	answer(t, u, "5")
	waitForLog(t, ring, "ui notice", `scaled \"web\" to 5`)
	if r := ls.spec(t, "web").Mode.Replicated.Replicas; *r != 5 {
		t.Errorf("replicas = %d, want 5", *r)
	}
	if n := afterCount(t, u, afters); n != 1 {
		t.Errorf("after ran %d times", n)
	}
}

func TestScalePromptRefusals(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	// A global service has no replica count.
	onLoop(t, u, func() { u.openScalePrompt("agent", u.ctree, nil) })
	waitForLog(t, ring, "ui notice", `service \"agent\" is not replicated`)
	waitFront(t, u, pageInfo)
	keyCode(t, u, tcell.KeyEnter)

	// An empty count is refused; Esc changes nothing either.
	openEditor(t, u, pageScalePrompt, func(b tview.Primitive, a func()) { u.openScalePrompt("web", b, a) })
	answer(t, u, "")
	waitForLog(t, ring, "ui notice", "invalid replica count")
	keyCode(t, u, tcell.KeyEnter)
	openEditor(t, u, pageScalePrompt, func(b tview.Primitive, a func()) { u.openScalePrompt("web", b, a) })
	keyCode(t, u, tcell.KeyEscape)
	waitFront(t, u, pageMain)
	if v := ls.version("web"); v != 1 {
		t.Errorf("a refused scale updated the service (version %d)", v)
	}

	// A service that does not exist says so.
	onLoop(t, u, func() { u.openScalePrompt("nope", u.ctree, nil) })
	waitForLog(t, ring, "ui notice", "cannot load service")
}

func TestPortsEditorAddsAPort(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	afters := openEditor(t, u, pageListEdit, func(b tview.Primitive, a func()) { u.openPortsEditor("web", b, a) })
	keyRune(t, u, 'a')
	waitFront(t, u, pageListEditPrompt)
	answer(t, u, "9090:90/udp")
	waitFront(t, u, pageListEdit)
	keyRune(t, u, 'w')
	confirmAndWait(t, u, ring, "service updated")
	var got []string
	for _, p := range ls.spec(t, "web").EndpointSpec.Ports {
		got = append(got, swarmops.FormatServicePort(p))
	}
	if want := []string{"8080:80/tcp", "9090:90/udp"}; !slices.Equal(got, want) {
		t.Errorf("ports = %q, want %q", got, want)
	}
	if n := afterCount(t, u, afters); n != 1 {
		t.Errorf("after ran %d times", n)
	}
}

func TestPortsEditorRefusesABadPort(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	openEditor(t, u, pageListEdit, func(b tview.Primitive, a func()) { u.openPortsEditor("web", b, a) })
	keyRune(t, u, 'a')
	waitFront(t, u, pageListEditPrompt)
	answer(t, u, "80")
	waitForLog(t, ring, "ui notice", "port must be PUBLISHED:TARGET")
	if v := ls.version("web"); v != 1 {
		t.Errorf("version %d after a refused entry", v)
	}
}

func TestLabelsEditorFormAddsALabel(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	openEditor(t, u, pageListEdit, func(b tview.Primitive, a func()) { u.openLabelsEditor("web", b, a) })
	keyRune(t, u, 'a')
	waitFront(t, u, pageListEditPrompt)
	f := frontForm(t, u)
	if title := formTitle(t, u, f); !strings.Contains(title, "add label") {
		t.Errorf("title %q", title)
	}

	// The form refuses an empty key and a key with "=", in its title.
	clickButton(t, u, f, "Save")
	if title := formTitle(t, u, f); !strings.Contains(title, "the key is required") {
		t.Errorf("empty key: title %q", title)
	}
	setField(t, u, f, "key", "a=b")
	clickButton(t, u, f, "Save")
	if title := formTitle(t, u, f); !strings.Contains(title, "must not contain =") {
		t.Errorf("key with =: title %q", title)
	}

	setField(t, u, f, "key", "traefik.http.routers.web.rule")
	setField(t, u, f, "value", "Host(`app.example.com`)")
	clickButton(t, u, f, "Save")
	waitFront(t, u, pageListEdit)
	keyRune(t, u, 'w')
	confirmAndWait(t, u, ring, "service updated")
	labels := ls.spec(t, "web").Labels
	if labels["team"] != "core" || labels["traefik.http.routers.web.rule"] != "Host(`app.example.com`)" {
		t.Errorf("labels = %v", labels)
	}
}

func TestLabelsEditorEditPrefillsTheForm(t *testing.T) {
	u, _, _ := editUI(t, editSwarm())
	openEditor(t, u, pageListEdit, func(b tview.Primitive, a func()) { u.openLabelsEditor("web", b, a) })
	keyRune(t, u, 'e')
	waitFront(t, u, pageListEditPrompt)
	f := frontForm(t, u)
	var k, v string
	onLoop(t, u, func() {
		k = fieldText(f.GetFormItemByLabel("key").(*tview.TextArea))
		v = fieldText(f.GetFormItemByLabel("value").(*tview.TextArea))
	})
	if k != "team" || v != "core" {
		t.Errorf("edit form starts with %q=%q", k, v)
	}
	if title := formTitle(t, u, f); !strings.Contains(title, "edit label") {
		t.Errorf("title %q", title)
	}
	clickButton(t, u, f, "Cancel")
	waitFront(t, u, pageListEdit)
}

func TestEnvEditorWritesTheEnvironment(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	afters := openEditor(t, u, pageEnvEdit, func(b tview.Primitive, a func()) { u.openEnvEditor("web", b, a) })
	ta := focusedAs[*tview.TextArea](t, u)
	if got := strings.TrimSpace(ta.GetText()); got != "A=1" {
		t.Errorf("the editor opens on %q", got)
	}
	// Unchanged: :w closes and says so, without an update.
	for _, ev := range parseKeys(t, ":w<CR>") {
		key(t, u, ev)
	}
	waitFront(t, u, pageMain)
	if v := ls.version("web"); v != 1 {
		t.Errorf("an unchanged :w updated the service (version %d)", v)
	}

	afters = openEditor(t, u, pageEnvEdit, func(b tview.Primitive, a func()) { u.openEnvEditor("web", b, a) })
	ta = focusedAs[*tview.TextArea](t, u)
	onLoop(t, u, func() { ta.SetText("A=1\nB=two", false) })
	for _, ev := range parseKeys(t, ":w<CR>") {
		key(t, u, ev)
	}
	confirmAndWait(t, u, ring, "service updated")
	if env := ls.spec(t, "web").TaskTemplate.ContainerSpec.Env; !slices.Equal(env, []string{"A=1", "B=two"}) {
		t.Errorf("env = %q", env)
	}
	if n := afterCount(t, u, afters); n != 1 {
		t.Errorf("after ran %d times", n)
	}
}

func TestEnvEditorListSwitchesToTheListEditor(t *testing.T) {
	u, _, _ := editUI(t, editSwarm())
	openEditor(t, u, pageEnvEdit, func(b tview.Primitive, a func()) { u.openEnvEditor("web", b, a) })
	ta := focusedAs[*tview.TextArea](t, u)
	onLoop(t, u, func() { ta.SetText("A=1\nC=3", false) })
	for _, ev := range parseKeys(t, ":list<CR>") {
		key(t, u, ev)
	}
	waitFront(t, u, pageListEdit)
	l := focusedAs[*tview.List](t, u)
	var rows []string
	onLoop(t, u, func() {
		for i := 0; i < l.GetItemCount(); i++ {
			m, _ := l.GetItemText(i)
			rows = append(rows, m)
		}
	})
	if !strings.Contains(strings.Join(rows, "|"), "C=3") {
		t.Errorf(":list did not carry the typed text over: %q", rows)
	}
}

func TestSecretsEditorAttachesASecret(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	openEditor(t, u, pageListEdit, func(b tview.Primitive, a func()) { u.openSecretsEditor("web", b, a) })
	keyRune(t, u, 'a')
	waitFront(t, u, pageListEditPrompt)
	answer(t, u, "nope")
	waitForLog(t, ring, "ui notice", `unknown secret \"nope\"`)
	keyCode(t, u, tcell.KeyEnter) // the notice
	waitFront(t, u, pageListEdit)
	keyRune(t, u, 'a')
	waitFront(t, u, pageListEditPrompt)
	answer(t, u, "db_pass")
	waitFront(t, u, pageListEdit)
	keyRune(t, u, 'w')
	confirmAndWait(t, u, ring, "service updated")
	var got []string
	for _, r := range ls.spec(t, "web").TaskTemplate.ContainerSpec.Secrets {
		got = append(got, r.SecretName+"="+r.SecretID)
	}
	if want := []string{"api_key=sec-api", "db_pass=sec-db"}; !slices.Equal(got, want) {
		t.Errorf("secrets = %q, want %q", got, want)
	}
}

func TestNetworksEditorAttachesANetwork(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	openEditor(t, u, pageListEdit, func(b tview.Primitive, a func()) { u.openNetworksEditor("web", b, a) })
	keyRune(t, u, 'a')
	waitFront(t, u, pageListEditPrompt)
	answer(t, u, "net-back") // an id is shown, and kept, as its name
	waitFront(t, u, pageListEdit)
	keyRune(t, u, 'w')
	confirmAndWait(t, u, ring, "service updated")
	var got []string
	for _, n := range ls.spec(t, "web").TaskTemplate.Networks {
		got = append(got, n.Target)
	}
	if want := []string{"net-front", "net-back"}; !slices.Equal(got, want) {
		t.Errorf("networks = %q, want %q", got, want)
	}
}

func TestNetworksEditorOpensTheAliasEditor(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	openEditor(t, u, pageListEdit, func(b tview.Primitive, a func()) { u.openNetworksEditor("web", b, a) })
	keyRune(t, u, 'A') // aliases of the network under the cursor: front
	waitForList := func(want string) {
		t.Helper()
		waitFront(t, u, pageListEdit)
		l := focusedAs[*tview.List](t, u)
		var first string
		onLoop(t, u, func() { first, _ = l.GetItemText(0) })
		if !strings.Contains(first, want) {
			t.Fatalf("first row %q, want %q", first, want)
		}
	}
	waitForList("www")
	keyRune(t, u, 'a')
	waitFront(t, u, pageListEditPrompt)
	answer(t, u, "app")
	waitFront(t, u, pageListEdit)
	keyRune(t, u, 'w')
	confirmAndWait(t, u, ring, "service updated")
	if a := ls.spec(t, "web").TaskTemplate.Networks[0].Aliases; !slices.Equal(a, []string{"www", "app"}) {
		t.Errorf("aliases = %q", a)
	}

	// Not attached yet: the alias editor says to apply the network first.
	onLoop(t, u, func() { u.openAliasEditorForNet("web", "back", "net-back", u.ctree, nil) })
	waitForLog(t, ring, "ui notice", "is not attached to network")
}

func TestPlacementMenuConstraintsAndSpread(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	openEditor(t, u, pagePlacementMenu, func(b tview.Primitive, a func()) { u.openPlacementMenu("web", b, a) })
	keyCode(t, u, tcell.KeyEnter) // Constraints
	waitFront(t, u, pageListEdit)
	keyRune(t, u, 'a')
	waitFront(t, u, pageListEditPrompt)
	answer(t, u, "node.role==manager")
	waitFront(t, u, pageListEdit)
	keyRune(t, u, 'w')
	confirmAndWait(t, u, ring, "service updated")
	if c := ls.spec(t, "web").TaskTemplate.Placement.Constraints; !slices.Equal(c, []string{"node.role==manager"}) {
		t.Errorf("constraints = %q", c)
	}
	keyCode(t, u, tcell.KeyEnter) // the notice

	openEditor(t, u, pagePlacementMenu, func(b tview.Primitive, a func()) { u.openPlacementMenu("web", b, a) })
	keyCode(t, u, tcell.KeyTab) // to "Spread preferences"
	keyCode(t, u, tcell.KeyEnter)
	waitFront(t, u, pageListEdit)
	keyRune(t, u, 'a')
	waitFront(t, u, pageListEditPrompt)
	answer(t, u, "node.labels.zone")
	waitFront(t, u, pageListEdit)
	keyRune(t, u, 'w')
	waitFront(t, u, pageConfirm)
	keyCode(t, u, tcell.KeyEnter)
	waitFront(t, u, pageInfo) // service updated
	keyCode(t, u, tcell.KeyEnter)
	if v := ls.version("web"); v != 3 {
		t.Errorf("version %d after two updates", v)
	}
	prefs := ls.spec(t, "web").TaskTemplate.Placement.Preferences
	if len(prefs) != 1 || prefs[0].Spread.SpreadDescriptor != "node.labels.zone" {
		t.Errorf("preferences = %+v", prefs)
	}

	// Cancel hands the keyboard back and opens nothing.
	openEditor(t, u, pagePlacementMenu, func(b tview.Primitive, a func()) { u.openPlacementMenu("web", b, a) })
	keyCode(t, u, tcell.KeyEscape)
	waitFront(t, u, pageMain)
}

func TestMountsEditorAddsVolumeAndBindMounts(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	afters := openEditor(t, u, pageListEdit, func(b tview.Primitive, a func()) { u.openMountsEditor("web", b, a) })

	// A volume mount, read-only.
	keyRune(t, u, 'a')
	waitFront(t, u, pageListEditPrompt)
	f := frontForm(t, u)
	if title := formTitle(t, u, f); !strings.Contains(title, "add mount") {
		t.Errorf("title %q", title)
	}
	clickButton(t, u, f, "Save")
	if title := formTitle(t, u, f); !strings.Contains(title, "source and container path are required") {
		t.Errorf("empty form: title %q", title)
	}
	setField(t, u, f, "volume", "cache")
	setField(t, u, f, "container path", "/cache")
	toggleCheck(t, u, f, "read-only")
	clickButton(t, u, f, "Save")
	waitFront(t, u, pageListEdit)

	// A bind mount: the toggle swaps the volume field for the host path and
	// carries what was typed across.
	keyRune(t, u, 'a')
	waitFront(t, u, pageListEditPrompt)
	f = frontForm(t, u)
	setField(t, u, f, "volume", "/srv/conf")
	toggleCheck(t, u, f, "bind mount")
	var host string
	onLoop(t, u, func() {
		if f.GetFormItemByLabel("volume") != nil {
			t.Error("the volume field is still shown for a bind mount")
		}
		host = fieldText(f.GetFormItemByLabel("host path").(*tview.TextArea))
	})
	if host != "/srv/conf" {
		t.Errorf("host path %q, want what was typed as the volume", host)
	}
	setField(t, u, f, "container path", "/etc/app")
	clickButton(t, u, f, "Save")
	waitFront(t, u, pageListEdit)

	keyRune(t, u, 'w')
	waitFront(t, u, pageConfirm)
	keyCode(t, u, tcell.KeyEnter)
	waitForLog(t, ring, "ui notice", "service updated")
	got := ls.spec(t, "web").TaskTemplate.ContainerSpec.Mounts
	want := []mount.Mount{
		{Type: mount.TypeVolume, Source: "data", Target: "/data"},
		{Type: mount.TypeVolume, Source: "cache", Target: "/cache", ReadOnly: true},
		{Type: mount.TypeBind, Source: "/srv/conf", Target: "/etc/app"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("mounts = %+v\nwant %+v", got, want)
	}
	if n := afterCount(t, u, afters); n != 1 {
		t.Errorf("after ran %d times", n)
	}
}

// The confirm for a bind mount warns that the source must exist on every
// node the service can run on, and names them; a constraint narrows them.
func TestMountsEditorBindConfirmNamesTheNodes(t *testing.T) {
	fs := editSwarm()
	fs.Services[0].Spec.TaskTemplate.Placement = &swarm.Placement{Constraints: []string{"node.labels.zone==a", "engine.foo==bar"}}
	u, _, _ := editUI(t, fs)
	openEditor(t, u, pageListEdit, func(b tview.Primitive, a func()) { u.openMountsEditor("web", b, a) })
	keyRune(t, u, 'w') // volumes only: no warning
	waitFront(t, u, pageConfirm)
	if text := drawFront(t, u, 240, 60); strings.Contains(text, "ALREADY") {
		t.Errorf("a volume-only list warns:\n%s", text)
	}
	keyCode(t, u, tcell.KeyEscape)
	waitFront(t, u, pageListEdit)

	keyRune(t, u, 'a')
	waitFront(t, u, pageListEditPrompt)
	f := frontForm(t, u)
	toggleCheck(t, u, f, "bind mount")
	setField(t, u, f, "host path", "/srv/a")
	setField(t, u, f, "container path", "/a")
	clickButton(t, u, f, "Save")
	waitFront(t, u, pageListEdit)
	keyRune(t, u, 'w')
	waitFront(t, u, pageConfirm)
	if text := drawFront(t, u, 240, 60); !hasAll(text, "ALREADY", "1 node(s): node-a", "binds: /srv/a", "unevaluated constraints: engine.foo==bar", "Update the mounts?") {
		t.Errorf("confirm:\n%s", text)
	}
}

func TestMountsEditorEditPrefillsTheForm(t *testing.T) {
	u, _, _ := editUI(t, editSwarm())
	openEditor(t, u, pageListEdit, func(b tview.Primitive, a func()) { u.openMountsEditor("web", b, a) })
	keyRune(t, u, 'e')
	waitFront(t, u, pageListEditPrompt)
	f := frontForm(t, u)
	var vol, tgt string
	onLoop(t, u, func() {
		vol = f.GetFormItemByLabel("volume").(*tview.InputField).GetText()
		tgt = fieldText(f.GetFormItemByLabel("container path").(*tview.TextArea))
	})
	if vol != "data" || tgt != "/data" {
		t.Errorf("edit form starts with %q → %q", vol, tgt)
	}
	if title := formTitle(t, u, f); !strings.Contains(title, "edit mount") {
		t.Errorf("title %q", title)
	}
	// Unticking bind on a volume mount keeps the volume (round trip).
	toggleCheck(t, u, f, "bind mount")
	toggleCheck(t, u, f, "bind mount")
	onLoop(t, u, func() { vol = f.GetFormItemByLabel("volume").(*tview.InputField).GetText() })
	if vol != "data" {
		t.Errorf("after bind on/off the volume is %q", vol)
	}
	clickButton(t, u, f, "Cancel")
	waitFront(t, u, pageListEdit)
}

func TestResourcesEditorSetsLimits(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	afters := openEditor(t, u, pageResEdit, func(b tview.Primitive, a func()) { u.openResourcesEditor("web", b, a) })
	f := frontForm(t, u)
	for _, tc := range []struct{ field, value, want string }{
		{"CPU limit (cores)", "abc", "⚠"},
		{"CPU reservation", "2", "CPU reservation exceeds the limit"},
	} {
		setField(t, u, f, "CPU limit (cores)", "1")
		setField(t, u, f, "CPU reservation", "")
		setField(t, u, f, tc.field, tc.value)
		clickButton(t, u, f, "Apply")
		if title := formTitle(t, u, f); !strings.Contains(title, tc.want) {
			t.Errorf("%s=%s: title %q, want %q", tc.field, tc.value, title, tc.want)
		}
	}
	setField(t, u, f, "CPU reservation", "")
	setField(t, u, f, "Memory limit", "256m")
	setField(t, u, f, "Memory reservation", "512m")
	clickButton(t, u, f, "Apply")
	if title := formTitle(t, u, f); !strings.Contains(title, "memory reservation exceeds the limit") {
		t.Errorf("memory: title %q", title)
	}

	setField(t, u, f, "CPU limit (cores)", "0.5")
	setField(t, u, f, "Memory limit", "512m")
	setField(t, u, f, "Memory reservation", "")
	clickButton(t, u, f, "Apply")
	confirmAndWait(t, u, ring, "service updated")
	lim := ls.spec(t, "web").TaskTemplate.Resources.Limits
	if lim.NanoCPUs != 5e8 || lim.MemoryBytes != 512<<20 {
		t.Errorf("limits = %+v", lim)
	}
	if n := afterCount(t, u, afters); n != 1 {
		t.Errorf("after ran %d times", n)
	}
}

func TestImageUpdateConfirmAndPicker(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	// A plain confirm on a target.
	onLoop(t, u, func() { u.confirmImageUpdate("web", "nginx:1.28", false, u.ctree, nil) })
	confirmAndWait(t, u, ring, `updating \"web\"`)
	if img := ls.spec(t, "web").TaskTemplate.ContainerSpec.Image; img != "nginx:1.28" {
		t.Errorf("image = %q", img)
	}
	keyCode(t, u, tcell.KeyEnter) // the notice

	info := upgradeInfo{repo: "nginx", current: "1.28", newer: []string{"1.29"}, all: []string{"1.27", "1.28", "1.29"}}
	// The picker starts on the newest version; a tag the registry does not
	// list is refused.
	openEditor(t, u, pageImageVersion, func(b tview.Primitive, a func()) { u.openImageVersionPicker("web", info, b, a) })
	if in := focusedAs[*tview.InputField](t, u); in.GetText() != "1.29" {
		t.Errorf("picker starts on %q", in.GetText())
	}
	answer(t, u, "9.9")
	waitForLog(t, ring, "ui notice", `no such tag in nginx: \"9.9\"`)
	keyCode(t, u, tcell.KeyEnter)

	// An older tag is a downgrade, and the confirm says so.
	openEditor(t, u, pageImageVersion, func(b tview.Primitive, a func()) { u.openImageVersionPicker("web", info, b, a) })
	answer(t, u, "1.27")
	waitFront(t, u, pageConfirm)
	if text := drawFront(t, u, 240, 60); !hasAll(text, "DOWNGRADE", "nginx:1.27") {
		t.Errorf("downgrade confirm:\n%s", text)
	}
	keyCode(t, u, tcell.KeyEnter)
	waitFront(t, u, pageInfo) // updating …
	keyCode(t, u, tcell.KeyEnter)
	if img := ls.spec(t, "web").TaskTemplate.ContainerSpec.Image; img != "nginx:1.27" {
		t.Errorf("image after the downgrade = %q", img)
	}

	// Nothing to offer: no tags and no target.
	onLoop(t, u, func() { u.openImageVersionPicker("web", upgradeInfo{repo: "nginx"}, u.ctree, nil) })
	waitForLog(t, ring, "ui notice", "No versions available")
	keyCode(t, u, tcell.KeyEnter)

	// A digest-pinned :latest goes straight to the confirm.
	onLoop(t, u, func() {
		u.openImageVersionPicker("web", upgradeInfo{repo: "nginx", target: "nginx:latest@sha256:abc"}, u.ctree, nil)
	})
	waitFront(t, u, pageConfirm)
	keyCode(t, u, tcell.KeyEscape)
	waitFront(t, u, pageMain)
}

func TestForceUpdateBumpsTheForceCounter(t *testing.T) {
	u, ls, ring := editUI(t, editSwarm())
	onLoop(t, u, func() { u.openForceUpdate("web", u.ctree, nil) })
	confirmAndWait(t, u, ring, `force-updating \"web\"`)
	if n := ls.spec(t, "web").TaskTemplate.ForceUpdate; n != 1 {
		t.Errorf("ForceUpdate = %d, want 1", n)
	}
}

func TestEditorLoadFailuresSayWhy(t *testing.T) {
	u, _, ring := editUI(t, editSwarm())
	for _, tc := range []struct {
		open func()
		want string
	}{
		{func() { u.openPortsEditor("nope", u.ctree, nil) }, "cannot load ports"},
		{func() { u.openLabelsEditor("nope", u.ctree, nil) }, "cannot load labels"},
		{func() { u.openEnvEditor("nope", u.ctree, nil) }, "cannot load env"},
		{func() { u.openSecretsEditor("nope", u.ctree, nil) }, "cannot load secrets"},
		{func() { u.openNetworksEditor("nope", u.ctree, nil) }, "cannot load networks"},
		{func() { u.openMountsEditor("nope", u.ctree, nil) }, "cannot load mounts"},
		{func() { u.openResourcesEditor("nope", u.ctree, nil) }, "cannot load resources"},
		{func() { u.openPlacementConstraintsEditor("nope", u.ctree, nil) }, "cannot load placement"},
		{func() { u.openSpreadEditor("nope", u.ctree, nil) }, "cannot load spread preferences"},
	} {
		onLoop(t, u, tc.open)
		waitForLog(t, ring, "ui notice", tc.want)
	}
}
