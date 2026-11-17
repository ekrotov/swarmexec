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
  - deny: ["volume.remove", "volume.create"]
    volumes: ["prod_*"]
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
		{Request{Action: "volume.remove", Volume: "prod_db_data"}, false, "rule 4"},
		{Request{Action: "volume.create", Volume: "prod_new"}, false, "rule 4"},
		{Request{Action: "volume.remove", Volume: "staging_db"}, true, "default"},
		{Request{Action: "volume.list"}, true, "default"},
		// The image's user counts: no -u on a root image is a root shell, and
		// "root:root", "0:1" and "00" are root too.
		{Request{Action: "exec", EffectiveUser: "root"}, false, "rule 3"},
		{Request{Action: "exec", User: "root:root", EffectiveUser: "root:root"}, false, "rule 3"},
		{Request{Action: "exec", User: "0:1", EffectiveUser: "0:1"}, false, "rule 3"},
		{Request{Action: "exec", User: "00", EffectiveUser: "00"}, false, "rule 3"},
		{Request{Action: "exec", EffectiveUser: "app"}, true, "default"},
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
		"glob of nothing": "default: allow\nrules:\n  - deny: [\"secret.*\"]",
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

// "kill" stops workloads, so an older "container.*" grant (written for the
// listing and the event watch) must not start covering it; "*" does.
func TestKillIsNotUnderTheContainerGlob(t *testing.T) {
	p := mustParse(t, "default: deny\nrules:\n  - allow: [\"container.*\"]\n", false)
	if ok, _ := p.Decide(Request{Action: "kill"}); ok {
		t.Error(`"container.*" must not allow kill`)
	}
	p = mustParse(t, "default: deny\nrules:\n  - allow: [\"*\"]\n", false)
	if ok, _ := p.Decide(Request{Action: "kill"}); !ok {
		t.Error(`"*" must cover kill`)
	}
	if _, err := Parse([]byte("default: allow\nrules:\n  - deny: [kill]\n"), false); err != nil {
		t.Errorf("kill must be a known action: %v", err)
	}
}

// "inspect" hands out the container's environment (credentials), so an older
// "container.*" grant must not start covering it either; "*" does.
func TestInspectIsNotUnderTheContainerGlob(t *testing.T) {
	p := mustParse(t, "default: deny\nrules:\n  - allow: [\"container.*\"]\n", false)
	if ok, _ := p.Decide(Request{Action: "inspect"}); ok {
		t.Error(`"container.*" must not allow inspect`)
	}
	if ok, _ := p.Decide(Request{Action: "container.events"}); !ok {
		t.Error(`"container.*" still covers the event watch`)
	}
	p = mustParse(t, "default: deny\nrules:\n  - allow: [\"*\"]\n", false)
	if ok, _ := p.Decide(Request{Action: "inspect"}); !ok {
		t.Error(`"*" must cover inspect`)
	}
	if _, err := Parse([]byte("default: allow\nrules:\n  - deny: [inspect]\n"), false); err != nil {
		t.Errorf("inspect must be a known action: %v", err)
	}
	if _, err := Parse([]byte("default: allow\nrules:\n  - deny: [container.inspect]\n"), false); err == nil {
		t.Error("container.inspect must not be an action")
	}
}

// A condition no matched action can carry is a rule that matches nothing; Parse
// refuses it (SEC-01).
func TestParseRefusesDeadConditions(t *testing.T) {
	cases := []struct{ rule, want string }{
		{`deny: ["volume.*"]
    services: ["prod_*"]`, "rule 1: services never applies to volume.list, volume.create, volume.remove, volume.prune"},
		{`deny: [exec]
    ports: [22]`, "ports never applies to exec"},
		{`deny: [logs]
    users: [root]`, "users never applies to logs"},
		{`deny: [image.prune]
    volumes: ["x"]`, "volumes never applies to image.prune"},
	}
	for _, c := range cases {
		_, err := Parse([]byte("default: allow\nrules:\n  - "+c.rule+"\n"), false)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want containing %q", c.rule, err, c.want)
		}
	}
}

// A condition that applies to at least one matched action stays valid: a
// "deny *" for a service still covers every container action.
func TestParseAcceptsPartlyApplicableConditions(t *testing.T) {
	for _, r := range []string{
		`deny: ["*"]
    services: ["prod_*"]`,
		`deny: [exec, logs]
    users: [root]`,
		`deny: ["volume.*"]
    volumes: ["prod_*"]`,
		`deny: [exec]
    users: [""]`,
	} {
		if _, err := Parse([]byte("default: allow\nrules:\n  - "+r+"\n"), false); err != nil {
			t.Errorf("%q: %v", r, err)
		}
	}
}

func TestNormID(t *testing.T) {
	for in, want := range map[string]string{"": "", "root": "root", "0": "0", "00": "0", "007:010": "7:10", "0:0": "0:0", "a0": "a0"} {
		if got := normID(in); got != want {
			t.Errorf("normID(%q) = %q, want %q", in, got, want)
		}
	}
}
