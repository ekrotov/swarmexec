// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

// Every command's positional-argument contract, written down.
//
// `swarmexec logs api worker` used to be ACCEPTED and to follow only `api`.
// Nothing said `worker` had been dropped, so the operator watched one service
// while believing they were watching two — and that silence reads as a bug in
// their own setup rather than in ours.
//
// The mismatch behind it is between two declarations that sit apart: the Args
// validator, and how many of the args RunE actually reads. Nothing in the
// language ties them together, so they are tied here. The listed counts are how
// many positional arguments the command accepts; a command missing from the
// table fails the test, so a new one cannot quietly skip the question.
var argContract = map[string][]int{
	"swarmexec config show":     {0},
	"swarmexec context create":  {1},
	"swarmexec context ls":      {0},
	"swarmexec context rm":      {1, 2, 3}, // removes every name given
	"swarmexec context use":     {1},
	"swarmexec doctor":          {0},
	"swarmexec down":            {0},
	"swarmexec exec":            {1, 2, 3}, // target, then the command as args[1:]
	"swarmexec init":            {0},
	"swarmexec logs":            {1, 2, 3}, // every target is followed; 0 only with --stack
	"swarmexec port-forward":    {2},       // target and port spec
	"swarmexec ps":              {0, 1},
	"swarmexec security report": {0},
	"swarmexec stack deploy":    {1, 2}, // file, optional stack name
	"swarmexec stack diff":      {1, 2},
	"swarmexec stack export":    {1},
	"swarmexec stack ls":        {0},
	"swarmexec ui":              {0, 1},
	"swarmexec volume ls":       {0, 1},
	"swarmexec volume rm":       {1},
	"swarmexec wait":            {1, 2, 3}, // every service is waited for; 0 only with --stack
}

func TestEveryCommandTakesTheArgumentsItUses(t *testing.T) {
	root := newRootCmd(&globalFlags{}, Version{})
	seen := map[string]bool{}

	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c.Args == nil {
			return // a parent that only groups subcommands
		}
		path := c.CommandPath()
		want, listed := argContract[path]
		if !listed {
			t.Errorf("%q has no entry in argContract — say how many positional "+
				"arguments it takes, and make sure RunE reads all of them", path)
			return
		}
		seen[path] = true

		accepts := map[int]bool{}
		for _, n := range want {
			accepts[n] = true
		}
		for n := 0; n <= 3; n++ {
			err := c.Args(c, make([]string, n))
			if accepts[n] && err != nil {
				t.Errorf("%q should accept %d argument(s) but refuses: %v", path, n, err)
			}
			if !accepts[n] && err == nil {
				t.Errorf("%q accepts %d argument(s), which the contract does not list — "+
					"either RunE uses them all, or the Args validator is too loose", path, n)
			}
		}
	}
	walk(root)

	for path := range argContract {
		if !seen[path] {
			t.Errorf("%q is in argContract but not in the command tree — stale entry", path)
		}
	}
}

// The original regression was `logs api worker` following api and silently
// dropping worker. Several targets are now followed, so the guarantee is the
// other half: none is accepted that is not used, and zero needs --stack.
func TestLogsTargets(t *testing.T) {
	c := newLogsCmd(&globalFlags{})
	if err := c.Args(c, []string{"api", "worker"}); err != nil {
		t.Errorf("several targets must be accepted: %v", err)
	}
	if err := c.Args(c, nil); err == nil {
		t.Error("no target and no --stack must be refused")
	}
	if err := c.Flags().Set("stack", "shop"); err != nil {
		t.Fatal(err)
	}
	if err := c.Args(c, nil); err != nil {
		t.Errorf("--stack alone names what to follow: %v", err)
	}
}
