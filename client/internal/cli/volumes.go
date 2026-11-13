// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	units "github.com/docker/go-units"
	"github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	cterm "swarmexec/client/internal/term"
	pb "swarmexec/internal/pb"
)

// swarmVolume is a volume aggregated across the nodes that have it.
type swarmVolume = agentcall.Volume

func newVolumeCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "volume",
		Aliases: []string{"volumes"},
		Short:   "List and remove volumes across the swarm",
	}
	cmd.AddCommand(newVolumeLsCmd(g), newVolumeRmCmd(g))
	return cmd
}

type volumeFlags struct {
	all            bool
	nodes          []string
	force          bool
	yes            bool
	json           bool
	size           bool
	sort           string
	reverse        bool
	connectTimeout time.Duration
}

func newVolumeLsCmd(g *globalFlags) *cobra.Command {
	f := &volumeFlags{}
	cmd := &cobra.Command{
		Use:   "ls [name-filter]",
		Short: "List volumes across all nodes and which nodes hold each",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVolumeLs(cmd, g, f, args)
		},
	}
	cmd.Flags().DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "per-node connect timeout")
	cmd.Flags().BoolVar(&f.json, "json", false, "output JSON instead of a table")
	cmd.Flags().BoolVar(&f.size, "size", false, "also compute each volume's on-disk size (slower: du per volume)")
	cmd.Flags().StringVar(&f.sort, "sort", "name", "sort by: name|nodes|used|age|size (size implies --size)")
	cmd.Flags().BoolVar(&f.reverse, "reverse", false, "reverse the sort direction")
	return cmd
}

func newVolumeRmCmd(g *globalFlags) *cobra.Command {
	f := &volumeFlags{}
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a volume on all nodes (--all) or specific nodes (--node)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVolumeRm(cmd, g, f, args)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.all, "all", false, "remove on every node that holds the volume")
	fl.StringArrayVar(&f.nodes, "node", nil, "remove only on these nodes (repeatable)")
	fl.BoolVar(&f.force, "force", false, "pass docker's force flag")
	fl.BoolVarP(&f.yes, "yes", "y", false, "do not prompt for confirmation")
	fl.DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "per-node connect timeout")
	return cmd
}

