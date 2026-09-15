// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/cli/cli/compose/loader"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
)

// FromFile loads a stack file and reduces it the same way a deployed stack is
// reduced.
//
// It goes through docker's OWN compose loader and converter — the same code
// `docker stack deploy` runs — rather than reading the YAML directly. That is
// the point: the question a diff answers is "would deploying this file change
// anything?", and the only way to answer it honestly is to work out what
// deploying it would actually submit. Reading the YAML ourselves would compare
// what the file SAYS against what the cluster HAS, and those are different
// languages: compose's short port syntax, its restart aliases, its variable
// interpolation and its defaults all sit in between.
func FromFile(ctx context.Context, cli *client.Client, path, stackName string) (*Stack, error) {
	cfg, err := loadComposeFile(path)
	if err != nil {
		return nil, err
	}
	ns := convert.NewNamespace(stackName)

	// Secret and config references are resolved against the cluster, because
	// that is what deploy does; a reference to something that does not exist is
	// an error there too, and finding it here is the cheaper place.
	specs, err := convert.Services(ctx, ns, cfg, cli)
	if err != nil {
		return nil, fmt.Errorf("convert %s: %w", filepath.Base(path), err)
	}

	st := &Stack{
		Name:     stackName,
		Services: map[string]*Service{},
		Networks: map[string]*Network{},
		Secrets:  map[string]*Resource{},
		Configs:  map[string]*Resource{},
		Volumes:  map[string]*Resource{},
	}
	// What the FILE owns: everything it declares without "external: true". The
	// converter has already prefixed exactly those with the stack namespace, so
	// this is the same rule the deployed side applies via the stack label.
	owned := map[string]bool{}
	for n, v := range cfg.Networks {
		if !v.External.External {
			owned[stackName+"_"+n] = true
		}
	}
	for n, v := range cfg.Secrets {
		if !v.External.External {
			owned[stackName+"_"+n] = true
		}
	}
	for n, v := range cfg.Configs {
		if !v.External.External {
			owned[stackName+"_"+n] = true
		}
	}
	for n, v := range cfg.Volumes {
		if !v.External.External {
			owned[stackName+"_"+n] = true
		}
	}
	// Compose invents a "default" network for services that name none, and the
	// deploy creates it as "<stack>_default" with the overlay driver. The file
	// never declares it, so without this the deployed side (which sees a real,
	// stack-labelled network and strips the prefix) and the file side would
	// disagree about its name and about whether it exists at all — a difference
	// on every service of the most ordinary stack file there is. Caught by
	// deploying a file and diffing it against what it had just produced.
	if _, declared := cfg.Networks["default"]; !declared && usesImplicitDefault(specs, stackName) {
		owned[stackName+"_default"] = true
		st.Networks["default"] = &Network{Driver: defaultNetworkDriver}
	}

	names := Names{Namespace: stackName, Owned: owned}

	gaps := map[string]bool{}
	for name, spec := range specs {
		st.Services[name] = ServiceFromSpec(stackName, spec, names)
		for _, u := range Unmodelled(spec) {
			gaps[u] = true
		}
	}
	for name, n := range cfg.Networks {
		st.Networks[name] = &Network{
			Driver:     n.Driver,
			DriverOpts: driverOpts(n.DriverOpts),
			Attachable: n.Attachable,
			Internal:   n.Internal,
			Labels:     stackLabels(n.Labels),
			External:   n.External.External,
			Name:       n.Name,
		}
	}
	// Secrets, configs and volumes render as external on both sides, so the two
	// agree by construction rather than by luck: the deployed side genuinely
	// cannot know more (a secret's value is write-only), and a file that inlines
	// a value must not have that value end up in a diff.
	for name := range cfg.Secrets {
		st.Secrets[name] = &Resource{External: true}
	}
	for name := range cfg.Configs {
		st.Configs[name] = &Resource{External: true}
	}
	for name := range cfg.Volumes {
		st.Volumes[name] = &Resource{External: true}
	}
	if len(gaps) > 0 {
		st.Notes = append(st.Notes,
			"Not carried by this rendering, and therefore NOT compared: "+strings.Join(sortedKeys(gaps), ", ")+".")
	}
	return st, nil
}

// loadComposeFile reads and parses one stack file.
//
// Environment interpolation uses the process environment, exactly as
// `docker stack deploy` does. That makes the diff depend on the environment it
// is run in — which is correct rather than unfortunate: a file with ${TAG} in
// it describes a different stack for a different TAG, and pretending otherwise
// would give a clean diff for a deploy that would change the image.
func loadComposeFile(path string) (*composetypes.Config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dict, err := loader.ParseYAML(body)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	wd, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		wd = filepath.Dir(path)
	}
	cfg, err := loader.Load(composetypes.ConfigDetails{
		WorkingDir:  wd,
		ConfigFiles: []composetypes.ConfigFile{{Filename: path, Config: dict}},
		Environment: environMap(),
	})
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", filepath.Base(path), err)
	}
	return cfg, nil
}

func environMap() map[string]string {
	out := map[string]string{}
	for _, e := range os.Environ() {
		if i := strings.IndexByte(e, '='); i > 0 {
			out[e[:i]] = e[i+1:]
		}
	}
	return out
}

// UnsupportedProperties names compose keys that swarm ignores outright —
// `build`, `depends_on`, `restart` at service level and so on. They are not
// errors and not differences, but a file that relies on them does not describe
// what will run, and the operator should hear that from us rather than discover
// it later.
func UnsupportedProperties(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dict, err := loader.ParseYAML(body)
	if err != nil {
		return nil, err
	}
	return loader.GetUnsupportedProperties(dict), nil
}

// usesImplicitDefault reports whether any converted service attaches to the
// stack's implicit default network.
func usesImplicitDefault(specs map[string]swarm.ServiceSpec, stackName string) bool {
	target := stackName + "_default"
	for _, spec := range specs {
		for _, n := range spec.TaskTemplate.Networks {
			if n.Target == target {
				return true
			}
		}
	}
	return false
}
