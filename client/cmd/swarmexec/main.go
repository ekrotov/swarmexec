// Command swarmexec is the operator CLI: an interactive, cluster-wide
// `docker exec -it` for Docker Swarm. See client/REQUIREMENTS-client.md.
package main

import (
	"os"

	"swarmexec/client/internal/cli"
)

// Injected at build time via -ldflags. protoVersion tracks CONTRACT.md's proto
// package/version (CONTRACT.md §7).
var (
	version      = "dev"
	protoVersion = "swarmexec/v1"
)

func main() {
	os.Exit(cli.Execute(cli.Version{Binary: version, Proto: protoVersion}))
}
