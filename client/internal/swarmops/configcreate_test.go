// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"swarmexec/client/internal/swarmfake"
)

// CreateConfig creates through the manager API, with the labels, and refuses a
// taken name the way CreateSecret does: configs are immutable too.
func TestCreateConfig(t *testing.T) {
	fs := &swarmfake.Swarm{}
	api := httptest.NewServer(fs)
	defer api.Close()
	dcli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	defer dcli.Close()
	ctx := context.Background()

	if err := CreateConfig(ctx, dcli, "  nginx.conf  ", []byte("server {}\n"), map[string]string{"team": "web"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	cfgs, err := ListConfigs(ctx, dcli)
	if err != nil || len(cfgs) != 1 {
		t.Fatalf("list after create: %v, %v", cfgs, err)
	}
	if c := cfgs[0]; c.Name != "nginx.conf" || c.Labels["team"] != "web" {
		t.Errorf("created %+v", c)
	}
	if err := CreateConfig(ctx, dcli, "nginx.conf", []byte("x"), nil); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("a taken name must be refused, got %v", err)
	}
	if err := CreateConfig(ctx, dcli, " ", []byte("x"), nil); err == nil {
		t.Error("an empty name must be refused")
	}
	if err := CreateConfig(ctx, dcli, "empty", nil, nil); err == nil {
		t.Error("empty content must be refused")
	}
}
