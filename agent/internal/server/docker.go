// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"github.com/moby/moby/client"
)

// DockerClient is the narrow subset of github.com/moby/moby/client.Client
// the agent relies on. Defining it as an interface keeps the exec bridge
// testable with an in-memory fake. *client.Client satisfies it directly
// (asserted below).
type DockerClient interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ExecCreate(ctx context.Context, containerID string, options client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(ctx context.Context, execID string, options client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecResize(ctx context.Context, execID string, options client.ExecResizeOptions) (client.ExecResizeResult, error)
	ExecInspect(ctx context.Context, execID string, options client.ExecInspectOptions) (client.ExecInspectResult, error)
	ContainerLogs(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	// The four below exist for port-forward sidecars: a container joined to the
	// target's network namespace, through whose stdio the agent bridges bytes.
	// See DESIGN-port-forward.md §5.
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerAttach(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	VolumeList(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error)
	VolumeCreate(ctx context.Context, options client.VolumeCreateOptions) (client.VolumeCreateResult, error)
	VolumeRemove(ctx context.Context, volumeID string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error)
	DiskUsage(ctx context.Context, options client.DiskUsageOptions) (client.DiskUsageResult, error)
	// ContainerStats takes a resource reading of one container. Callers must
	// ask for a single one-shot sample (Stream: false, IncludePreviousSample:
	// false), not the streaming form, on purpose: the stats cache keeps the
	// previous reading itself, which is what a CPU percentage needs, and a
	// stream per container would cost far more than it buys at our resolution.
	ContainerStats(ctx context.Context, containerID string, options client.ContainerStatsOptions) (client.ContainerStatsResult, error)
	// Images are node-local and invisible to the manager; these two back the
	// per-node image view and its cleanup.
	ImageList(ctx context.Context, options client.ImageListOptions) (client.ImageListResult, error)
	ImagePrune(ctx context.Context, options client.ImagePruneOptions) (client.ImagePruneResult, error)
	// Info reports the node's own capacity (CPU count, total memory) — the
	// denominator for "how loaded is this node".
	Info(ctx context.Context, options client.InfoOptions) (client.SystemInfoResult, error)
	// Events streams docker daemon events; the volume-size cache watches
	// volume create/destroy to refresh reactively.
	Events(ctx context.Context, options client.EventsListOptions) client.EventsResult
}

// The real Docker client must keep satisfying the interface; this breaks the
// build (rather than main.go at wiring time) if an upstream signature drifts.
var _ DockerClient = (*client.Client)(nil)

// swarmServiceLabel is the container label Docker sets on swarm task containers
// carrying the owning service's name.
const swarmServiceLabel = "com.docker.swarm.service.name"
