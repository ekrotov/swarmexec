// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/session"
)

// cliError carries an exit code out of a cobra RunE. A silent error prints
// nothing (used for a remote command's own non-zero exit, where the stream has
// already produced all relevant output). A non-silent error prints its message
// to stderr. Transport/agent failures use session.TransportFailure (125) so
// scripts can distinguish them from a remote exit code (REQUIREMENTS §6).
type cliError struct {
	code   int
	err    error
	silent bool
}

func (e *cliError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return ""
}

func (e *cliError) Unwrap() error { return e.err }

// The process exit codes. This is the one table: every command resolves its
// outcome to one of these, reportError applies it, the site's exit-code table
// is generated from exitTable and a test fails when the two disagree.
const (
	exitOK          = 0
	exitNo          = 1 // a command-specific "no": a difference, a failed wait, a declined confirmation
	exitUsage       = 2 // flag / argument / configuration error
	exitTransport   = session.TransportFailure
	exitHangup      = 129 // 128 + SIGHUP
	exitInterrupted = 130 // 128 + SIGINT
	exitTerminated  = 143 // 128 + SIGTERM
)

// exitRow is one line of the documented exit-code table. Meaning is HTML, as
// it is pasted into site/docs/configuration.html verbatim.
type exitRow struct {
	Code    string // "0", "1", ... or "N" for a propagated remote code
	Meaning string
}

// exitTable documents every exit code swarmexec returns, in display order.
var exitTable = []exitRow{
	{"0", `success`},
	{"1", `a command-specific "no": <code>stack diff</code> found a difference (and <code>--json</code> still printed it), <code>stack deploy</code>/<code>stack check</code> found a problem, <code>wait</code> did not converge, <code>secret rotate</code> failed, <code>mcp</code> ended with an error, a <code>cp</code> the agent refused (policy, the size cap, no such path), or you declined a confirmation (<code>down</code>, <code>volume rm</code>, <code>secret rotate</code>)`},
	{"2", `usage / flag / configuration error, a mistyped subcommand under a group (<code>swarmexec stack chekc</code>: <code>unknown command "chekc" for "swarmexec stack"</code>), or an invalid interactive selection. A wrong number of arguments also prints the command's usage line (<code>usage: swarmexec exec [flags] &lt;service|…&gt; …</code>)`},
	{"125", `transport failure or any other runtime error — unreachable manager or agent, dial/TLS error, stream error, or a read command (<code>volume ls</code>, <code>stats</code>, <code>doctor</code>) that no agent answered, so an empty result cannot be mistaken for “nothing there” (mirrors Docker's <code>125</code>)`},
	{"129", `stopped by SIGHUP (the terminal went away) while a command ran`},
	{"130", `interrupted with Ctrl-C (SIGINT): the command's context is cancelled, its cleanups run, then it exits. A second Ctrl-C ends the process immediately`},
	{"143", `stopped by SIGTERM, handled like Ctrl-C`},
	{"N", `for <code>exec</code>, the remote command's own non-zero exit code, propagated verbatim`},
}

// ExitTableHTML renders exitTable as the rows of the site's exit-code table.
func ExitTableHTML() string {
	var b strings.Builder
	for _, r := range exitTable {
		fmt.Fprintf(&b, "          <tr><td><code>%s</code></td><td>%s</td></tr>\n", r.Code, r.Meaning)
	}
	return b.String()
}

// exitCodeForSignal is the conventional 128+n code for a stop signal.
func exitCodeForSignal(sig os.Signal) int {
	switch sig {
	case syscall.SIGTERM:
		return exitTerminated
	case syscall.SIGHUP:
		return exitHangup
	}
	return exitInterrupted
}

// runtimeErrors makes every command's RunE resolve to the table: an error that
// is already a cliError or an argError keeps its code, anything else is a
// runtime failure (125), not a usage error. What cobra itself rejects (flags,
// required flags, unknown commands) never passes through a RunE and so
// stays a usage error in reportError.
func runtimeErrors(c *cobra.Command) {
	for _, sub := range c.Commands() {
		runtimeErrors(sub)
	}
	run := c.RunE
	if run == nil {
		return
	}
	c.RunE = func(cmd *cobra.Command, args []string) error {
		err := run(cmd, args)
		var ce *cliError
		var ae *argError
		if err == nil || errors.As(err, &ce) || errors.As(err, &ae) {
			return err
		}
		return &cliError{code: exitTransport, err: err}
	}
}

// argError is a positional-argument error from a command's Args validator,
// carrying the command's usage line. Cobra's own message ("requires at least
// 1 arg(s), only received 0") says what is wrong but not what was expected,
// and SilenceUsage keeps cobra from printing the usage itself — it would
// otherwise follow every runtime error too.
type argError struct {
	err   error
	usage string
}

func (e *argError) Error() string { return e.err.Error() }

func (e *argError) Unwrap() error { return e.err }

// usageOnArgErrors wraps the Args validator of c and every command under it,
// so an argument-count error carries the command's usage line out to Execute.
// One wrapper over the tree rather than a line in each command: a command
// added later gets it without anyone remembering to.
func usageOnArgErrors(c *cobra.Command) {
	for _, sub := range c.Commands() {
		usageOnArgErrors(sub)
	}
	validate := c.Args
	if validate == nil {
		return
	}
	c.Args = func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return &argError{err: err, usage: cmd.UseLine()}
		}
		return nil
	}
}
