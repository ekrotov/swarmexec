package cli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
)

func newPsCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ps [service]",
		Short: "List candidate tasks/containers and the node each runs on",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPs(cmd, g, args)
		},
	}
	return cmd
}

func runPs(cmd *cobra.Command, g *globalFlags, args []string) error {
	cfg, err := g.resolveConfig(cmd)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	var service string
	if len(args) == 1 {
		service = args[0]
	}

	dcli, err := newDockerClient(g.dockerContext)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	r := resolve.New(dcli, addrModeOf(cfg))
	cands, err := r.Candidates(ctx, service)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	if len(cands) == 0 {
		if service != "" {
			return &cliError{code: session.TransportFailure, err: fmt.Errorf("no running tasks for service %q", service)}
		}
		fmt.Fprintln(os.Stderr, "no running tasks")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tSLOT\tCONTAINER\tNODE\tIP\tUPTIME")
	for _, c := range cands {
		slot := "-"
		if c.Slot > 0 {
			slot = fmt.Sprintf("%d", c.Slot)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			orDash(c.Service), slot, shortID(c.ContainerID), orDash(c.NodeName), orDash(c.NodeAddr), uptime(c.Uptime))
	}
	return w.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
