// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	"swarmexec/client/internal/swarmops"
	"swarmexec/internal/pb"
	"swarmexec/internal/prunesched"
)

// The `node` group is the cli twin of the TUI's Nodes tab (review P-02): the
// list, the per-node prune, availability and labels, and the cluster-wide
// prune schedule (which used to be the top-level `prune-schedule`). Every
// action runs the code the tab runs — swarmops for the manager side, agentcall
// for the node side — with the same confirms, as prompts with --yes.

func newNodeCmd(g *globalFlags) *cobra.Command {
	cmd := group(&cobra.Command{
		Use:     "node",
		Aliases: []string{"nodes"},
		Short:   "List the swarm's nodes, prune them, set availability and labels, schedule prunes",
	})
	cmd.AddCommand(newNodeLsCmd(g), newNodePruneCmd(g), newNodeUpdateCmd(g), newPruneScheduleCmd(g, "swarmexec node prune-schedule"))
	return cmd
}

// --- node ls -----------------------------------------------------------------

type nodeLsFlags struct {
	json           bool
	skipAgents     bool
	connectTimeout time.Duration
}

// nodeRow is one node as `node ls --json` prints it. Volumes is null when the
// node's agent did not answer (or --skip-agents): unknown, not zero.
type nodeRow struct {
	Hostname     string            `json:"hostname"`
	ID           string            `json:"id"`
	Role         string            `json:"role"`
	Leader       bool              `json:"leader,omitempty"`
	Availability string            `json:"availability"`
	State        string            `json:"state"`
	Addr         string            `json:"addr,omitempty"`
	Engine       string            `json:"engine,omitempty"`
	OS           string            `json:"os,omitempty"`
	Arch         string            `json:"arch,omitempty"`
	NanoCPUs     int64             `json:"nano_cpus,omitempty"`
	MemoryBytes  int64             `json:"memory_bytes,omitempty"`
	Tasks        int               `json:"tasks"`
	Volumes      *int              `json:"volumes"`
	Labels       map[string]string `json:"labels,omitempty"`
}

func newNodeLsCmd(g *globalFlags) *cobra.Command {
	f := &nodeLsFlags{}
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List the nodes: role, availability, state, engine, running tasks, volumes, labels",
		Long: "The Nodes tab as a table. TASKS counts the running tasks the manager placed\n" +
			"on the node; VOLS is the node's volume count, asked of its agent (\"-\" when\n" +
			"the agent did not answer, or with --skip-agents); LABELS is the number of\n" +
			"node labels (--json lists them).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runNodeLs(cmd, g, f)
		},
	}
	cmd.Flags().BoolVar(&f.json, "json", false, "output JSON instead of a table")
	cmd.Flags().BoolVar(&f.skipAgents, "skip-agents", false, "do not ask the agents for volume counts (faster)")
	cmd.Flags().DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "per-node connect timeout")
	return cmd
}

func runNodeLs(cmd *cobra.Command, g *globalFlags, f *nodeLsFlags) error {
	c, err := connectCluster(cmd, g)
	if err != nil {
		return err
	}
	defer c.dcli.Close()
	nodes, err := swarmops.ListNodes(c.ctx, c.dcli)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	var vols map[string]int
	if !f.skipAgents {
		vols = c.volumeCounts(f.connectTimeout)
	}
	rows := make([]nodeRow, 0, len(nodes))
	for _, n := range nodes {
		rows = append(rows, nodeRowOf(n, vols))
	}
	if f.json {
		return printJSON(cmd.OutOrStdout(), rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "no nodes")
		return nil
	}
	return printNodes(cmd.OutOrStdout(), rows)
}

// volumeCounts is each answering node's volume count, as the Nodes tab
// counts them; a node whose agent did not answer has no entry. Nil when the
// agents cannot be used at all.
func (c *clusterConn) volumeCounts(connectTimeout time.Duration) map[string]int {
	ac, err := c.agents()
	if err != nil {
		return nil
	}
	defer ac.Close()
	ns, err := c.r.Nodes(c.ctx)
	if err != nil {
		return nil
	}
	vs, errs := ac.Volumes(c.ctx, ns, connectTimeout)
	counts := swarmops.VolumeCountsByNode(vs)
	for _, n := range ns {
		if _, failed := errs[n.Name]; !failed {
			counts[n.Name] += 0 // answered with no volumes: 0, not unknown
		}
	}
	return counts
}

