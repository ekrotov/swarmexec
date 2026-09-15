// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package secscan

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// A report is written to a file and read later, often by someone who was not
// at the terminal when it was produced. That changes what it has to say. On
// screen the operator knows what they were looking at; in a file the report has
// to carry its own context — which cluster, when, with which build, what was
// examined, and just as importantly what was NOT reachable. A report that omits
// its gaps reads as an all-clear for ground it never covered.

// ReportMeta is the context a report carries about its own production.
type ReportMeta struct {
	Context   string    // docker context / cluster the scan ran against
	Generated time.Time // zero means "do not state a time"
	Tool      string    // swarmexec build that produced it
}

// ServiceScan is one service and what the service analyzers found on it. The
// caller supplies the findings rather than the spec, so the ui can reuse the
// scan it already has instead of running every analyzer a second time.
type ServiceScan struct {
	Name     string
	Findings []Finding
}

// Group is every finding about one subject.
type Group struct {
	Subject  Subject
	Findings []Finding
}

// Max is the worst severity in the group.
func (g Group) Max() Severity { return MaxSeverity(g.Findings) }

// Counts is a tally by severity.
type Counts struct{ High, Medium, Low int }

// Total is the number of findings counted.
func (c Counts) Total() int { return c.High + c.Medium + c.Low }

func (c *Counts) add(s Severity) {
	switch s {
	case SevHigh:
		c.High++
	case SevMedium:
		c.Medium++
	default:
		c.Low++
	}
}

// Report is a finished scan: the cluster groups, the service groups, the
// tallies, and the gaps.
type Report struct {
	Meta ReportMeta

	Cluster  []Group // networks, nodes, secrets, configs, the swarm itself
	Services []Group // one per service that has findings

	Totals Counts

	ServiceCount int // services scanned, including those with no findings
	NetworkCount int
	SecretCount  int
	ConfigCount  int
	NodeCount    int

	// Gaps are the things this report could not cover. They are not findings —
	// nothing is wrong — but leaving them out would let the report be read as
	// covering ground it never reached.
	Gaps []string
}

// BuildReport assembles a report from an already-scanned service set and a
// cluster. It runs the cluster analyzers; the service analyzers have already
// run by the time their findings arrive here.
func BuildReport(meta ReportMeta, svcs []ServiceScan, c Cluster) Report {
	r := Report{
		Meta:         meta,
		ServiceCount: len(svcs),
		NetworkCount: len(c.Networks),
		SecretCount:  len(c.Secrets),
		ConfigCount:  len(c.Configs),
		NodeCount:    len(c.Nodes),
	}

	// Cluster findings, grouped by subject and ordered worst first.
	bySubject := map[Subject][]Finding{}
	var order []Subject
	for _, sf := range ScanCluster(c) {
		if _, seen := bySubject[sf.Subject]; !seen {
			order = append(order, sf.Subject)
		}
		bySubject[sf.Subject] = append(bySubject[sf.Subject], sf.Finding)
	}
	for _, s := range order {
		r.Cluster = append(r.Cluster, Group{Subject: s, Findings: bySubject[s]})
	}
	sortGroups(r.Cluster)

	for _, s := range svcs {
		if len(s.Findings) == 0 {
			continue
		}
		r.Services = append(r.Services, Group{
			Subject:  Subject{Kind: SubjectService, Name: s.Name},
			Findings: s.Findings,
		})
	}
	sortGroups(r.Services)

	for _, g := range append(append([]Group{}, r.Cluster...), r.Services...) {
		for _, f := range g.Findings {
			r.Totals.add(f.Severity)
		}
	}

	if !c.AgentsChecked {
		r.Gaps = append(r.Gaps, "The node agents were not queried, so agent version skew was not checked.")
	}
	if c.Swarm == nil {
		r.Gaps = append(r.Gaps, "The swarm configuration could not be read; manager autolock is reported as unknown.")
	}
	if len(c.Nodes) == 0 {
		r.Gaps = append(r.Gaps, "No nodes were listed, so nothing node-specific was checked.")
	}
	return r
}

// sortGroups orders groups worst-first, then by name, so the reader meets the
// findings that matter before the housekeeping. Ties break on the name alone,
// which keeps the ordering of two scans of an unchanged cluster identical.
func sortGroups(gs []Group) {
	sort.SliceStable(gs, func(i, j int) bool {
		a, b := gs[i], gs[j]
		if am, bm := a.Max(), b.Max(); am != bm {
			return am > bm
		}
		if a.Subject.Kind != b.Subject.Kind {
			return a.Subject.Kind < b.Subject.Kind
		}
		return a.Subject.Name < b.Subject.Name
	})
}

