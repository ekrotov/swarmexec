// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package endpoint

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// endpoint is the bottom of the client: everything that reaches a cluster goes
// through it, and it reaches nothing itself. That is what lets the MCP server,
// the commands and the TUI share one resolution instead of three — before this
// package existed, mcp was handed a dialer closure because it was not allowed
// to import the package the dialer lived in.
//
// The allowed dependencies are listed rather than the forbidden ones: a layer
// at the bottom grows upwards one innocent-looking import at a time, and the
// next one will look innocent too.
func TestEndpointStaysAtTheBottom(t *testing.T) {
	allowed := map[string]bool{
		"swarmexec/client/internal/clientlog": true, // logging, no cluster
		"swarmexec/client/internal/dockerctx": true, // reads docker's contexts
		"swarmexec/client/internal/term":      true, // the terminal prompt
	}
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
				t.Errorf("%s imports %s; the endpoint layer has no screen", path, p)
			case strings.HasPrefix(p, "swarmexec/") && !allowed[p]:
				t.Errorf("%s imports %s; everything reaches a cluster through this package, "+
					"so it must not reach back up — add it to the allow list only with a reason", path, p)
			}
		}
	}
}
