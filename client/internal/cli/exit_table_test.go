// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The exit-code table on the site is generated from exitTable. This fails the
// day someone edits one and not the other (CLI-03).
func TestExitTableMatchesSitePage(t *testing.T) {
	path := filepath.Join("..", "..", "..", "site", "docs", "configuration.html")
	page, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := ApplyExitTable(string(page))
	if err != nil {
		t.Fatal(err)
	}
	if want != string(page) {
		t.Fatal("site/docs/configuration.html exit-code table differs from exitTable: run `make cli-docs`")
	}
	for _, c := range []int{exitOK, exitNo, exitUsage, exitTransport, exitHangup, exitInterrupted, exitTerminated} {
		if !strings.Contains(string(page), "<code>"+strconv.Itoa(c)+"</code>") {
			t.Errorf("exit code %d is not in the documented table", c)
		}
	}
}

// Every exit code in the command code is one of the table's names, never a
// bare number: a command cannot invent a code the table does not document.
func TestCommandsUseOnlyTableCodes(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.KeyValueExpr:
					if id, ok := x.Key.(*ast.Ident); ok && id.Name == "code" {
						checked++
						if _, bare := x.Value.(*ast.BasicLit); bare {
							t.Errorf("%s: bare exit code %v; use a name from the exit table", fset.Position(x.Pos()), x.Value)
						}
					}
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "failed" && len(x.Args) > 0 {
						checked++
						if _, bare := x.Args[0].(*ast.BasicLit); bare {
							t.Errorf("%s: bare exit code in failed(); use a name from the exit table", fset.Position(x.Pos()))
						}
					}
				}
				return true
			})
		}
	}
	if checked < 50 {
		t.Fatalf("only %d code sites found; the scan is broken", checked)
	}
}

// A runtime error that is not a cliError used to fall into the usage bucket
// (exit 2), whatever the command. It is a runtime failure (125); what cobra
// itself rejects stays 2.
func TestUnclassifiedRuntimeErrorIsNotUsage(t *testing.T) {
	root := newRootCmd(&globalFlags{}, Version{})
	root.AddCommand(&cobra.Command{Use: "boom", RunE: func(*cobra.Command, []string) error {
		return errors.New("json write failed")
	}})
	runtimeErrors(root)
	root.SetArgs([]string{"boom"})
	err := root.Execute()
	var sink strings.Builder
	if code := reportError(&sink, err, &globalFlags{}); code != exitTransport {
		t.Errorf("runtime error: exit %d, want %d", code, exitTransport)
	}
	for _, args := range [][]string{{"--nope"}, {"ps", "--nope"}, {"exec"}} {
		fresh := newRootCmd(&globalFlags{}, Version{})
		fresh.SetArgs(args)
		fresh.SetOut(&strings.Builder{})
		fresh.SetErr(&strings.Builder{})
		if code := reportError(&sink, fresh.Execute(), &globalFlags{}); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

// Declining a confirmation is the same answer everywhere: 1, "aborted".
func TestDeclinedConfirmationIsExitNo(t *testing.T) {
	g := cliCluster(t, allAgentsDownSwarm(), nil)
	_, _, err := runCmd(t, newDownCmd(g))
	if err == nil || exitCode(err) != exitNo || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("down: err = %v (code %d), want aborted with %d", err, exitCode(err), exitNo)
	}
}

// secret rotate promised 125 for an unreachable manager and ended with 1.
func TestSecretRotateUnreachableManagerIs125(t *testing.T) {
	g := cliCluster(t, allAgentsDownSwarm(), nil)
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	f := filepath.Join(t.TempDir(), "v")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCmd(t, newSecretRotateCmd(g), "db", "--from-file", f, "--yes")
	if err == nil || exitCode(err) != exitTransport {
		t.Fatalf("err = %v (code %d), want %d", err, exitCode(err), exitTransport)
	}
}
