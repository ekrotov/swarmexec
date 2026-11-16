// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/swarmfake"
	pb "swarmexec/internal/pb"
)

// volumeLsCluster: node 127.0.0.1 answers with three volumes (shop_data
// mounted by shop_web, old_cache, scratch without a creation time or size),
// node 127.0.0.2 does not answer. created is the volumes' creation stamp.
func volumeLsCluster(t *testing.T) (g *globalFlags, created string) {
	t.Helper()
	created = time.Now().Add(-49 * time.Hour).UTC().Format(time.RFC3339)
	agent := &fakeAgent{
		vols: []*pb.VolumeInfo{
			{Name: "shop_data", Driver: "local", CreatedAt: created, SizeBytes: 3 << 20, SizeKnown: true},
			{Name: "old_cache", Driver: "local", CreatedAt: created, SizeBytes: 1 << 20, SizeKnown: true},
			{Name: "scratch", Driver: "nfs"},
		},
		ctrs: []*pb.ContainerInfo{{Name: "shop_web.1.abc", Service: "shop_web", Volumes: []string{"shop_data"}}},
	}
	f := &swarmfake.Swarm{Nodes: []swarm.Node{readyNode("n1", agentNodeHost), readyNode("n2", "127.0.0.2")}}
	return cliCluster(t, f, agent), created
}

const volumeLsJSON = `[
  {
    "name": "shop_data",
    "driver": "local",
    "nodes": [
      "127.0.0.1"
    ],
    "used_by": 1,
    "consumers": [
      {
        "service": "shop_web",
        "container": "shop_web.1.abc",
        "node": "127.0.0.1"
      }
    ],
    "created_at": "STAMP"
  }
]
`

const volumeLsSizedJSON = `[
  {
    "name": "old_cache",
    "driver": "local",
    "nodes": [
      "127.0.0.1"
    ],
    "used_by": 0,
    "consumers": [],
    "created_at": "STAMP",
    "size_bytes": 1048576
  },
  {
    "name": "scratch",
    "driver": "nfs",
    "nodes": [
      "127.0.0.1"
    ],
    "used_by": 0,
    "consumers": []
  },
  {
    "name": "shop_data",
    "driver": "local",
    "nodes": [
      "127.0.0.1"
    ],
    "used_by": 1,
    "consumers": [
      {
        "service": "shop_web",
        "container": "shop_web.1.abc",
        "node": "127.0.0.1"
      }
    ],
    "created_at": "STAMP",
    "size_bytes": 3145728
  }
]
`

// `volume ls` end to end: table and JSON, with and without sizes, a name
// filter, the sort flags, and the warning for the node that did not answer.
func TestVolumeLsOutput(t *testing.T) {
	g, created := volumeLsCluster(t)
	const warn = "warning: node 127.0.0.2 unreachable: "
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"table", []string{"--connect-timeout", "2s"},
			"VOLUME     DRIVER  NODES      USED BY  AGE\n" +
				"old_cache  local   127.0.0.1  -        2d1h\n" +
				"scratch    nfs     127.0.0.1  -        -\n" +
				"shop_data  local   127.0.0.1  1        2d1h\n"},
		{"table sized, filtered, by size", []string{"--sort", "size", "shop", "--connect-timeout", "2s"},
			"VOLUME     DRIVER  NODES      USED BY  AGE   SIZE\n" +
				"shop_data  local   127.0.0.1  1        2d1h  3.146MB\n"},
		{"table sized, reversed", []string{"--size", "--sort", "used", "--reverse", "--connect-timeout", "2s"},
			"VOLUME     DRIVER  NODES      USED BY  AGE   SIZE\n" +
				"old_cache  local   127.0.0.1  -        2d1h  1.049MB\n" +
				"scratch    nfs     127.0.0.1  -        -     -\n" +
				"shop_data  local   127.0.0.1  1        2d1h  3.146MB\n"},
		{"json", []string{"--json", "data", "--connect-timeout", "2s"}, strings.ReplaceAll(volumeLsJSON, "STAMP", created)},
		{"json sized", []string{"--json", "--size", "--connect-timeout", "2s"}, strings.ReplaceAll(volumeLsSizedJSON, "STAMP", created)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errOut, err := runCmd(t, newVolumeLsCmd(g), c.args...)
			if err != nil {
				t.Fatal(err)
			}
			if out != c.want {
				t.Errorf("stdout:\n%s\nwant:\n%s", out, c.want)
			}
			if len(errOut) < len(warn) || errOut[:len(warn)] != warn {
				t.Errorf("stderr = %q, want the unreachable-node warning", errOut)
			}
		})
	}

	if _, _, err := runCmd(t, newVolumeLsCmd(g), "--sort", "colour"); err == nil || err.Error() != `invalid --sort "colour" (want name|nodes|used|age|size)` {
		t.Errorf("bad sort: %v", err)
	}
}
