// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/swarmops"
)

// `stack rm` is the cli twin of the TUI's stack removal (review P-02): the
// same swarmops listing and removal, docker's semantics — everything carrying
// the stack's label — with the confirm naming each object before anything
// goes.

func newStackRmCmd(g *globalFlags) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rm <stack>",
		Short: "Remove a stack: its services, secrets, configs and networks",
		Long: "Remove everything `docker stack rm` would: the services, secrets, configs\n" +
			"and networks labelled com.docker.stack.namespace=<stack>. It lists them and\n" +
			"asks first; --yes answers in advance, and without a terminal and without\n" +
			"--yes nothing is removed.\n\n" +
			"Unlike docker it waits a few seconds for a network whose endpoints are still\n" +
			"draining, and when something will not go it removes the rest and names\n" +
			"exactly what is left behind (exit 125).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStackRm(cmd, g, args[0], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not prompt for confirmation")
	return cmd
}

func runStackRm(cmd *cobra.Command, g *globalFlags, stack string, yes bool) error {
	ctx := cmdContext(cmd)
	dcli, err := newDockerClient(ctx, g.dockerContext)
	if err != nil {
		return &cliError{code: exitTransport, err: err}
	}
	defer dcli.Close()
	contents, err := swarmops.StackContentsOf(ctx, dcli, stack)
	if err != nil {
		return &cliError{code: exitTransport, err: fmt.Errorf("read stack %s: %w", stack, err)}
	}
	if contents.Empty() {
		return &cliError{code: exitTransport, err: fmt.Errorf("nothing labelled for stack %q — nothing to remove", stack)}
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	fmt.Fprintf(out, "stack %q holds %s:\n", stack, contents.Counts())
	writeStackContents(out, contents)
	if !yes && !confirm(fmt.Sprintf("Remove stack %q? All its tasks stop; this cannot be undone.", stack)) {
		fmt.Fprintln(errOut, "aborted — nothing was removed (non-interactive runs need --yes)")
		return &cliError{code: exitTransport, silent: true}
	}
	errs := swarmops.RemoveStack(ctx, dcli, contents)
	if len(errs) == 0 {
		fmt.Fprintf(out, "removed stack %q (%s)\n", stack, contents.Counts())
		return nil
	}
	// Say what did NOT go: the rest is already gone.
	fmt.Fprintf(errOut, "stack %q removed, except:\n", stack)
	for _, e := range errs {
		fmt.Fprintf(errOut, "  %s\n", e)
	}
	return &cliError{code: exitTransport, silent: true}
}

// writeStackContents lists a stack's objects, one per line, in removal order.
func writeStackContents(w io.Writer, c swarmops.StackContents) {
	for _, it := range c.Items() {
		fmt.Fprintf(w, "  %-8s %s\n", it.Kind, it.Name)
	}
}

// --- stack check / security check --------------------------------------------

// newStackCheckCmd is `stack deploy --check` under its own name (review M-01,
// P-02b): the MCP tool is stack_check, and the security group lists it as
// `security check`.
func newStackCheckCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "check <file> [stack]",
		Short: "Check a stack file for security antipatterns without deploying it",
		Long: "Load a stack file, run the security checks `stack deploy` runs over what it\n" +
			"would actually deploy, and print the findings. Never deploys. The stack\n" +
			"name defaults to the file's base name.\n\n" +
			"Exit code 0 means nothing above informational, 1 a finding a deploy would\n" +
			"stop at — so it can gate a pipeline. Same as `stack deploy --check`, and\n" +
			"the MCP tool stack_check.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStackDeploy(cmd, g, &stackDeployFlags{check: true}, args)
		},
	}
}
