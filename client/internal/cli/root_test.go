// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestInfoText(t *testing.T) {
	out := infoText(Version{Binary: "v1.2.3", Proto: "swarmexec/v1"})
	for _, want := range []string{"v1.2.3", "swarmexec/v1", contactEmail, "license"} {
		if !strings.Contains(out, want) {
			t.Errorf("info output missing %q:\n%s", want, out)
		}
	}
}

// A wrong argument count names what the command expects: cobra's message
// alone ("requires at least 1 arg(s), only received 0") does not, and
// SilenceUsage keeps cobra from printing the usage. Every command gets it,
// through the one wrapper newRootCmd puts over the tree.
func TestArgErrorPrintsTheUsageLine(t *testing.T) {
	for _, c := range []struct {
		args  []string
		usage string
	}{
		{[]string{"exec"}, "usage: swarmexec exec [flags] <service|service.slot|task-id|container-id> [-- <cmd> [args...]]"},
		{[]string{"port-forward", "web"}, "usage: swarmexec port-forward [flags] <service|service.slot|task-id|container-id> [local:]remote"},
		{[]string{"context", "use"}, "usage: swarmexec context use <name>"},
	} {
		g := &globalFlags{}
		root := newRootCmd(g, Version{})
		root.SetArgs(c.args)
		root.SetOut(io.Discard)
		var out bytes.Buffer
		code := reportError(&out, root.Execute(), g)
		if code != usageExitCode {
			t.Errorf("%v: exit %d, want %d", c.args, code, usageExitCode)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "swarmexec: ") || !strings.HasPrefix(lines[1], c.usage) {
			t.Errorf("%v printed:\n%s\nwant the error, then %q", c.args, out.String(), c.usage)
		}
	}

	// Any other error keeps to its one line.
	var out bytes.Buffer
	if code := reportError(&out, errors.New("unknown flag: --nope"), &globalFlags{}); code != usageExitCode || strings.Contains(out.String(), "usage:") {
		t.Errorf("a flag error printed %q (exit %d)", out.String(), code)
	}
	if code := reportError(&out, nil, &globalFlags{}); code != 0 {
		t.Errorf("no error: exit %d", code)
	}
}
