// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/docker/cli/cli/connhelper/ssh"
)

// Every ssh hop swarmexec makes starts its own `ssh` process: one per exec, per
// log stream, per port-forward, per stats poll, per node, every refresh cycle.
// Each of those paid for a TCP handshake, a key exchange and an authentication
// before a single byte of ours moved — against a bastion that was already
// connected a moment ago, for the same user, on the same port.
//
// OpenSSH has had the answer since 2004: the first connection to a destination
// opens a socket, and every later one becomes a channel on it. The remaining
// cost is the channel. Nothing about our own traffic changes.
//
// This is deliberately built out of ssh's own options rather than a master
// process we start and have to reap. A master we owned would need a lifetime,
// a shutdown path and a story for the case where swarmexec is killed — and a
// leftover master that nobody can reach is worse than no sharing at all.
// ControlPersist gives the master a lifetime that survives us without needing
// us.

// sshMuxEnv switches connection sharing off. It exists because our flags land
// on the ssh command line, which outranks ~/.ssh/config: an operator who has
// deliberately configured ControlPath themselves needs a way to keep it, and
// sharing is the kind of thing that is blamed first when a bastion misbehaves.
// Setting it to 0/off/false returns behaviour to one process per dial exactly.
const sshMuxEnv = "SWARMEXEC_SSH_MULTIPLEX"

// sshMuxPersist is how long the master outlives its last channel.
//
// Not "yes": a master that never exits on its own is a process an operator did
// not start, cannot see, and will find still holding a bastion connection hours
// after closing the tool. A minute covers the way swarmexec actually works —
// the ui refreshes every ten seconds, and a run of commands in a terminal comes
// in bursts — while bounding how long anything of ours lives past the exit.
const sshMuxPersist = "60s"

// sshMuxPathLimit keeps the control socket inside the kernel's sun_path limit
// (108 bytes on Linux, 104 on macOS). Over it, ssh does not truncate and retry
// — it fails the connection outright, which would turn a performance
// improvement into an outage. Below the cap we share; above it we do not.
const sshMuxPathLimit = 100

// sshMuxOpts returns the ssh options that make connections to this destination
// share one transport, or nil when sharing is unavailable or switched off.
//
// The keepalive options travel WITH the sharing on purpose. Without sharing, an
// ssh connection that has stopped answering costs exactly one dial: the caller
// times out and everything else carries on. With sharing it costs all of them —
// the hung master is the transport, and every new channel waits on it. So the
// thing that bounds that failure is part of the thing that introduces it, and
// the opt-out above removes both together rather than leaving half a change
// behind.
func sshMuxOpts(sp *ssh.Spec, proxyJump string) []string {
	// Windows OpenSSH has never implemented connection sharing; passing these
	// would fail the connection rather than be ignored.
	if runtime.GOOS == "windows" {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(sshMuxEnv))) {
	case "0", "off", "false", "no":
		return nil
	}
	path := sshControlPath(sp, proxyJump)
	if path == "" {
		return nil
	}
	// A race is possible here and is harmless: several dials starting at once
	// may each try to become master, and the ones that lose the bind fall back
	// to an ordinary connection. That is today's behaviour, for the first burst
	// only — once a master exists, every later dial rides it.
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + path,
		"-o", "ControlPersist=" + sshMuxPersist,
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
}

// sshControlPath is the socket two dials must agree on to share a connection,
// and must NOT agree on when they are not the same connection.
//
// ssh offers %C for this and it is not enough: it hashes local host, remote
// host, port and user — but not the jump hosts. Two Docker contexts naming the
// same manager through different bastions hash identically under %C, and the
// second one would then be silently carried over the first one's route. Since
// B4 the whole point of this code is that a channel cannot end up at a cluster
// other than the one it was resolved for; a socket key that ignores the route
// would reintroduce exactly that, one layer down.
func sshControlPath(sp *ssh.Spec, proxyJump string) string {
	dir := sshMuxDir()
	if dir == "" {
		return ""
	}
	// NUL separates the fields so no combination of values can collide by
	// running into its neighbour.
	key := strings.Join([]string{
		sp.User, sp.Host, sp.Port, strings.TrimSpace(proxyJump),
	}, "\x00")
	sum := sha256.Sum256([]byte(key))
	path := filepath.Join(dir, hex.EncodeToString(sum[:])[:16])
	if len(path) > sshMuxPathLimit {
		return ""
	}
	return path
}

// sshMuxDir is where the control sockets live.
//
// Whoever can write this directory can put a socket in it, and a control socket
// is a live authenticated session on the bastion — reaching it is reaching the
// far end. So it is never /tmp, however convenient: a shared directory means an
// attacker can pre-create ours and MkdirAll will happily succeed on it.
// XDG_RUNTIME_DIR is the right home (per-user, 0700, on tmpfs, cleared at
// logout); the user's cache directory is the fallback, and both sit inside the
// user's own tree. If neither is available we simply do not share.
//
// Stale sockets — from a master killed with SIGKILL rather than allowed to
// expire — are left alone deliberately. ssh already handles them: it finds the
// socket dead, says so, and opens an ordinary connection. Sweeping the
// directory instead risks deleting the socket of a master that is very much
// alive, which would orphan it with no way to reach it.
func sshMuxDir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if strings.TrimSpace(base) == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return ""
		}
		base = cache
	}
	dir := filepath.Join(base, "swarmexec", "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return dir
}

// sshEndpointOpts is the full set of ssh flags for reaching one endpoint: the
// jump hosts it must hop through, and the sharing that keeps those hops from
// being paid again.
//
// Both channels to a cluster go through here — the Docker API's connection
// helper and the agent tunnel — which is what lets them share a transport
// instead of opening one each. They already route identically by construction
// (dockerEndpoint); now they connect identically too.
//
// A host that will not parse yields the jump flags alone rather than an error:
// the endpoint is unusable for other reasons and will say so in a moment, and
// declining to share is never the interesting failure.
func sshEndpointOpts(host, proxyJump string) []string {
	flags := proxyJumpFlags(proxyJump)
	sp, err := ssh.ParseURL(host)
	if err != nil {
		return flags
	}
	return append(flags, sshMuxOpts(sp, proxyJump)...)
}
