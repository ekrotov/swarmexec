// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"context"
	"fmt"
	"sort"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/cli/cli/compose/convert"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// Deploying a stack is CLIENT-side work: the engine has no "deploy a stack"
// call, only the pieces. `docker stack deploy` does this same dance inside the
// docker CLI, where every function of it is unexported and tied to that CLI's
// own interfaces — so it is rebuilt here rather than imported.
//
// The order below is the part that matters and is not obvious: networks,
// secrets and configs must exist before a service can reference them, and a
// service that references something missing fails to create, leaving the stack
// half-applied. The conversion itself is docker's own, so what gets submitted
// is what `docker stack deploy` would submit.

// defaultNetworkDriver is what a stack network gets when the file does not say.
// Not compose's default ("bridge"), which is not swarm-scoped and which a
// service therefore cannot attach to.
const defaultNetworkDriver = "overlay"

// ApplyResult is what a deploy did, for reporting it afterwards.
type ApplyResult struct {
	Created  []string // services that did not exist before
	Updated  []string // services that did
	Networks []string // networks created
	Secrets  []string // secrets created
	Configs  []string // configs created
	Removed  []string // services pruned (only with Prune)
}

// ApplyOptions mirrors the flags of `docker stack deploy` that make sense here.
type ApplyOptions struct {
	// Prune removes services of this stack that the file no longer declares.
	// Off by default, exactly as docker has it: a file that is a subset of the
	// stack is far more often an accident than an instruction to delete.
	Prune bool
}

// deployClient is the slice of the docker client a deploy needs.
type deployClient interface {
	swarmReader
	NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	NetworkInspect(ctx context.Context, id string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	SecretCreate(ctx context.Context, options client.SecretCreateOptions) (client.SecretCreateResult, error)
	ConfigCreate(ctx context.Context, options client.ConfigCreateOptions) (client.ConfigCreateResult, error)
	ServiceCreate(ctx context.Context, options client.ServiceCreateOptions) (client.ServiceCreateResult, error)
	ServiceUpdate(ctx context.Context, serviceID string, options client.ServiceUpdateOptions) (client.ServiceUpdateResult, error)
	ServiceRemove(ctx context.Context, serviceID string, options client.ServiceRemoveOptions) (client.ServiceRemoveResult, error)
}

// Apply applies a loaded stack file to the cluster.
func Apply(ctx context.Context, cli *client.Client, stackName string, cfg *composetypes.Config, opts ApplyOptions) (*ApplyResult, error) {
	ns := convert.NewNamespace(stackName)
	res := &ApplyResult{}

	if err := deployNetworks(ctx, cli, ns, cfg, res); err != nil {
		return res, err
	}
	if err := deploySecrets(ctx, cli, ns, cfg, res); err != nil {
		return res, err
	}
	if err := deployConfigs(ctx, cli, ns, cfg, res); err != nil {
		return res, err
	}

	// Conversion happens AFTER the secrets and configs exist: it resolves each
	// reference to an id against the cluster, so doing it first would fail on
	// exactly the objects this deploy is about to create.
	specs, err := convert.Services(ctx, ns, cfg, cli)
	if err != nil {
		return res, fmt.Errorf("convert services: %w", err)
	}

	existing, err := deployedServices(ctx, cli, stackName)
	if err != nil {
		return res, err
	}
	for _, short := range sortedKeys(specs) {
		spec := specs[short]
		cur, found := existing[spec.Name]
		if !found {
			if _, err := cli.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: spec, QueryRegistry: true}); err != nil {
				return res, fmt.Errorf("create service %s: %w", spec.Name, err)
			}
			res.Created = append(res.Created, spec.Name)
			continue
		}
		_, err := cli.ServiceUpdate(ctx, cur.ID, client.ServiceUpdateOptions{Version: cur.Version, Spec: spec, QueryRegistry: true})
		if err != nil {
			return res, fmt.Errorf("update service %s: %w", spec.Name, err)
		}
		res.Updated = append(res.Updated, spec.Name)
	}

	if opts.Prune {
		if err := pruneServices(ctx, cli, specs, existing, res); err != nil {
			return res, err
		}
	}
	return res, nil
}

// deployedServices maps a stack's current services by their full name.
func deployedServices(ctx context.Context, cli deployClient, stackName string) (map[string]swarm.Service, error) {
	f := make(client.Filters).Add("label", stackLabel+"="+stackName)
	svcsRes, err := cli.ServiceList(ctx, client.ServiceListOptions{Filters: f})
	svcs := svcsRes.Items
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	out := make(map[string]swarm.Service, len(svcs))
	for _, s := range svcs {
		out[s.Spec.Name] = s
	}
	return out, nil
}

