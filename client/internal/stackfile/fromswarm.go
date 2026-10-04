// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/moby/moby/client"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
)

// stackLabel is how docker marks everything that belongs to a stack. It is the
// only thing that makes a stack a stack — there is no stack object in the
// engine, just services wearing the same label.
const stackLabel = "com.docker.stack.namespace"

// swarmReader is the slice of the docker client this needs. An interface, so
// the reduction can be tested against fixtures rather than a live cluster.
type swarmReader interface {
	ServiceList(ctx context.Context, options client.ServiceListOptions) (client.ServiceListResult, error)
	NetworkList(ctx context.Context, options client.NetworkListOptions) (client.NetworkListResult, error)
	SecretList(ctx context.Context, options client.SecretListOptions) (client.SecretListResult, error)
	ConfigList(ctx context.Context, options client.ConfigListOptions) (client.ConfigListResult, error)
}

// FromSwarm reads a deployed stack back into the model.
//
// Two things it cannot recover, and says so rather than pretending otherwise:
// secret VALUES are write-only in the engine API and can never be read back,
// and a volume's CONTENTS live on the nodes. Both are therefore declared
// external. An export is a description of a stack, not a backup of it, and the
// difference matters most exactly when someone is relying on it.
func FromSwarm(ctx context.Context, cli swarmReader, name string) (*Stack, error) {
	f := make(client.Filters).Add("label", stackLabel+"="+name)
	svcsRes, err := cli.ServiceList(ctx, client.ServiceListOptions{Filters: f})
	svcs := svcsRes.Items
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	if len(svcs) == 0 {
		return nil, fmt.Errorf("no stack named %q is deployed (no service carries %s=%s)", name, stackLabel, name)
	}

	st := &Stack{
		Name:     name,
		Services: map[string]*Service{},
		Networks: map[string]*Network{},
		Secrets:  map[string]*Resource{},
		Configs:  map[string]*Resource{},
		Volumes:  map[string]*Resource{},
	}

	// The network list comes first: a deployed service records the network's ID,
	// not its name, and a reduction that cannot resolve it would put an opaque
	// id where the file has a name — a difference on every service.
	netsRes, err := cli.NetworkList(ctx, client.NetworkListOptions{})
	nets := netsRes.Items
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	byID := map[string]string{}
	byName := map[string]network.Summary{}
	owned := map[string]bool{}
	for _, n := range nets {
		byID[n.ID] = n.Name
		byName[n.Name] = n
		if n.Labels[stackLabel] == name {
			owned[n.Name] = true
		}
	}
	// Secrets and configs the stack owns, by their FULL name. Ownership is the
	// label, not the name: a secret merely called "<stack>_something" that the
	// stack never created keeps its name (see Names).
	ownSecretsConfigs(ctx, cli, name, owned)
	ownVolumes(svcs, name, owned)

	names := Names{
		Namespace:   name,
		NetworkName: func(id string) string { return byID[id] },
		Owned:       owned,
	}

	gaps := map[string]bool{}
	for _, s := range svcs {
		// A service of the stack always carries the prefix — the stack label on
		// it is what put it in this list in the first place.
		short := stripNamespace(name, s.Spec.Name)
		st.Services[short] = ServiceFromSpec(name, s.Spec, names)
		for _, u := range Unmodelled(s.Spec) {
			gaps[u] = true
		}
	}

	readNetworks(name, byName, st)
	readSecretsAndConfigs(ctx, cli, name, st)
	collectVolumes(st, names)

	if len(st.Secrets) > 0 {
		st.Notes = append(st.Notes,
			"Secrets are declared external: the engine never returns a secret's value, so it cannot be exported. Recreating this stack elsewhere means creating the secrets first.")
	}
	if len(st.Volumes) > 0 {
		st.Notes = append(st.Notes,
			"Volumes are declared external and carry no data: this describes the declarations, not the contents on the nodes.")
	}
	if len(gaps) > 0 {
		// The dangerous failure here is a confident silence, so the fields the
		// rendering cannot carry are named rather than dropped.
		st.Notes = append(st.Notes,
			"Not carried by this rendering, and therefore NOT compared: "+strings.Join(sortedKeys(gaps), ", ")+".")
	}
	return st, nil
}

func readNetworks(ns string, byName map[string]network.Summary, st *Stack) {
	// Which networks the services actually attach to. A stack can also declare
	// networks nothing uses, but the engine has no record of that — an unused
	// declaration leaves no trace once deployed, so it cannot be recovered.
	used := map[string]bool{}
	for _, s := range st.Services {
		for n := range s.Networks {
			used[n] = true
		}
	}
	if len(used) == 0 {
		return
	}
	for short := range used {
		full, own := ns+"_"+short, true
		n, ok := byName[full]
		if !ok {
			// Not prefixed: the service attaches to a network that exists
			// outside the stack, which compose spells as external.
			n, ok = byName[short]
			own = false
		}
		if !ok {
			// Referenced but not present. Say so rather than omitting it — a
			// silently missing network is exactly the sort of drift this tool
			// is for.
			st.Networks[short] = &Network{External: true}
			st.Notes = append(st.Notes, fmt.Sprintf("Network %q is attached by a service but no longer exists.", short))
			continue
		}
		if !own || n.Labels[stackLabel] != ns {
			st.Networks[short] = &Network{External: true, Name: n.Name}
			continue
		}
		st.Networks[short] = &Network{
			Driver:     n.Driver,
			DriverOpts: driverOpts(n.Options),
			Attachable: n.Attachable,
			Internal:   n.Internal,
			Labels:     stackLabels(n.Labels),
		}
	}
}

