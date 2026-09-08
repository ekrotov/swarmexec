// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package secscan

import (
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/swarm"
)

// rootUserAnalyzer flags services whose container is not pinned to a non-root
// user. An explicit root (User=0/root) is high. An unset user is only SevLow:
// it is the Swarm default on nearly every service, and whether it actually runs
// as root then depends on the image's own USER, which the manager spec does not
// reveal — badging it would put a marker on almost every row and destroy the
// at-a-glance signal (see Actionable).
type rootUserAnalyzer struct{}

func (rootUserAnalyzer) Analyze(svc swarm.Service) []Finding {
	cs := containerSpec(svc)
	if cs == nil {
		return nil
	}
	raw := strings.TrimSpace(cs.User)
	// User is "name[:group]" or "uid[:gid]"; only the user part matters. Trim
	// each part: Docker tolerates "root : root" and passes the user through.
	name := raw
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(name)

	switch {
	case isRootUser(name):
		return []Finding{{
			Rule:     "root-user",
			Title:    "runs as root",
			Detail:   "the service explicitly runs its container as root (User=" + raw + ")",
			Severity: SevHigh,
		}}
	case name == "":
		return []Finding{{
			Rule:     "root-user",
			Title:    "no user set",
			Detail:   "no non-root user is set; the container runs as the image's default user, which is often root",
			Severity: SevLow,
		}}
	default:
		return nil // an explicit non-root user — good
	}
}

// isRootUser reports whether a user part means uid 0. It parses numerically so
// padded/signed forms ("00", "+0", "0000") are caught, not just the literal "0".
func isRootUser(name string) bool {
	if strings.EqualFold(name, "root") {
		return true
	}
	if n, err := strconv.ParseInt(name, 10, 64); err == nil {
		return n == 0
	}
	return false
}

// credentialWords are "_"-delimited key tokens that name a credential. Matching
// whole tokens (not substrings) is what keeps COMPASS / SURPASS / PASSENGER_PORT
// / BYPASS_AUTH from being reported as secrets.
var credentialWords = map[string]bool{
	"PASSWORD": true, "PASSWD": true, "PASS": true, "PASSPHRASE": true,
	"SECRET": true, "SECRETS": true, "TOKEN": true, "APIKEY": true,
	"CREDENTIAL": true, "CREDENTIALS": true, "PRIVATEKEY": true,
}

// credentialPairs are two-token sequences that together name a credential.
// "KEY" alone is far too common (LICENSE_KEY, PUBLIC_KEY, SORT_KEY) to match.
var credentialPairs = [][2]string{
	{"API", "KEY"}, {"ACCESS", "KEY"}, {"PRIVATE", "KEY"},
	{"SECRET", "KEY"}, {"CLIENT", "SECRET"}, {"AUTH", "TOKEN"},
}

// nonSecretSuffixes end a key that references or describes a credential rather
// than holding one: a path, a URL, an identifier or a knob about it.
var nonSecretSuffixes = []string{
	"_FILE", "_PATH", "_URL", "_URI", "_NAME", "_ID", "_TYPE",
	"_ENABLED", "_REQUIRED", "_LENGTH", "_TIMEOUT", "_TTL", "_EXPIRY", "_ALGORITHM",
}

// secretEnvAnalyzer flags a literal credential embedded in an environment
// variable in the service spec (readable by anyone who can read the spec). It
// exempts the *_FILE convention and its relatives, and values that are plainly
// not a secret (a path, a boolean, a number). The value itself is never read
// into a finding — only the key name.
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
		if !ok {
			continue // no "=": nothing embedded
		}
		val = strings.TrimSpace(val)
		if val == "" || seen[key] {
			continue
		}
		if !isCredentialKey(strings.ToUpper(strings.TrimSpace(key))) || !looksSecret(val) {
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

// isCredentialKey reports whether an upper-cased env key names a credential.
func isCredentialKey(key string) bool {
	for _, suf := range nonSecretSuffixes {
		if strings.HasSuffix(key, suf) {
			return false // a reference to, or a setting about, a credential
		}
	}
	tokens := strings.FieldsFunc(key, func(r rune) bool { return r == '_' || r == '-' || r == '.' })
	for _, t := range tokens {
		if credentialWords[t] {
			return true
		}
	}
	for i := 0; i+1 < len(tokens); i++ {
		for _, p := range credentialPairs {
			if tokens[i] == p[0] && tokens[i+1] == p[1] {
				return true
			}
		}
	}
	return false
}

// looksSecret rejects values that cannot be a credential: an absolute path (the
// *_FILE pattern spelled differently), a boolean, or a bare number (a length, a
// count, a flag). Anything else is treated as a literal secret.
func looksSecret(val string) bool {
	if strings.HasPrefix(val, "/") {
		return false // a path — the secret lives in a file, not here
	}
	switch strings.ToLower(val) {
	case "true", "false", "yes", "no", "on", "off":
		return false
	}
	if _, err := strconv.ParseFloat(val, 64); err == nil {
		return false // a number: a length/count/flag, not a credential
	}
	return true
}

// splitEnv splits a "KEY=VALUE" entry. ok is false when there is no "=".
func splitEnv(kv string) (key, val string, ok bool) {
	i := strings.IndexByte(kv, '=')
	if i < 0 {
		return "", "", false
	}
	return kv[:i], kv[i+1:], true
}
