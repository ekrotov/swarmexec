// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package clidocs renders the cobra command tree as the site's CLI reference
// page (site/docs/cli.html, served at /docs/cli; review W-05). The help text
// and the reference are then one source: `make cli-docs` regenerates the page
// and `make cli-docs-check` (CI: test) fails when the checked-in copy differs
// from what the current tree renders.
//
// The page frame matches the hand-written docs pages: the top bar and the
// docs page list are read from site/partials (nav.html, docnav.html) and
// marked the way site/nav.sh marks them, so `nav.sh check` passes on the
// generated page as on every other one.
package clidocs

import (
	"bytes"
	_ "embed"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// RenamedAnnotation marks a hidden command or flag as the old name of
// something that was renamed; its value is what to use now. The reference
// lists these in one table (the names still work, which a hidden entry would
// otherwise never tell anyone), derived from the tree so it cannot drift.
const RenamedAnnotation = "swarmexec/renamed-to"

// PageName is the docs topic the reference is served as (/docs/cli).
const PageName = "cli"

//go:embed page.html.tmpl
var pageTemplate string

var tmpl = template.Must(template.New("page").Parse(pageTemplate))

// Page is the data the template renders.
type Page struct {
	Nav, DocNav template.HTML // the partials, already marked for this page
	Commands    []Command
	Global      []Flag // the root's persistent flags, connection group aside
	Connection  []Flag
	Renamed     []Renamed
}

// Renamed is an old name that still works, and what replaced it.
type Renamed struct {
	Old, New string
}

// Command is one section of the page.
type Command struct {
	Path     string // "swarmexec stack deploy"
	Anchor   string // "stack-deploy"
	Depth    int    // 0 for the root, 1 for `swarmexec stack`, …
	Short    string
	Usage    []string
	Aliases  []string
	Long     []Block
	Example  string
	Flags    []Flag
	Subs     []Ref
	Topic    bool // a help topic (`swarmexec help connection`), not a command
	Inherits bool // takes the global flags
}

// Ref links to another section.
type Ref struct {
	Path, Anchor, Short string
}

// Block is a piece of help text: a paragraph, preformatted lines or a list.
type Block struct {
	Kind  string // "p", "label", "pre", "ul"
	Text  string
	Items []string
}

// Flag is one row of a flags table.
type Flag struct {
	Name, Default, Usage string
}

// Render writes the reference page for root to w. nav and docnav are the
// contents of site/partials/nav.html and site/partials/docnav.html.
func Render(w io.Writer, root *cobra.Command, nav, docnav string) error {
	p := Page{
		Nav:    template.HTML(markNav(nav)),       // #nosec G203 -- our own partial
		DocNav: template.HTML(markDocNav(docnav)), // #nosec G203 -- our own partial
	}
	var conn []*pflag.Flag
	var global []*pflag.Flag
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		if isConnection(f) {
			conn = append(conn, f)
		} else {
			global = append(global, f)
		}
	})
	p.Global, p.Connection = flagRows(global), flagRows(conn)
	p.Commands = collect(root, 0)
	collectRenamed(root, &p.Renamed)
	return tmpl.Execute(w, p)
}

