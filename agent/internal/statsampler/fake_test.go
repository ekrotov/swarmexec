// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package statsampler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
)

// fakeDocker is the sampler's four Docker calls over in-memory state. Stats
// frames are served in order per container, the last one repeating, so two
// passes see two readings.
type fakeDocker struct {
	mu          sync.Mutex
	containers  []container.Summary
	listErr     error
	inspect     map[string]container.InspectResponse
	inspects    int
	statsFrames map[string][]container.StatsResponse
	statsIdx    map[string]int
	statsOpts   []client.ContainerStatsOptions
	info        system.Info
	infoCalls   int
}

func (f *fakeDocker) ContainerList(_ context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return client.ContainerListResult{}, f.listErr
	}
	return client.ContainerListResult{Items: append([]container.Summary(nil), f.containers...)}, nil
}

func (f *fakeDocker) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects++
	if c, ok := f.inspect[id]; ok {
		return client.ContainerInspectResult{Container: c}, nil
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{ID: id, Config: &container.Config{}}}, nil
}

func (f *fakeDocker) ContainerStats(_ context.Context, id string, opts client.ContainerStatsOptions) (client.ContainerStatsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsOpts = append(f.statsOpts, opts)
	frames := f.statsFrames[id]
	if len(frames) == 0 {
		return client.ContainerStatsResult{}, errors.New("no stats for " + id)
	}
	if f.statsIdx == nil {
		f.statsIdx = map[string]int{}
	}
	i := min(f.statsIdx[id], len(frames)-1)
	f.statsIdx[id] = i + 1
	body, err := json.Marshal(frames[i])
	if err != nil {
		return client.ContainerStatsResult{}, err
	}
	return client.ContainerStatsResult{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (f *fakeDocker) Info(_ context.Context, _ client.InfoOptions) (client.SystemInfoResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoCalls++
	return client.SystemInfoResult{Info: f.info}, nil
}

func (f *fakeDocker) inspectCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inspects
}

func (f *fakeDocker) statsCallOpts() []client.ContainerStatsOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]client.ContainerStatsOptions(nil), f.statsOpts...)
}
