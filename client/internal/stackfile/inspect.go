// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"context"
	"fmt"
	"sort"

	"github.com/docker/cli/cli/compose/convert"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"swarmexec/client/internal/secscan"
)

// Checking a stack file before it is deployed does NOT get its own set of
// rules. The file is converted to the very swarm.ServiceSpec values the deploy
// would submit, and the EXISTING analyzers run on those — the same eight checks
// that badge the tree, fill the "!" overlay and the security report.
//
// Two sets of rules would be the obvious way to build this and the wrong one.
// They drift: a rule tightened in one place and not the other means the gate
// passes a file that the tree then flags the moment it is running, and an
// operator who has been told "no findings" has been told something false. The
// second reason is subtler — a compose-level rule would inspect what the file
// SAYS, while the deployed spec is what the cluster will DO, and compose's
// defaults, aliases and shorthands sit in between.

// Plan is a stack file, converted and checked, ready to be deployed or refused.
type Plan struct {
	Stack    string
	Path     string
	Config   *composetypes.Config
	Services []PlannedService

	// Unsupported are compose keys swarm ignores outright. Not findings, but a
	// file that leans on them does not describe what will run.
	Unsupported []string
}

// PlannedService is one service of the file with what the analyzers found on it.
type PlannedService struct {
	Name     string
	Findings []secscan.Finding
}

// Actionable reports whether anything above informational was found — the same
// threshold the tree uses to put a shield on a row, so the gate and the badge
// agree about what counts.
func (p *Plan) Actionable() bool {
	for _, s := range p.Services {
		if secscan.Actionable(s.Findings) {
			return true
		}
	}
	return false
}

// Worst is the highest severity anywhere in the plan.
func (p *Plan) Worst() secscan.Severity {
	worst := secscan.SevLow
	for _, s := range p.Services {
		if m := secscan.MaxSeverity(s.Findings); m > worst {
			worst = m
		}
	}
	return worst
}

// Flagged is the services carrying something worth acting on, worst first.
func (p *Plan) Flagged() []PlannedService {
	var out []PlannedService
	for _, s := range p.Services {
		if secscan.Actionable(s.Findings) {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := secscan.MaxSeverity(out[i].Findings), secscan.MaxSeverity(out[j].Findings)
		if a != b {
			return a > b
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// PlanFile loads a stack file, converts it the way a deploy would, and runs the
// security analyzers over the result.
//
// The conversion needs the cluster: a service's secret and config references
// are resolved to ids against it, exactly as deploy does. The ones the file
// creates itself count as present (see plannedClient); a file naming an
// external secret that does not exist fails HERE, before anything has been
// created — which is the cheap place for it to fail.
func PlanFile(ctx context.Context, cli client.APIClient, path, stackName string) (*Plan, error) {
	cfg, err := loadComposeFile(path)
	if err != nil {
		return nil, err
	}
	p := &Plan{Stack: stackName, Path: path, Config: cfg}
	if un, uerr := UnsupportedProperties(path); uerr == nil {
		p.Unsupported = un
	}

	ns := convert.NewNamespace(stackName)
	planned, err := newPlannedClient(cli, ns, cfg)
	if err != nil {
		return nil, fmt.Errorf("convert %s: %w", path, err)
	}
	specs, err := convert.Services(ctx, ns, cfg, planned)
	if err != nil {
		return nil, fmt.Errorf("convert %s: %w", path, err)
	}
	for _, short := range sortedKeys(specs) {
		// secscan reads a whole Service; only the Spec is populated, which is
		// all any analyzer looks at.
		p.Services = append(p.Services, PlannedService{
			Name:     short,
			Findings: secscan.Scan(swarm.Service{Spec: specs[short]}),
		})
	}
	return p, nil
}
