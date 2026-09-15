// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/secscan"
	"swarmexec/client/internal/session"
	"swarmexec/client/internal/stackfile"
)

// `stack deploy` is the write half. What makes it worth having over
// `docker stack deploy` is the gate: the file is checked BEFORE anything is
// created, with the same analyzers that badge the tree, and an operator who
// does not like what they see can still walk away.
//
// The check is not advisory noise. It runs on the converted spec — what the
// cluster will actually do — and it stops by default. A warning that scrolls
// past while the deploy proceeds is a warning nobody reads.

type stackDeployFlags struct {
	prune bool
	force bool
	yes   bool
	check bool
}

func newStackDeployCmd(g *globalFlags) *cobra.Command {
	f := &stackDeployFlags{}
	cmd := &cobra.Command{
		Use:   "deploy <file> [stack]",
		Short: "Deploy a stack file, after checking it for security antipatterns",
		Long: "Load a stack file, run the security checks over what it would actually\n" +
			"deploy, and apply it. The stack name defaults to the file's base name.\n\n" +
			"If anything above informational is found the deploy STOPS and asks. Use\n" +
			"--yes to answer in advance (for CI), or --check to look without deploying.\n" +
			"--force deploys despite findings without asking — say it deliberately.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStackDeploy(cmd, g, f, args)
		},
	}
	cmd.Flags().BoolVar(&f.prune, "prune", false, "remove services of the stack the file no longer declares")
	cmd.Flags().BoolVar(&f.force, "force", false, "deploy even though the check found something, without asking")
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "do not ask; deploy if the check found nothing actionable")
	cmd.Flags().BoolVar(&f.check, "check", false, "only run the checks, never deploy")
	return cmd
}

func runStackDeploy(cmd *cobra.Command, g *globalFlags, f *stackDeployFlags, args []string) error {
	path := args[0]
	name := stackNameFromPath(path)
	if len(args) > 1 {
		name = args[1]
	}
	ctx := cmdContext(cmd)
	dcli, err := newDockerClient(ctx, g.dockerContext)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	plan, err := stackfile.PlanFile(ctx, dcli, path, name)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	writePlanReport(out, plan)

	if f.check {
		if plan.Actionable() {
			// A checking mode that always exits 0 cannot gate anything.
			return &cliError{code: 1, silent: true}
		}
		return nil
	}

	if plan.Actionable() && !f.force {
		if !f.yes && !confirmDeploy(cmd, plan) {
			fmt.Fprintln(errOut, "aborted — nothing was deployed")
			return &cliError{code: 1, silent: true}
		}
		if f.yes {
			// --yes means "do not ask", not "ignore what you found". Saying
			// otherwise would turn a security gate into a formality the first
			// time someone put it in CI.
			fmt.Fprintln(errOut, "refusing to deploy: the check found something above informational.")
			fmt.Fprintln(errOut, "Fix the file, or deploy deliberately with --force.")
			return &cliError{code: 1, silent: true}
		}
	}

	res, err := stackfile.Apply(ctx, dcli, name, plan.Config, stackfile.ApplyOptions{Prune: f.prune})
	if err != nil {
		// A partially applied stack is the dangerous state, so say what DID
		// happen before the error rather than only the error.
		if s := res.Summary(); s != "nothing to do" {
			fmt.Fprintf(errOut, "partially applied before the failure: %s\n", s)
		}
		return &cliError{code: session.TransportFailure, err: err}
	}
	fmt.Fprintf(out, "deployed %s — %s\n", name, res.Summary())
	return nil
}

// writePlanReport prints what the checks found, worst first.
func writePlanReport(w io.Writer, plan *stackfile.Plan) {
	if len(plan.Unsupported) > 0 {
		fmt.Fprintf(w, "note: swarm ignores these keys in %s: %s\n\n",
			plan.Path, strings.Join(plan.Unsupported, ", "))
	}
	flagged := plan.Flagged()
	if len(flagged) == 0 {
		// Name what was looked for, or "nothing found" is a claim with nothing
		// behind it — the same rule the risks overlay follows.
		fmt.Fprintf(w, "%d service(s) checked, nothing above informational.\n", len(plan.Services))
		fmt.Fprintf(w, "Checked: %s.\n\n", strings.Join(secscan.Checks(), ", "))
		return
	}
	fmt.Fprintf(w, "%d of %d service(s) flagged in %s:\n\n", len(flagged), len(plan.Services), plan.Path)
	for _, s := range flagged {
		fmt.Fprintf(w, "  %s\n", s.Name)
		for _, find := range s.Findings {
			if find.Severity == secscan.SevLow {
				continue // informational; true of nearly every service
			}
			fmt.Fprintf(w, "    %-7s %s — %s\n", find.Severity, find.Title, find.Detail)
		}
		fmt.Fprintln(w)
	}
}

// confirmDeploy asks, on the terminal, whether to go ahead anyway.
func confirmDeploy(cmd *cobra.Command, plan *stackfile.Plan) bool {
	in := cmd.InOrStdin()
	fmt.Fprintf(cmd.ErrOrStderr(), "Deploy %s anyway? [y/N] ", plan.Stack)
	var answer string
	if _, err := fmt.Fscanln(in, &answer); err != nil {
		// No answer — a closed or non-interactive stdin — is a no. Defaulting
		// the other way would make an unattended run deploy a flagged file.
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
