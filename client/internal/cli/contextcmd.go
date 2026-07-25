// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/dockerctx"
)

// newContextCmd manages Docker CLI contexts — the same ones swarmexec resolves
// for the manager API (--context / $DOCKER_CONTEXT). It writes docker's own
// on-disk format, so contexts created here are interchangeable with the docker
// CLI's. This lets an operator manage contexts without docker installed at all.
func newContextCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "context",
		Aliases: []string{"ctx"},
		Short:   "Manage Docker contexts used for the manager API",
	}
	cmd.AddCommand(
		newContextCreateCmd(),
		newContextLsCmd(),
		newContextUseCmd(),
		newContextRmCmd(),
	)
	return cmd
}

func newContextCreateCmd() *cobra.Command {
	var (
		host        string
		description string
		useIt       bool
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a Docker context pointing at a manager host",
		Long: "Create a Docker context in docker's own store, so `swarmexec --context\n" +
			"<name>` (and the docker CLI) can use it. The host is a docker daemon\n" +
			"endpoint — typically an ssh:// bastion to a Swarm manager, or tcp:// / unix://.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if host == "" {
				return &cliError{code: usageExitCode, err: fmt.Errorf("--docker-host is required (e.g. ssh://ops@manager)")}
			}
			if err := dockerctx.Create(args[0], host, description); err != nil {
				return &cliError{code: usageExitCode, err: err}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created context %q -> %s\n", args[0], host)
			if useIt {
				if err := dockerctx.Use(args[0]); err != nil {
					return &cliError{code: usageExitCode, err: err}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "current context is now %q\n", args[0])
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&host, "docker-host", "", "docker daemon endpoint (ssh:// | tcp:// | unix:// | npipe://)")
	fl.StringVar(&description, "description", "", "optional description")
	fl.BoolVar(&useIt, "use", false, "also make it the current context")
	return cmd
}

func newContextLsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List Docker contexts",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctxs, err := dockerctx.List()
			if err != nil {
				return &cliError{code: usageExitCode, err: err}
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), ctxs)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tCURRENT\tDOCKER ENDPOINT")
			for _, c := range ctxs {
				cur := ""
				if c.Current {
					cur = "*"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", c.Name, cur, c.Host)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON instead of a table")
	return cmd
}

func newContextUseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "use <name>",
		Short: "Set the current Docker context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := dockerctx.Use(args[0]); err != nil {
				return &cliError{code: usageExitCode, err: err}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "current context is now %q\n", args[0])
			return nil
		},
	}
	return cmd
}

func newContextRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "rm <name> [name...]",
		Aliases: []string{"remove"},
		Short:   "Remove one or more Docker contexts",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, name := range args {
				if err := dockerctx.Remove(name, force); err != nil {
					return &cliError{code: usageExitCode, err: err}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "removed context %q\n", name)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "remove even if it is the current context (resets selection to default)")
	return cmd
}
