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

	"github.com/docker/docker/client"
	units "github.com/docker/go-units"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dial"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	cterm "swarmexec/client/internal/term"
	pb "swarmexec/internal/pb"
)

// swarmVolume is a volume aggregated across the nodes that have it.
type swarmVolume struct {
	Name   string
	Driver string
	Nodes  []resolve.Node // nodes that hold a copy of this volume
}

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
	cfg, ctx, dcli, nodes, err := volumeSetup(cmd, g)
	if err != nil {
		return err
	}
	vols, errs := indexVolumes(ctx, cfg, nodes, f.connectTimeout)
	if e := noAgentIfAllDown(ctx, dcli, nodes, errs); e != nil {
		return e
	}
	usage := indexVolumeUsage(ctx, cfg, nodes, f.connectTimeout)
	var sizes map[string]int64
	if f.size {
		sizes = indexVolumeSizes(ctx, cfg, nodes, f.connectTimeout)
	}

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
			SizeBytes *int64        `json:"size_bytes,omitempty"`
		}
		rows := make([]volRow, 0, len(vols))
		for _, v := range vols {
			if filter != "" && !strings.Contains(v.Name, filter) {
				continue
			}
			cons := make([]consumerRow, 0, len(usage[v.Name]))
			for _, c := range usage[v.Name] {
				cons = append(cons, consumerRow{Service: c.Service, Container: c.Container, Node: c.Node})
			}
			row := volRow{Name: v.Name, Driver: v.Driver, Nodes: nodeNames(v.Nodes), UsedBy: len(cons), Consumers: cons}
			if f.size {
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
	header := "VOLUME\tDRIVER\tNODES\tUSED BY"
	if f.size {
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
		row := fmt.Sprintf("%s\t%s\t%s\t%s", v.Name, v.Driver, strings.Join(nodeNames(v.Nodes), ","), usedBy)
		if f.size {
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
	cfg, ctx, dcli, nodes, err := volumeSetup(cmd, g)
	if err != nil {
		return err
	}
	vols, errs := indexVolumes(ctx, cfg, nodes, f.connectTimeout)
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
	for _, res := range removeOnNodes(ctx, cfg, rmNodes, name, f.force, f.connectTimeout) {
		if res.err != nil {
			failed = true
			fmt.Printf("%s: error: %v\n", res.node.Name, res.err)
		} else {
			fmt.Printf("%s: removed\n", res.node.Name)
		}
	}
	if failed {
		return &cliError{code: session.TransportFailure, silent: true}
	}
	return nil
}

// volumeSetup resolves config + manager client + the swarm node list.
func volumeSetup(cmd *cobra.Command, g *globalFlags) (config.Config, context.Context, *client.Client, []resolve.Node, error) {
	cfg, err := g.resolveConfig(cmd)
	if err != nil {
		return cfg, nil, nil, nil, &cliError{code: usageExitCode, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, nil, nil, nil, &cliError{code: usageExitCode, err: err}
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	dcli, err := newDockerClient(g.dockerContext)
	if err != nil {
		return cfg, ctx, nil, nil, &cliError{code: session.TransportFailure, err: err}
	}
	nodes, err := resolve.New(dcli, addrModeOf(cfg)).Nodes(ctx)
	if err != nil {
		return cfg, ctx, dcli, nil, &cliError{code: session.TransportFailure, err: fmt.Errorf("list swarm nodes: %w", err)}
	}
	return cfg, ctx, dcli, nodes, nil
}

// noAgentIfAllDown returns the "run init" error when every node failed to answer
// and no agent service is deployed (the most likely cause).
func noAgentIfAllDown(ctx context.Context, dcli *client.Client, nodes []resolve.Node, errs map[string]error) error {
	if len(nodes) > 0 && len(errs) == len(nodes) && !agentDeployed(ctx, dcli) {
		return &cliError{code: session.TransportFailure, err: errNoAgent}
	}
	return nil
}

// indexVolumes queries every node's agent in parallel and aggregates volumes by
// name. Unreachable nodes are returned in the errors map (keyed by node name).
func indexVolumes(ctx context.Context, cfg config.Config, nodes []resolve.Node, connectTimeout time.Duration) ([]swarmVolume, map[string]error) {
	type res struct {
		node resolve.Node
		vols []*pb.VolumeInfo
		err  error
	}
	out := make([]res, len(nodes))
	forEachNode(nodes, func(i int, n resolve.Node) {
		vs, err := listNodeVolumes(ctx, cfg, n, false, connectTimeout)
		out[i] = res{node: n, vols: vs, err: err}
	})

	byName := map[string]*swarmVolume{}
	errs := map[string]error{}
	for _, r := range out {
		if r.err != nil {
			errs[r.node.Name] = r.err
			continue
		}
		for _, v := range r.vols {
			sv := byName[v.Name]
			if sv == nil {
				sv = &swarmVolume{Name: v.Name, Driver: v.Driver}
				byName[v.Name] = sv
			}
			sv.Nodes = append(sv.Nodes, r.node)
		}
	}
	vols := make([]swarmVolume, 0, len(byName))
	for _, sv := range byName {
		sort.Slice(sv.Nodes, func(i, j int) bool { return sv.Nodes[i].Name < sv.Nodes[j].Name })
		vols = append(vols, *sv)
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })
	return vols, errs
}

func listNodeVolumes(ctx context.Context, cfg config.Config, n resolve.Node, withSize bool, connectTimeout time.Duration) ([]*pb.VolumeInfo, error) {
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := dial.Dial(dctx, n.DialHost, cfg.Port, cfg)
	cancel()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := pb.NewAgentClient(conn).ListVolumes(ctx, &pb.ListVolumesRequest{WithSize: withSize})
	if err != nil {
		return nil, wrapGRPC(err)
	}
	return resp.Volumes, nil
}

// indexVolumeSizes returns volume name -> total on-disk bytes, summed across the
// nodes that hold it. Computing sizes is expensive (du-style), so this is a
// separate opt-in pass. Volumes with no known size on any node are absent from
// the map (callers render them as "-"). Unreachable nodes are skipped.
func indexVolumeSizes(ctx context.Context, cfg config.Config, nodes []resolve.Node, connectTimeout time.Duration) map[string]int64 {
	per := make([][]*pb.VolumeInfo, len(nodes))
	forEachNode(nodes, func(i int, n resolve.Node) {
		vs, err := listNodeVolumes(ctx, cfg, n, true, connectTimeout)
		if err != nil {
			return
		}
		per[i] = vs
	})
	sizes := map[string]int64{}
	for _, vs := range per {
		for _, v := range vs {
			// Skip when the agent didn't report a size (older agent that predates
			// size reporting) or it's not available (non-local driver).
			if !v.GetSizeKnown() || v.GetSizeBytes() < 0 {
				continue
			}
			sizes[v.GetName()] += v.GetSizeBytes()
		}
	}
	return sizes
}

// humanBytes renders a byte count for display, or "-" when unknown (negative).
func humanBytes(n int64) string {
	if n < 0 {
		return "-"
	}
	return units.HumanSize(float64(n))
}

// volumeConsumer is a container (and its service) that mounts a volume.
type volumeConsumer struct {
	Service   string
	Container string
	Node      string
}

// indexVolumeUsage maps each volume name to the containers that mount it, by
// asking every node's agent for its containers and their named volumes. Nodes
// whose agent is unreachable are skipped (best-effort: usage may be partial).
func indexVolumeUsage(ctx context.Context, cfg config.Config, nodes []resolve.Node, connectTimeout time.Duration) map[string][]volumeConsumer {
	type res struct {
		node       resolve.Node
		containers []*pb.ContainerInfo
	}
	out := make([]res, len(nodes))
	forEachNode(nodes, func(i int, n resolve.Node) {
		cs, err := listNodeContainers(ctx, cfg, n, connectTimeout)
		if err != nil {
			return // leave out[i].containers nil
		}
		out[i] = res{node: n, containers: cs}
	})

	usage := map[string][]volumeConsumer{}
	for _, r := range out {
		for _, c := range r.containers {
			for _, vol := range c.GetVolumes() {
				usage[vol] = append(usage[vol], volumeConsumer{
					Service:   c.GetService(),
					Container: c.GetName(),
					Node:      r.node.Name,
				})
			}
		}
	}
	for _, cs := range usage {
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].Service != cs[j].Service {
				return cs[i].Service < cs[j].Service
			}
			return cs[i].Container < cs[j].Container
		})
	}
	return usage
}

func listNodeContainers(ctx context.Context, cfg config.Config, n resolve.Node, connectTimeout time.Duration) ([]*pb.ContainerInfo, error) {
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := dial.Dial(dctx, n.DialHost, cfg.Port, cfg)
	cancel()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := pb.NewAgentClient(conn).ListContainers(ctx, &pb.ListRequest{})
	if err != nil {
		return nil, wrapGRPC(err)
	}
	return resp.Containers, nil
}

type rmResult struct {
	node resolve.Node
	err  error
}

// removeOnNodes removes the volume on each node concurrently.
func removeOnNodes(ctx context.Context, cfg config.Config, nodes []resolve.Node, name string, force bool, connectTimeout time.Duration) []rmResult {
	out := make([]rmResult, len(nodes))
	forEachNode(nodes, func(i int, n resolve.Node) {
		out[i] = rmResult{node: n, err: removeNodeVolume(ctx, cfg, n, name, force, connectTimeout)}
	})
	return out
}

func removeNodeVolume(ctx context.Context, cfg config.Config, n resolve.Node, name string, force bool, connectTimeout time.Duration) error {
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := dial.Dial(dctx, n.DialHost, cfg.Port, cfg)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = pb.NewAgentClient(conn).RemoveVolume(ctx, &pb.RemoveVolumeRequest{Name: name, Force: force})
	return wrapGRPC(err)
}

// helpers

func wrapGRPC(err error) error {
	if err == nil {
		return nil
	}
	if agentTooOld(err) {
		return errAgentTooOld
	}
	if st, ok := status.FromError(err); ok {
		return fmt.Errorf("%s: %s", st.Code(), st.Message())
	}
	return err
}

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
