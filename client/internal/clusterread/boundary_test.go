// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package clusterread

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// clusterread is what a cluster reports: the topology read both front ends
// render, per-node stats, node endpoints and their duplicates, the nodes
// themselves, the overview and the security gathering. It is the one client
// layer that asks BOTH the manager and the node agents, which is why it sits
// above swarmops rather than inside it.
//
// It still has no screen, and nothing below it may reach back up.
func TestClusterreadStaysBelowTheCLI(t *testing.T) {
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
				t.Errorf("%s imports %s; a reading has no screen", path, p)
			case p == "swarmexec/client/internal/cli":
				t.Errorf("%s imports the cli package; cli reads, never the reverse", path)
			case p == "swarmexec/client/internal/dial":
				t.Errorf("%s imports dial; agent connections belong to agentcall", path)
			case p == "swarmexec/client/internal/swarmfake" && !strings.HasSuffix(path, "_test.go"):
				t.Errorf("%s imports swarmfake outside a test", path)
			}
		}
	}
}
