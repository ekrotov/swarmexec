// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"strings"
	"testing"
)

const example = `
default: allow
rules:
  - deny: [image.prune.all]
    reason: no sweeping prune on production
  - deny: [portforward]
    ports: [5432, 3306]
    reason: no forwards to the databases
  - deny: [exec]
    users: [root, "0", "0:0"]
  - deny: ["volume.*"]
    services: ["prod_*"]
`

func mustParse(t *testing.T, src string, verified bool) *Policy {
	t.Helper()
	p, err := Parse([]byte(src), verified)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDecide(t *testing.T) {
	p := mustParse(t, example, false)
	cases := []struct {
		req   Request
		allow bool
		why   string
	}{
		{Request{Action: "image.prune"}, true, "policy default: allow"},
		{Request{Action: "image.prune.all"}, false, "rule 1 (deny image.prune.all): no sweeping prune"},
		{Request{Action: "portforward", Port: 5432}, false, "rule 2"},
		{Request{Action: "portforward", Port: 8080}, true, "default"},
		{Request{Action: "exec", User: "root"}, false, "rule 3"},
		{Request{Action: "exec", User: "0:0"}, false, "rule 3"},
		{Request{Action: "exec", User: "1000"}, true, "default"},
		{Request{Action: "volume.remove", Service: "prod_db"}, false, "rule 4"},
		{Request{Action: "volume.remove", Service: "staging_db"}, true, "default"},
		// A rule on services does not match a request that has none: a volume
		// not used by any service is not "a prod_ service".
		{Request{Action: "volume.remove"}, true, "default"},
	}
	for _, c := range cases {
		allow, why := p.Decide(c.req)
		if allow != c.allow || !strings.Contains(why, c.why) {
			t.Errorf("%+v: got %v %q, want %v containing %q", c.req, allow, why, c.allow, c.why)
		}
	}
}

// The first matching rule decides, so an allow can carve out an exception
// before a broader deny.
func TestFirstMatchWins(t *testing.T) {
	p := mustParse(t, `
default: deny
rules:
  - allow: [exec]
    identities: [alice]
  - deny: ["*"]
    reason: everything else
`, true)
	if ok, _ := p.Decide(Request{Action: "exec", Identity: "alice"}); !ok {
		t.Error("alice's exec should be allowed by rule 1")
	}
	if ok, why := p.Decide(Request{Action: "exec", Identity: "bob"}); ok || !strings.Contains(why, "rule 2") {
		t.Errorf("bob: %v %q", ok, why)
	}
}

// Every way to write a policy that silently does something other than it
// says must fail at load instead.
func TestParseRefusesWhatWouldMislead(t *testing.T) {
	cases := map[string]string{
		"no default":      "rules: []",
		"bad default":     "default: maybe",
		"misspelt action": "default: allow\nrules:\n  - deny: [image.prunee]",
		"glob of nothing": "default: allow\nrules:\n  - deny: [\"network.*\"]",
		"misspelt key":    "default: allow\nrules:\n  - deny: [exec]\n    user: [root]",
		"both verdicts":   "default: allow\nrules:\n  - deny: [exec]\n    allow: [logs]",
		"no actions":      "default: allow\nrules:\n  - reason: forgot",
		"broken pattern":  "default: allow\nrules:\n  - deny: [exec]\n    services: [\"[\"]",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src), true); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// With a shared secret and no client CA the identity is what the client
// claims. A rule on it would read like a restriction and restrict nothing.
func TestIdentityRulesNeedVerifiedIdentities(t *testing.T) {
	src := "default: allow\nrules:\n  - deny: [exec]\n    identities: [intern]"
	if _, err := Parse([]byte(src), false); err == nil || !strings.Contains(err.Error(), "cannot verify identities") {
		t.Errorf("unverified identities must be refused, got %v", err)
	}
	if _, err := Parse([]byte(src), true); err != nil {
		t.Errorf("verified identities may be matched: %v", err)
	}
}
