// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/resolve"
)

// A completion attached to a path that is not in the tree is a no-op nobody
// notices: rename `wait` and its <TAB> just stops working. So every path and
// every flag in the table must exist, and must end up wired.
func TestEveryCompletionTargetExists(t *testing.T) {
	g := &globalFlags{}
	root := newRootCmd(g, Version{})
	byPath := map[string]*cobra.Command{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		byPath[c.CommandPath()] = c
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(root)

	args, flags := completionTargets(g)
	for path := range args {
		c, ok := byPath[path]
		if !ok {
			t.Errorf("completion for %q, which is not a command", path)
			continue
		}
		if c.ValidArgsFunction == nil {
			t.Errorf("%q is in the table but has no completion attached", path)
		}
	}
	for path, fs := range flags {
		c, ok := byPath[path]
		if !ok {
			t.Errorf("flag completion for %q, which is not a command", path)
			continue
		}
		for name := range fs {
			if c.Flags().Lookup(name) == nil {
				t.Errorf("%q has no --%s flag to complete", path, name)
			}
		}
	}
}

// exec's arguments after the target are the command to run; offering service
// names there would be wrong.
func TestOnlyFirstArgCompletesNothingAfterTheTarget(t *testing.T) {
	called := false
	f := onlyFirstArg(func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		called = true
		return []string{"web"}, cobra.ShellCompDirectiveNoFileComp
	})
	if got, _ := f(nil, nil, ""); len(got) != 1 || !called {
		t.Errorf("first argument: got %v", got)
	}
	called = false
	if got, _ := f(nil, []string{"web"}, ""); got != nil || called {
		t.Errorf("after the target nothing must be offered, got %v", got)
	}
}

func TestSlotNames(t *testing.T) {
	got := slotNames([]resolve.Candidate{
		{Service: "web", Slot: 10}, {Service: "web", Slot: 2}, {Service: "agent", Slot: 0},
	})
	if strings.Join(got, " ") != "web.10 web.2" {
		t.Errorf("slots = %v (a global task has no slot to offer)", got)
	}
}

// The one thing that makes a completion over ssh safe: it can never stop to
// ask for a password, because nobody is there to type it. What the flags then
// look like is the endpoint package's own test (TestNonInteractiveSSHNeverPrompts);
// what matters here is that the completion path is what turns it on, and that
// it does so BEFORE the connection is attempted.
func TestCompletionTurnsSSHPromptingOff(t *testing.T) {
	src, err := os.ReadFile("completion.go")
	if err != nil {
		t.Fatal(err)
	}
	fn := string(src)
	i := strings.Index(fn, "func completionClient(")
	if i < 0 {
		t.Fatal("completionClient is gone; this test has to follow it")
	}
	body := fn[i:]
	if j := strings.Index(body, "\n}\n"); j > 0 {
		body = body[:j]
	}
	set := strings.Index(body, "endpoint.SetNonInteractive()")
	dial := strings.Index(body, "Connect(")
	switch {
	case set < 0:
		t.Error("completionClient no longer forbids ssh to prompt")
	case dial >= 0 && set > dial:
		t.Error("ssh prompting is forbidden only after the connection is made")
	}
}
