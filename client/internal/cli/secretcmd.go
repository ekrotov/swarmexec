// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/session"
	cterm "swarmexec/client/internal/term"
)

func newSecretCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "secret",
		Aliases: []string{"secrets"},
		Short:   "Work with swarm secrets",
	}
	cmd.AddCommand(newSecretRotateCmd(g))
	return cmd
}

type secretRotateFlags struct {
	fromFile  string
	fromStdin bool
	yes       bool
	timeout   time.Duration
}

func newSecretRotateCmd(g *globalFlags) *cobra.Command {
	f := &secretRotateFlags{}
	cmd := &cobra.Command{
		Use:   "rotate <name> (--from-file FILE | --from-stdin)",
		Short: "Replace a secret's content, keeping its name",
		Long: "Give a secret new content without renaming it, so stack files that name it\n" +
			"stay valid. Swarm secrets are immutable and cannot be removed while in use,\n" +
			"so every service using it is parked on a temporary secret meanwhile:\n\n" +
			"  1  create <name>_rotating with the new content (same labels + a marker)\n" +
			"  2  switch every service to <name>_rotating          (rolling update)\n" +
			"  3  remove the old <name>\n" +
			"  4  create <name> again: new content, original labels\n" +
			"  5  switch every service back to <name>              (rolling update)\n" +
			"  6  remove <name>_rotating\n\n" +
			"Only the secret behind each mount changes: target path, uid, gid and mode\n" +
			"stay. Every service using the secret restarts twice. There is no rollback\n" +
			"to the old content — the old secret object is deleted.\n\n" +
			"The value is sent byte for byte (nothing trimmed or added), must not be\n" +
			"empty and is limited to 500 KB. It is never printed.\n\n" +
			"If a run stops half-way, the services keep running on <name>_rotating,\n" +
			"which holds the new content. Run the same command again to finish: it\n" +
			"reads where the last run stopped and continues there.\n\n" +
			"If the secret came from a stack file with `file:`, put the new content in\n" +
			"that file too, or the next `docker stack deploy` fails on the changed data.\n\n" +
			"Without --yes, the services that will restart are listed and a y/N answer\n" +
			"is read from the terminal; without a terminal, --yes is required.\n\n" +
			"Exit code 0 means rotated, 1 a failure (the message names the step and, when\n" +
			"there is something to resume, how), 2 a usage error, 125 an unreachable manager.",
		Example: "  swarmexec secret rotate db_password --from-file ./db_password.txt\n" +
			"  printf %s \"$NEW\" | swarmexec secret rotate api_token --from-stdin --yes",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretRotate(cmd, g, f, args[0])
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.fromFile, "from-file", "", "read the new value from this file")
	fl.BoolVar(&f.fromStdin, "from-stdin", false, "read the new value from stdin")
	fl.BoolVarP(&f.yes, "yes", "y", false, "do not ask for confirmation")
	fl.DurationVar(&f.timeout, "timeout", defaultRotateTimeout, "how long to wait for each service's rolling update")
	return cmd
}

// readRotateValue reads the new value from the one source the flags name.
func readRotateValue(f *secretRotateFlags, stdin io.Reader) ([]byte, error) {
	switch {
	case f.fromFile != "" && f.fromStdin:
		return nil, errors.New("give --from-file or --from-stdin, not both")
	case f.fromFile != "":
		fi, err := os.Stat(f.fromFile)
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", f.fromFile)
		}
		r, err := os.Open(f.fromFile)
		if err != nil {
			return nil, err
		}
		defer func() { _ = r.Close() }()
		return io.ReadAll(io.LimitReader(r, maxSecretSize+1))
	case f.fromStdin:
		return io.ReadAll(io.LimitReader(stdin, maxSecretSize+1))
	}
	return nil, errors.New("the new value is required: --from-file FILE or --from-stdin")
}

