// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"fmt"
	"io"
	"strings"

	"swarmexec/client/internal/secscan"
)

// NameFromPath derives a stack name from a file name the way an operator
// would expect: "prod/web.yml" is the "web" stack.
func NameFromPath(path string) string {
	base := path
	if i := lastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		if len(base) > len(ext) && base[len(base)-len(ext):] == ext {
			return base[:len(base)-len(ext)]
		}
	}
	return base
}

func lastIndexAny(s, chars string) int {
	for i := len(s) - 1; i >= 0; i-- {
		for _, c := range chars {
			if rune(s[i]) == c {
				return i
			}
		}
	}
	return -1
}

// WritePlanReport prints what the checks found, worst first.
func WritePlanReport(w io.Writer, plan *Plan) {
	if len(plan.Unsupported) > 0 {
		fmt.Fprintf(w, "note: swarm ignores these keys in %s: %s\n\n",
			plan.Path, strings.Join(plan.Unsupported, ", "))
	}
	flagged := plan.Flagged()
	if len(flagged) == 0 {
		// Name what was looked for, or "nothing found" is a claim with nothing
		// behind it — the same rule the risks overlay follows.
		fmt.Fprintf(w, "%d service(s) checked, nothing above informational.\n", len(plan.Services))
		fmt.Fprintf(w, "Checked: %s.\n\n", strings.Join(secscan.Checks(), ", "))
		return
	}
	fmt.Fprintf(w, "%d of %d service(s) flagged in %s:\n\n", len(flagged), len(plan.Services), plan.Path)
	for _, s := range flagged {
		fmt.Fprintf(w, "  %s\n", s.Name)
		for _, find := range s.Findings {
			if find.Severity == secscan.SevLow {
				continue // informational; true of nearly every service
			}
			fmt.Fprintf(w, "    %-7s %s — %s\n", find.Severity, find.Title, find.Detail)
		}
		fmt.Fprintln(w)
	}
}
