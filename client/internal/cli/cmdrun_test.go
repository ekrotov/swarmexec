// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cliconfig "github.com/docker/cli/cli/config"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"swarmexec/client/internal/swarmfake"
	pb "swarmexec/internal/pb"
)

// These run CLI commands end to end — flags in, printed text out — against a
// swarmfake manager and one fake agent, the way runInitOn does for init.

// agentNodeHost is the hostname the fake cluster's agent node reports: with
// addr_mode hostname, that is where the agent is dialled.
const agentNodeHost = "127.0.0.1"

// cliCluster points the Docker environment at f and writes a client config
// whose agent port is agent's (nil: a port nobody listens on). Nodes that
// should reach the agent are named agentNodeHost; any other loopback name
// (127.0.0.2) refuses.
func cliCluster(t *testing.T, f *swarmfake.Swarm, agent pb.AgentServer) *globalFlags {
	t.Helper()
	api := swarmfake.Serve(t, f)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(api.URL, "http://"))
	t.Setenv("DOCKER_CONTEXT", "")
	dir := t.TempDir()
	prev := cliconfig.Dir()
	cliconfig.SetDir(dir)
	t.Cleanup(func() { cliconfig.SetDir(prev) })
	t.Setenv("DOCKER_CONFIG", dir)

	lis, err := net.Listen("tcp", agentNodeHost+":0")
	if err != nil {
		t.Fatal(err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	if agent != nil {
		gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{swarmfake.SelfSignedPair(t)}})))
		pb.RegisterAgentServer(gs, agent)
		go func() { _ = gs.Serve(lis) }()
		t.Cleanup(gs.Stop)
	} else {
		lis.Close()
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf("port: %d\naddr_mode: hostname\ninsecure: true\nagent_secret: s3cret\n", port)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return &globalFlags{configPath: cfgPath}
}

// runCmd executes cmd with args and returns what it printed. The commands
// write to os.Stdout/os.Stderr directly, so both are swapped for the run.
func runCmd(t *testing.T, cmd *cobra.Command, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	capture := func(f **os.File) func() string {
		r, w, perr := os.Pipe()
		if perr != nil {
			t.Fatal(perr)
		}
		orig := *f
		*f = w
		var buf bytes.Buffer
		done := make(chan struct{})
		go func() { _, _ = io.Copy(&buf, r); close(done) }()
		return func() string {
			*f = orig
			w.Close()
			<-done
			r.Close()
			return buf.String()
		}
	}
	stopOut, stopErr := capture(&os.Stdout), capture(&os.Stderr)
	cmd.SetArgs(args)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	err = cmd.Execute()
	return stopOut(), stopErr(), err
}
