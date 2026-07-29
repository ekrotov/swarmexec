// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/docker/docker/api/types/volume"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestVolumeSizeCache_RefreshAndGet(t *testing.T) {
	d := newFakeDocker()
	d.volumes = []*volume.Volume{
		{Name: "a", UsageData: &volume.UsageData{Size: 100}},
		{Name: "b", UsageData: &volume.UsageData{Size: 200}},
	}
	c := newVolumeSizeCache(d, quietLogger(), 0)

	// Before any scan the cache is not ready (the cli then renders "…").
	if _, _, ready := c.get(); ready {
		t.Fatal("cache should not be ready before the first refresh")
	}

	c.refresh(context.Background())
	sizes, at, ready := c.get()
	if !ready {
		t.Fatal("cache should be ready after refresh")
	}
	if at.IsZero() {
		t.Error("computedAt should be set after refresh")
	}
	if sizes["a"] != 100 || sizes["b"] != 200 {
		t.Errorf("sizes = %v, want a=100 b=200", sizes)
	}

	// A failing scan keeps the previous cache rather than blanking it.
	d.diskUsageErr = errors.New("boom")
	c.refresh(context.Background())
	if s2, _, ready := c.get(); !ready || s2["a"] != 100 {
		t.Errorf("failed refresh should keep the previous cache, got ready=%v a=%d", ready, s2["a"])
	}
}

func TestVolumeSizeCache_DefaultInterval(t *testing.T) {
	c := newVolumeSizeCache(newFakeDocker(), quietLogger(), 0)
	if c.interval != volumeSizeInterval {
		t.Errorf("interval = %v, want default %v", c.interval, volumeSizeInterval)
	}
}
