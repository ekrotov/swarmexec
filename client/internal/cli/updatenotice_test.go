// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/updatecheck"
)

// No test in this package asks the real site: every run of the release
// check goes to a stub that knows no newer release, unless a test installs
// its own (stubUpdate, which puts this default back when the test ends). Set
// in TestMain rather than init, so it is visible where the test binary starts.
func TestMain(m *testing.M) {
	checkForUpdate = func(context.Context, string, string) (string, bool) { return "", false }
	os.Exit(m.Run())
}

// shownText is markup as the screen shows it, trimmed.
func shownText(s string) string {
	return strings.TrimSpace(tview.NewTextView().SetDynamicColors(true).SetText(s).GetText(true))
}

// updateCall is one call the stub saw.
type updateCall struct{ current, cfgDir string }

// updateCalls records the stub's calls; safe across goroutines.
type updateCalls struct {
	mu    sync.Mutex
	calls []updateCall
}

func (c *updateCalls) list() []updateCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]updateCall(nil), c.calls...)
}

// stubUpdate makes the release check answer newer (ok when newer != "") and
// records its calls; the package stub is back after the test.
func stubUpdate(t *testing.T, newer string) *updateCalls {
	t.Helper()
	rec := &updateCalls{}
	prev := checkForUpdate
	checkForUpdate = func(_ context.Context, current, dir string) (string, bool) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.calls = append(rec.calls, updateCall{current, dir})
		return newer, newer != ""
	}
	t.Cleanup(func() { checkForUpdate = prev })
	return rec
}

func TestStatusVersionText(t *testing.T) {
	for _, c := range []struct {
		newer string
		width int
		want  string
	}{
		{"", 0, "v1.26.0"},
		{"", 120, "v1.26.0"},
		{"", 80, ""},
		{"v1.27.0", 0, "v1.26.0 → v1.27.0"},
		{"v1.27.0", 120, "v1.26.0 → v1.27.0"},
		{"v1.27.0", 100, "v1.26.0 → v1.27.0"},
		{"v1.27.0", 99, "↑ v1.27.0"},
		{"v1.27.0", 60, "↑ v1.27.0"},
		{"v1.27.0", 59, ""},
	} {
		got := statusVersionText("v1.26.0", c.newer, c.width)
		if p := shownText(got); p != c.want {
			t.Errorf("newer %q width %d: %q, want %q", c.newer, c.width, p, c.want)
		}
		if c.newer != "" && got != "" && !strings.HasPrefix(got, "[yellow]") {
			t.Errorf("newer release not in yellow: %q", got)
		}
	}
}

// F17 with T-17: at 120 columns the right end says "v1.26.0 → v1.27.0", at
// 80 only "↑ v1.27.0" beside the contexts hint — and the left part still
// fits beside it.
func TestStatusLineShowsNewerReleaseAt80And120Columns(t *testing.T) {
	u := statusUI(t)
	u.g.version.Binary = "v1.26.0"
	u.newerRelease = "v1.27.0"
	line := func(w int) string {
		u.resizeSidebar(w)
		return drawPrimitive(t, u.statusRow, w, 1)[0]
	}

	wide := line(120)
	if !strings.HasSuffix(strings.TrimSpace(wide), "v1.26.0 → v1.27.0") || !strings.Contains(wide, "agents") {
		t.Errorf("120 columns: %q", wide)
	}

	narrow := line(80)
	if !strings.Contains(narrow, "c contexts  ·  ↑ v1.27.0") || strings.Contains(narrow, "v1.26.0") {
		t.Errorf("80 columns: %q", narrow)
	}
	if !strings.Contains(narrow, "ctx default") || !strings.Contains(narrow, "1n · 0/1 ag") {
		t.Errorf("80 columns lost the left part: %q", narrow)
	}
	if left := tview.TaggedStringWidth(u.status.GetText(false)); left+u.statusRightWidth > 80 {
		t.Errorf("status %d + right %d overflow 80 columns", left, u.statusRightWidth)
	}
}

func TestHelpTitleNamesNewerRelease(t *testing.T) {
	u := &ui{g: &globalFlags{version: Version{Binary: "v1.26.0"}}}
	if got := u.helpTitle(); got != " keybindings — swarmexec v1.26.0 " {
		t.Errorf("no newer release: %q", got)
	}
	u.newerRelease = "v1.27.0"
	if got := u.helpTitle(); got != " keybindings — swarmexec v1.26.0 (v1.27.0 available) " {
		t.Errorf("newer release: %q", got)
	}
}