// driverOpts drops the options the daemon fills in for every overlay network.
// They are present on every deployed network and in no stack file.
func driverOpts(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range in {
		if strings.HasPrefix(k, "com.docker.network.driver.overlay.vxlanid_list") {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ownSecretsConfigs adds the full names of the secrets and configs this stack
// created. Best effort: if a listing fails nothing is marked owned, which keeps
// full names on both sides — wordier, but never wrong.
func ownSecretsConfigs(ctx context.Context, cli swarmReader, ns string, owned map[string]bool) {
	if secs, err := cli.SecretList(ctx, client.SecretListOptions{}); err == nil {
		for _, s := range secs.Items {
			if s.Spec.Labels[stackLabel] == ns {
				owned[s.Spec.Name] = true
			}
		}
	}
	if cfgs, err := cli.ConfigList(ctx, client.ConfigListOptions{}); err == nil {
		for _, c := range cfgs.Items {
			if c.Spec.Labels[stackLabel] == ns {
				owned[c.Spec.Name] = true
			}
		}
	}
}

// ownVolumes adds the named volumes this stack created. Volumes are node-local
// and the manager cannot list them cluster-wide, but it does not need to: the
// deploy writes the stack label into each volume mount's options, so the
// service spec itself says which volumes are the stack's.
func ownVolumes(svcs []swarm.Service, ns string, owned map[string]bool) {
	for _, s := range svcs {
		cs := s.Spec.TaskTemplate.ContainerSpec
		if cs == nil {
			continue
		}
		for _, m := range cs.Mounts {
			if m.Type == mount.TypeVolume && m.Source != "" && m.VolumeOptions != nil &&
				m.VolumeOptions.Labels[stackLabel] == ns {
				owned[m.Source] = true
			}
		}
	}
}

// readSecretsAndConfigs records which of the referenced secrets and configs
// belong to this stack. Best effort: a listing failure leaves them declared as
// plain external references, which is what they are from the file's side
// anyway, so it cannot produce a wrong comparison.
func readSecretsAndConfigs(ctx context.Context, cli swarmReader, ns string, st *Stack) {
	wantSec, wantCfg := map[string]bool{}, map[string]bool{}
	for _, s := range st.Services {
		for _, m := range s.Secrets {
			wantSec[m.Source] = true
		}
		for _, m := range s.Configs {
			wantCfg[m.Source] = true
		}
	}
	for name := range wantSec {
		st.Secrets[name] = &Resource{External: true}
	}
	for name := range wantCfg {
		st.Configs[name] = &Resource{External: true}
	}
	if secs, err := cli.SecretList(ctx, client.SecretListOptions{}); err == nil {
		for _, s := range secs.Items {
			if short := stripNamespace(ns, s.Spec.Name); wantSec[short] && s.Spec.Labels[stackLabel] == ns {
				st.Secrets[short] = &Resource{External: true, Name: s.Spec.Name, Labels: stackLabels(s.Spec.Labels)}
			}
		}
	}
	if cfgs, err := cli.ConfigList(ctx, client.ConfigListOptions{}); err == nil {
		for _, c := range cfgs.Items {
			if short := stripNamespace(ns, c.Spec.Name); wantCfg[short] && c.Spec.Labels[stackLabel] == ns {
				st.Configs[short] = &Resource{External: true, Name: c.Spec.Name, Labels: stackLabels(c.Spec.Labels)}
			}
		}
	}
}

// collectVolumes declares the named volumes the services mount. Only named
// volumes: a bind mount is a host path, not a stack-level declaration. A
// volume the stack created keeps its full name, so an export deployed again
// mounts the same volume rather than a new one under the short name.
func collectVolumes(st *Stack, names Names) {
	for _, s := range st.Services {
		for _, v := range s.Volumes {
			src := v
			if i := strings.IndexByte(src, ':'); i > 0 {
				src = src[:i]
			}
			// A leading slash or dot means a host path.
			if src == "" || strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") {
				continue
			}
			r := &Resource{External: true}
			if full := names.Namespace + "_" + src; names.Owned[full] {
				r.Name = full
			}
			st.Volumes[src] = r
		}
	}
}

// StackNames lists the stacks deployed on the cluster, for a picker that does
// not make the operator remember them.
func StackNames(ctx context.Context, cli swarmReader) ([]string, error) {
	svcsRes, err := cli.ServiceList(ctx, client.ServiceListOptions{})
	svcs := svcsRes.Items
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, s := range svcs {
		if ns := s.Spec.Labels[stackLabel]; ns != "" {
			seen[ns] = true
		}
	}
	out := sortedKeys(seen)
	sort.Strings(out)
	return out, nil
}
