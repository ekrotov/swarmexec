// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The MCP server reads the cluster through swarmops, logstream, agentcall and
// stackfile; the cli only wires it up. Importing the cli back — or the TUI
// toolkit — would make the server depend on the whole front end again, so it
// is checked here. Test files included: a fake that needs the cli is the same
// leak. The fake swarm is for tests only.
func TestMCPStaysBelowTheCLI(t *testing.T) {
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
				t.Errorf("%s imports %s; the MCP server has no screen", path, p)
			case p == "swarmexec/client/internal/cli":
				t.Errorf("%s imports the cli package; cli wires mcp, never the reverse", path)
			case p == "swarmexec/client/internal/dial":
				t.Errorf("%s imports dial; agent connections belong to agentcall", path)
			case p == "swarmexec/client/internal/swarmfake" && !strings.HasSuffix(path, "_test.go"):
				t.Errorf("%s imports swarmfake outside a test", path)
			}
		}
	}
}