func nodeRowOf(n swarmops.NodeInfo, vols map[string]int) nodeRow {
	row := nodeRow{
		Hostname: n.Hostname, ID: n.ID, Role: n.Role, Leader: n.Leader,
		Availability: n.Availability, State: n.State, Addr: n.Addr,
		Engine: n.EngineVersion, OS: n.OS, Arch: n.Arch,
		NanoCPUs: n.NanoCPUs, MemoryBytes: n.MemoryBytes,
		Tasks: n.Tasks, Labels: n.Labels,
	}
	if v, ok := vols[n.Hostname]; ok {
		row.Volumes = &v
	}
	return row
}

func printNodes(out io.Writer, rows []nodeRow) error {
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tROLE\tAVAIL\tSTATE\tENGINE\tTASKS\tVOLS\tLABELS")
	for _, r := range rows {
		name := r.Hostname
		if r.Leader {
			name += " (leader)"
		}
		vols := "-"
		if r.Volumes != nil {
			vols = fmt.Sprint(*r.Volumes)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%d\n", name, orDash(r.Role), orDash(r.Availability),
			orDash(r.State), orDash(r.Engine), r.Tasks, vols, len(r.Labels))
	}
	return w.Flush()
}

// matchNode finds a node by hostname, full id, or unique id prefix — the
// references `docker node` takes.
func matchNode[T any](items []T, ref string, id, name func(T) string) (T, error) {
	var zero T
	var prefixed []T
	for _, it := range items {
		if name(it) == ref || id(it) == ref {
			return it, nil
		}
		if strings.HasPrefix(id(it), ref) {
			prefixed = append(prefixed, it)
		}
	}
	switch len(prefixed) {
	case 1:
		return prefixed[0], nil
	case 0:
		return zero, &cliError{code: session.TransportFailure, err: fmt.Errorf("no node %q in the swarm", ref)}
	}
	return zero, &cliError{code: usageExitCode, err: fmt.Errorf("node id prefix %q is ambiguous; give more of it, or the hostname", ref)}
}

// --- node prune --------------------------------------------------------------

type nodePruneFlags struct {
	categories     string
	until          string
	yes            bool
	connectTimeout time.Duration
}

func newNodePruneCmd(g *globalFlags) *cobra.Command {
	f := &nodePruneFlags{}
	cmd := &cobra.Command{
		Use:   "prune <node>",
		Short: "Free disk on one node: stopped containers, images, build cache, networks, volumes",
		Long: "Prune one node through its agent, the TUI's node prune. Without\n" +
			"--categories it sweeps the safe ones: stopped containers, untagged images,\n" +
			"build cache and unused local networks. \"images\" (every unused image,\n" +
			"including those of services scaled to zero) and \"volumes\" (DATA, named\n" +
			"volumes included) are only ever pruned when named.\n\n" +
			"Before pruning it shows what each category would free, as the agent last\n" +
			"measured it, and asks; --yes answers in advance. Without a terminal and\n" +
			"without --yes nothing is pruned.",
		Example: "  swarmexec node prune worker-1\n" +
			"  swarmexec node prune worker-1 --categories containers,images --yes\n" +
			"  swarmexec node prune worker-1 --containers-until 2h",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNodePrune(cmd, g, f, args[0])
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.categories, "categories", "", "comma-separated: "+strings.Join(prunesched.Categories, ", ")+
		" (default containers,dangling-images,buildcache,networks)")
	fl.StringVar(&f.until, "containers-until", "", "keep stopped containers this long after they stop (default 24h; 0 keeps none)")
	fl.BoolVarP(&f.yes, "yes", "y", false, "do not prompt for confirmation")
	fl.DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "per-node connect timeout")
	return cmd
}