func runVolumeLs(cmd *cobra.Command, g *globalFlags, f *volumeFlags, args []string) error {
	ac, ctx, dcli, nodes, err := volumeSetup(cmd, g)
	if err != nil {
		return err
	}
	defer ac.Close()
	if !validVolumeSort(f.sort) {
		return &cliError{code: usageExitCode, err: fmt.Errorf("invalid --sort %q (want name|nodes|used|age|size)", f.sort)}
	}
	vols, errs := ac.Volumes(ctx, nodes, f.connectTimeout)
	if e := noAgentIfAllDown(ctx, dcli, nodes, errs); e != nil {
		return e
	}
	usage := ac.VolumeUsage(ctx, nodes, f.connectTimeout)
	wantSize := f.size || f.sort == "size"
	var sizes map[string]int64
	if wantSize {
		sizes = ac.VolumeSizes(ctx, nodes, f.connectTimeout)
	}
	desc := defaultVolumeSortDesc(f.sort)
	if f.reverse {
		desc = !desc
	}
	sortVolumesBy(vols, f.sort, desc, usage, sizes)

	filter := ""
	if len(args) == 1 {
		filter = args[0]
	}

	if f.json {
		type consumerRow struct {
			Service   string `json:"service"`
			Container string `json:"container"`
			Node      string `json:"node"`
		}
		type volRow struct {
			Name      string        `json:"name"`
			Driver    string        `json:"driver"`
			Nodes     []string      `json:"nodes"`
			UsedBy    int           `json:"used_by"`
			Consumers []consumerRow `json:"consumers"`
			CreatedAt string        `json:"created_at,omitempty"`
			SizeBytes *int64        `json:"size_bytes,omitempty"`
		}
		rows := make([]volRow, 0, len(vols))
		for _, v := range vols {
			if filter != "" && !strings.Contains(v.Name, filter) {
				continue
			}
			cons := make([]consumerRow, 0, len(usage[v.Name]))
			for _, c := range usage[v.Name] {
				cons = append(cons, consumerRow(c))
			}
			row := volRow{Name: v.Name, Driver: v.Driver, Nodes: nodeNames(v.Nodes), UsedBy: len(cons), Consumers: cons}
			if !v.Created.IsZero() {
				row.CreatedAt = v.Created.UTC().Format(time.RFC3339)
			}
			if wantSize {
				if sz, ok := sizes[v.Name]; ok {
					row.SizeBytes = &sz
				}
			}
			rows = append(rows, row)
		}
		if err := printJSON(os.Stdout, rows); err != nil {
			return err
		}
		reportNodeErrors(errs)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	header := "VOLUME\tDRIVER\tNODES\tUSED BY\tAGE"
	if wantSize {
		header += "\tSIZE"
	}
	fmt.Fprintln(w, header)
	for _, v := range vols {
		if filter != "" && !strings.Contains(v.Name, filter) {
			continue
		}
		usedBy := "-"
		if n := len(usage[v.Name]); n > 0 {
			usedBy = strconv.Itoa(n)
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s", v.Name, v.Driver, strings.Join(nodeNames(v.Nodes), ","), usedBy, volumeAge(v.Created))
		if wantSize {
			sz := int64(-1)
			if s, ok := sizes[v.Name]; ok {
				sz = s
			}
			row += "\t" + humanBytes(sz)
		}
		fmt.Fprintln(w, row)
	}
	_ = w.Flush()
	reportNodeErrors(errs)
	return nil
}

func runVolumeRm(cmd *cobra.Command, g *globalFlags, f *volumeFlags, args []string) error {
	if !f.all && len(f.nodes) == 0 {
		return &cliError{code: usageExitCode, err: fmt.Errorf("specify --all or one/more --node")}
	}
	name := args[0]
	ac, ctx, dcli, nodes, err := volumeSetup(cmd, g)
	if err != nil {
		return err
	}
	defer ac.Close()
	vols, errs := ac.Volumes(ctx, nodes, f.connectTimeout)
	if e := noAgentIfAllDown(ctx, dcli, nodes, errs); e != nil {
		return e
	}
	reportNodeErrors(errs)

	var target *swarmVolume
	for i := range vols {
		if vols[i].Name == name {
			target = &vols[i]
			break
		}
	}
	if target == nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("volume %q not found on any node", name)}
	}

	rmNodes := target.Nodes
	if len(f.nodes) > 0 {
		rmNodes = filterNodes(target.Nodes, f.nodes)
		if len(rmNodes) == 0 {
			return &cliError{code: session.TransportFailure, err: fmt.Errorf("volume %q is not on any of the requested nodes", name)}
		}
	}

	if !f.yes {
		if !confirm(fmt.Sprintf("Remove volume %q on %d node(s) [%s]?", name, len(rmNodes), strings.Join(nodeNames(rmNodes), ","))) {
			fmt.Fprintln(os.Stderr, "aborted")
			return &cliError{code: session.TransportFailure, silent: true}
		}
	}

	failed := false
	for _, res := range ac.RemoveVolume(ctx, rmNodes, name, f.force, f.connectTimeout) {
		if res.Err != nil {
			failed = true
			fmt.Printf("%s: error: %v\n", res.Node.Name, res.Err)
		} else {
			fmt.Printf("%s: removed\n", res.Node.Name)
		}
	}
	if failed {
		return &cliError{code: session.TransportFailure, silent: true}
	}
	return nil
}

// volumeSetup resolves the agent client + manager client + the swarm node
// list. On success the caller owns the agent client and must Close it.
func volumeSetup(cmd *cobra.Command, g *globalFlags) (*agentcall.Client, context.Context, *client.Client, []resolve.Node, error) {
	dockerEP := resolveEndpoint(g.dockerContext)
	cfg, err := g.resolveConfig(cmd, dockerEP)
	if err != nil {
		return nil, nil, nil, nil, &cliError{code: usageExitCode, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, nil, nil, &cliError{code: usageExitCode, err: err}
	}
	ctx := cmdContext(cmd)
	dcli, err := dockerEP.connect(ctx)
	if err != nil {
		return nil, ctx, nil, nil, &cliError{code: session.TransportFailure, err: err}
	}
	nodes, err := resolve.New(dcli, addrModeOf(cfg)).Nodes(ctx)
	if err != nil {
		return nil, ctx, dcli, nil, &cliError{code: session.TransportFailure, err: fmt.Errorf("list swarm nodes: %w", err)}
	}
	return agentcall.New(cfg), ctx, dcli, nodes, nil
}

// noAgentIfAllDown returns the "run init" error when every node failed to answer
// and no agent service is deployed (the most likely cause).
func noAgentIfAllDown(ctx context.Context, dcli *client.Client, nodes []resolve.Node, errs map[string]error) error {
	if len(nodes) > 0 && len(errs) == len(nodes) && !agentDeployed(ctx, dcli) {
		return &cliError{code: session.TransportFailure, err: errNoAgent}
	}
	return nil
}

// humanBytes renders a byte count for display, or "-" when unknown (negative).
func humanBytes(n int64) string {
	if n < 0 {
		return "-"
	}
	return units.HumanSize(float64(n))
}

// volumeAge renders a volume's creation time as a compact relative age
// (e.g. "3d"), or "-" when unknown.
func volumeAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return uptime(time.Since(t))
}

