// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/rivo/tview"

	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/updatecheck"
)

// checkForUpdate is the release check (F17). A variable so the package's
// tests can never reach the real site (see updatenotice_test.go).
var checkForUpdate = updatecheck.Latest

// configDir is the directory of the config file this run reads: where the
// release check keeps its cache. "" when there is no home to put it in.
func (g *globalFlags) configDir() string {
	p := g.configPath
	if p == "" {
		p = config.DefaultFilePath()
	}
	if p == "" {
		return ""
	}
	return filepath.Dir(p)
}

// newerRelease is a swarmexec release newer than this binary, if cfg allows
// asking (ui.check_updates, SWARMEXEC_NO_UPDATE_CHECK) and the site — or the
// day-old cache — knows one. Silent on every failure.
func (g *globalFlags) newerRelease(ctx context.Context, cfg config.Config) (string, bool) {
	if !cfg.UI.UpdateChecks() {
		return "", false
	}
	return checkForUpdate(ctx, g.version.Binary, g.configDir())
}

// startNewerRelease runs newerRelease in the background and returns how to
// wait for its answer ("" when there is no newer release). For a command
// that has other work to do meanwhile.
func (g *globalFlags) startNewerRelease(ctx context.Context, cfg config.Config) func() string {
	ch := make(chan string, 1)
	go func() {
		v, _ := g.newerRelease(ctx, cfg)
		ch <- v
	}()
	var (
		once sync.Once
		v    string
	)
	return func() string {
		once.Do(func() { v = <-ch })
		return v
	}
}

// updateFlashFor is how long the startup notice of a newer release stays in
// the footer.
const updateFlashFor = 8 * time.Second

// startReleaseCheck runs the release check off the UI goroutine; a newer
// release lands on it through QueueUpdateDraw. Called once from
// startSession, on the main goroutine, which is why cfg is read here and not
// in the goroutine.
func (u *ui) startReleaseCheck(ctx context.Context) {
	if u.g == nil {
		return
	}
	g, cfg, app := u.g, u.cfg, u.app
	go func() {
		newer, ok := g.newerRelease(ctx, cfg)
		if !ok || ctx.Err() != nil {
			return
		}
		app.QueueUpdateDraw(func() { u.showNewerRelease(newer) })
	}()
}

// showNewerRelease puts a newer release into the status line and the ?
// title, and says so once in the footer. Runs on the UI goroutine.
func (u *ui) showNewerRelease(newer string) {
	clientlog.L().Info("newer release available", "current", u.binaryVersion(), "latest", newer)
	u.newerRelease = newer
	u.updateStatusRight()
	u.updateStatus()
	u.flashFor(updateNotice(newer, u.screenWidth), updateFlashFor)
}

// updateNotice is the footer line announcing newer, as long as fits width
// (0: not drawn yet).
func updateNotice(newer string, width int) string {
	v := tview.Escape(newer)
	full := " [yellow]swarmexec " + v + " is available[white] — see " + updatecheck.InstallURL
	if width == 0 || tview.TaggedStringWidth(full) <= width {
		return full
	}
	return " [yellow]" + v + " available[white] — swarmshell.dev/docs/install"
}

// helpTitle is the ? overlay's title: it names the version, which a narrow
// status line leaves out (T-17), and a newer release when there is one.
func (u *ui) helpTitle() string {
	t := " keybindings — swarmexec " + tview.Escape(u.binaryVersion())
	if u.newerRelease != "" {
		t += " (" + tview.Escape(u.newerRelease) + " available)"
	}
	return t + " "
}