// pruneChoiceFromFlags is the dialog's choice, from the flags. Pure.
func pruneChoiceFromFlags(categories, until string) (pruneChoice, error) {
	choice := defaultPruneChoice()
	if strings.TrimSpace(categories) != "" {
		names, err := prunesched.ParseCategories(categories)
		if err != nil {
			return choice, err
		}
		choice.ticked = map[pb.PruneCategory]bool{}
		for _, pc := range pruneCats {
			for _, n := range names {
				if n == pc.name {
					choice.ticked[pc.cat] = true
				}
			}
		}
		if choice.ticked[pb.PruneCategory_PRUNE_CATEGORY_IMAGES] && choice.ticked[pb.PruneCategory_PRUNE_CATEGORY_DANGLING_IMAGES] {
			return choice, errors.New("images already includes dangling-images: name one of them")
		}
	}
	if until != "" {
		if _, err := prunesched.ParseUntil(until); err != nil {
			return choice, err
		}
		choice.until = strings.TrimSpace(until)
	}
	return choice, nil
}

func runNodePrune(cmd *cobra.Command, g *globalFlags, f *nodePruneFlags, ref string) error {
	choice, err := pruneChoiceFromFlags(f.categories, f.until)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	c, err := connectCluster(cmd, g)
	if err != nil {
		return err
	}
	defer c.dcli.Close()
	ac, err := c.agents()
	if err != nil {
		return err
	}
	defer ac.Close()
	ns, err := c.r.Nodes(c.ctx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("list swarm nodes: %w", err)}
	}
	target, err := matchNode(ns, ref, func(n resolve.Node) string { return n.ID }, func(n resolve.Node) string { return n.Name })
	if err != nil {
		return err
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	// The figures are for the confirm only; without them it still says what
	// goes, just not how much.
	stats, serr := ac.ReclaimStats(c.ctx, target, false, f.connectTimeout)
	if serr != nil {
		fmt.Fprintf(errOut, "note: no reclaimable figures from %s's agent: %v\n", target.Name, serr)
	}
	// "…" is the dialog's "still loading"; here nothing more is coming.
	fmt.Fprint(out, strings.ReplaceAll(plainText(pruneConfirmBody(target.Name, choice, stats)), " — …\n", " — no figure\n"))
	if !f.yes && !confirm("Prune?") {
		fmt.Fprintln(errOut, "aborted — nothing was pruned (non-interactive runs need --yes)")
		return &cliError{code: session.TransportFailure, silent: true}
	}
	resp, err := ac.Prune(c.ctx, target, choice.request(), f.connectTimeout)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("prune on %s: %w", target.Name, err)}
	}
	fmt.Fprint(out, "\n"+plainText(pruneResultText(target.Name, resp)))
	for _, r := range resp.GetResults() {
		if r.GetError() != "" {
			return &cliError{code: session.TransportFailure, silent: true}
		}
	}
	return nil
}

// tviewTag is a colour tag of the TUI's texts: [red], [white], [-] ….
var tviewTag = regexp.MustCompile(`\[(?:[a-z]+|-)\]`)

// plainText turns a TUI text into plain terminal text: colour tags out, and
// an escaped "[[" (escapeTags) back to "[".
func plainText(s string) string {
	s = strings.ReplaceAll(s, "[[", "\x00")
	s = tviewTag.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, "\x00", "[")
}

// --- node update -------------------------------------------------------------

type nodeUpdateFlags struct {
	availability string
	labelAdd     []string
	labelRm      []string
	yes          bool
}

func newNodeUpdateCmd(g *globalFlags) *cobra.Command {
	f := &nodeUpdateFlags{}
	cmd := &cobra.Command{
		Use:   "update <node>",
		Short: "Set a node's availability (active, pause, drain) or its labels",
		Long: "Change a node the way the TUI's Nodes tab does. Applies at once: a node has\n" +
			"no rolling update. Draining moves every task off the node, so it asks first\n" +
			"(--yes answers in advance); active and pause apply directly — pause only\n" +
			"stops new placements.",
		Example: "  swarmexec node update worker-1 --availability drain --yes\n" +
			"  swarmexec node update worker-1 --label-add zone=b --label-rm old",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNodeUpdate(cmd, g, f, args[0])
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.availability, "availability", "", "active | pause | drain")
	fl.StringArrayVar(&f.labelAdd, "label-add", nil, "add or change a node label, key=value (repeatable)")
	fl.StringArrayVar(&f.labelRm, "label-rm", nil, "remove a node label by key (repeatable)")
	fl.BoolVarP(&f.yes, "yes", "y", false, "do not prompt before draining")
	return cmd
}

