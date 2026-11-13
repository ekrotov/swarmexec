// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"strings"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/resolve"
)

// Images are node-local and the manager API cannot see them at all — a node
// whose disk has filled with old layers is invisible from the cluster side.
// That is why this goes to each agent directly, like volumes do.

// nodeImages is one node's image footprint (agentcall.Images).
type nodeImages = agentcall.NodeImages

// imageSection renders a node's image footprint for the node detail. Absent
// when no agent answered, so an unreachable or too-old agent leaves the rest of
// the detail untouched rather than showing a confident zero.
func imageSection(ni nodeImages, known bool) string {
	if !known {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n  [gray]images on this node[-]\n")
	fmt.Fprintf(&b, "    %-10s %s in %d image(s)\n", "on disk", formatMemBytes(ni.Total), ni.Count)
	switch {
	case ni.Dangling > 0:
		fmt.Fprintf(&b, "    %-10s [green]%s[-] in untagged leftovers\n", "reclaim", formatMemBytes(ni.Dangling))
	default:
		fmt.Fprintf(&b, "    %-10s [gray]nothing untagged to reclaim[-]\n", "reclaim")
	}
	if ni.Unused > 0 {
		// Deliberately phrased as a cost, not an offer: these images belong to
		// something that is merely not running right now.
		fmt.Fprintf(&b, "    [gray]a further %s is tagged but unused — removing it means\n"+
			"               pulling those images again.[-]\n", formatMemBytes(ni.Unused))
	}
	return b.String()
}

// pruneConfirmText is the body of the confirm dialog. It is a pure function
// because it is the last thing an operator reads before something irreversible,
// and the two modes must not be able to drift into describing each other.
func pruneConfirmText(node string, ni nodeImages, all bool) string {
	if !all {
		return fmt.Sprintf(
			"Remove untagged images on %q?\n\n"+
				"Frees about [green]%s[white] in leftovers from rebuilds.\n\n"+
				"[gray]Nothing can start from an untagged image, so this cannot\n"+
				"stop a service or force a pull.[white]",
			node, formatMemBytes(ni.Dangling))
	}
	return fmt.Sprintf(
		"Remove [red]every unused image[white] on %q?\n\n"+
			"Frees about [green]%s[white] — the %s in untagged leftovers\n"+
			"plus %s of tagged images.\n\n"+
			"[red]This also removes images no container is running RIGHT NOW.[white]\n"+
			"[gray]On a swarm node that includes every service scaled to zero and\n"+
			"every task between restarts: each will have to pull its image again.\n"+
			"If the registry is unreachable, they will not come back up.[white]",
		node, formatMemBytes(ni.Dangling+ni.Unused), formatMemBytes(ni.Dangling), formatMemBytes(ni.Unused))
}

// nodeByHostname finds the dialable node for a hostname. The node table row
// carries no dial address — which address reaches an agent depends on the
// configured address mode, and only the resolver applies it.
func nodeByHostname(ctx context.Context, r *resolve.Resolver, hostname string) (resolve.Node, error) {
	nodes, err := r.Nodes(ctx)
	if err != nil {
		return resolve.Node{}, err
	}
	for _, n := range nodes {
		if n.Name == hostname {
			return n, nil
		}
	}
	return resolve.Node{}, fmt.Errorf("node %q is no longer in the cluster", hostname)
}
