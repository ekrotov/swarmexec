// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// swarmops is the manager-side logic without a screen. The moment it imports
// the TUI toolkit or the cli package, the layering is back to convention: the
// data layer could call into the presentation, and a test of a service edit
// would link the whole TUI again. Both directions are checked here, tests
// included — a fake that needs tview is the same leak.
func TestSwarmopsStaysBelowTheCLI(t *testing.T) {
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
				t.Errorf("%s imports %s; swarmops must stay free of the TUI", path, p)
			case p == "swarmexec/client/internal/cli":
				t.Errorf("%s imports the cli package; cli calls swarmops, never the reverse", path)
			case p == "swarmexec/client/internal/dial":
				t.Errorf("%s imports dial; agent connections belong to agentcall", path)
			case p == "swarmexec/client/internal/agentcall" && !strings.HasSuffix(path, "_test.go"):
				// Six files used to. That is what made "manager-side logic"
				// false for a third of the package and put the cluster
				// readings next to the service editors; they are in
				// clusterread now, which imports this package and not the
				// other way round.
				t.Errorf("%s imports agentcall; anything that asks a node agent belongs in clusterread", path)
			case p == "swarmexec/client/internal/clusterread":
				t.Errorf("%s imports clusterread; clusterread reads through swarmops, never the reverse", path)
			}
		}
	}
}
