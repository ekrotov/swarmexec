// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/resolve"
)

// Dynamic shell completion: `swarmexec exec <TAB>` offers the services that
// exist, `logs web.<TAB>` the slots, `--stack <TAB>` the stacks, `--context
// <TAB>` the docker contexts. Cobra already generates the shell scripts
// (`swarmexec completion bash|zsh|fish|powershell`); this tells them what the
// candidates are.
//
// Two rules make a completion safe to press. It has a hard deadline, because
// the shell waits on it and a completion that hangs is worse than none. And it
// never prompts and never prints an error: it runs without a terminal, so a
// failure offers nothing, quietly. Only the manager API is asked — no agent.

const completionTimeout = 2 * time.Second

type completeFunc = func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective)

// registerCompletions attaches the candidate lists to the finished command
// tree, by command path, so the commands themselves stay as they were.
func registerCompletions(root *cobra.Command, g *globalFlags) {
	args, flags := completionTargets(g)
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if f, ok := args[c.CommandPath()]; ok {
			c.ValidArgsFunction = f
		}
		for name, f := range flags[c.CommandPath()] {
			if c.Flags().Lookup(name) != nil {
				_ = c.RegisterFlagCompletionFunc(name, f)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	_ = root.RegisterFlagCompletionFunc("context", completeContexts)
}

// completionTargets is which command completes what, by command path.
func completionTargets(g *globalFlags) (map[string]completeFunc, map[string]map[string]completeFunc) {
	services := completeServices(g, false)
	firstService := onlyFirstArg(completeServices(g, false))
	stacks := completeStacks(g)
	args := map[string]completeFunc{
		"swarmexec exec":         firstService,
		"swarmexec port-forward": firstService,
		"swarmexec logs":         completeServices(g, true),
		"swarmexec ps":           onlyFirstArg(services),
		"swarmexec ui":           onlyFirstArg(services),
		"swarmexec wait":         services,
		"swarmexec stack export": onlyFirstArg(stacks),
		"swarmexec context use":  onlyFirstArg(completeContexts),
		"swarmexec context rm":   completeContexts,
	}
	flags := map[string]map[string]completeFunc{
		"swarmexec logs":       {"stack": stacks},
		"swarmexec wait":       {"stack": stacks},
		"swarmexec stack diff": {"stack": stacks},
	}
	return args, flags
}

// completionClient connects to the manager with the completion deadline and
// with ssh forbidden to prompt.
func completionClient(ctx context.Context, g *globalFlags) (*client.Client, bool) {
	sshNonInteractive.Store(true)
	dcli, err := resolveEndpoint(g.dockerContext).connect(ctx)
	if err != nil {
		return nil, false
	}
	return dcli, true
}

// completionServices lists the bare services — names and stack labels only.
// Not resolve.Services: that also fetches task status and runs the security
// analyzers over every spec, which is right for the tree and far too slow for
// a key press (it ran past the deadline on a cluster with ~60 services).
func completionServices(ctx context.Context, dcli *client.Client) ([]swarm.Service, error) {
	return dcli.ServiceList(ctx, types.ServiceListOptions{})
}

// completeServices offers service names; with slots, a word that already
// names a service and a dot offers that service's running slots.
func completeServices(g *globalFlags, slots bool) completeFunc {
	return func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		ctx, cancel := context.WithTimeout(context.Background(), completionTimeout)
		defer cancel()
		dcli, ok := completionClient(ctx, g)
		if !ok {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		if slots {
			if i := strings.LastIndex(toComplete, "."); i > 0 {
				r := resolve.New(dcli, resolve.AddrHostname)
				if cands, err := r.Candidates(ctx, toComplete[:i]); err == nil {
					return slotNames(cands), cobra.ShellCompDirectiveNoFileComp
				}
			}
		}
		svcs, err := completionServices(ctx, dcli)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		names := make([]string, 0, len(svcs))
		for _, s := range svcs {
			names = append(names, s.Spec.Name)
		}
		sort.Strings(names)
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}

func slotNames(cands []resolve.Candidate) []string {
	var out []string
	for _, c := range cands {
		if c.Slot > 0 {
			out = append(out, c.Service+"."+strconv.Itoa(c.Slot))
		}
	}
	sort.Strings(out)
	return out
}

// completeStacks offers the names of deployed stacks.
func completeStacks(g *globalFlags) completeFunc {
	return func(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		ctx, cancel := context.WithTimeout(context.Background(), completionTimeout)
		defer cancel()
		dcli, ok := completionClient(ctx, g)
		if !ok {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		svcs, err := completionServices(ctx, dcli)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		seen := map[string]bool{}
		var out []string
		for _, s := range svcs {
			if st := s.Spec.Labels["com.docker.stack.namespace"]; st != "" && !seen[st] {
				seen[st] = true
				out = append(out, st)
			}
		}
		sort.Strings(out)
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeContexts offers docker contexts. Local files only, no network.
func completeContexts(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	cs, err := dockerctx.List()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// onlyFirstArg completes the first positional argument and nothing after it —
// exec's later arguments are the command to run, not names.
func onlyFirstArg(f completeFunc) completeFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveDefault
		}
		return f(cmd, args, toComplete)
	}
}