// volumeCreated renders a volume's creation time as an absolute timestamp, or
// "-" when unknown.
func volumeCreated(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// validVolumeSort reports whether field is a recognized --sort value.
func validVolumeSort(field string) bool {
	switch field {
	case "name", "nodes", "used", "age", "size":
		return true
	}
	return false
}

// defaultVolumeSortDesc picks the natural default direction for a sort field:
// descending (biggest/most first) for nodes/used/size, ascending for name; age
// ascends so the oldest (highest age) come first.
func defaultVolumeSortDesc(field string) bool {
	switch field {
	case "nodes", "used", "size":
		return true
	default:
		return false
	}
}

// sortVolumesBy orders vols in place by field/direction. For size/age an unknown
// value always sorts last (regardless of direction); name is the tiebreaker.
func sortVolumesBy(vols []swarmVolume, field string, desc bool, usage map[string][]volumeConsumer, sizes map[string]int64) {
	known := func(v swarmVolume) bool {
		switch field {
		case "size":
			_, ok := sizes[v.Name]
			return ok
		case "age":
			return !v.Created.IsZero()
		default:
			return true
		}
	}
	less := func(a, b swarmVolume) bool {
		switch field {
		case "nodes":
			if len(a.Nodes) != len(b.Nodes) {
				return len(a.Nodes) < len(b.Nodes)
			}
		case "used":
			if ua, ub := len(usage[a.Name]), len(usage[b.Name]); ua != ub {
				return ua < ub
			}
		case "age":
			if !a.Created.Equal(b.Created) {
				return a.Created.Before(b.Created) // earlier = older
			}
		case "size":
			if sa, sb := sizes[a.Name], sizes[b.Name]; sa != sb {
				return sa < sb
			}
		}
		return a.Name < b.Name
	}
	sort.SliceStable(vols, func(i, j int) bool {
		a, b := vols[i], vols[j]
		if ka, kb := known(a), known(b); ka != kb {
			return ka // known before unknown, both directions
		}
		if desc {
			return less(b, a)
		}
		return less(a, b)
	})
}

// volumeConsumer is a container (and its service) that mounts a volume.
type volumeConsumer = agentcall.VolumeConsumer

// newVolumeOpts is the create-volume form's input. Driver empty means the
// daemon default ("local").
type newVolumeOpts struct {
	Name   string
	Driver string
	Labels map[string]string
}

// buildVolumeCreateReq validates the form input and builds the RPC request.
// Split out from the create call so the mapping is unit-testable.
func buildVolumeCreateReq(o newVolumeOpts) (*pb.CreateVolumeRequest, error) {
	name := strings.TrimSpace(o.Name)
	if name == "" {
		return nil, fmt.Errorf("volume name is required")
	}
	return &pb.CreateVolumeRequest{
		Name:   name,
		Driver: strings.TrimSpace(o.Driver),
		Labels: o.Labels,
	}, nil
}

// helpers

func nodeNames(nodes []resolve.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Name
	}
	return out
}

func filterNodes(have []resolve.Node, want []string) []resolve.Node {
	set := map[string]bool{}
	for _, w := range want {
		set[w] = true
	}
	var out []resolve.Node
	for _, n := range have {
		if set[n.Name] || set[n.ID] {
			out = append(out, n)
		}
	}
	return out
}

func reportNodeErrors(errs map[string]error) {
	if len(errs) == 0 {
		return
	}
	names := make([]string, 0, len(errs))
	for n := range errs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "warning: node %s unreachable: %v\n", n, errs[n])
	}
}

func confirm(prompt string) bool {
	if !cterm.IsTerminal(os.Stdin.Fd()) {
		return false // non-interactive: refuse without --yes
	}
	line, ok := promptLine(fmt.Sprintf("%s [y/N]: ", prompt))
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
