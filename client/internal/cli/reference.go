// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"github.com/spf13/cobra"
)

// ReferenceRoot returns the whole command tree as `swarmexec --help` shows it,
// without running anything: the source of the generated CLI reference
// (site/docs/cli.html, `make cli-docs`). The version is left empty so the
// output does not depend on the build; cobra's own --version flag and
// completion command, which it adds only when the root executes, are added
// here so the reference lists them too.
func ReferenceRoot() *cobra.Command {
	root := newRootCmd(&globalFlags{}, Version{})
	root.InitDefaultVersionFlag()
	root.InitDefaultCompletionCmd()
	return root
}
