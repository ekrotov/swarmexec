// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/docker/cli/cli/connhelper/ssh"
)

// muxEnv points the control sockets at a directory this test owns, so nothing
// here depends on the developer's XDG_RUNTIME_DIR or home directory.
func muxEnv(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("connection sharing is not available on Windows")
	}
	t.Setenv(sshMuxEnv, "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}

// optValue pulls the value of an `-o Name=value` pair out of an ssh argv.
func optValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-o" && strings.HasPrefix(args[i+1], name+"=") {
			return strings.TrimPrefix(args[i+1], name+"=")
		}
	}
	return ""
}

// The whole point: the Docker API channel and the agent tunnel must land on the
// SAME socket for the same endpoint, or they open a connection each and the
// sharing buys nothing for the case it was built for.
func TestBothChannelsShareOneSocket(t *testing.T) {
	muxEnv(t)
	const host = "ssh://deploy@bastion:2222"
	const jump = "edge"

	sp, err := ssh.ParseURL(host)
	if err != nil {
		t.Fatal(err)
	}
	api := optValue(sshEndpointOpts(host, jump), "ControlPath")
	tunnel := optValue(sshForwardArgs(sp, "node-1:9443", jump), "ControlPath")

	if api == "" || tunnel == "" {
		t.Fatalf("both channels should share: api=%q tunnel=%q", api, tunnel)
	}
	if api != tunnel {
		t.Errorf("the two channels to one endpoint must share a socket:\n api    = %s\n tunnel = %s", api, tunnel)
	}
}

// Two dials to the same destination have to agree on the socket across separate
// processes, so the key may not contain anything per-run.
func TestControlPathIsStableForOneDestination(t *testing.T) {
	muxEnv(t)
	sp := &ssh.Spec{User: "deploy", Host: "bastion", Port: "22"}

	first := sshControlPath(sp, "edge")
	for i := 0; i < 5; i++ {
		if again := sshControlPath(sp, "edge"); again != first {
			t.Fatalf("control path is not stable: %s vs %s", first, again)
		}
	}
}

// ssh's own %C token hashes user, host and port but NOT the jump hosts. Two
// contexts reaching one manager through different bastions would share a socket
// under it, and the second would silently travel the first one's route — the
// same class of mistake B4 removed one layer up. The key must separate them.
func TestRouteIsPartOfTheSocketIdentity(t *testing.T) {
	muxEnv(t)
	sp := &ssh.Spec{User: "deploy", Host: "manager", Port: "22"}

	direct := sshControlPath(sp, "")
	viaEdge := sshControlPath(sp, "edge")
	viaOther := sshControlPath(sp, "other")

	if direct == "" {
		t.Fatal("expected a control path")
	}
	if direct == viaEdge || viaEdge == viaOther || direct == viaOther {
		t.Errorf("different routes must not share a socket: direct=%s edge=%s other=%s", direct, viaEdge, viaOther)
	}
}

// Everything that selects a different connection has to select a different
// socket, not just the route.
func TestDestinationFieldsSeparateSockets(t *testing.T) {
	muxEnv(t)
	base := &ssh.Spec{User: "deploy", Host: "bastion", Port: "22"}
	seen := map[string]string{}
	for name, sp := range map[string]*ssh.Spec{
		"base":       base,
		"other user": {User: "ops", Host: "bastion", Port: "22"},
		"other host": {User: "deploy", Host: "bastion-2", Port: "22"},
		"other port": {User: "deploy", Host: "bastion", Port: "2222"},
		"no user":    {Host: "bastion", Port: "22"},
	} {
		p := sshControlPath(sp, "")
		if prev, dup := seen[p]; dup {
			t.Errorf("%q and %q share a socket but are different connections", name, prev)
		}
		seen[p] = name
	}
}

// Over the kernel's sun_path limit ssh does not truncate and carry on — it
// fails the connection. Declining to share is the only safe answer, because the
// alternative turns a speed-up into an outage.
func TestOverlongSocketPathDisablesSharing(t *testing.T) {
	muxEnv(t)
	deep := filepath.Join(t.TempDir(), strings.Repeat("d", 80))
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", deep)

	sp := &ssh.Spec{User: "deploy", Host: "bastion"}
	if p := sshControlPath(sp, ""); p != "" {
		t.Errorf("a path of %d bytes should have disabled sharing, got %s", len(p), p)
	}
	if opts := sshMuxOpts(sp, ""); opts != nil {
		t.Errorf("no socket means no sharing options, got %v", opts)
	}
}

// A control socket is a live authenticated session on the bastion; whoever can
// write the directory can reach the far end. It belongs in the user's own tree,
// created private.
func TestSocketDirectoryIsPrivateToTheUser(t *testing.T) {
	muxEnv(t)
	dir := sshMuxDir()
	if dir == "" {
		t.Fatal("expected a socket directory")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("socket directory is reachable by others: mode %04o", mode)
	}
	if !strings.HasPrefix(dir, os.Getenv("XDG_RUNTIME_DIR")) {
		t.Errorf("socket directory should follow XDG_RUNTIME_DIR, got %s", dir)
	}
}

// The opt-out has to return behaviour to exactly one process per dial —
// including the keepalives, which only exist to bound the failure that sharing
// introduces. Half a change left behind is worse than none.
func TestOptOutRemovesEverything(t *testing.T) {
	muxEnv(t)
	sp := &ssh.Spec{User: "deploy", Host: "bastion"}
	for _, v := range []string{"0", "off", "false", "no", "OFF"} {
		t.Setenv(sshMuxEnv, v)
		if opts := sshMuxOpts(sp, ""); opts != nil {
			t.Errorf("%s=%q should switch sharing off, got %v", sshMuxEnv, v, opts)
		}
		for _, arg := range sshForwardArgs(sp, "node-1:9443", "") {
			if arg == "-o" {
				t.Errorf("%s=%q should leave the plain argv, got %v",
					sshMuxEnv, v, sshForwardArgs(sp, "node-1:9443", ""))
				break
			}
		}
	}
}

// The master must outlive its last channel — that is what makes the NEXT dial
// cheap — but it must not outlive it forever, or the operator is left with a
// bastion connection they did not start and cannot see.
func TestMasterHasABoundedLifetime(t *testing.T) {
	muxEnv(t)
	opts := sshMuxOpts(&ssh.Spec{Host: "bastion"}, "")
	persist := optValue(opts, "ControlPersist")
	if persist == "" || persist == "yes" {
		t.Errorf("ControlPersist must be a finite time, got %q", persist)
	}
	if optValue(opts, "ServerAliveInterval") == "" {
		t.Error("a shared master that stops answering blocks every later channel; it needs a keepalive")
	}
}
