// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/session"
	"swarmexec/client/internal/stackfile"
)

// `stack export` and `stack diff` are two readings of the same rendering. Export
// answers "what is deployed, written down"; diff answers "would deploying this
// file change it". Both go through the same normalizer, so an export fed
// straight back into diff produces no differences — which is the cheapest test
// of whether the rendering is faithful, and one worth being able to run.

func newStackCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stack",
		Short: "Work with deployed stacks",
	}
	cmd.AddCommand(newStackExportCmd(g), newStackDiffCmd(g), newStackDeployCmd(g), newStackLsCmd(g))
	return cmd
}

func newStackLsCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List the stacks deployed on the cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dcli, err := newDockerClient(cmdContext(cmd), g.dockerContext)
			if err != nil {
				return &cliError{code: session.TransportFailure, err: err}
			}
			names, err := stackfile.StackNames(cmdContext(cmd), dcli)
			if err != nil {
				return &cliError{code: session.TransportFailure, err: err}
			}
			for _, n := range names {
				fmt.Fprintln(cmd.OutOrStdout(), n)
			}
			return nil
		},
	}
}

func newStackExportCmd(g *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "export <stack>",
		Short: "Write a deployed stack out as a compose file",
		Long: "Read a deployed stack back from the cluster and write it as compose-shaped\n" +
			"YAML. Writes to stdout unless -o is given.\n\n" +
			"This is a description of the stack, not a backup of it: a secret's value is\n" +
			"write-only in the engine API and a volume's contents live on the nodes, so\n" +
			"both are declared external. The file says so in its own header.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dcli, err := newDockerClient(cmdContext(cmd), g.dockerContext)
			if err != nil {
				return &cliError{code: session.TransportFailure, err: err}
			}
			st, err := stackfile.FromSwarm(cmdContext(cmd), dcli, args[0])
			if err != nil {
				return &cliError{code: session.TransportFailure, err: err}
			}
			doc, err := st.Document()
			if err != nil {
				return err
			}
			if output == "" {
				_, err = fmt.Fprint(cmd.OutOrStdout(), doc)
				return err
			}
			if err := writeReportFile(output, doc); err != nil {
				return &cliError{code: session.TransportFailure, err: err}
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s — %d service(s)\n", output, len(st.Services))
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "write to this file instead of stdout")
	return cmd
}

func newStackDiffCmd(g *globalFlags) *cobra.Command {
	var stackName string
	cmd := &cobra.Command{
		Use:   "diff <file> [stack]",
		Short: "Compare a stack file against the deployed stack",
		Long: "Show what deploying <file> would change. The stack name defaults to the\n" +
			"file's base name, the same default `docker stack deploy` does not have —\n" +
			"give it explicitly when the file is not named after the stack.\n\n" +
			"A \"+\" line is something deploying the file would add, a \"-\" is something it\n" +
			"would remove. Both sides are normalised the same way, so an unchanged stack\n" +
			"produces an empty diff rather than pages of daemon defaults.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			name := stackName
			if len(args) > 1 {
				name = args[1]
			}
			if name == "" {
				name = stackNameFromPath(path)
			}
			dcli, err := newDockerClient(cmdContext(cmd), g.dockerContext)
			if err != nil {
				return &cliError{code: session.TransportFailure, err: err}
			}
			ctx := cmdContext(cmd)

			deployed, err := stackfile.FromSwarm(ctx, dcli, name)
			if err != nil {
				return &cliError{code: session.TransportFailure, err: err}
			}
			file, err := stackfile.FromFile(ctx, dcli, path, name)
			if err != nil {
				return &cliError{code: usageExitCode, err: err}
			}
			d, err := stackfile.Compare(deployed, file, path)
			if err != nil {
				return err
			}
			// Unsupported compose keys are not differences, but a file that
			// relies on them does not describe what will run.
			if un, uerr := stackfile.UnsupportedProperties(path); uerr == nil && len(un) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"note: swarm ignores these keys in %s: %v\n", path, un)
			}
			fmt.Fprint(cmd.OutOrStdout(), d.Text())
			if !d.Empty() {
				// Same convention as `diff` and `git diff --exit-code`: a
				// difference is a non-zero exit, so this is usable in CI.
				return &cliError{code: 1, silent: true}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&stackName, "stack", "", "stack name to compare against (default: the file's base name)")
	return cmd
}

// stackNameFromPath derives a stack name from a file name the way an operator
// would expect: "prod/web.yml" is the "web" stack.
func stackNameFromPath(path string) string {
	base := path
	if i := lastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		if len(base) > len(ext) && base[len(base)-len(ext):] == ext {
			return base[:len(base)-len(ext)]
		}
	}
	return base
}

func lastIndexAny(s, chars string) int {
	for i := len(s) - 1; i >= 0; i-- {
		for _, c := range chars {
			if rune(s[i]) == c {
				return i
			}
		}
	}
	return -1
}
