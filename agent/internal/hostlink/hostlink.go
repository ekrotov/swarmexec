// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package hostlink reads the node's own network interfaces from the host's
// sysfs.
//
// It exists because the agent cannot see them any other way. A swarm service
// gets its own network namespace — swarm has no `network_mode: host` — so the
// agent's own interfaces are a veth pair into a bridge, and asking the kernel
// about them answers a question nobody asked: it would report 1500 for a node
// whose real link carries 1450, which is exactly the wrong number to report,
// because 1500 is also what Docker already assumes.
//
// sysfs bind-mounted from the host does give the host's view. What it does not
// give is addresses — those live in the network namespace — so this package
// reports names, MTUs and types, and leaves the question of WHICH link is the
// underlay to the client, which knows the node's address and can show its
// reasoning.
package hostlink

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DefaultSysfs is where the agent's deployment mounts the host's sysfs. It is
// a mount, not a path the agent can reach on its own: without it Read returns
// nothing and every caller treats the MTU as unknown.
const DefaultSysfs = "/host/sys"

// Link is one interface as sysfs describes it.
type Link struct {
	Name      string
	MTU       uint32
	Operstate string
	// ARPType is sysfs "type": 1 Ethernet, 772 loopback, other for tunnels and
	// the like. Raw, because what counts as a candidate is the client's call.
	ARPType uint32
}

// Read lists the node's interfaces under sysfsRoot (…/class/net/*). A missing
// or unreadable root yields no links and no error: the mount is optional, and
// a node without it reports "unknown", which every caller must already handle.
func Read(sysfsRoot string) []Link {
	root := strings.TrimSpace(sysfsRoot)
	if root == "" {
		return nil
	}
	dir := filepath.Join(root, "class", "net")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]Link, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if name == "" || strings.ContainsAny(name, "/\\") {
			continue
		}
		mtu := readUint(filepath.Join(dir, name, "mtu"))
		if mtu == 0 {
			// No MTU means the entry is not a link we can say anything about
			// (a dangling symlink, a races-away interface). Reporting it with
			// a zero would invite a caller to treat 0 as a measurement.
			continue
		}
		out = append(out, Link{
			Name:      name,
			MTU:       mtu,
			Operstate: readString(filepath.Join(dir, name, "operstate")),
			ARPType:   readUint(filepath.Join(dir, name, "type")),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func readString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readUint(path string) uint32 {
	n, err := strconv.ParseUint(readString(path), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}