// nodeLabelsAfter is labels with the adds and removals applied. Pure.
func nodeLabelsAfter(labels map[string]string, add, rm []string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range labels {
		out[k] = v
	}
	for _, kv := range add {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("--label-add %q: want key=value", kv)
		}
		out[strings.TrimSpace(k)] = v
	}
	for _, k := range rm {
		delete(out, strings.TrimSpace(k))
	}
	return out, nil
}

func parseAvailability(s string) (swarm.NodeAvailability, error) {
	switch a := swarm.NodeAvailability(strings.ToLower(strings.TrimSpace(s))); a {
	case swarm.NodeAvailabilityActive, swarm.NodeAvailabilityPause, swarm.NodeAvailabilityDrain:
		return a, nil
	}
	return "", fmt.Errorf("--availability must be active, pause or drain, not %q", s)
}

func runNodeUpdate(cmd *cobra.Command, g *globalFlags, f *nodeUpdateFlags, ref string) error {
	fl := cmd.Flags()
	if !fl.Changed("availability") && len(f.labelAdd) == 0 && len(f.labelRm) == 0 {
		return &cliError{code: usageExitCode, err: errors.New("nothing to change: give --availability, --label-add or --label-rm")}
	}
	var avail swarm.NodeAvailability
	if fl.Changed("availability") {
		a, err := parseAvailability(f.availability)
		if err != nil {
			return &cliError{code: usageExitCode, err: err}
		}
		avail = a
	}
	c, err := connectCluster(cmd, g)
	if err != nil {
		return err
	}
	defer c.dcli.Close()
	nodes, err := swarmops.ListNodes(c.ctx, c.dcli)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	n, err := matchNode(nodes, ref, func(n swarmops.NodeInfo) string { return n.ID }, func(n swarmops.NodeInfo) string { return n.Hostname })
	if err != nil {
		return err
	}
	labels, err := nodeLabelsAfter(n.Labels, f.labelAdd, f.labelRm)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	out := cmd.OutOrStdout()
	if avail != "" {
		if err := c.setAvailability(out, cmd.ErrOrStderr(), n, avail, f.yes); err != nil {
			return err
		}
	}
	if len(f.labelAdd) > 0 || len(f.labelRm) > 0 {
		if err := swarmops.SetNodeLabels(c.ctx, c.dcli, n.ID, n.Version, labels); err != nil {
			return &cliError{code: session.TransportFailure, err: err}
		}
		fmt.Fprintf(out, "%s: labels %s\n", n.Hostname, labelsText(labels))
	}
	return nil
}

// setAvailability applies one availability, behind the drain confirm.
func (c *clusterConn) setAvailability(out, errOut io.Writer, n swarmops.NodeInfo, to swarm.NodeAvailability, yes bool) error {
	if to == swarm.NodeAvailabilityDrain && !yes {
		fmt.Fprintf(out, "Draining %q stops its %d running task(s) and reschedules them on other nodes.\n"+
			"Services whose tasks cannot be placed elsewhere will go unschedulable.\n", n.Hostname, n.Tasks)
		if !confirm("Drain?") {
			fmt.Fprintln(errOut, "aborted — the node was not changed (non-interactive runs need --yes)")
			return &cliError{code: session.TransportFailure, silent: true}
		}
	}
	if err := swarmops.SetNodeAvailability(c.ctx, c.dcli, n.ID, n.Version, to); err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("set %s to %s: %w", n.Hostname, to, err)}
	}
	fmt.Fprintf(out, "%s: availability %s\n", n.Hostname, to)
	return nil
}

// labelsText renders labels sorted, "k=v, k2=v2", or "(none)".
func labelsText(labels map[string]string) string {
	if len(labels) == 0 {
		return "(none)"
	}
	kv := make([]string, 0, len(labels))
	for k, v := range labels {
		kv = append(kv, k+"="+v)
	}
	sort.Strings(kv)
	return strings.Join(kv, ", ")
}
