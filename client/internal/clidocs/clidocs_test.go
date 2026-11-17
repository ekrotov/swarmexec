// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package clidocs

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"swarmexec/client/internal/cli"
)

const (
	testNav    = "<header class=\"rail\">\n  <a href=\"/docs\">Docs</a>\n</header>\n"
	testDocNav = "      <aside class=\"docside\">\n        <a href=\"/docs/cli\">CLI reference</a>\n      </aside>\n"
)

// synthetic is a small tree with everything the reference must leave out.
func synthetic() *cobra.Command {
	run := func(*cobra.Command, []string) error { return nil }
	root := &cobra.Command{Use: "tool", Short: "the tool", RunE: run}
	pf := root.PersistentFlags()
	pf.String("context", "", "docker context")
	pf.String("ca", "", "CA certificate")
	_ = pf.SetAnnotation("ca", connectionAnnotation, []string{"connection"})
	pf.String("old-global", "", "renamed")
	_ = pf.MarkHidden("old-global")

	stack := &cobra.Command{Use: "stack", Short: "stack things"}
	deploy := &cobra.Command{
		Use: "deploy <file>", Short: "deploy a stack", Aliases: []string{"up"}, RunE: run,
		Long: "Deploys it.\n\nColumns:\n  NAME   the name\n  AGE    how old\n\n- first\n- second\n  continued",
	}
	deploy.Flags().Bool("check", false, "check first")
	deploy.Flags().StringP("output", "o", "table", "output format")
	deploy.Flags().Bool("secret-switch", false, "not for users")
	_ = deploy.Flags().MarkHidden("secret-switch")
	stack.AddCommand(deploy)
	stack.AddCommand(&cobra.Command{Use: "internal-only", Short: "hidden", Hidden: true, RunE: run})
	stack.AddCommand(&cobra.Command{Use: "old", Short: "gone", Deprecated: "use deploy", RunE: run})
	root.AddCommand(stack)
	root.AddCommand(&cobra.Command{Use: "topic", Short: "a help topic", Long: "About it."})
	return root
}

func render(t *testing.T, root *cobra.Command) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, root, testNav, testDocNav); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestRenderLeavesOutHiddenAndDeprecated(t *testing.T) {
	out := render(t, synthetic())
	for _, absent := range []string{"--old-global", "--secret-switch", "internal-only", `id="stack-old"`} {
		if strings.Contains(out, absent) {
			t.Errorf("page contains %q, which is hidden or deprecated", absent)
		}
	}
	for _, present := range []string{
		`id="tool"`, `id="stack"`, `id="stack-deploy"`, `id="topic"`,
		"<code>tool help topic</code>", // a help topic, as it is invoked
		`<div class="cmd">tool stack deploy &lt;file&gt; [flags]</div>`,
		"Aliases: <code>up</code>",
		"<code>-o, --output string</code></td><td><code>table</code>",
		`href="/docs" aria-current="true"`,
		`href="/docs/cli" aria-current="page"`,
	} {
		if !strings.Contains(out, present) {
			t.Errorf("page lacks %q", present)
		}
	}
	// --ca is a connection flag: it is in the connection table, not the global one.
	global := out[strings.Index(out, `id="global-flags"`):strings.Index(out, `id="connection-flags"`)]
	conn := out[strings.Index(out, `id="connection-flags"`):]
	if strings.Contains(global, "--ca") || !strings.Contains(global, "--context") {
		t.Errorf("global flags table wrong:\n%s", global)
	}
	if !strings.Contains(conn, "--ca") {
		t.Errorf("connection flags table lacks --ca:\n%s", conn)
	}
}

func TestTextBlocks(t *testing.T) {
	got := textBlocks("Deploys it,\nall of it.\n\nColumns:\n  NAME   the name\n\n  AGE    how old\n\n- first\n- second\n  continued\n\n#### Linux:\n\nDone.")
	want := []Block{
		{Kind: "p", Text: "Deploys it, all of it."},
		{Kind: "p", Text: "Columns:"},
		{Kind: "pre", Text: "NAME   the name\n\nAGE    how old"},
		{Kind: "ul", Items: []string{"first", "second continued"}},
		{Kind: "label", Text: "Linux:"},
		{Kind: "p", Text: "Done."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("textBlocks:\n got %#v\nwant %#v", got, want)
	}
}

// The real tree: every command the help lists has a section with its usage
// line and every visible flag; no hidden flag shows up.
func TestRealTreeEveryCommandHasASection(t *testing.T) {
	root := cli.ReferenceRoot()
	out := render(t, root)
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if !listed(c) {
			return
		}
		if !strings.Contains(out, `id="`+Anchor(c)+`"`) {
			t.Errorf("%s has no section", c.CommandPath())
		}
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if !f.Hidden && !strings.Contains(out, "--"+f.Name+"</code>") && !strings.Contains(out, "--"+f.Name+" ") {
				t.Errorf("%s: flag --%s is missing", c.CommandPath(), f.Name)
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	if strings.Contains(out, "<code>--log-level") {
		t.Error("the hidden --log-level alias is in the reference")
	}
	if !strings.Contains(out, "<code>--client-log-level string</code>") {
		t.Error("--client-log-level is missing from the global flags")
	}
}
