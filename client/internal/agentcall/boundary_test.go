// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every agent connection goes through this package — that is what lets one
// place own their lifetime and know which agent supports what. A new
// dial.Dial elsewhere would quietly bypass both, so the boundary is checked:
// outside agentcall (and dial itself), nothing in the client imports dial, and
// nothing imports tview here.
func TestOnlyAgentcallDialsAgents(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		dir := filepath.Base(filepath.Dir(path))
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch {
			case p == "swarmexec/client/internal/dial" && dir != "agentcall" && dir != "dial":
				t.Errorf("%s imports dial; agent connections belong to agentcall", path)
			case dir == "agentcall" && strings.Contains(p, "tview"):
				t.Errorf("%s imports %s; agentcall must stay free of the TUI", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
