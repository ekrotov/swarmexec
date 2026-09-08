// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package secscan

import (
	"strings"

	"github.com/docker/docker/api/types/swarm"
)

// rootUserAnalyzer flags services whose container is not pinned to a non-root
// user. An explicit root (User=0/root) is high; an unset user is medium, since
// whether it actually runs as root then depends on the image's own USER, which
// the manager spec does not reveal — we flag the missing guardrail, not a
// certainty.
type rootUserAnalyzer struct{}

func (rootUserAnalyzer) Analyze(svc swarm.Service) []Finding {
	cs := containerSpec(svc)
	if cs == nil {
		return nil
	}
	// User is "name[:group]" or "uid[:gid]"; only the user part matters.
	name := strings.TrimSpace(cs.User)
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	switch {
	case name == "0" || strings.EqualFold(name, "root"):
		return []Finding{{
			Rule:     "root-user",
			Title:    "runs as root",
			Detail:   "the service explicitly runs its container as root (User=" + strings.TrimSpace(cs.User) + ")",
			Severity: SevHigh,
		}}
	case name == "":
		return []Finding{{
			Rule:     "root-user",
			Title:    "no user set",
			Detail:   "no non-root user is set; the container runs as the image's default user, which is often root",
			Severity: SevMedium,
		}}
	default:
		return nil // an explicit non-root user — good
	}
}

// secretKeyHints are substrings (upper-case) of an env var NAME that suggest it
// carries a credential. Kept deliberately broad-but-not-generic: "PASS" catches
// PASSWORD/PASSWD/PASSPHRASE/DB_PASS; bare "KEY" is excluded (too many benign
// LICENSE_KEY/PUBLIC_KEY names) in favour of the specific *_KEY forms.
var secretKeyHints = []string{
	"PASS", "SECRET", "TOKEN", "APIKEY", "API_KEY",
	"ACCESS_KEY", "PRIVATE_KEY", "CREDENTIAL",
}

// secretEnvAnalyzer flags a literal credential embedded in an environment
// variable in the service spec (readable by anyone who can read the spec). It
// exempts the *_FILE convention (a path to a mounted secret, not the value) and
// does not look at Docker secrets (ContainerSpec.Secrets), which are the right
// way to do this. The value itself is never read into a finding.
type secretEnvAnalyzer struct{}

func (secretEnvAnalyzer) Analyze(svc swarm.Service) []Finding {
	cs := containerSpec(svc)
	if cs == nil {
		return nil
	}
	var out []Finding
	seen := map[string]bool{}
	for _, kv := range cs.Env {
		key, val, ok := splitEnv(kv)
		if !ok || strings.TrimSpace(val) == "" {
			continue // no "=", or empty value: nothing embedded
		}
		up := strings.ToUpper(key)
		if strings.HasSuffix(up, "_FILE") { // *_FILE points at a secret file — good practice
			continue
		}
		if !matchesSecretHint(up) || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Finding{
			Rule:     "secret-in-env",
			Title:    "secret in environment variable",
			Detail:   "the env var " + key + " holds a literal value in the service spec; use a Docker secret or a *_FILE reference instead",
			Severity: SevHigh,
		})
	}
	return out
}

// splitEnv splits a "KEY=VALUE" entry. ok is false when there is no "=".
func splitEnv(kv string) (key, val string, ok bool) {
	i := strings.IndexByte(kv, '=')
	if i < 0 {
		return "", "", false
	}
	return kv[:i], kv[i+1:], true
}

func matchesSecretHint(upperKey string) bool {
	for _, h := range secretKeyHints {
		if strings.Contains(upperKey, h) {
			return true
		}
	}
	return false
}
