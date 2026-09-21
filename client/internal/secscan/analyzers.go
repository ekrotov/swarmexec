// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package secscan

import (
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/mount"
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
	name, group := raw, ""
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name, group = name[:i], strings.TrimSpace(name[i+1:])
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
	case name == "" && isRootUser(group):
		// ":root" / ":0" — no user, but an explicit root GROUP. Reporting this
		// as a plain "no user set" hides the deliberate part: someone wrote the
		// root group down on purpose, and group root grants access to every
		// root-group-owned path in the image.
		return []Finding{{
			Rule:     "root-user",
			Title:    "runs in the root group",
			Detail:   "no user is set and the group is explicitly root (User=" + raw + ")",
			Severity: SevMedium,
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

// dockerSocketAnalyzer flags a container that gets the Docker socket bind-mounted
// in. Anything that can talk to the socket can start a privileged container on
// that node, so this is effectively root on the host — the single most valuable
// finding here. Read-only does not help: the socket is an API, not a file whose
// contents matter.
type dockerSocketAnalyzer struct{}

func (dockerSocketAnalyzer) Analyze(svc swarm.Service) []Finding {
	cs := containerSpec(svc)
	if cs == nil {
		return nil
	}
	for _, m := range cs.Mounts {
		if m.Type != mount.TypeBind || !isDockerSocketPath(m.Source) {
			continue
		}
		detail := "the Docker socket is bind-mounted at " + m.Target +
			"; anything in this container can start a privileged container on the node, i.e. it is root on the host"
		if m.ReadOnly {
			detail += " (read-only does not help — the socket is an API, not a file)"
		}
		return []Finding{{
			Rule:     "docker-socket",
			Title:    "Docker socket mounted in",
			Detail:   detail,
			Severity: SevHigh,
		}}
	}
	return nil
}

// isDockerSocketPath reports whether a bind source is the Docker daemon socket.
func isDockerSocketPath(src string) bool {
	src = strings.TrimSpace(src)
	return src == "/var/run/docker.sock" || src == "/run/docker.sock" ||
		strings.HasSuffix(src, "/docker.sock")
}

// dangerousCaps are added capabilities that hand out most of root's power. ALL
// is the blanket case; the rest each allow a documented container escape or host
// interference.
var dangerousCaps = map[string]string{
	"ALL":             "grants every capability",
	"SYS_ADMIN":       "near-root: mount, namespace and cgroup control",
	"SYS_MODULE":      "can load kernel modules",
	"SYS_PTRACE":      "can inspect and control other processes",
	"SYS_RAWIO":       "raw I/O access to devices",
	"DAC_READ_SEARCH": "bypasses file read permission checks",
	"NET_ADMIN":       "full control of the node's networking",
	"NET_RAW":         "can forge and sniff raw packets",
}

// capabilityAnalyzer flags added Linux capabilities. Swarm has no --privileged,
// so capabilities are how a service asks for extra host power.
type capabilityAnalyzer struct{}

func (capabilityAnalyzer) Analyze(svc swarm.Service) []Finding {
	cs := containerSpec(svc)
	if cs == nil {
		return nil
	}
	var out []Finding
	for _, c := range cs.CapabilityAdd {
		// Docker accepts both "CAP_SYS_ADMIN" and "SYS_ADMIN", in any case.
		name := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(c)), "CAP_")
		if name == "" {
			continue
		}
		if why, dangerous := dangerousCaps[name]; dangerous {
			out = append(out, Finding{
				Rule:     "added-capability",
				Title:    "capability " + name + " added",
				Detail:   "the container is granted CAP_" + name + " — " + why,
				Severity: SevHigh,
			})
			continue
		}
		out = append(out, Finding{
			Rule:     "added-capability",
			Title:    "capability " + name + " added",
			Detail:   "the container is granted CAP_" + name + ", a privilege beyond the default set",
			Severity: SevMedium,
		})
	}
	return out
}

// hostNetworkAnalyzer flags a service attached to the host network: the
// container shares the node's network stack, so network isolation and swarm's
// published-port mapping no longer apply.
type hostNetworkAnalyzer struct{}

func (hostNetworkAnalyzer) Analyze(svc swarm.Service) []Finding {
	for _, n := range svc.Spec.TaskTemplate.Networks {
		if strings.EqualFold(strings.TrimSpace(n.Target), "host") {
			return []Finding{{
				Rule:     "host-network",
				Title:    "runs on the host network",
				Detail:   "the container shares the node's network stack — no network isolation, and it can reach anything the node can, including services bound to localhost",
				Severity: SevHigh,
			}}
		}
	}
	return nil
}

// confinementAnalyzer flags a container that has had its kernel-level sandbox
// switched off.
type confinementAnalyzer struct{}

func (confinementAnalyzer) Analyze(svc swarm.Service) []Finding {
	cs := containerSpec(svc)
	if cs == nil || cs.Privileges == nil {
		return nil
	}
	var out []Finding
	if s := cs.Privileges.Seccomp; s != nil && s.Mode == swarm.SeccompModeUnconfined {
		out = append(out, Finding{
			Rule:     "unconfined",
			Title:    "seccomp disabled",
			Detail:   "seccomp is set to unconfined — the container may make any syscall, removing the main barrier to kernel exploits",
			Severity: SevHigh,
		})
	}
	if a := cs.Privileges.AppArmor; a != nil && a.Mode == swarm.AppArmorModeDisabled {
		out = append(out, Finding{
			Rule:     "unconfined",
			Title:    "AppArmor disabled",
			Detail:   "AppArmor confinement is disabled for this container",
			Severity: SevHigh,
		})
	}
	return out
}

// resourceLimitAnalyzer notes a service with no CPU/memory limit. Informational
// only (SevLow): almost no service sets limits, so flagging it would mark nearly
// every row and drown the findings that need acting on — see Actionable.
type resourceLimitAnalyzer struct{}

func (resourceLimitAnalyzer) Analyze(svc swarm.Service) []Finding {
	res := svc.Spec.TaskTemplate.Resources
	if res != nil && res.Limits != nil &&
		(res.Limits.NanoCPUs > 0 || res.Limits.MemoryBytes > 0) {
		return nil
	}
	return []Finding{{
		Rule:     "no-resource-limits",
		Title:    "no resource limits",
		Detail:   "no CPU or memory limit is set; a runaway container can consume the whole node and starve everything else on it",
		Severity: SevLow,
	}}
}

// imagePinAnalyzer notes an image that is not pinned to a specific version.
// Informational only (SevLow), for the same reason as resourceLimitAnalyzer:
// :latest is far too common for a badge to stay meaningful.
type imagePinAnalyzer struct{}

func (imagePinAnalyzer) Analyze(svc swarm.Service) []Finding {
	cs := containerSpec(svc)
	if cs == nil {
		return nil
	}
	ref := strings.TrimSpace(cs.Image)
	if ref == "" || strings.Contains(ref, "@sha256:") {
		return nil // digest-pinned: exactly reproducible
	}
	tag := imageTagOf(ref)
	if tag != "" && tag != "latest" {
		return nil
	}
	what := "the image has no tag, so it resolves to :latest"
	if tag == "latest" {
		what = "the image is pinned to :latest"
	}
	return []Finding{{
		Rule:     "unpinned-image",
		Title:    "image not pinned",
		Detail:   what + " — the running version can change without a spec change, so a deploy is not reproducible",
		Severity: SevLow,
	}}
}

// imageTagOf returns an image reference's tag, or "" when it carries none. A
// colon inside the registry host's port (registry:5000/img) is not a tag.
func imageTagOf(ref string) string {
	i := strings.LastIndexByte(ref, ':')
	if i < 0 {
		return ""
	}
	if strings.ContainsRune(ref[i+1:], '/') {
		return "" // the colon belonged to a host:port, not a tag
	}
	return ref[i+1:]
}

// Describe implementations — the list the UI shows as "what was checked".
func (dockerSocketAnalyzer) Describe() string  { return "Docker socket mounted in" }
func (capabilityAnalyzer) Describe() string    { return "added Linux capabilities" }
func (hostNetworkAnalyzer) Describe() string   { return "host network" }
func (confinementAnalyzer) Describe() string   { return "seccomp / AppArmor disabled" }
func (rootUserAnalyzer) Describe() string      { return "container user" }
func (secretEnvAnalyzer) Describe() string     { return "secrets in environment variables" }
func (resourceLimitAnalyzer) Describe() string { return "missing resource limits" }
func (imagePinAnalyzer) Describe() string      { return "unpinned image" }