func TestUpdateNoticeFitsTheFooter(t *testing.T) {
	full := shownText(updateNotice("v1.27.0", 120))
	if full != "swarmexec v1.27.0 is available — see "+updatecheck.InstallURL {
		t.Errorf("120 columns: %q", full)
	}
	short := updateNotice("v1.27.0", 80)
	if w := tview.TaggedStringWidth(short); w > 80 || !strings.Contains(shownText(short), "v1.27.0 available") || !strings.Contains(short, "/docs/install") {
		t.Errorf("80 columns (%d wide): %q", w, shownText(short))
	}
	if shownText(updateNotice("v1.27.0", 0)) != full {
		t.Error("before the first frame the full notice is used")
	}
}

// The check runs only when the config allows it, with this binary's version
// and the config file's directory.
func TestNewerReleaseRespectsConfig(t *testing.T) {
	calls := stubUpdate(t, "v1.27.0")
	dir := t.TempDir()
	g := &globalFlags{version: Version{Binary: "v1.26.0"}, configPath: filepath.Join(dir, "config.yaml")}

	off := false
	cfg := config.Default()
	cfg.UI.CheckUpdates = &off
	if v, ok := g.newerRelease(context.Background(), cfg); ok || len(calls.list()) != 0 {
		t.Fatalf("disabled: %q %v, %d calls", v, ok, len(calls.list()))
	}

	if v, ok := g.newerRelease(context.Background(), config.Default()); !ok || v != "v1.27.0" {
		t.Fatalf("enabled: %q %v", v, ok)
	}
	if c := calls.list()[0]; c.current != "v1.26.0" || c.cfgDir != dir {
		t.Errorf("called with %+v, want v1.26.0 and %s", c, dir)
	}
}

func TestConfigDirFollowsConfigPath(t *testing.T) {
	t.Setenv("SWARMEXEC_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := (&globalFlags{}).configDir(); got != filepath.Join("/xdg", "swarmexec") {
		t.Errorf("default: %q", got)
	}
	if got := (&globalFlags{configPath: "/etc/se/c.yaml"}).configDir(); got != "/etc/se" {
		t.Errorf("--config: %q", got)
	}
}

// End to end on a running UI: the check goes off the UI goroutine, the
// answer lands through QueueUpdateDraw, the status line and the footer say
// so — and with no newer release nothing changes.
func TestReleaseCheckLandsOnRunningUI(t *testing.T) {
	stubUpdate(t, "v1.27.0")
	u := statusUI(t)
	u.g.version.Binary = "v1.26.0"
	u.app = tview.NewApplication()
	u.help = tview.NewTextView().SetDynamicColors(true)
	u.setFooter("keys")
	root := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(u.statusRow, 1, 0, false).AddItem(u.help, 1, 0, false)
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(120, 2)
	u.app.SetScreen(screen).SetRoot(root, true)
	done := make(chan struct{})
	go func() { defer close(done); _ = u.app.Run() }()
	defer func() { u.app.Stop(); <-done }()
	onLoop(t, u, func() { u.resizeSidebar(120) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	onLoop(t, u, func() { u.startReleaseCheck(ctx) })

	deadline := time.Now().Add(5 * time.Second)
	for {
		var right, footer string
		onLoop(t, u, func() { right, footer = u.statusRight.GetText(true), u.help.GetText(true) })
		if strings.Contains(right, "v1.26.0 → v1.27.0") {
			if !strings.Contains(footer, "swarmexec v1.27.0 is available — see "+updatecheck.InstallURL) {
				t.Errorf("footer: %q", footer)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status right never showed the newer release: %q", right)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReleaseCheckWithoutNewerReleaseChangesNothing(t *testing.T) {
	calls := stubUpdate(t, "")
	u := runningUI(t)
	u.g = &globalFlags{version: Version{Binary: "v1.26.0"}}
	u.setFooter("keys")
	onLoop(t, u, func() { u.startReleaseCheck(context.Background()) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := 0
		onLoop(t, u, func() { n = len(calls.list()) })
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	onLoop(t, u, func() {
		if u.newerRelease != "" || u.help.GetText(true) != "keys" {
			t.Errorf("newer %q footer %q", u.newerRelease, u.help.GetText(true))
		}
	})
}
