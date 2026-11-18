// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentdeploy

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// agentdeploy writes to a cluster; it does not talk to an operator. Importing
// the cli back would put the deployment's contract with internal/deploy inside
// the package that owns flags and printing, which is where it came from.
func TestAgentdeployStaysBelowTheCLI(t *testing.T) {
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
				t.Errorf("%s imports %s; deploying has no screen", path, p)
			case strings.Contains(p, "spf13/cobra"):
				t.Errorf("%s imports cobra; the flags belong to the init command", path)
			case p == "swarmexec/client/internal/cli":
				t.Errorf("%s imports the cli package; cli deploys, never the reverse", path)
			case p == "swarmexec/client/internal/swarmfake" && !strings.HasSuffix(path, "_test.go"):
				t.Errorf("%s imports swarmfake outside a test", path)
			}
		}
	}
}
