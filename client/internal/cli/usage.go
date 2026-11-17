// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Grouped flag help (review C-03). Every command inherits the root's ten
// connection flags — TLS files, the agent secret, how a node is dialled —
// and cobra printed all of them under every command's help, where they made
// up most of the text and buried the two or three flags everybody uses. The
// connection flags carry an annotation; a command's help lists the everyday
// global flags and then points at `swarmexec help connection`, which (with
// the root's --help) is the one place they are written out in full.

// flagGroupAnnotation marks a flag's help group; connectionGroup is the value
// for the connection flags.
const (
	flagGroupAnnotation = "swarmexec/flag-group"
	connectionGroup     = "connection"
)

// connectionHelpTopic is the help topic command listing the connection flags.
const connectionHelpTopic = "connection"

// markConnectionFlags puts the named flags of fs into the connection group.
func markConnectionFlags(fs *pflag.FlagSet, names ...string) {
	for _, n := range names {
		if err := fs.SetAnnotation(n, flagGroupAnnotation, []string{connectionGroup}); err != nil {
			panic(err) // a typo in a flag name: fail every test, not silently
		}
	}
}

// isConnectionFlag reports whether f is in the connection group.
func isConnectionFlag(f *pflag.Flag) bool {
	for _, g := range f.Annotations[flagGroupAnnotation] {
		if g == connectionGroup {
			return true
		}
	}
	return false
}

// splitFlags returns fs's visible flags in two sets: the connection group and
// the rest.
func splitFlags(fs *pflag.FlagSet) (rest, conn *pflag.FlagSet) {
	rest = pflag.NewFlagSet("rest", pflag.ContinueOnError)
	conn = pflag.NewFlagSet("connection", pflag.ContinueOnError)
	rest.SortFlags, conn.SortFlags = fs.SortFlags, fs.SortFlags
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		if isConnectionFlag(f) {
			conn.AddFlag(f)
		} else {
			rest.AddFlag(f)
		}
	})
	return rest, conn
}

// connectionFlagsText is the connection group's flag table, as the root's
// help and the help topic print it.
func connectionFlagsText(root *cobra.Command) string {
	_, conn := splitFlags(root.PersistentFlags())
	return strings.TrimRight(conn.FlagUsages(), " \n")
}

// newConnectionHelpTopic is `swarmexec help connection`: a command without a
// Run, which cobra lists under "Additional help topics" and prints with
// `help`. Built after the root's flags are defined, from those flags.
func newConnectionHelpTopic(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   connectionHelpTopic,
		Short: "Flags for reaching the agents: TLS, the agent secret, node addressing",
		Long: `Connection flags reach the swarmexec agents on the nodes. Every command takes
them; they override the matching config file keys and SWARMEXEC_* variables
(see "swarmexec config show"). Most setups set them once in the config file
and never on the command line.

Connection Flags:
` + connectionFlagsText(root),
	}
}

// groupedUsage is the usage function for the whole tree: cobra's default
// layout, with the connection flags split off the inherited ones. The root
// lists them in full; every other command names the help topic instead.
func groupedUsage(c *cobra.Command) error {
	w := c.OutOrStderr()
	fmt.Fprint(w, "Usage:")
	if c.Runnable() {
		fmt.Fprintf(w, "\n  %s", c.UseLine())
	}
	if c.HasAvailableSubCommands() {
		fmt.Fprintf(w, "\n  %s [command]", c.CommandPath())
	}
	if len(c.Aliases) > 0 {
		fmt.Fprintf(w, "\n\nAliases:\n  %s", c.NameAndAliases())
	}
	if c.HasExample() {
		fmt.Fprintf(w, "\n\nExamples:\n%s", c.Example)
	}
	if c.HasAvailableSubCommands() {
		fmt.Fprint(w, "\n\nAvailable Commands:")
		for _, sub := range c.Commands() {
			if sub.IsAvailableCommand() || sub.Name() == "help" {
				fmt.Fprintf(w, "\n  %-*s %s", sub.NamePadding(), sub.Name(), sub.Short)
			}
		}
	}
	local, localConn := splitFlags(c.LocalFlags())
	inherited, inheritedConn := splitFlags(c.InheritedFlags())
	writeFlags(w, "Flags", local)
	writeFlags(w, "Connection Flags", localConn)
	writeFlags(w, "Global Flags", inherited)
	if inheritedConn.HasFlags() {
		fmt.Fprintf(w, "\n\nConnection flags (--ca, --agent-secret, --port, …): see \"%s help %s\"",
			c.Root().Name(), connectionHelpTopic)
	}
	if c.HasHelpSubCommands() {
		fmt.Fprint(w, "\n\nAdditional help topics:")
		for _, sub := range c.Commands() {
			if sub.IsAdditionalHelpTopicCommand() {
				fmt.Fprintf(w, "\n  %-*s %s", sub.CommandPathPadding(), sub.CommandPath(), sub.Short)
			}
		}
	}
	if c.HasAvailableSubCommands() {
		fmt.Fprintf(w, "\n\nUse \"%s [command] --help\" for more information about a command.", c.CommandPath())
	}
	fmt.Fprintln(w)
	return nil
}

// writeFlags prints one flag group under its title, nothing when it is empty.
func writeFlags(w io.Writer, title string, fs *pflag.FlagSet) {
	if !fs.HasFlags() {
		return
	}
	fmt.Fprintf(w, "\n\n%s:\n%s", title, strings.TrimRight(fs.FlagUsages(), " \n"))
}
