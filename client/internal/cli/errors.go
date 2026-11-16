// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import "github.com/spf13/cobra"

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

// usageExitCode is returned for flag/usage errors, mirroring conventional tools.
const usageExitCode = 2

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
