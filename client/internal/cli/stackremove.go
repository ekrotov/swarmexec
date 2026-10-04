// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/client"
)

// Removing a stack is the most destructive thing this program does: it is the
// only action that deletes objects the operator never named. So the arithmetic
// — what carries the label, in what order it goes — is a data layer behind an
// interface, provable without a daemon, rather than a handful of API calls
// inside a UI callback.
//
// The semantics are `docker stack rm`'s, deliberately: services, secrets,
// configs and networks bearing com.docker.stack.namespace=<stack>. A tool that
// borrows docker's vocabulary and then deletes a different set of objects would
// be its own kind of surprise — and the operator's mental model of what a stack
// *is* comes from that command.

// stackLabelKey is the only link between an object and its stack: swarm has no
// stack object, and `docker stack deploy` stamps this label on everything it
// creates. Docker's own constant, not ours, which is why a local copy is not
// the kind of duplication that drifts.
const stackLabelKey = "com.docker.stack.namespace"

// stackAPI is the slice of the Docker client a stack removal needs.
// *client.Client satisfies it.
type stackAPI interface {
	ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error)
	ServiceRemove(context.Context, string, client.ServiceRemoveOptions) (client.ServiceRemoveResult, error)
	SecretList(context.Context, client.SecretListOptions) (client.SecretListResult, error)
	SecretRemove(context.Context, string, client.SecretRemoveOptions) (client.SecretRemoveResult, error)
	ConfigList(context.Context, client.ConfigListOptions) (client.ConfigListResult, error)
	ConfigRemove(context.Context, string, client.ConfigRemoveOptions) (client.ConfigRemoveResult, error)
	NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error)
	NetworkRemove(context.Context, string, client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
}

// The assertion lives here so a Docker client upgrade that changes one of these
// signatures breaks at the definition, not at the call site.
var _ stackAPI = (*client.Client)(nil)

// stackObject is one thing a stack owns: the id it is deleted by, and the name
// it is reported as.
type stackObject struct{ ID, Name string }

// stackContents is everything labelled for one stack, held in the order it has
// to be removed in.
type stackContents struct {
	Services []stackObject
	Secrets  []stackObject
	Configs  []stackObject
	Networks []stackObject
}

func (c stackContents) empty() bool {
	return len(c.Services)+len(c.Secrets)+len(c.Configs)+len(c.Networks) == 0
}

// counts renders "4 services, 2 secrets, 1 network" — only what is actually
// there. A confirm that lists "0 configs" invites the reader to skim it.
func (c stackContents) counts() string {
	var parts []string
	for _, p := range []struct {
		n    int
		one  string
		many string
	}{
		{len(c.Services), "service", "services"},
		{len(c.Secrets), "secret", "secrets"},
		{len(c.Configs), "config", "configs"},
		{len(c.Networks), "network", "networks"},
	} {
		switch {
		case p.n == 1:
			parts = append(parts, "1 "+p.one)
		case p.n > 1:
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.many))
		}
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

// stackContentsOf lists everything labelled for stack. Filtered at the daemon,
// so a stack whose name is a prefix of another's cannot be caught by accident.
func stackContentsOf(ctx context.Context, api stackAPI, stack string) (stackContents, error) {
	f := make(client.Filters).Add("label", stackLabelKey+"="+stack)
	var c stackContents

	svcsRes, err := api.ServiceList(ctx, client.ServiceListOptions{Filters: f})
	svcs := svcsRes.Items
	if err != nil {
		return c, fmt.Errorf("list services: %w", err)
	}
	for _, s := range svcs {
		c.Services = append(c.Services, stackObject{ID: s.ID, Name: s.Spec.Name})
	}

	secsRes, err := api.SecretList(ctx, client.SecretListOptions{Filters: f})
	secs := secsRes.Items
	if err != nil {
		return c, fmt.Errorf("list secrets: %w", err)
	}
	for _, s := range secs {
		c.Secrets = append(c.Secrets, stackObject{ID: s.ID, Name: s.Spec.Name})
	}

	cfgsRes, err := api.ConfigList(ctx, client.ConfigListOptions{Filters: f})
	cfgs := cfgsRes.Items
	if err != nil {
		return c, fmt.Errorf("list configs: %w", err)
	}
	for _, cf := range cfgs {
		c.Configs = append(c.Configs, stackObject{ID: cf.ID, Name: cf.Spec.Name})
	}

	netsRes, err := api.NetworkList(ctx, client.NetworkListOptions{Filters: f})
	nets := netsRes.Items
	if err != nil {
		return c, fmt.Errorf("list networks: %w", err)
	}
	for _, n := range nets {
		c.Networks = append(c.Networks, stackObject{ID: n.ID, Name: n.Name})
	}
	return c, nil
}

// networkRetries and networkRetryWait bound the wait for a network whose
// endpoints have not drained yet.
//
// Removing a service is not synchronous with its tasks going away, so the first
// attempt at the stack's network routinely fails with "has active endpoints".
// `docker stack rm` reports exactly that and gives up, which leaves the
// operator with a stack that is removed except for a network they now have to
// clean up by hand. Waiting a few seconds turns the common case into a complete
// removal; a network that is still busy after that is genuinely held by
// something outside this stack, and then the message is the right outcome.
var (
	networkRetries   = 6
	networkRetryWait = time.Second
)

// removeStack deletes everything in c, in docker's order, and returns one error
// per object that refused.
//
// It does not stop at the first failure. A stack left half-removed because one
// secret is still referenced elsewhere is worse than one removed as far as it
// can be, with a report of exactly what would not go.
func removeStack(ctx context.Context, api stackAPI, c stackContents) []error {
	var errs []error
	fail := func(kind string, o stackObject, err error) {
		errs = append(errs, fmt.Errorf("%s %s: %w", kind, o.Name, err))
	}

	// Services first: while they exist, everything else is in use by definition.
	for _, o := range c.Services {
		if _, err := api.ServiceRemove(ctx, o.ID, client.ServiceRemoveOptions{}); err != nil {
			fail("service", o, err)
		}
	}
	for _, o := range c.Secrets {
		if _, err := api.SecretRemove(ctx, o.ID, client.SecretRemoveOptions{}); err != nil {
			fail("secret", o, err)
		}
	}
	for _, o := range c.Configs {
		if _, err := api.ConfigRemove(ctx, o.ID, client.ConfigRemoveOptions{}); err != nil {
			fail("config", o, err)
		}
	}
	// Networks last, and patiently: see networkRetries.
	for _, o := range c.Networks {
		var err error
		for attempt := 0; attempt < networkRetries; attempt++ {
			if _, err = api.NetworkRemove(ctx, o.ID, client.NetworkRemoveOptions{}); err == nil || ctx.Err() != nil {
				break
			}
			if attempt == networkRetries-1 {
				break // no point sleeping after the last attempt
			}
			select {
			case <-ctx.Done():
			case <-time.After(networkRetryWait):
			}
		}
		if err != nil {
			fail("network", o, err)
		}
	}
	return errs
}
