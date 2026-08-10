// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeDocker is an in-memory DockerClient. The exec "process" is driven by a
// net.Pipe: the agent writes stdin to one end and reads output from it; the
// test plays the container side via the other end.
type fakeDocker struct {
	mu sync.Mutex

	containers []types.Container
	inspect    map[string]types.ContainerJSON

	listErr   error
	createErr error
	attachErr error

	// container side of the attach pipe, handed to the test.
	containerConn net.Conn
	// attachedCh delivers the container side to the test once attach happens.
	attachedCh chan net.Conn
	// exit code returned by ContainerExecInspect.
	exitCode int
	running  bool

	resizes   []container.ResizeOptions
	resizeErr error

	execTty bool

	logsReader io.ReadCloser
	logsErr    error

	volumes         []*volume.Volume
	volumeRemErr    error
	removedVols     []string
	volumeCreateErr error
	createdVolOpts  volume.CreateOptions
	diskUsageErr    error

	// port-forward sidecar state
	createContainerErr error
	startErr           error
	createdConfig      *container.Config
	createdHostConfig  *container.HostConfig
	removedContainers  []string
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{inspect: map[string]types.ContainerJSON{}, attachedCh: make(chan net.Conn, 1)}
}

// waitAttach blocks until the agent attaches and returns the container side.
func (f *fakeDocker) waitAttach() net.Conn { return <-f.attachedCh }

func (f *fakeDocker) ContainerList(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.containers, nil
}

func (f *fakeDocker) ContainerInspect(_ context.Context, id string) (types.ContainerJSON, error) {
	if c, ok := f.inspect[id]; ok {
		return c, nil
	}
	return types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{ID: id}, Config: &container.Config{}}, nil
}

func (f *fakeDocker) ContainerExecCreate(_ context.Context, _ string, cfg container.ExecOptions) (types.IDResponse, error) {
	if f.createErr != nil {
		return types.IDResponse{}, f.createErr
	}
	f.execTty = cfg.Tty
	return types.IDResponse{ID: "exec-123"}, nil
}

// ContainerExecAttach returns a HijackedResponse wired to one end of a net.Pipe.
// The test holds the other end via TestConn().
func (f *fakeDocker) ContainerExecAttach(_ context.Context, _ string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	if f.attachErr != nil {
		return types.HijackedResponse{}, f.attachErr
	}
	agentSide, containerSide := newHalfDuplex()
	f.containerConn = containerSide
	f.attachedCh <- containerSide
	return types.HijackedResponse{Conn: agentSide, Reader: bufio.NewReader(agentSide)}, nil
}

// --- port-forward sidecar surface ---

func (f *fakeDocker) ContainerCreate(_ context.Context, cfg *container.Config, hostCfg *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createContainerErr != nil {
		return container.CreateResponse{}, f.createContainerErr
	}
	f.createdConfig, f.createdHostConfig = cfg, hostCfg
	return container.CreateResponse{ID: "sidecar-1"}, nil
}

func (f *fakeDocker) ContainerStart(_ context.Context, _ string, _ container.StartOptions) error {
	return f.startErr
}

// ContainerAttach mirrors ContainerExecAttach: the test drives the sidecar side
// of the pipe, writing stdcopy-framed control lines and payload.
func (f *fakeDocker) ContainerAttach(_ context.Context, _ string, _ container.AttachOptions) (types.HijackedResponse, error) {
	if f.attachErr != nil {
		return types.HijackedResponse{}, f.attachErr
	}
	agentSide, containerSide := newHalfDuplex()
	f.containerConn = containerSide
	f.attachedCh <- containerSide
	return types.HijackedResponse{Conn: agentSide, Reader: bufio.NewReader(agentSide)}, nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedContainers = append(f.removedContainers, id)
	return nil
}

// RemovedContainers reports sidecars the agent cleaned up.
func (f *fakeDocker) RemovedContainers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removedContainers...)
}

func (f *fakeDocker) ContainerExecResize(_ context.Context, _ string, opts container.ResizeOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, opts)
	return f.resizeErr
}

