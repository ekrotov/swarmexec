package server

import (
	"context"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
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
	VolumeList(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
	DiskUsage(ctx context.Context, options types.DiskUsageOptions) (types.DiskUsage, error)
}

// swarmServiceLabel is the container label Docker sets on swarm task containers
// carrying the owning service's name.
const swarmServiceLabel = "com.docker.swarm.service.name"
