// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/swarmfake"
)

// An empty cluster says "no stacks" on stderr; stdout stays one stack name
// per line, nothing else.
func TestStackLsEmptyAndListed(t *testing.T) {
	g := cliCluster(t, &swarmfake.Swarm{}, nil)
	out, errOut, err := runCmd(t, newStackLsCmd(g))
	if err != nil || out != "" || errOut != "no stacks\n" {
		t.Errorf("empty: %q %q %v", out, errOut, err)
	}

	g = cliCluster(t, &swarmfake.Swarm{Services: []swarm.Service{stackedService("s1", "shop_web", "shop", 1)}}, nil)
	out, errOut, err = runCmd(t, newStackLsCmd(g))
	if err != nil || out != "shop\n" || errOut != "" {
		t.Errorf("listed: %q %q %v", out, errOut, err)
	}
}

// stack diff speaks --json like every other command; the old --format json
// still works but is out of the help.
func TestStackDiffJSONFlag(t *testing.T) {
	file := filepath.Join(t.TempDir(), "shop.yml")
	if err := os.WriteFile(file, []byte("services:\n  web:\n    image: nginx:1.28\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := cliCluster(t, &swarmfake.Swarm{Services: []swarm.Service{stackedService("s1", "shop_web", "shop", 1)}}, nil)
	for _, args := range [][]string{{"--json", file}, {"--format", "json", file}} {
		out, _, err := runCmd(t, newStackDiffCmd(g), args...)
		var got struct {
			Differs bool `json:"differs"`
		}
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil || !got.Differs {
			t.Errorf("%v: %q (%v)", args, out, jerr)
		}
		var ce *cliError
		if !errors.As(err, &ce) || ce.code != 1 {
			t.Errorf("%v: exit %v", args, err)
		}
	}
	if f := newStackDiffCmd(g).Flags().Lookup("format"); f == nil || !f.Hidden {
		t.Error("--format should stay, hidden")
	}
}

// stack deploy's gate override is --despite-findings; --force stays as a
// hidden alias that needs no value and sets the same flag.
func TestStackDeployDespiteFindingsAlias(t *testing.T) {
	cmd := newStackDeployCmd(&globalFlags{})
	fs := cmd.Flags()
	if f := fs.Lookup("force"); f == nil || !f.Hidden {
		t.Fatal("--force should stay, hidden")
	}
	if err := fs.Parse([]string{"--force"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := fs.GetBool("despite-findings"); !v {
		t.Error("--force did not set --despite-findings")
	}
}

// An empty volume ls keeps its header and says why there is nothing under it.
func TestVolumeLsEmptyNote(t *testing.T) {
	if got := noVolumesNote(nil); got != "no volumes" {
		t.Error(got)
	}
	if got := noVolumesNote([]string{"pg"}); got != `no volumes matching "pg"` {
		t.Error(got)
	}
}