// Markdown renders the report. Plain CommonMark with pipe tables: it has to
// read acceptably as raw text in a terminal as well as rendered in a browser,
// because that is how a file like this actually gets looked at.
func (r Report) Markdown() string {
	var b strings.Builder

	title := "Security report"
	if r.Meta.Context != "" {
		title += " — " + mdText(r.Meta.Context)
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	var provenance []string
	if !r.Meta.Generated.IsZero() {
		provenance = append(provenance, "Generated "+r.Meta.Generated.UTC().Format(time.RFC3339))
	}
	if r.Meta.Tool != "" {
		provenance = append(provenance, "by "+mdText(r.Meta.Tool))
	}
	if len(provenance) > 0 {
		fmt.Fprintf(&b, "%s.\n\n", strings.Join(provenance, " "))
	}
	fmt.Fprintf(&b, "Scanned %s, %s, %s, %s and %s.\n\n",
		plural(r.ServiceCount, "service", "services"),
		plural(r.NetworkCount, "network", "networks"),
		plural(r.SecretCount, "secret", "secrets"),
		plural(r.ConfigCount, "config", "configs"),
		plural(r.NodeCount, "node", "nodes"))

	r.writeSummary(&b)
	r.writeGaps(&b)
	r.writeGroups(&b, "Cluster", r.Cluster,
		"Nothing was found at the cluster level.")
	r.writeGroups(&b, "Services", r.Services,
		"No service was flagged.")
	r.writeChecks(&b)
	return b.String()
}

func (r Report) writeSummary(b *strings.Builder) {
	b.WriteString("## Summary\n\n")
	if r.Totals.Total() == 0 {
		// Say what was examined, or "nothing found" is a claim with no weight
		// behind it. The check list follows at the end of the report.
		b.WriteString("No findings. See **What was checked** below for what that covers.\n\n")
		return
	}
	b.WriteString("| Severity | Findings |\n|---|---:|\n")
	for _, row := range []struct {
		name string
		n    int
	}{{"high", r.Totals.High}, {"medium", r.Totals.Medium}, {"low", r.Totals.Low}} {
		fmt.Fprintf(b, "| %s | %d |\n", row.name, row.n)
	}
	fmt.Fprintf(b, "| **total** | **%d** |\n\n", r.Totals.Total())

	// Low findings hold for almost every service (an unset user is the Swarm
	// default), so a bare total would read as alarming noise. Name what is
	// actually actionable.
	act := r.Totals.High + r.Totals.Medium
	switch act {
	case 0:
		b.WriteString("Nothing above **low**. Low findings are informational and true of most services.\n\n")
	default:
		fmt.Fprintf(b, "**%d** finding(s) above low, across %d subject(s).\n\n", act, r.actionableSubjects())
	}
}

func (r Report) actionableSubjects() int {
	n := 0
	for _, g := range append(append([]Group{}, r.Cluster...), r.Services...) {
		if g.Max() > SevLow {
			n++
		}
	}
	return n
}

func (r Report) writeGaps(b *strings.Builder) {
	if len(r.Gaps) == 0 {
		return
	}
	b.WriteString("## Not covered\n\n")
	for _, g := range r.Gaps {
		fmt.Fprintf(b, "- %s\n", mdText(g))
	}
	b.WriteString("\n")
}

func (r Report) writeGroups(b *strings.Builder, heading string, gs []Group, empty string) {
	fmt.Fprintf(b, "## %s\n\n", heading)
	if len(gs) == 0 {
		fmt.Fprintf(b, "%s\n\n", empty)
		return
	}
	for _, g := range gs {
		fmt.Fprintf(b, "### %s `%s`\n\n", titleWord(string(g.Subject.Kind)), mdCode(g.Subject.Name))
		b.WriteString("| Severity | Check | Detail |\n|---|---|---|\n")
		for _, f := range g.Findings {
			fmt.Fprintf(b, "| %s | %s | %s |\n",
				f.Severity, mdCell(f.Title), mdCell(f.Detail))
		}
		b.WriteString("\n")
	}
}

func (r Report) writeChecks(b *strings.Builder) {
	b.WriteString("## What was checked\n\n")
	write := func(label string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(b, "**%s**\n\n", label)
		for _, c := range items {
			fmt.Fprintf(b, "- %s\n", mdText(c))
		}
		b.WriteString("\n")
	}
	write("Per service", Checks())
	write("Cluster-wide", ClusterChecks())
}

// mdCell makes a string safe inside a pipe-table cell. Two things can break the
// table: a literal pipe, and a newline. Neither is hypothetical — a finding's
// detail can quote an environment variable name, and Docker puts no
// restrictions on those at all, while a dial error arrives with line breaks in
// it.
func mdCell(s string) string {
	return strings.ReplaceAll(oneLine(s), "|", `\|`)
}

// mdText escapes the few characters that would turn body text into markup.
// Deliberately minimal: over-escaping makes the raw file harder to read, and
// the raw file is half the point.
func mdText(s string) string {
	r := strings.NewReplacer("*", `\*`, "_", `\_`, "`", "\\`", "|", `\|`)
	return r.Replace(oneLine(s))
}

// mdCode escapes a value going inside backticks. A name containing a backtick
// would otherwise end the span and let the rest of the line become markup;
// Docker object names cannot contain one today, but the report must not depend
// on that staying true.
func mdCode(s string) string {
	return strings.ReplaceAll(oneLine(s), "`", "'")
}

// titleWord capitalises an ASCII subject kind for a heading. strings.Title is
// deprecated and does far more than this needs; the kinds are our own constants.
func titleWord(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
