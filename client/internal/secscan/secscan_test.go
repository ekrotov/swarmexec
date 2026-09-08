// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package secscan

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/swarm"
)

// svcWith builds a service whose task container spec has the given user and env.
func svcWith(user string, env ...string) swarm.Service {
	var s swarm.Service
	s.Spec.Name = "svc"
	s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{User: user, Env: env}
	return s
}

func hasRule(fs []Finding, rule string) *Finding {
	for i := range fs {
		if fs[i].Rule == rule {
			return &fs[i]
		}
	}
	return nil
}

func TestRootUserAnalyzer(t *testing.T) {
	cases := []struct {
		user     string
		wantSev  Severity
		wantFind bool
	}{
		{"root", SevHigh, true},
		{"0", SevHigh, true},
		{"0:0", SevHigh, true},
		{"", SevMedium, true}, // no user set → medium (image may still be root)
		{"1000", 0, false},    // explicit non-root uid → no finding
		{"app:app", 0, false}, // explicit non-root name → no finding
		{"nonroot", 0, false},
	}
	for _, c := range cases {
		got := rootUserAnalyzer{}.Analyze(svcWith(c.user))
		f := hasRule(got, "root-user")
		if !c.wantFind {
			if f != nil {
				t.Errorf("user %q: unexpected finding %+v", c.user, *f)
			}
			continue
		}
		if f == nil {
			t.Errorf("user %q: expected a root-user finding, got none", c.user)
			continue
		}
		if f.Severity != c.wantSev {
			t.Errorf("user %q: severity = %v, want %v", c.user, f.Severity, c.wantSev)
		}
	}

	// A nil container spec must not panic and must yield nothing.
	if got := (rootUserAnalyzer{}).Analyze(swarm.Service{}); got != nil {
		t.Errorf("nil container spec: got %+v, want nil", got)
	}
}

func TestSecretEnvAnalyzer(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want bool
	}{
		{"literal password", []string{"POSTGRES_PASSWORD=hunter2"}, true},
		{"db pass", []string{"DB_PASS=s3cr3t"}, true},
		{"api token", []string{"API_TOKEN=abc"}, true},
		{"secret key", []string{"SECRET_KEY=xyz"}, true},
		{"file reference exempt", []string{"POSTGRES_PASSWORD_FILE=/run/secrets/pw"}, false},
		{"empty value", []string{"POSTGRES_PASSWORD="}, false},
		{"benign var", []string{"LOG_LEVEL=info"}, false},
		{"public key benign", []string{"PUBLIC_KEY=/etc/pub.pem"}, false}, // bare KEY is not matched
		{"no equals", []string{"JUSTAFLAG"}, false},
	}
	for _, c := range cases {
		got := secretEnvAnalyzer{}.Analyze(svcWith("1000", c.env...))
		f := hasRule(got, "secret-in-env")
		if (f != nil) != c.want {
			t.Errorf("%s: got finding=%v, want %v (env=%v)", c.name, f != nil, c.want, c.env)
			continue
		}
		if f != nil {
			if f.Severity != SevHigh {
				t.Errorf("%s: severity = %v, want high", c.name, f.Severity)
			}
			// The value must never leak into the finding — only the key name.
			for _, secret := range []string{"hunter2", "s3cr3t", "abc", "xyz"} {
				if strings.Contains(f.Detail, secret) {
					t.Errorf("%s: finding detail leaks a secret value (%q): %q", c.name, secret, f.Detail)
				}
			}
		}
	}
}

func TestScanOrdersAndMaxSeverity(t *testing.T) {
	// Root (high) + a secret (high) + but also an empty-user would be medium;
	// here explicit root is high and the secret is high.
	s := svcWith("root", "MYSQL_PASSWORD=abc")
	fs := Scan(s)
	if len(fs) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(fs), fs)
	}
	if MaxSeverity(fs) != SevHigh {
		t.Errorf("MaxSeverity = %v, want high", MaxSeverity(fs))
	}
	// Highest first (both high here); a medium must sort after a high.
	s2 := svcWith("", "MYSQL_PASSWORD=abc") // medium (no user) + high (secret)
	fs2 := Scan(s2)
	if len(fs2) != 2 || fs2[0].Severity != SevHigh || fs2[1].Severity != SevMedium {
		t.Errorf("scan not ordered high-first: %+v", fs2)
	}
	if MaxSeverity(nil) != SevLow {
		t.Errorf("MaxSeverity(nil) = %v, want low", MaxSeverity(nil))
	}
}

// Register must make a new analyzer visible to Scan.
func TestRegister(t *testing.T) {
	before := len(Scan(svcWith("1000"))) // a clean service: no findings
	Register(stubAnalyzer{})
	defer func() { registry = registry[:len(registry)-1] }() // undo for other tests
	after := Scan(svcWith("1000"))
	if len(after) != before+1 {
		t.Errorf("registered analyzer not run: before=%d after=%d", before, len(after))
	}
}

type stubAnalyzer struct{}

func (stubAnalyzer) Analyze(swarm.Service) []Finding {
	return []Finding{{Rule: "stub", Title: "stub", Severity: SevLow}}
}