// rotateConfirmText lists what a rotation does, for the CLI prompt and the
// TUI confirm alike.
func rotateConfirmText(p rotatePlan) string {
	var b strings.Builder
	if p.Resume {
		fmt.Fprintf(&b, "Resuming an interrupted rotation of %s at step %d/%d.\n", p.Name, p.StartStep, rotateSteps)
		if p.StartStep == 5 {
			fmt.Fprintf(&b, "%s was already re-created by that run and keeps its content: the value given now is not used.\n", p.Name)
		}
		b.WriteString("\n")
	}
	if len(p.Users) == 0 {
		fmt.Fprintf(&b, "No service uses %s: it is replaced, nothing restarts.\n", p.Name)
	} else {
		fmt.Fprintf(&b, "Services using %s (%d):\n", p.Name, len(p.Users))
		for _, u := range p.Users {
			targets := make([]string, len(u.Targets))
			for i, t := range u.Targets {
				targets[i] = secretTargetPath(t)
			}
			note := ""
			if p.Resume {
				switch {
				case u.OnTemp:
					note = "  (on " + p.Temp + " — restarts once more)"
				case p.StartStep == 5:
					note = "  (already done)"
				default:
					note = "  (restarts twice)"
				}
			}
			fmt.Fprintf(&b, "  %s → %s%s\n", u.Name, strings.Join(targets, ", "), note)
		}
		if p.Resume {
			b.WriteString("\nA fresh rotation restarts each service twice (two rolling updates);\nthis one finishes what the interrupted run left.\n")
		} else {
			b.WriteString("\nEach service restarts twice (two rolling updates).\n")
		}
	}
	b.WriteString("No rollback to the old content: the old secret object is deleted.\n")
	if p.Stack != "" {
		fmt.Fprintf(&b, "\nHint: %s belongs to stack %q. If the stack file defines it with `file:`,\n"+
			"put the new content in that file too, or the next stack deploy fails.\n", p.Name, p.Stack)
	}
	return b.String()
}

// secretTargetPath is where a target ends up in the container: a bare name is
// under /run/secrets, an absolute path is used as is.
func secretTargetPath(t string) string {
	if strings.HasPrefix(t, "/") {
		return t
	}
	return "/run/secrets/" + t
}

func runSecretRotate(cmd *cobra.Command, g *globalFlags, f *secretRotateFlags, name string) error {
	if err := validateRotateName(name); err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	if f.timeout <= 0 {
		return &cliError{code: usageExitCode, err: errors.New("--timeout must be positive")}
	}
	data, err := readRotateValue(f, cmd.InOrStdin())
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	if err := validateRotateValue(data); err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	ctx := cmdContext(cmd)
	dcli, err := newDockerClient(ctx, g.dockerContext)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	plan, err := planRotation(ctx, dcli, name)
	if err != nil {
		return &cliError{code: 1, err: err}
	}
	stderr := cmd.ErrOrStderr()
	if !f.yes {
		if f.fromStdin || !cterm.IsTerminal(os.Stdin.Fd()) {
			return &cliError{code: usageExitCode, err: errors.New("not asking for confirmation without a terminal (stdin is the value or not a TTY) — pass --yes")}
		}
		fmt.Fprint(stderr, rotateConfirmText(plan))
		if !confirm(fmt.Sprintf("Rotate secret %s?", name)) {
			return &cliError{code: 1, err: errors.New("aborted")}
		}
	}
	err = rotateSecret(ctx, dcli, name, data, rotateOptions{
		Timeout:  f.timeout,
		Progress: func(e rotateEvent) { fmt.Fprintln(stderr, e.String()) },
	})
	if err != nil {
		if hint := rotateResumeHint(err, "re-run `swarmexec secret rotate "+name+"`"); hint != "" {
			err = fmt.Errorf("%w\n%s", err, hint)
		}
		return &cliError{code: 1, err: err}
	}
	fmt.Fprintf(stderr, "rotated secret %s\n", name)
	return nil
}
