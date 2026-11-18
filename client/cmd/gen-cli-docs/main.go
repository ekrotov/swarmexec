// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Command gen-cli-docs writes the site's CLI reference (site/docs/cli.html)
// from the swarmexec command tree. Run it from the repository root, via
// `make cli-docs`; `make cli-docs-check` regenerates and fails on a diff.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"swarmexec/client/internal/cli"
	"swarmexec/client/internal/clidocs"
)

func main() {
	site := flag.String("site", "site", "the site directory (partials/ are read from it)")
	out := flag.String("o", "site/docs/cli.html", "the page to write")
	flag.Parse()

	page, err := clidocs.RenderSite(*site, cli.ReferenceRoot())
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-cli-docs:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, page, 0o644); err != nil { // #nosec G306 -- a public web page
		fmt.Fprintln(os.Stderr, "gen-cli-docs:", err)
		os.Exit(1)
	}

	// The exit-code table on the configuration page comes from the code too.
	conf := filepath.Join(*site, "docs", "configuration.html")
	prev, err := os.ReadFile(conf) // #nosec G304 -- a path under the site directory
	if err == nil {
		var upd string
		if upd, err = cli.ApplyExitTable(string(prev)); err == nil {
			err = os.WriteFile(conf, []byte(upd), 0o644) // #nosec G306 -- a public web page
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-cli-docs:", err)
		os.Exit(1)
	}
}
