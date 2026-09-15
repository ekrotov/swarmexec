// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"fmt"
	"strings"
)

// Diff is the comparison of a stack file against a deployed stack.
type Diff struct {
	FileLabel   string
	SwarmLabel  string
	Hunks       []Hunk
	Notes       []string
	fileLines   []string
	deployLines []string
}

// Empty reports whether deploying the file would change nothing this rendering
// can see. The qualifier is deliberate: see Notes for what it does not cover.
func (d *Diff) Empty() bool { return len(d.Hunks) == 0 }

// Hunk is one contiguous run of changes with its surrounding context, in the
// shape `diff -u` prints.
type Hunk struct {
	FileStart, FileCount     int
	DeployStart, DeployCount int
	Lines                    []DiffLine
}

// DiffLine is one line of a hunk. Op is ' ', '-' or '+'.
type DiffLine struct {
	Op   byte
	Text string
}

// Compare renders both stacks and diffs the results.
//
// Direction: the DEPLOYED stack is the "before" and the FILE is the "after", so
// a "+" line is something deploying the file would add and a "-" is something
// it would remove. That is the direction the question is asked in — "what would
// this file change?" — and getting it backwards makes every reading of the
// output wrong in a way that is hard to notice.
func Compare(deployed, file *Stack, fileLabel string) (*Diff, error) {
	before, err := deployed.YAML()
	if err != nil {
		return nil, fmt.Errorf("render deployed stack: %w", err)
	}
	after, err := file.YAML()
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", fileLabel, err)
	}
	d := &Diff{
		FileLabel:   fileLabel,
		SwarmLabel:  "deployed stack " + deployed.Name,
		fileLines:   splitLines(after),
		deployLines: splitLines(before),
	}
	d.Hunks = unified(d.deployLines, d.fileLines, 3)

	// Both sides' notes travel with the result. A clean diff means "nothing
	// differs in what this can see", and what it cannot see has to be attached
	// to that statement or the statement is misleading.
	seen := map[string]bool{}
	for _, n := range append(append([]string{}, deployed.Notes...), file.Notes...) {
		if !seen[n] {
			seen[n] = true
			d.Notes = append(d.Notes, n)
		}
	}
	return d, nil
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// Text renders the diff the way `git diff` does, so it can be read, pasted into
// a review, or piped into anything that understands a unified diff.
func (d *Diff) Text() string {
	var b strings.Builder
	if d.Empty() {
		fmt.Fprintf(&b, "no differences between %s and %s\n", d.FileLabel, d.SwarmLabel)
		d.writeNotes(&b)
		return b.String()
	}
	fmt.Fprintf(&b, "--- %s\n", d.SwarmLabel)
	fmt.Fprintf(&b, "+++ %s\n", d.FileLabel)
	for _, h := range d.Hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.DeployStart, h.DeployCount, h.FileStart, h.FileCount)
		for _, l := range h.Lines {
			b.WriteByte(l.Op)
			b.WriteString(l.Text)
			b.WriteByte('\n')
		}
	}
	d.writeNotes(&b)
	return b.String()
}

func (d *Diff) writeNotes(b *strings.Builder) {
	if len(d.Notes) == 0 {
		return
	}
	b.WriteString("\nNot compared:\n")
	for _, n := range d.Notes {
		fmt.Fprintf(b, "  - %s\n", n)
	}
}

// unified builds hunks from two line sequences, with `ctx` lines of context.
//
// The matching is a plain LCS. The inputs are two renderings of the same stack,
// so they are already close to each other and the quadratic table is cheap;
// reaching for a smarter algorithm would buy nothing a human could measure on
// files this size.
func unified(a, b []string, ctx int) []Hunk {
	ops := lcsOps(a, b)
	if len(ops) == 0 {
		return nil
	}

	// Which positions in the op list are changes, so context can be measured
	// around them.
	changed := make([]bool, len(ops))
	any := false
	for i, o := range ops {
		if o.Op != ' ' {
			changed[i] = true
			any = true
		}
	}
	if !any {
		return nil
	}

	var hunks []Hunk
	i := 0
	for i < len(ops) {
		if !changed[i] {
			i++
			continue
		}
		start := max(0, i-ctx)
		end := i
		// Extend while another change is within 2*ctx — closer than that and two
		// hunks would share context lines, which reads worse than one hunk.
		for end < len(ops) {
			next := -1
			for j := end; j < len(ops) && j <= end+2*ctx; j++ {
				if changed[j] {
					next = j
				}
			}
			if next < 0 {
				break
			}
			end = next + 1
		}
		stop := min(len(ops), end+ctx)

		h := Hunk{DeployStart: 0, FileStart: 0}
		// Line numbers are 1-based and count only the lines each side has.
		aLine, bLine := 1, 1
		for k := 0; k < start; k++ {
			switch ops[k].Op {
			case ' ':
				aLine++
				bLine++
			case '-':
				aLine++
			case '+':
				bLine++
			}
		}
		h.DeployStart, h.FileStart = aLine, bLine
		for k := start; k < stop; k++ {
			o := ops[k]
			h.Lines = append(h.Lines, DiffLine{Op: o.Op, Text: o.Text})
			switch o.Op {
			case ' ':
				h.DeployCount++
				h.FileCount++
			case '-':
				h.DeployCount++
			case '+':
				h.FileCount++
			}
		}
		hunks = append(hunks, h)
		i = stop
	}
	return hunks
}

// lcsOps turns two sequences into a single op list: ' ' common, '-' only in a,
// '+' only in b.
func lcsOps(a, b []string) []DiffLine {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	// table[i][j] = length of the longest common subsequence of a[i:] and b[j:].
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else {
				table[i][j] = max(table[i+1][j], table[i][j+1])
			}
		}
	}
	var out []DiffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, DiffLine{Op: ' ', Text: a[i]})
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			out = append(out, DiffLine{Op: '-', Text: a[i]})
			i++
		default:
			out = append(out, DiffLine{Op: '+', Text: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, DiffLine{Op: '-', Text: a[i]})
	}
	for ; j < m; j++ {
		out = append(out, DiffLine{Op: '+', Text: b[j]})
	}
	return out
}
