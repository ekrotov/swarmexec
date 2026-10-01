// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"fmt"
	"os"

	"swarmexec/internal/policy"
)

// Policy authorizes requests by a rule file (see internal/policy, and the
// honest limits of what that is in its package comment). It replaces AllowAll
// when the agent runs with -policy-file.
type Policy struct{ p *policy.Policy }

// LoadPolicy reads and validates the rule file. identityVerified is whether
// request identities can be trusted; see policy.Parse.
func LoadPolicy(path string, identityVerified bool) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy file: %w", err)
	}
	p, err := policy.Parse(b, identityVerified)
	if err != nil {
		return nil, err
	}
	return &Policy{p: p}, nil
}

func (a *Policy) Authorize(_ context.Context, req Request) Decision {
	allow, why := a.p.Decide(policy.Request{
		Action:   req.Action,
		Identity: req.Identity,
		Service:  req.Service,
		Port:     req.Port,
		User:     req.User,
	})
	return Decision{Allow: allow, Reason: why}
}

var _ Authorizer = (*Policy)(nil)
