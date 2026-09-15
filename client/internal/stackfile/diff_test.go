// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"strings"
	"testing"
)

func stackOf(services map[string]*Service) *Stack {
	return &Stack{Name: "demo", Services: services}
}

// Direction is the thing that must not be wrong: a "+" is what deploying the
// file would ADD. Reading the output backwards makes every conclusion drawn
// from it wrong, and nothing in the text itself would give that away.
func TestDiffDirectionIsWhatTheFileWouldDo(t *testing.T) {
	deployed := stackOf(map[string]*Service{"web": {Image: "nginx:1.25"}})
	file := stackOf(map[string]*Service{"web": {Image: "nginx:1.27"}})

	d, err := Compare(deployed, file, "web.yml")
	if err != nil {
		t.Fatal(err)
	}
	if d.Empty() {
		t.Fatal("an image change must not compare equal")
	}
	txt := d.Text()
	if !strings.Contains(txt, "+    image: nginx:1.27") {
		t.Errorf("the file's value should be the addition:\n%s", txt)
	}
	if !strings.Contains(txt, "-    image: nginx:1.25") {
		t.Errorf("the deployed value should be the removal:\n%s", txt)
	}
}

// Two identical stacks must produce nothing at all, or the tool is noise.
func TestIdenticalStacksDiffEmpty(t *testing.T) {
	mk := func() *Stack {
		return stackOf(map[string]*Service{
			"web": {Image: "nginx:1.27", Environment: map[string]string{"B": "2", "A": "1"}},
			"db":  {Image: "postgres:16"},
		})
	}
	d, err := Compare(mk(), mk(), "web.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Empty() {
		t.Errorf("identical stacks differ:\n%s", d.Text())
	}
	if !strings.Contains(d.Text(), "no differences") {
		t.Errorf("an empty diff should say so:\n%s", d.Text())
	}
}

// Whatever neither side can see has to travel with the verdict. "No
// differences" plus a silent omission is how a comparison tool misleads.
func TestNotComparedTravelsWithACleanDiff(t *testing.T) {
	a := stackOf(map[string]*Service{"web": {Image: "nginx"}})
	a.Notes = []string{"Secrets are declared external."}
	b := stackOf(map[string]*Service{"web": {Image: "nginx"}})

	d, err := Compare(a, b, "web.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Empty() {
		t.Fatalf("expected a clean diff, got:\n%s", d.Text())
	}
	if !strings.Contains(d.Text(), "Not compared:") || !strings.Contains(d.Text(), "Secrets are declared external.") {
		t.Errorf("a clean diff must carry its caveats:\n%s", d.Text())
	}
}

// A service only one side has must appear whole, not as a scattering of lines.
func TestAddedServiceShowsAsAnAddition(t *testing.T) {
	deployed := stackOf(map[string]*Service{"web": {Image: "nginx"}})
	file := stackOf(map[string]*Service{"web": {Image: "nginx"}, "cache": {Image: "redis:7"}})

	d, err := Compare(deployed, file, "web.yml")
	if err != nil {
		t.Fatal(err)
	}
	txt := d.Text()
	if !strings.Contains(txt, "+  cache:") || !strings.Contains(txt, "+    image: redis:7") {
		t.Errorf("a new service should show as added lines:\n%s", txt)
	}
	if strings.Contains(txt, "-  web:") {
		t.Errorf("the unchanged service must not be rewritten:\n%s", txt)
	}
}

// The rendering is one side of a diff, so the same stack has to render the same
// way every time — Go map order is random and would otherwise be the loudest
// thing in the output.
func TestRenderingIsDeterministic(t *testing.T) {
	st := stackOf(map[string]*Service{
		"web": {Environment: map[string]string{"Z": "1", "A": "2", "M": "3"}, Labels: map[string]string{"b": "1", "a": "2"}},
		"db":  {Image: "postgres:16"},
	})
	first, err := st.YAML()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := st.YAML()
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("rendering %d differs from the first:\n%s\n---\n%s", i, first, again)
		}
	}
}

// Compose interpolates "$" when a file is LOADED, so an exported value carrying
// a literal dollar — an entrypoint doing $(cat /run/secrets/…) — comes back
// mangled or fails outright. Found by feeding a real export back into the diff.
//
// The escaping belongs to the written document only: both sides of a diff go
// through YAML(), and a file the operator wrote has already been interpolated,
// so escaping there would compare "$$" against "$".
func TestOnlyTheWrittenDocumentEscapesDollars(t *testing.T) {
	st := stackOf(map[string]*Service{
		"db": {Entrypoint: []string{"sh", "-c", "export P=$(cat /run/secrets/pw)"}},
	})

	doc, err := st.Document()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, "$$(cat /run/secrets/pw)") {
		t.Errorf("the written file must escape the dollar:\n%s", doc)
	}

	raw, err := st.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "$$") {
		t.Errorf("the diff rendering must NOT escape, or it compares $$ against $:\n%s", raw)
	}
}

// Escaping a key would rename an environment variable, so it is values only.
func TestDollarEscapingLeavesKeysAlone(t *testing.T) {
	st := stackOf(map[string]*Service{
		"web": {Environment: map[string]string{"WEIRD$NAME": "a$b"}},
	})
	doc, err := st.Document()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, "WEIRD$NAME:") {
		t.Errorf("the key must survive unescaped:\n%s", doc)
	}
	if !strings.Contains(doc, "a$$b") {
		t.Errorf("the value must be escaped:\n%s", doc)
	}
}

// The exported file has to say what it is not, because it is the artefact that
// gets kept and mailed long after the terminal output is gone.
func TestDocumentCarriesItsNotes(t *testing.T) {
	st := stackOf(map[string]*Service{"web": {Image: "nginx"}})
	st.Notes = []string{"Secrets are declared external: the engine never returns a secret's value."}
	doc, err := st.Document()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, "# stack: demo") {
		t.Errorf("the file should name its stack:\n%s", doc)
	}
	if !strings.Contains(doc, "# Secrets are declared external") {
		t.Errorf("the notes must be in the file, not only on the terminal:\n%s", doc)
	}
}
