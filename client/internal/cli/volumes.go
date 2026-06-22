package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/docker/docker/client"
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

	filter := ""
	if len(args) == 1 {
		filter = args[0]
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "VOLUME\tDRIVER\tNODES")
	for _, v := range vols {
		if filter != "" && !strings.Contains(v.Name, filter) {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", v.Name, v.Driver, strings.Join(nodeNames(v.Nodes), ","))
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
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n resolve.Node) {
			defer wg.Done()
			vs, err := listNodeVolumes(ctx, cfg, n, connectTimeout)
			out[i] = res{node: n, vols: vs, err: err}
		}(i, n)
	}
	wg.Wait()

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

func listNodeVolumes(ctx context.Context, cfg config.Config, n resolve.Node, connectTimeout time.Duration) ([]*pb.VolumeInfo, error) {
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := dial.Dial(dctx, n.DialHost, cfg.Port, cfg)
	cancel()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := pb.NewAgentClient(conn).ListVolumes(ctx, &pb.ListVolumesRequest{})
	if err != nil {
		return nil, wrapGRPC(err)
	}
	return resp.Volumes, nil
}

type rmResult struct {
	node resolve.Node
	err  error
}

// removeOnNodes removes the volume on each node concurrently.
func removeOnNodes(ctx context.Context, cfg config.Config, nodes []resolve.Node, name string, force bool, connectTimeout time.Duration) []rmResult {
	out := make([]rmResult, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n resolve.Node) {
			defer wg.Done()
			out[i] = rmResult{node: n, err: removeNodeVolume(ctx, cfg, n, name, force, connectTimeout)}
		}(i, n)
	}
	wg.Wait()
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
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", prompt)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(sc.Text())) {
	case "y", "yes":
		return true
	}
	return false
}
