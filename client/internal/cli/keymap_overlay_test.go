// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// An action an overlay answers to may not be remapped onto one of that
// overlay's fixed keys: node_prune=M would prune from the tab and measure in
// the detail, where M is checked first. Free keys are still accepted, and an
// action no overlay answers to may take the same key.
func TestLoadKeybinds_OverlayFixedKeysAreRefused(t *testing.T) {
	km, warns := loadKeybinds(writeKeys(t, "node_prune: M\nnode_prune_schedule: l\ncontext_use: i\nforward_stop: i\n"))
	if km.NodePrune != 'P' || km.NodePruneSched != 'S' || km.CtxUse != 'u' || km.FwdStop != 'X' {
		t.Errorf("fixed overlay keys must keep the defaults, got %+v", km)
	}
	if len(warns) != 4 {
		t.Fatalf("want four warnings, got %v", warns)
	}
	for _, w := range warns {
		if !strings.Contains(w, "is a fixed key in the") {
			t.Errorf("unexpected warning %q", w)
		}
	}
	if !strings.Contains(strings.Join(warns, "\n"), "node detail") {
		t.Errorf("the warning should name the overlay, got %v", warns)
	}

	km, warns = loadKeybinds(writeKeys(t, "node_prune: Z\nvolume_new: M\n"))
	if km.NodePrune != 'Z' || km.VolNew != 'M' || len(warns) != 0 {
		t.Errorf("free keys must be accepted, got prune=%q new=%q warnings=%v", km.NodePrune, km.VolNew, warns)
	}
}

// uiFuncs parses the non-test ui*.go files and returns every func by name,
// methods as Recv.Name.
func uiFuncs(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	files, _ := filepath.Glob("ui*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				recv := fd.Recv.List[0].Type
				if st, ok := recv.(*ast.StarExpr); ok {
					recv = st.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			funcs[name] = fd
		}
	}
	return funcs
}

// keybindsFields maps each keybinds field name to its yaml action name.
func keybindsFields() map[string]string {
	fields := map[string]string{}
	for _, a := range keyActions {
		var k keybinds
		*a.field(&k) = 1
		v := reflect.ValueOf(k)
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).Int() == 1 {
				fields[v.Type().Field(i).Name] = a.name
			}
		}
	}
	return fields
}

// Every overlayKeysets entry lists exactly the rune literals its key handler
// checks (minus the reserved j/k), and its handler answers to every action it
// names — so a key added to an overlay without being declared fails here,
// not in front of an operator whose remap went dead.
func TestOverlayKeysetsMatchTheSource(t *testing.T) {
	funcs, fields := uiFuncs(t), keybindsFields()
	for _, o := range overlayKeysets {
		fd := funcs[o.handler]
		if fd == nil {
			t.Errorf("%s: handler %s not found", o.name, o.handler)
			continue
		}
		runes, actions := map[rune]bool{}, map[string]bool{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind != token.CHAR {
					break
				}
				// Control runes are text ('\n'), not keys.
				if s, err := strconv.Unquote(x.Value); err == nil && []rune(s)[0] >= ' ' && !reservedRunes[[]rune(s)[0]] {
					runes[[]rune(s)[0]] = true
				}
			case *ast.SelectorExpr:
				if a, ok := fields[x.Sel.Name]; ok {
					actions[a] = true
				}
			}
			return true
		})
		for _, r := range o.fixed {
			if !runes[r] {
				t.Errorf("%s declares %q, which its handler no longer checks", o.name, r)
			}
			delete(runes, r)
		}
		for r := range runes {
			t.Errorf("%s handles %q but does not declare it in overlayKeysets", o.name, r)
		}
		for _, a := range o.actions {
			if !actions[a] {
				t.Errorf("%s declares action %s, which its handler does not use", o.name, a)
			}
		}
	}
}