func (f *fakeDocker) ContainerExecInspect(_ context.Context, _ string) (container.ExecInspect, error) {
	return container.ExecInspect{ExitCode: f.exitCode, Running: f.running}, nil
}

func (f *fakeDocker) ContainerLogs(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	if f.logsReader != nil {
		return f.logsReader, nil
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeDocker) VolumeList(_ context.Context, _ volume.ListOptions) (volume.ListResponse, error) {
	return volume.ListResponse{Volumes: f.volumes}, nil
}

func (f *fakeDocker) VolumeCreate(_ context.Context, opts volume.CreateOptions) (volume.Volume, error) {
	if f.volumeCreateErr != nil {
		return volume.Volume{}, f.volumeCreateErr
	}
	f.createdVolOpts = opts
	return volume.Volume{
		Name:       opts.Name,
		Driver:     opts.Driver,
		Mountpoint: "/var/lib/docker/volumes/" + opts.Name + "/_data",
		Scope:      "local",
		Labels:     opts.Labels,
	}, nil
}

func (f *fakeDocker) VolumeRemove(_ context.Context, name string, _ bool) error {
	if f.volumeRemErr != nil {
		return f.volumeRemErr
	}
	f.removedVols = append(f.removedVols, name)
	return nil
}

func (f *fakeDocker) DiskUsage(_ context.Context, _ types.DiskUsageOptions) (types.DiskUsage, error) {
	if f.diskUsageErr != nil {
		return types.DiskUsage{}, f.diskUsageErr
	}
	return types.DiskUsage{Volumes: f.volumes}, nil
}

func (f *fakeDocker) Events(_ context.Context, _ events.ListOptions) (<-chan events.Message, <-chan error) {
	// No events in tests; the volume-size cache is exercised via refresh().
	return make(chan events.Message), make(chan error)
}

func (f *fakeDocker) Resizes() []container.ResizeOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]container.ResizeOptions, len(f.resizes))
	copy(out, f.resizes)
	return out
}

// containerWriteTTY writes raw bytes to the container side (TTY mode).
func (f *fakeDocker) containerWriteTTY(p []byte) (int, error) {
	return f.containerConn.Write(p)
}

// containerWriteStd writes a stdcopy-framed chunk to the container side
// (non-TTY mode) on the given stream.
func (f *fakeDocker) containerWriteStd(stream stdcopy.StdType, p []byte) error {
	w := stdcopy.NewStdWriter(f.containerConn, stream)
	_, err := w.Write(p)
	return err
}

// closeContainer signals process exit / output EOF.
func (f *fakeDocker) closeContainer() { _ = f.containerConn.Close() }

var _ DockerClient = (*fakeDocker)(nil)
var _ io.Writer = (*msgWriter)(nil)

// fakeConn is one end of an in-memory, full-duplex connection that — unlike
// net.Pipe — supports half-close via CloseWrite (so the agent's CloseWrite
// delivers EOF to the container's stdin without tearing down the read side).
type fakeConn struct {
	r *io.PipeReader
	w *io.PipeWriter
}

// newHalfDuplex returns the agent side and the container side of a connection.
func newHalfDuplex() (agent *fakeConn, containerSide *fakeConn) {
	a2cR, a2cW := io.Pipe() // agent -> container (stdin)
	c2aR, c2aW := io.Pipe() // container -> agent (output)
	return &fakeConn{r: c2aR, w: a2cW}, &fakeConn{r: a2cR, w: c2aW}
}

func (c *fakeConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *fakeConn) Write(p []byte) (int, error) { return c.w.Write(p) }

// CloseWrite closes only the write direction, delivering EOF to the peer's
// reader. Implements the docker types.CloseWriter interface.
func (c *fakeConn) CloseWrite() error { return c.w.Close() }

func (c *fakeConn) Close() error {
	_ = c.w.Close()
	return c.r.Close()
}

func (c *fakeConn) LocalAddr() net.Addr                { return fakeAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr               { return fakeAddr{} }
func (c *fakeConn) SetDeadline(t time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(t time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

var _ net.Conn = (*fakeConn)(nil)