func deployNetworks(ctx context.Context, cli deployClient, ns convert.Namespace, cfg *composetypes.Config, res *ApplyResult) error {
	used := map[string]struct{}{}
	for _, s := range cfg.Services {
		for n := range s.Networks {
			used[n] = struct{}{}
		}
	}
	// A service that declares no network still lands on the stack's default
	// one, which compose creates implicitly.
	if len(used) == 0 {
		used["default"] = struct{}{}
	}
	create, external := convert.Networks(ns, cfg.Networks, used)

	// An external network the operator merely believes exists is the most
	// common reason a deploy half-applies: the services referencing it fail one
	// by one, after the rest of the stack has already been changed. Check first.
	for _, name := range external {
		if _, err := cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{}); err != nil {
			return fmt.Errorf("network %q is declared external but does not exist", name)
		}
	}

	for _, name := range sortedKeys(create) {
		if _, err := cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{}); err == nil {
			continue // already there; docker does not update networks either
		}
		opts := create[name]
		// Compose's own default driver is "bridge", which a swarm SERVICE cannot
		// use — the daemon refuses it with "only networks scoped to the swarm can
		// be used". docker stack deploy substitutes overlay for exactly this
		// reason, and without it a file that simply omits its networks (the most
		// ordinary file there is) creates a bridge network, then fails on the
		// first service and leaves the stack half-applied.
		if opts.Driver == "" {
			opts.Driver = defaultNetworkDriver
		}
		if _, err := cli.NetworkCreate(ctx, name, opts); err != nil {
			// Two deploys racing is normal in CI; the loser sees "already
			// exists" and that is not a failure.
			if cerrdefs.IsConflict(err) {
				continue
			}
			return fmt.Errorf("create network %s: %w", name, err)
		}
		res.Networks = append(res.Networks, name)
	}
	return nil
}

func deploySecrets(ctx context.Context, cli deployClient, ns convert.Namespace, cfg *composetypes.Config, res *ApplyResult) error {
	specs, err := convert.Secrets(ns, cfg.Secrets)
	if err != nil {
		return err
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	for _, spec := range specs {
		if _, err := cli.SecretCreate(ctx, client.SecretCreateOptions{Spec: spec}); err != nil {
			if cerrdefs.IsConflict(err) {
				// A secret's value is immutable in swarm; an existing one is
				// left exactly as it is. Changing a secret means creating a new
				// one under a new name, which is the whole point of the
				// convention.
				continue
			}
			return fmt.Errorf("create secret %s: %w", spec.Name, err)
		}
		res.Secrets = append(res.Secrets, spec.Name)
	}
	return nil
}

func deployConfigs(ctx context.Context, cli deployClient, ns convert.Namespace, cfg *composetypes.Config, res *ApplyResult) error {
	specs, err := convert.Configs(ns, cfg.Configs)
	if err != nil {
		return err
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	for _, spec := range specs {
		if _, err := cli.ConfigCreate(ctx, client.ConfigCreateOptions{Spec: spec}); err != nil {
			if cerrdefs.IsConflict(err) {
				continue
			}
			return fmt.Errorf("create config %s: %w", spec.Name, err)
		}
		res.Configs = append(res.Configs, spec.Name)
	}
	return nil
}

// pruneServices removes the stack's services that the file no longer declares.
func pruneServices(ctx context.Context, cli deployClient, specs map[string]swarm.ServiceSpec, existing map[string]swarm.Service, res *ApplyResult) error {
	wanted := map[string]bool{}
	for _, spec := range specs {
		wanted[spec.Name] = true
	}
	for _, name := range sortedKeys(existing) {
		if wanted[name] {
			continue
		}
		if _, err := cli.ServiceRemove(ctx, existing[name].ID, client.ServiceRemoveOptions{}); err != nil {
			return fmt.Errorf("remove service %s: %w", name, err)
		}
		res.Removed = append(res.Removed, name)
	}
	return nil
}

// Summary is a one-line account of what a deploy did, for the terminal and the
// ui alike so the two cannot word it differently.
func (r *ApplyResult) Summary() string {
	var parts []string
	add := func(label string, items []string) {
		if len(items) > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", len(items), label))
		}
	}
	add("service(s) created", r.Created)
	add("updated", r.Updated)
	add("network(s)", r.Networks)
	add("secret(s)", r.Secrets)
	add("config(s)", r.Configs)
	add("service(s) removed", r.Removed)
	if len(parts) == 0 {
		return "nothing to do"
	}
	return strings.Join(parts, ", ")
}
