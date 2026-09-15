// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/session"
)

type downFlags struct {
	serviceName string
	keepSecret  bool
	yes         bool
}

func newDownCmd(g *globalFlags) *cobra.Command {
	f := &downFlags{}
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Remove the agent service (and secret) provisioned by `init`",
		Long: "down is the inverse of init: it removes the global agent service and, by\n" +
			"default, the shared-secret Docker secret. It does not touch the client config.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDown(cmd, g, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.serviceName, "service-name", defaultServiceNm, "name of the agent service to remove")
	fl.BoolVar(&f.keepSecret, "keep-secret", false, "do not remove the shared-secret Docker secret")
	fl.BoolVarP(&f.yes, "yes", "y", false, "do not prompt for confirmation")
	return cmd
}

func runDown(cmd *cobra.Command, g *globalFlags, f *downFlags) error {
	ctx := cmdContext(cmd)
	out := cmd.OutOrStdout()

	tctx, cerr := resolveContext(out, g)
	if cerr != nil {
		return cerr
	}
	dcli, err := newDockerClient(ctx, tctx.Name)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	fmt.Fprintf(out, "removing agents from Docker context %q (%s)\n", tctx.Name, hostOrDefault(tctx.Host))

	svc, err := findRemovableAgent(ctx, dcli, f.serviceName)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	if svc == nil {
		fmt.Fprintf(out, "no agent service found (looked for %q and the %s=%s label) — nothing to do\n", f.serviceName, agentRoleLabel, agentRoleValue)
		return nil
	}

	what := fmt.Sprintf("service %q", svc.Spec.Name)
	if !f.keepSecret {
		what += fmt.Sprintf(" and secret %q", agentSecretName)
	}
	if !f.yes && !confirm(fmt.Sprintf("Remove %s on context %q? This stops remote exec/logs/volume.", what, tctx.Name)) {
		fmt.Fprintln(out, "aborted")
		return &cliError{code: session.TransportFailure, silent: true}
	}

	if err := dcli.ServiceRemove(ctx, svc.ID); err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("remove service %q: %w", svc.Spec.Name, err)}
	}
	fmt.Fprintf(out, "service %q: removed\n", svc.Spec.Name)

	if !f.keepSecret {
		if err := removeSecret(ctx, dcli, agentSecretName); err != nil {
			// The secret may still be held briefly by draining tasks; don't fail.
			fmt.Fprintf(out, "secret %q: not removed (%v) — remove later with `docker secret rm %s`\n", agentSecretName, err, agentSecretName)
		} else {
			fmt.Fprintf(out, "secret %q: removed\n", agentSecretName)
		}
	}

	fmt.Fprintln(out, "\n✓ done")
	return nil
}

// findRemovableAgent locates the agent service by name, falling back to the
// role label so `down` still finds it if the name differs.
func findRemovableAgent(ctx context.Context, dcli *client.Client, name string) (*swarm.Service, error) {
	if svc, err := serviceByName(ctx, dcli, name); err != nil {
		return nil, err
	} else if svc != nil {
		return svc, nil
	}
	list, err := dcli.ServiceList(ctx, types.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", agentRoleLabel+"="+agentRoleValue)),
	})
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	if len(list) > 0 {
		return &list[0], nil
	}
	return nil, nil
}

func removeSecret(ctx context.Context, dcli *client.Client, name string) error {
	list, err := dcli.SecretList(ctx, types.SecretListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return err
	}
	for _, s := range list {
		if s.Spec.Name == name {
			return dcli.SecretRemove(ctx, s.ID)
		}
	}
	return nil // not present — fine
}