// RenderSite renders the reference for root with the partials read from the
// site directory siteDir and returns the page.
func RenderSite(siteDir string, root *cobra.Command) ([]byte, error) {
	nav, err := os.ReadFile(filepath.Join(siteDir, "partials", "nav.html"))
	if err != nil {
		return nil, err
	}
	docnav, err := os.ReadFile(filepath.Join(siteDir, "partials", "docnav.html"))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := Render(&buf, root, string(nav), string(docnav)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// markNav marks the Docs item of the top bar as the current section, the way
// site/nav.sh expects it on a docs topic page.
func markNav(nav string) string {
	return strings.TrimRight(strings.Replace(nav, `href="/docs"`, `href="/docs" aria-current="true"`, 1), "\n")
}

// markDocNav marks this page's entry in the docs page list.
func markDocNav(docnav string) string {
	href := `href="/docs/` + PageName + `"`
	return strings.TrimRight(strings.Replace(docnav, href, href+` aria-current="page"`, 1), "\n")
}

// connectionAnnotation is cli's flag-group annotation (cli.IsConnectionFlag);
// repeated here so a synthetic tree in a test can carry it too.
const connectionAnnotation = "swarmexec/flag-group"

func isConnection(f *pflag.Flag) bool {
	for _, g := range f.Annotations[connectionAnnotation] {
		if g == "connection" {
			return true
		}
	}
	return false
}

// listed reports whether c gets a section: every command the help lists,
// and the help topics; never a hidden or deprecated one, nor `help` itself.
func listed(c *cobra.Command) bool {
	if !c.HasParent() {
		return true
	}
	if c.Hidden || c.Deprecated != "" || c.Name() == "help" {
		return false
	}
	return c.IsAvailableCommand() || c.IsAdditionalHelpTopicCommand()
}

// collect walks the tree depth first, in the order the help lists commands.
func collect(c *cobra.Command, depth int) []Command {
	cmd := Command{
		Path:     displayPath(c),
		Anchor:   Anchor(c),
		Depth:    depth,
		Short:    c.Short,
		Aliases:  c.Aliases,
		Long:     textBlocks(c.Long),
		Example:  strings.TrimRight(dedent(c.Example), "\n"),
		Topic:    c.IsAdditionalHelpTopicCommand(),
		Inherits: c.HasParent() && c.HasAvailableInheritedFlags() && !c.IsAdditionalHelpTopicCommand(),
	}
	if c.Runnable() {
		cmd.Usage = append(cmd.Usage, c.UseLine())
	}
	if c.HasAvailableSubCommands() {
		cmd.Usage = append(cmd.Usage, c.CommandPath()+" [command]")
	}
	if c.HasParent() {
		// The root's own flags are its persistent ones, in their own tables.
		var local []*pflag.Flag
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if !f.Hidden {
				local = append(local, f)
			}
		})
		cmd.Flags = flagRows(local)
	} else {
		c.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			if !f.Hidden {
				cmd.Flags = append(cmd.Flags, flagRow(f))
			}
		})
	}
	var below []Command
	for _, sub := range c.Commands() {
		if !listed(sub) {
			continue
		}
		cmd.Subs = append(cmd.Subs, Ref{Path: displayPath(sub), Anchor: Anchor(sub), Short: sub.Short})
		below = append(below, collect(sub, depth+1)...)
	}
	return append([]Command{cmd}, below...)
}

// displayPath is how c is invoked: a help topic is "swarmexec help <topic>".
func displayPath(c *cobra.Command) string {
	if c.IsAdditionalHelpTopicCommand() && c.HasParent() {
		return c.Parent().CommandPath() + " help " + c.Name()
	}
	return c.CommandPath()
}

// Anchor is the id of c's section: its path without the binary name, words
// joined by "-" ("stack-deploy"); the root's is the binary name.
func Anchor(c *cobra.Command) string {
	if !c.HasParent() {
		return c.Name()
	}
	parts := strings.Fields(c.CommandPath())[1:]
	return strings.Join(parts, "-")
}

func flagRows(fs []*pflag.Flag) []Flag {
	rows := make([]Flag, 0, len(fs))
	for _, f := range fs {
		rows = append(rows, flagRow(f))
	}
	return rows
}

// flagRow renders a flag as the help does: "-n, --tail int".
func flagRow(f *pflag.Flag) Flag {
	varname, usage := pflag.UnquoteUsage(f)
	name := "--" + f.Name
	if f.Shorthand != "" && f.ShorthandDeprecated == "" {
		name = "-" + f.Shorthand + ", " + name
	}
	if varname != "" {
		name += " " + varname
	}
	def := f.DefValue
	switch def {
	case "", "[]", "<nil>":
		def = ""
	}
	return Flag{Name: name, Default: def, Usage: usage}
}

