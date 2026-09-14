// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// DockerClient is the narrow subset of github.com/docker/docker/client.Client
// the agent relies on. Defining it as an interface keeps the exec bridge
// testable with an in-memory fake. *client.Client satisfies it directly.
type DockerClient interface {
	ContainerList(ctx context.Context, options container.ListOptions) ([]types.Container, error)
	ContainerInspect(ctx context.Context, containerID string) (types.ContainerJSON, error)
	ContainerExecCreate(ctx context.Context, containerID string, config container.ExecOptions) (types.IDResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, config container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecResize(ctx context.Context, execID string, options container.ResizeOptions) error
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
	ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error)
	// The four below exist for port-forward sidecars: a container joined to the
	// target's network namespace, through whose stdio the agent bridges bytes.
	// See DESIGN-port-forward.md §5.
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerAttach(ctx context.Context, containerID string, options container.AttachOptions) (types.HijackedResponse, error)
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	VolumeList(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error)
	VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
	DiskUsage(ctx context.Context, options types.DiskUsageOptions) (types.DiskUsage, error)
	// ContainerStatsOneShot takes a single resource reading of one container.
	// One-shot rather than the streaming form on purpose: the stats cache keeps
	// the previous reading itself, which is what a CPU percentage needs, and a
	// stream per container would cost far more than it buys at our resolution.
	ContainerStatsOneShot(ctx context.Context, containerID string) (container.StatsResponseReader, error)
	// Images are node-local and invisible to the manager; these two back the
	// per-node image view and its cleanup.
	ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error)
	ImagesPrune(ctx context.Context, pruneFilter filters.Args) (image.PruneReport, error)
	// Info reports the node's own capacity (CPU count, total memory) — the
	// denominator for "how loaded is this node".
	Info(ctx context.Context) (system.Info, error)
	// Events streams docker daemon events; the volume-size cache watches
	// volume create/destroy to refresh reactively.
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
}

// swarmServiceLabel is the container label Docker sets on swarm task containers
// carrying the owning service's name.
const swarmServiceLabel = "com.docker.swarm.service.name"
