// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"testing"

	"swarmexec/client/internal/clusterread"
	"swarmexec/client/internal/mcpaccess"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/stackfile"
)

// sanitizeStacks, piece by piece: file-side credentials masked (saying when
// they differ from the deployed value, never what either is), deployed env
// masked, access labels gone from every labelled object, and a deploy section
// that held only access labels dropped.
func TestSanitizeStacks(t *testing.T) {
	access := func(extra map[string]string) map[string]string {
		m := map[string]string{"swarmexec.mcp.enable": "true"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	deployed := &stackfile.Stack{
		Services: map[string]*stackfile.Service{
			"web": {
				Environment: map[string]string{"DB_PASSWORD": "old", "API_TOKEN": "same", "PORT": "80"},
				Labels:      access(map[string]string{"team": "a"}),
				Deploy:      &stackfile.Deploy{Labels: access(nil)},
			},
		},
		Networks: map[string]*stackfile.Network{"net": {Labels: access(nil)}},
		Secrets:  map[string]*stackfile.Resource{"s": {Labels: access(map[string]string{"k": "v"})}},
	}
	file := &stackfile.Stack{
		Services: map[string]*stackfile.Service{
			"web": {
				Environment: map[string]string{"DB_PASSWORD": "new", "API_TOKEN": "same", "PORT": "8080"},
				Deploy:      &stackfile.Deploy{Labels: access(map[string]string{"keep": "1"})},
			},
			"fresh": {Environment: map[string]string{"SECRET_KEY": "x"}},
		},
		Configs: map[string]*stackfile.Resource{"c": {Labels: access(nil)}},
		Volumes: map[string]*stackfile.Resource{"v": {Labels: access(nil)}},
	}
	sanitizeStacks(deployed, file)

	fw := file.Services["web"]
	if got := fw.Environment["DB_PASSWORD"]; got != mcpaccess.Masked+" (differs from deployed)" {
		t.Errorf("changed credential = %q", got)
	}
	if got := fw.Environment["API_TOKEN"]; got != mcpaccess.Masked {
		t.Errorf("unchanged credential = %q, want plain mask", got)
	}
	if got := fw.Environment["PORT"]; got != "8080" {
		t.Errorf("file PORT = %q, want it untouched", got)
	}
	if got := file.Services["fresh"].Environment["SECRET_KEY"]; got != mcpaccess.Masked {
		t.Errorf("credential of a service not deployed = %q, want plain mask", got)
	}
	if fw.Deploy == nil || len(fw.Deploy.Labels) != 1 || fw.Deploy.Labels["keep"] != "1" {
		t.Errorf("file deploy = %+v, want only the keep label", fw.Deploy)
	}

	dw := deployed.Services["web"]
	if dw.Environment["DB_PASSWORD"] != mcpaccess.Masked || dw.Environment["PORT"] != "80" {
		t.Errorf("deployed env = %v, want credentials masked and PORT kept", dw.Environment)
	}
	if len(dw.Labels) != 1 || dw.Labels["team"] != "a" {
		t.Errorf("deployed labels = %v, want only team", dw.Labels)
	}
	if dw.Deploy != nil {
		t.Errorf("deploy with only access labels = %+v, want nil", dw.Deploy)
	}
	for name, labels := range map[string]map[string]string{
		"network": deployed.Networks["net"].Labels,
		"config":  file.Configs["c"].Labels,
		"volume":  file.Volumes["v"].Labels,
	} {
		if labels != nil {
			t.Errorf("%s labels = %v, want nil", name, labels)
		}
	}
	if got := deployed.Secrets["s"].Labels; len(got) != 1 || got["k"] != "v" {
		t.Errorf("secret labels = %v, want only k", got)
	}
}

// Without agent usage a service row carries no health at all — absence is
// not "all healthy".
func TestOverviewServiceRowWithoutUsage(t *testing.T) {
	row := clusterread.OverviewServiceRow(resolve.Service{Name: "web", Running: 1, Desired: 2}, nil, nil)
	if row.Health != nil || row.Replicas != "1/2" {
		t.Errorf("row = %+v, want no health and replicas 1/2", row)
	}
}

// Nothing deployed yet: the file side is still masked and stripped.
func TestSanitizeStacksNothingDeployed(t *testing.T) {
	file := &stackfile.Stack{Services: map[string]*stackfile.Service{
		"web": {Environment: map[string]string{"DB_PASSWORD": "p"}, Labels: map[string]string{"swarmexec.mcp.enable": "true"}},
	}}
	sanitizeStacks(nil, file)
	if got := file.Services["web"].Environment["DB_PASSWORD"]; got != mcpaccess.Masked {
		t.Errorf("credential = %q, want plain mask", got)
	}
	if got := file.Services["web"].Labels; got != nil {
		t.Errorf("labels = %v, want nil", got)
	}
}
