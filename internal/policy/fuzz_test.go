// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package policy

import "testing"

// The rule file is operator-written YAML the agent reads at start-up, and
// reloads: a malformed one must be an error, never a panic, and a policy that
// did parse must be able to decide any request (a rule with a bad glob that
// slipped through Parse would blow up here, on the first request).
func FuzzParse(f *testing.F) {
	f.Add([]byte(example), false)
	f.Add([]byte(example), true)
	f.Add([]byte("default: deny\nrules:\n  - allow: [exec]\n    identities: [\"ci-*\"]\n"), true)
	f.Add([]byte("default: allow\nrules:\n  - deny: [volume.remove]\n    volumes: [\"[\"]\n"), false)
	f.Add([]byte("default: maybe"), false)
	f.Add([]byte("rules: [[[["), false)
	f.Add([]byte{}, false)
	f.Fuzz(func(t *testing.T, data []byte, verified bool) {
		p, err := Parse(data, verified)
		if err != nil {
			return
		}
		for _, action := range []string{"exec", "portforward", "volume.remove", "image.prune.all", "logs"} {
			p.Decide(Request{Action: action, Identity: "ci-1", Service: "web", Port: 5432, User: "root", Volume: "prod_data"})
		}
	})
}
