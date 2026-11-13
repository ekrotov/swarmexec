// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dial"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
	"swarmexec/internal/prunesched"
)

// Disk cleanup on a node: what a prune would free there (the agent measures it
// hourly) and the prune itself, per category. Like images, all of it is
// node-local and invisible to the manager, so it goes to each agent directly.

const (
	reclaimTimeout      = 15 * time.Second
	reclaimMeasureLimit = 3 * time.Minute // measure_now runs a full measurement
	nodePruneTimeout    = 10 * time.Minute
)

// pruneCat describes one category for the views: how it is named, whether it
// is the dangerous one, and what it means — in one table so the dialog, the
// confirm text and the result cannot describe a category three ways.
type pruneCat struct {
	cat    pb.PruneCategory
	label  string
	danger bool
}

// pruneCats is the display order, the same order the agent runs them.
var pruneCats = []pruneCat{
	{pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS, "stopped containers", false},
	{pb.PruneCategory_PRUNE_CATEGORY_IMAGES, "every unused image", true},
	{pb.PruneCategory_PRUNE_CATEGORY_DANGLING_IMAGES, "untagged images", false},
	{pb.PruneCategory_PRUNE_CATEGORY_BUILD_CACHE, "build cache", false},
	{pb.PruneCategory_PRUNE_CATEGORY_NETWORKS, "unused local networks", false},
	{pb.PruneCategory_PRUNE_CATEGORY_VOLUMES, "unused volumes", true},
}

func catLabel(c pb.PruneCategory) string {
	for _, pc := range pruneCats {
		if pc.cat == c {
			return pc.label
		}
	}
	return c.String()
}

