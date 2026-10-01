// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Command swarmexec is the operator CLI: an interactive, cluster-wide
// `docker exec -it` for Docker Swarm. See client/REQUIREMENTS-client.md.
package main

import (
	"os"

	"swarmexec/client/internal/cli"
	"swarmexec/internal/pb"
)

// version is the binary's release, injected at build time via -ldflags. The
// protocol version is not build metadata and is not injected: it is
// pb.ProtocolVersion, compiled from the same package as the agent's.
var version = "dev"

func main() {
	os.Exit(cli.Execute(cli.Version{Binary: version, Proto: pb.ProtocolVersion}))
}