// textBlocks splits a command's Long text into paragraphs, lists and
// preformatted runs. Blank lines separate blocks; a run of indented lines is
// preformatted (tables, examples, flag lists), unless it continues a list
// item; lines starting with "- " or "* " are list items.
func textBlocks(s string) []Block {
	var b blockBuilder
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		b.line(strings.TrimRight(l, " \t"))
	}
	b.flush()
	return b.out
}

// blockBuilder collects the lines of the block being read.
type blockBuilder struct {
	out              []Block
	para, pre, items []string
}

func (b *blockBuilder) line(l string) {
	indented := l != "" && (l[0] == ' ' || l[0] == '\t')
	switch {
	case l == "" && len(b.pre) > 0:
		b.pre = append(b.pre, "") // a blank line inside a preformatted run
	case l == "":
		b.flush()
	case isBullet(l):
		b.open("ul")
		b.items = append(b.items, strings.TrimSpace(l[2:]))
	case indented && len(b.items) > 0 && len(b.pre) == 0:
		b.items[len(b.items)-1] += " " + strings.TrimSpace(l)
	case indented:
		b.open("pre")
		b.pre = append(b.pre, l)
	default:
		b.open("p")
		b.para = append(b.para, l)
	}
}

// open ends the block being read unless it is of the given kind.
func (b *blockBuilder) open(kind string) {
	if (kind != "p" && len(b.para) > 0) || (kind != "pre" && len(b.pre) > 0) || (kind != "ul" && len(b.items) > 0) {
		b.flush()
	}
}

// flush ends the block being read.
func (b *blockBuilder) flush() {
	if len(b.para) > 0 {
		b.out = append(b.out, paragraph(strings.Join(b.para, " ")))
		b.para = nil
	}
	if len(b.items) > 0 {
		b.out = append(b.out, Block{Kind: "ul", Items: b.items})
		b.items = nil
	}
	for len(b.pre) > 0 && b.pre[len(b.pre)-1] == "" {
		b.pre = b.pre[:len(b.pre)-1]
	}
	if len(b.pre) > 0 {
		b.out = append(b.out, Block{Kind: "pre", Text: dedent(strings.Join(b.pre, "\n"))})
		b.pre = nil
	}
}

// paragraph is a block of running text; a markdown heading in it (cobra's
// completion help has "#### Linux:") is a label.
func paragraph(text string) Block {
	if t := strings.TrimLeft(text, "#"); t != text && strings.HasPrefix(t, " ") {
		return Block{Kind: "label", Text: strings.TrimSpace(t)}
	}
	return Block{Kind: "p", Text: text}
}

func isBullet(l string) bool {
	return strings.HasPrefix(l, "- ") || strings.HasPrefix(l, "* ")
}

// dedent removes the indentation all non-blank lines of s share.
func dedent(s string) string {
	lines := strings.Split(s, "\n")
	min := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if min < 0 || n < min {
			min = n
		}
	}
	if min <= 0 {
		return s
	}
	for i, l := range lines {
		if len(l) >= min {
			lines[i] = l[min:]
		} else {
			lines[i] = strings.TrimLeft(l, " \t")
		}
	}
	return strings.Join(lines, "\n")
}

// collectRenamed gathers the annotated hidden commands and flags, in tree
// order. A renamed command is not descended into: its children carry the old
// name as a prefix and are renamed along with it.
func collectRenamed(c *cobra.Command, out *[]Renamed) {
	path := displayPath(c)
	each := func(fs *pflag.FlagSet) {
		fs.VisitAll(func(f *pflag.Flag) {
			if to := f.Annotations[RenamedAnnotation]; len(to) > 0 {
				*out = append(*out, Renamed{Old: path + " --" + f.Name, New: to[0]})
			}
		})
	}
	each(c.PersistentFlags())
	each(c.LocalNonPersistentFlags())
	for _, s := range c.Commands() {
		if to := s.Annotations[RenamedAnnotation]; to != "" {
			*out = append(*out, Renamed{Old: displayPath(s), New: to})
			continue
		}
		collectRenamed(s, out)
	}
}
