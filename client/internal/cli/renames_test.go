// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"swarmexec/client/internal/clidocs"
)

// renamed is every old name that still works, with what replaced it (review
// CLI-07: four renames, each hand-made and none written down). Adding a rename
// means adding a row here AND annotating it (markRenamed / addHiddenFlagAliases),
// which is also what puts it in the CLI reference's "Renamed names" table.
var renamed = []struct {
	cmd      string // the command the flag lives on ("" = root, persistent)
	old, now string // flag names without dashes; for a command: old path / new path
	command  bool
}{
	{"", "log-level", "client-log-level", false},
	{"", "log-file", "client-log-file", false},
	{"stack deploy", "force", "despite-findings", false},
	{"stack diff", "format", "json", false},
	{"prune-schedule", "", "node prune-schedule", true},
}

func find(t *testing.T, root *cobra.Command, path string) *cobra.Command {
	t.Helper()
	if path == "" {
		return root
	}
	c, rest, err := root.Find(strings.Fields(path))
	if err != nil || len(rest) != 0 {
		t.Fatalf("no command %q: %v %v", path, err, rest)
	}
	return c
}

// Every rename still works under its old name, is hidden from the help and
// says what it became.
func TestRenamedFlagsStillWork(t *testing.T) {
	for _, r := range renamed {
		if r.command {
			continue
		}
		root := newRootCmd(&globalFlags{}, Version{})
		c := find(t, root, r.cmd)
		var f *pflag.Flag
		if r.cmd == "" {
			f = root.PersistentFlags().Lookup(r.old)
		} else {
			f = c.Flags().Lookup(r.old)
		}
		if f == nil {
			t.Errorf("--%s is gone from %q", r.old, r.cmd)
			continue
		}
		if !f.Hidden {
			t.Errorf("--%s must stay out of the help", r.old)
		}
		if got := f.Annotations[clidocs.RenamedAnnotation]; len(got) != 1 || !strings.Contains(got[0], r.now) {
			t.Errorf("--%s is not documented as renamed to %s: %v", r.old, r.now, got)
		}
	}
}

// The renamed flags take effect, not just parse.
func TestRenamedFlagsTakeEffect(t *testing.T) {
	g := &globalFlags{}
	root := newRootCmd(g, Version{})
	if err := root.PersistentFlags().Parse([]string{"--log-level", "error", "--log-file", "/x.log"}); err != nil {
		t.Fatal(err)
	}
	if g.logLevel != "error" || g.logFile != "/x.log" {
		t.Errorf("--log-level/--log-file: %q %q", g.logLevel, g.logFile)
	}

	dc := newStackDeployCmd(&globalFlags{})
	if err := dc.Flags().Parse([]string{"--force"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := dc.Flags().GetBool("despite-findings"); !v {
		t.Error("--force no longer sets --despite-findings")
	}

	dd := newStackDiffCmd(&globalFlags{})
	if err := dd.Flags().Parse([]string{"--format", "json"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := dd.Flags().GetString("format"); v != "json" {
		t.Errorf("--format json = %q", v)
	}
}

// The old `prune-schedule` command is still there, hidden, and runs.
func TestRenamedCommandStillWorks(t *testing.T) {
	root := newRootCmd(&globalFlags{}, Version{})
	old := find(t, root, "prune-schedule")
	if !old.Hidden || old.Annotations[clidocs.RenamedAnnotation] != "swarmexec node prune-schedule" {
		t.Errorf("hidden %v, annotation %v", old.Hidden, old.Annotations)
	}
	if find(t, root, "node prune-schedule") == nil || len(old.Commands()) == 0 {
		t.Error("old or new prune-schedule has no subcommands")
	}
}

// No hidden flag or command may exist without being written down: that is how
// the four silent renames came about. Only the entries of `renamed` and cobra's
// own are allowed.
func TestEveryHiddenNameIsADocumentedRename(t *testing.T) {
	known := map[string]bool{}
	for _, r := range renamed {
		known[r.cmd+"|"+r.old] = true
	}
	root := newRootCmd(&globalFlags{}, Version{})
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		path := strings.TrimPrefix(c.CommandPath(), "swarmexec")
		path = strings.TrimSpace(path)
		each := func(fs *pflag.FlagSet) {
			fs.VisitAll(func(f *pflag.Flag) {
				if f.Hidden && !known[path+"|"+f.Name] && f.Annotations[clidocs.RenamedAnnotation] == nil {
					t.Errorf("%q: --%s is hidden but not a documented rename", path, f.Name)
				}
			})
		}
		each(c.PersistentFlags())
		each(c.LocalNonPersistentFlags())
		for _, s := range c.Commands() {
			if s.Hidden && !known[s.Name()+"|"] && s.Annotations[clidocs.RenamedAnnotation] == "" && s.Name() != "__askpass" {
				t.Errorf("command %q is hidden but not a documented rename", s.CommandPath())
			}
			walk(s)
		}
	}
	walk(root)
}

func TestRenamedAnnotationMatchesClidocs(t *testing.T) {
	if renamedAnnotation != clidocs.RenamedAnnotation {
		t.Errorf("%q != %q", renamedAnnotation, clidocs.RenamedAnnotation)
	}
}

// The reference lists the renames, derived from the same annotations.
func TestReferenceListsTheRenames(t *testing.T) {
	var out bytes.Buffer
	if err := clidocs.Render(&out, ReferenceRoot(), "", ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Renamed names", "swarmexec --log-level", "swarmexec stack deploy --force",
		"swarmexec stack diff --format", "swarmexec prune-schedule"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("reference lacks %q", want)
		}
	}
}
