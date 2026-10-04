// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"context"

	"github.com/docker/cli/cli/compose/convert"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// plannedClient answers the secret and config lookups of docker's converter as
// the cluster will look once the deploy has created what the file brings.
//
// The converter resolves every secret and config reference to an id against
// the cluster. Deploy creates the file's own secrets and configs first and
// converts afterwards, so that works there; but the check before a deploy and
// a diff convert BEFORE anything is created, and refused a perfectly ordinary
// first deploy with "secret not found". The objects the file declares itself
// are therefore answered as present, under the exact names deploy will create
// (taken from the same convert.Secrets / convert.Configs deploy uses). A
// reference to an external secret that does not exist still fails, as it
// would on deploy.
type plannedClient struct {
	client.APIClient
	secrets map[string]bool // names the deploy will create
	configs map[string]bool
}

// plannedPendingID marks a reference to an object the deploy will create; it
// never reaches the cluster, only the plan and the diff.
const plannedPendingID = "(created by this deploy)"

func newPlannedClient(cli client.APIClient, ns convert.Namespace, cfg *composetypes.Config) (*plannedClient, error) {
	p := &plannedClient{APIClient: cli, secrets: map[string]bool{}, configs: map[string]bool{}}
	secrets, err := convert.Secrets(ns, cfg.Secrets)
	if err != nil {
		return nil, err
	}
	for _, s := range secrets {
		p.secrets[s.Name] = true
	}
	configs, err := convert.Configs(ns, cfg.Configs)
	if err != nil {
		return nil, err
	}
	for _, c := range configs {
		p.configs[c.Name] = true
	}
	return p, nil
}

func (p *plannedClient) SecretList(ctx context.Context, opts client.SecretListOptions) (client.SecretListResult, error) {
	res, err := p.APIClient.SecretList(ctx, opts)
	if err != nil {
		return res, err
	}
	have := map[string]bool{}
	for _, s := range res.Items {
		have[s.Spec.Name] = true
	}
	for name := range opts.Filters["name"] {
		if p.secrets[name] && !have[name] {
			var s swarm.Secret
			s.ID, s.Spec.Name = plannedPendingID, name
			res.Items = append(res.Items, s)
		}
	}
	return res, nil
}

func (p *plannedClient) ConfigList(ctx context.Context, opts client.ConfigListOptions) (client.ConfigListResult, error) {
	res, err := p.APIClient.ConfigList(ctx, opts)
	if err != nil {
		return res, err
	}
	have := map[string]bool{}
	for _, c := range res.Items {
		have[c.Spec.Name] = true
	}
	for name := range opts.Filters["name"] {
		if p.configs[name] && !have[name] {
			var c swarm.Config
			c.ID, c.Spec.Name = plannedPendingID, name
			res.Items = append(res.Items, c)
		}
	}
	return res, nil
}