func fetchReclaim(ctx context.Context, cfg config.Config, n resolve.Node, measureNow bool, connectTimeout time.Duration) (*pb.ReclaimStatsResponse, error) {
	timeout := max(connectTimeout, reclaimTimeout)
	if measureNow {
		timeout = max(timeout, reclaimMeasureLimit)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dial.Dial(cctx, n.DialHost, cfg.Port, cfg)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := pb.NewAgentClient(conn).ReclaimStats(cctx, &pb.ReclaimStatsRequest{MeasureNow: measureNow})
	if err != nil {
		return nil, wrapGRPC(ctx, cfg, err)
	}
	return resp, nil
}

func pruneNode(ctx context.Context, cfg config.Config, n resolve.Node, req *pb.PruneRequest, connectTimeout time.Duration) (*pb.PruneResponse, error) {
	cctx, cancel := context.WithTimeout(ctx, max(connectTimeout, nodePruneTimeout))
	defer cancel()
	conn, err := dial.Dial(cctx, n.DialHost, cfg.Port, cfg)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := pb.NewAgentClient(conn).Prune(cctx, req)
	if err != nil {
		return nil, wrapGRPC(ctx, cfg, err)
	}
	return resp, nil
}

// reclaimFigure finds one category in a stats response.
func reclaimFigure(resp *pb.ReclaimStatsResponse, c pb.PruneCategory) (*pb.ReclaimCategory, bool) {
	for _, rc := range resp.GetCategories() {
		if rc.GetCategory() == c {
			return rc, true
		}
	}
	return nil, false
}

// figureText renders "3 · 1.2GB", or "3" for a category without sizes.
func figureText(rc *pb.ReclaimCategory) string {
	if rc == nil {
		return "…"
	}
	if !rc.GetBytesKnown() {
		return fmt.Sprintf("%d", rc.GetCount())
	}
	return fmt.Sprintf("%d · %s", rc.GetCount(), diskSize(rc.GetBytes()))
}

// ago renders how long ago an RFC3339 time was, coarsely.
func ago(rfc string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return rfc
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// untilText renders the agent's Go duration ("24h0m0s") the way an operator
// writes it ("24h").
func untilText(d string) string {
	if dd, err := time.ParseDuration(d); err == nil {
		return prunesched.FormatUntil(dd)
	}
	return d
}

// reclaimSection is the node detail's "reclaimable" block, with the schedule
// and its last run. err is the fetch error; an agent too old for the RPC gets
// a line saying so instead of a block of zeros.
func reclaimSection(resp *pb.ReclaimStatsResponse, err error, loading bool, now time.Time) string {
	var b strings.Builder
	b.WriteString("\n  [gray]reclaimable (measured on the node)[-]\n")
	switch {
	case loading && resp == nil:
		b.WriteString("    [gray]asking the agent…[-]\n")
		return b.String()
	case errors.Is(err, errAgentTooOld):
		b.WriteString("    [gray]agent too old for prune stats — update the agent image[-]\n")
		return b.String()
	case err != nil && resp == nil:
		fmt.Fprintf(&b, "    [gray]unavailable: %s[-]\n", escapeTags(err.Error()))
		return b.String()
	}
	if resp.GetMeasuredAt() == "" {
		b.WriteString("    [gray]not measured yet — the agent measures shortly after it starts (m: now)[-]\n")
	} else {
		for _, pc := range pruneCats {
			rc, ok := reclaimFigure(resp, pc.cat)
			if !ok {
				continue
			}
			label := pc.label
			if pc.cat == pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS && resp.GetContainersUntil() != "" {
				label = fmt.Sprintf("stopped > %s", untilText(resp.GetContainersUntil()))
			}
			line := fmt.Sprintf("    %-22s %s", label, figureText(rc))
			if rc.GetHeldBack() > 0 {
				line += fmt.Sprintf("  [gray](%d more kept, stopped recently)[-]", rc.GetHeldBack())
			}
			b.WriteString(line + "\n")
		}
		when := ago(resp.GetMeasuredAt(), now)
		if loading {
			when += ", measuring again…"
		}
		fmt.Fprintf(&b, "    [gray]measured %s[-]\n", when)
		if resp.GetMeasureThrottled() {
			b.WriteString("    [gray]measured very recently — showing those figures[-]\n")
		}
		if e := resp.GetMeasureError(); e != "" {
			fmt.Fprintf(&b, "    [orange]last measurement incomplete: %s[-]\n", escapeTags(e))
		}
	}
	b.WriteString(scheduleSection(resp.GetSchedule(), resp.GetLastRun(), now))
	return b.String()
}

// scheduleSection renders the agent's own view of the prune schedule and its
// last run — the agent's, not the service spec's, so an agent that has not
// restarted yet shows what it will really do.
func scheduleSection(s *pb.PruneSchedule, last *pb.PruneRun, now time.Time) string {
	var b strings.Builder
	b.WriteString("\n  [gray]prune schedule (this node's agent)[-]\n")
	switch {
	case s == nil:
		b.WriteString("    [gray]unknown[-]\n")
	case s.GetError() != "":
		fmt.Fprintf(&b, "    [red]invalid labels, schedule off:[-] %s\n", escapeTags(s.GetError()))
	case !s.GetEnabled():
		b.WriteString("    off\n")
	default:
		var cats []string
		for _, c := range s.GetCategories() {
			cats = append(cats, catLabel(c))
		}
		fmt.Fprintf(&b, "    %s, %d× a day from %s %s\n", strings.Join(s.GetDays(), ","), s.GetTimesPerDay(), s.GetStart(), s.GetTz())
		fmt.Fprintf(&b, "    %s\n", strings.Join(cats, ", "))
		if nr := s.GetNextRun(); nr != "" {
			if t, err := time.Parse(time.RFC3339, nr); err == nil {
				fmt.Fprintf(&b, "    next run %s\n", t.Format("Mon 2006-01-02 15:04 MST"))
			}
		}
	}
	if last != nil && last.GetStartedAt() != "" {
		var freed int64
		var parts []string
		for _, r := range last.GetResults() {
			freed += r.GetFreedBytes()
			switch {
			case r.GetError() != "":
				parts = append(parts, fmt.Sprintf("%s [red]failed[-]", catLabel(r.GetCategory())))
			case r.GetSkipped():
				parts = append(parts, catLabel(r.GetCategory())+" skipped")
			default:
				parts = append(parts, fmt.Sprintf("%s %d", catLabel(r.GetCategory()), r.GetCount()))
			}
		}
		fmt.Fprintf(&b, "    last run %s: freed %s\n", ago(last.GetStartedAt(), now), diskSize(freed))
		if len(parts) > 0 {
			fmt.Fprintf(&b, "      [gray]%s[-]\n", strings.Join(parts, " · "))
		}
		if e := last.GetError(); e != "" {
			fmt.Fprintf(&b, "      [red]error:[-] %s\n", escapeTags(e))
		}
	}
	return b.String()
}

// diskSize renders bytes in the binary units the rest of the node detail
// uses (the image block above it), so the same layer store does not read as
// GiB in one block and GB in the next. Zero is "0 B", not blank.
func diskSize(b int64) string {
	if s := formatMemBytes(b); s != "" {
		return s
	}
	return "0 B"
}

// escapeTags keeps an agent-supplied string from being read as tview markup.
func escapeTags(s string) string { return strings.ReplaceAll(s, "[", "[[") }

// pruneChoice is what the operator ticked in the prune dialog.
type pruneChoice struct {
	ticked map[pb.PruneCategory]bool
	until  string // containers_until as typed; "" is the agent's 24h
}

// defaultPruneChoice ticks the safe sweeps. Two things are deliberately left
// unticked: every unused image (it removes images a service scaled to zero
// still needs — the contract says a client must not default to it) and
// volumes, which are data and only ever an explicit choice.
func defaultPruneChoice() pruneChoice {
	return pruneChoice{ticked: map[pb.PruneCategory]bool{
		pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS:      true,
		pb.PruneCategory_PRUNE_CATEGORY_DANGLING_IMAGES: true,
		pb.PruneCategory_PRUNE_CATEGORY_BUILD_CACHE:     true,
		pb.PruneCategory_PRUNE_CATEGORY_NETWORKS:        true,
	}}
}

// toggle flips a category. The two image choices exclude each other: every
// unused image already includes the untagged ones.
func (c *pruneChoice) toggle(cat pb.PruneCategory) {
	c.ticked[cat] = !c.ticked[cat]
	if c.ticked[cat] {
		switch cat {
		case pb.PruneCategory_PRUNE_CATEGORY_IMAGES:
			c.ticked[pb.PruneCategory_PRUNE_CATEGORY_DANGLING_IMAGES] = false
		case pb.PruneCategory_PRUNE_CATEGORY_DANGLING_IMAGES:
			c.ticked[pb.PruneCategory_PRUNE_CATEGORY_IMAGES] = false
		}
	}
}

// selectAll is "all": everything EXCEPT volumes, which stay as they are — an
// explicit, separate tick. Pressed again when everything is already ticked,
// it clears all but volumes.
func (c *pruneChoice) selectAll() {
	allOn := c.ticked[pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS] && c.ticked[pb.PruneCategory_PRUNE_CATEGORY_IMAGES] &&
		c.ticked[pb.PruneCategory_PRUNE_CATEGORY_BUILD_CACHE] && c.ticked[pb.PruneCategory_PRUNE_CATEGORY_NETWORKS]
	for _, pc := range pruneCats {
		if pc.cat == pb.PruneCategory_PRUNE_CATEGORY_VOLUMES {
			continue
		}
		c.ticked[pc.cat] = !allOn && pc.cat != pb.PruneCategory_PRUNE_CATEGORY_DANGLING_IMAGES
	}
}

func (c pruneChoice) categories() []pb.PruneCategory {
	var out []pb.PruneCategory
	for _, pc := range pruneCats {
		if c.ticked[pc.cat] {
			out = append(out, pc.cat)
		}
	}
	return out
}

func (c pruneChoice) request() *pb.PruneRequest {
	return &pb.PruneRequest{Categories: c.categories(), ContainersUntil: c.until}
}

// expected is what the ticked categories should free, from the last
// measurement. Every unused image includes the untagged ones, which toggle
// already guarantees are not both ticked.
func (c pruneChoice) expected(resp *pb.ReclaimStatsResponse) int64 {
	var sum int64
	for _, cat := range c.categories() {
		if rc, ok := reclaimFigure(resp, cat); ok && rc.GetBytesKnown() {
			sum += rc.GetBytes()
		}
	}
	return sum
}

// untilLabel is the containers age filter as the dialog shows it.
func (c pruneChoice) untilLabel() string {
	if c.until == "" {
		return "24h"
	}
	return c.until
}

// pruneConfirmBody is the last thing an operator reads before an irreversible
// act, so it is a pure function with its own test: every ticked category, what
// it means, and the volume warning whenever volumes are in it.
func pruneConfirmBody(node string, c pruneChoice, resp *pb.ReclaimStatsResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Prune on %q:\n\n", node)
	for _, cat := range c.categories() {
		rc, _ := reclaimFigure(resp, cat)
		label := catLabel(cat)
		if cat == pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS {
			label = fmt.Sprintf("stopped containers (stopped > %s)", c.untilLabel())
		}
		if cat == pb.PruneCategory_PRUNE_CATEGORY_IMAGES || cat == pb.PruneCategory_PRUNE_CATEGORY_VOLUMES {
			label = "[red]" + label + "[white]"
		}
		fmt.Fprintf(&b, "  • %s — %s\n", label, figureText(rc))
	}
	if exp := c.expected(resp); exp > 0 {
		fmt.Fprintf(&b, "\nFrees about [green]%s[white].\n", diskSize(exp))
	}
	if c.ticked[pb.PruneCategory_PRUNE_CATEGORY_IMAGES] {
		b.WriteString("\n[red]Every unused image[white] includes images of services scaled to zero or between restarts: they will have to pull again.\n")
	}
	if c.ticked[pb.PruneCategory_PRUNE_CATEGORY_VOLUMES] {
		b.WriteString("\n[red]VOLUMES: every volume no container uses is DELETED with its data, named volumes included. This cannot be undone.[white]\n")
	}
	return b.String()
}

// pruneResultText summarizes a prune, per category.
func pruneResultText(node string, resp *pb.PruneResponse) string {
	var b strings.Builder
	var total int64
	for _, r := range resp.GetResults() {
		total += r.GetFreedBytes()
	}
	fmt.Fprintf(&b, "Pruned on %q — freed %s\n\n", node, diskSize(total))
	for _, r := range resp.GetResults() {
		label := catLabel(r.GetCategory())
		switch {
		case r.GetError() != "":
			fmt.Fprintf(&b, "  [red]✗[white] %s: %s\n", label, escapeTags(r.GetError()))
		case r.GetSkipped():
			fmt.Fprintf(&b, "  – %s: skipped\n", label)
		case r.GetCategory() == pb.PruneCategory_PRUNE_CATEGORY_NETWORKS:
			fmt.Fprintf(&b, "  [green]✓[white] %s: %d removed\n", label, r.GetCount())
		default:
			fmt.Fprintf(&b, "  [green]✓[white] %s: %d removed, %s\n", label, r.GetCount(), diskSize(r.GetFreedBytes()))
		}
	}
	return b.String()
}
