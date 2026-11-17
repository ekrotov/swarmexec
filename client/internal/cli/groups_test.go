// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// A mistyped subcommand under a group used to print help and exit 0, so
// `swarmexec stack chekc f.yml && deploy` ran the deploy (CLI-01). Every
// command that has subcommands must reject an unknown word with exit 2 and
// still show help when called bare.
func TestGroupsRejectUnknownSubcommand(t *testing.T) {
	root := newRootCmd(&globalFlags{}, Version{})
	var groups []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c != root && c.HasSubCommands() && c.Name() != "help" {
			groups = append(groups, c)
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(root)
	if len(groups) < 8 {
		t.Fatalf("found only %d groups, the walk is broken", len(groups))
	}
	for _, g := range groups {
		path := strings.Fields(g.CommandPath())[1:]
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			var errBuf bytes.Buffer
			fresh := newRootCmd(&globalFlags{}, Version{})
			fresh.SetArgs(append(append([]string{}, path...), "chekc"))
			fresh.SetOut(&bytes.Buffer{})
			fresh.SetErr(&bytes.Buffer{})
			err := fresh.Execute()
			code := reportError(&errBuf, err, &globalFlags{})
			if code != usageExitCode {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, usageExitCode, errBuf.String())
			}
			if !strings.Contains(errBuf.String(), `unknown command "chekc"`) {
				t.Errorf("stderr = %q, want unknown command", errBuf.String())
			}

			var out bytes.Buffer
			fresh = newRootCmd(&globalFlags{}, Version{})
			fresh.SetArgs(path)
			fresh.SetOut(&out)
			fresh.SetErr(&out)
			if err := fresh.Execute(); err != nil {
				t.Fatalf("bare group: %v", err)
			}
			if !strings.Contains(out.String(), "Usage:") {
				t.Errorf("bare group printed no help: %q", out.String())
			}
		})
	}
}
