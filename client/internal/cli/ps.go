// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/endpoint"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/dockerfmt"
)

func newPsCmd(g *globalFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ps [service]",
		Short: "List candidate tasks/containers and the node each runs on",
		Example: "  swarmexec ps\n" +
			"  swarmexec ps web\n" +
			"  swarmexec ps --json web",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPs(cmd, g, args, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON instead of a table")
	return cmd
}

type psRow struct {
	Service     string `json:"service"`
	Slot        int    `json:"slot,omitempty"`
	ContainerID string `json:"container_id"`
	Node        string `json:"node"`
	IP          string `json:"ip,omitempty"`
	UptimeSecs  int64  `json:"uptime_seconds,omitempty"`
}

func runPs(cmd *cobra.Command, g *globalFlags, args []string, asJSON bool) error {
	dockerEP := endpoint.Resolve(g.dockerContext)
	cfg, err := g.resolveConfig(cmd, dockerEP)
	if err != nil {
		return &cliError{code: exitUsage, err: err}
	}

	ctx := cmdContext(cmd)

	var service string
	if len(args) == 1 {
		service = args[0]
	}

	dcli, err := dockerEP.Connect(ctx)
	if err != nil {
		return &cliError{code: exitTransport, err: err}
	}
	r := resolve.New(dcli, addrModeOf(cfg))
	cands, err := r.Candidates(ctx, service)
	if err != nil {
		return &cliError{code: exitTransport, err: err}
	}
	if asJSON {
		rows := make([]psRow, 0, len(cands))
		for _, c := range cands {
			rows = append(rows, psRow{
				Service: c.Service, Slot: c.Slot, ContainerID: c.ContainerID,
				Node: c.NodeName, IP: c.NodeAddr, UptimeSecs: int64(c.Uptime.Seconds()),
			})
		}
		if err := printJSON(os.Stdout, rows); err != nil {
			return err
		}
		// Same outcome as the text mode below: a named service with no
		// running task is a failure, not an empty answer.
		if len(rows) == 0 && service != "" {
			return &cliError{code: exitTransport, err: fmt.Errorf("no running tasks for service %q", service)}
		}
		return nil
	}

	if len(cands) == 0 {
		if service != "" {
			return &cliError{code: exitTransport, err: fmt.Errorf("no running tasks for service %q", service)}
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
			dockerfmt.OrDash(c.Service), slot, dockerfmt.ShortID(c.ContainerID), dockerfmt.OrDash(c.NodeName), dockerfmt.OrDash(c.NodeAddr), uptime(c.Uptime))
	}
	return w.Flush()
}
