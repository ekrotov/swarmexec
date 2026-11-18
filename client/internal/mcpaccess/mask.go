// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcpaccess

import (
	"regexp"
	"strings"
)

// The scan's secret-in-env rule is tuned for precision: a finding that cries
// wolf is ignored. Masking has the opposite failure mode — a miss hands a
// credential to a model provider — so it uses a wider rule of its own:
//
//   - by key: the scan's credential words plus KEY, DSN, AUTH, BEARER and the
//     like, and unlike the scan it does not exempt _URL/_URI;
//   - by value shape, whatever the key: a URL with a password, a JWT, a PEM
//     private key, an htpasswd hash, a "password=..." pair.
//
// Value-shape masking replaces only the credential part and keeps the rest
// readable (postgres://app:<masked>@db:5432/x), so a masked value still says
// where it points. Whole-value masking is kept for keys that name a secret.

// sensitiveTokens are "_"/"."/"-"-delimited key tokens that name a credential
// beyond the scan's own list.
var sensitiveTokens = map[string]bool{
	"KEY": true, "DSN": true, "AUTH": true, "AUTHORIZATION": true, "BEARER": true,
	"COOKIE": true, "SALT": true, "SIGNATURE": true, "SIGNING": true, "PWD": true,
	"PASSWORD": true, "PASSWD": true, "PASS": true, "PASSPHRASE": true,
	"SECRET": true, "SECRETS": true, "TOKEN": true, "APIKEY": true,
	"CREDENTIAL": true, "CREDENTIALS": true, "PRIVATEKEY": true, "REQUIREPASS": true,
}

// sensitiveSubstrings catch compound names a tokenizer cannot split
// (traefik...basicauth.users, MYSQL_ROOT_PASSWORD fused as MYSQLPASSWORD).
var sensitiveSubstrings = []string{"PASSWORD", "BASICAUTH", "HTPASSWD", "PRIVATEKEY", "SECRETKEY", "APIKEY"}

// referenceSuffixes end a key that points at a credential or describes it.
// _URL and _URI are deliberately absent: they are handled by shape.
var referenceSuffixes = []string{
	"_FILE", "_PATH", "_NAME", "_ID", "_TYPE", "_ENABLED", "_REQUIRED",
	"_LENGTH", "_TIMEOUT", "_TTL", "_EXPIRY", "_ALGORITHM",
}

// SensitiveKey reports whether a key (env var, label, flag name) names a
// credential, by the masking rule.
func SensitiveKey(key string) bool {
	k := strings.ToUpper(strings.TrimSpace(key))
	for _, suf := range referenceSuffixes {
		if strings.HasSuffix(k, suf) {
			return false
		}
	}
	for _, sub := range sensitiveSubstrings {
		if strings.Contains(k, sub) {
			return true
		}
	}
	for _, t := range strings.FieldsFunc(k, func(r rune) bool { return r == '_' || r == '-' || r == '.' }) {
		if sensitiveTokens[t] {
			return true
		}
	}
	return false
}

var (
	// scheme://user:password@host — the password only.
	reURLPassword = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^\s/@:'"]*:)[^\s/@'"]+(@)`)
	// scheme://token@host where the userinfo has no colon is usually a user
	// name (ssh://git@host); only the well-known token prefixes are masked.
	reURLToken = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)(?:gh[pousr]_|glpat-|xox[abprs]-|sk-)[A-Za-z0-9_-]+(@)`)
	reJWT      = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)
	rePEM      = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)
	// user:$apr1$... / $2y$... — an htpasswd entry; the hash only.
	reHtpasswd = regexp.MustCompile(`(:)\$(?:apr1|2[abxy]?|1|5|6)\$[^\s,:]+`)
	// password=..., token: ... pairs inside free text and DSNs.
	rePair = regexp.MustCompile(`(?i)(\b[\w.-]*(?:password|passwd|pwd|passphrase|secret|token|api[_-]?key|access[_-]?key)[\w.-]*\s*=\s*)('[^']*'|"[^"]*"|[^\s&;,]+)`)
)

// MaskText masks the credential part of every credential-shaped substring of
// s and leaves the rest as it was. It is the value-shape half of the masking
// rule and is safe to run on any text.
func MaskText(s string) string {
	if s == "" {
		return s
	}
	if strings.Contains(s, "-----BEGIN") {
		s = rePEM.ReplaceAllString(s, Masked)
	}
	if strings.Contains(s, "eyJ") {
		s = reJWT.ReplaceAllString(s, Masked)
	}
	if strings.Contains(s, "://") {
		s = reURLPassword.ReplaceAllString(s, "${1}"+Masked+"${2}")
		s = reURLToken.ReplaceAllString(s, "${1}"+Masked+"${2}")
	}
	if strings.Contains(s, "$") {
		s = reHtpasswd.ReplaceAllString(s, "${1}"+Masked)
	}
	if strings.Contains(s, "=") {
		s = rePair.ReplaceAllString(s, "${1}"+Masked)
	}
	return s
}

// MaskValue masks the value of a key=value pair: the whole value when the key
// names a credential, otherwise just the credential-shaped parts of it. A key
// that names a URL, or a DSN holding one, keeps its address: only the password
// goes.
func MaskValue(key, val string) string {
	if !SensitiveKey(key) {
		return MaskText(val)
	}
	if m := MaskText(val); m != val && isURLKey(key) {
		return m
	}
	return Masked
}

func isURLKey(key string) bool {
	k := strings.ToUpper(strings.TrimSpace(key))
	return strings.HasSuffix(k, "_DSN") || k == "DSN"
}

// MaskArgs masks the credential in a command line: the value after a
// credential-named flag ("--requirepass x", "--password=x") and any
// credential-shaped substring of the other words.
func MaskArgs(args []string) []string {
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	maskNext := false
	for i, a := range args {
		switch {
		case maskNext:
			out[i] = Masked
			maskNext = false
		case strings.HasPrefix(a, "-") && strings.Contains(a, "="):
			flag, v, _ := strings.Cut(a, "=")
			out[i] = flag + "=" + MaskValue(strings.TrimLeft(flag, "-"), v)
		case strings.HasPrefix(a, "-") && SensitiveKey(strings.TrimLeft(a, "-")):
			out[i] = a
			maskNext = true
		default:
			out[i] = MaskText(a)
		}
	}
	return out
}
