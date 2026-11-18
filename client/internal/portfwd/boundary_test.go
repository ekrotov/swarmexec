// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package portfwd

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// portfwd carries an operator's traffic. It has no screen and no commands: the
// cli decides what to print and when to start a forward, and this package
// moves the bytes. Importing the front end back would put a data path inside a
// package that cannot be tested without a terminal, which is where it came
// from.
func TestPortfwdStaysBelowTheCLI(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found — is the test running in the package directory?")
	}
	for _, path := range files {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch {
			case strings.Contains(p, "tview"), strings.Contains(p, "tcell"):
				t.Errorf("%s imports %s; a forward has no screen", path, p)
			case p == "swarmexec/client/internal/cli":
				t.Errorf("%s imports the cli package; cli starts forwards, never the reverse", path)
			case p == "swarmexec/client/internal/dial":
				t.Errorf("%s imports dial; agent connections belong to agentcall", path)
			}
		}
	}
}
