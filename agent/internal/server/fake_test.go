// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// fakeDocker is an in-memory DockerClient. The exec "process" is driven by a
// net.Pipe: the agent writes stdin to one end and reads output from it; the
// test plays the container side via the other end.
type fakeDocker struct {
	mu sync.Mutex

	containers []container.Summary
	inspect    map[string]container.InspectResponse

	inspects       int
	imageDiskUsage client.ImagesDiskUsage
	images         []image.Summary
	imageListErr   error
	pruneReport    image.PruneReport
	pruneErr       error
	pruneFilters   []client.Filters
	statsFrames    map[string][]container.StatsResponse
	statsIdx       map[string]int
	statsOpts      []client.ContainerStatsOptions
	statsErr       error
	info           system.Info
	infoErr        error

	listErr   error
	createErr error
	attachErr error

	// container side of the attach pipe, handed to the test.
	containerConn net.Conn
	// attachedCh delivers the container side to the test once attach happens.
	attachedCh chan net.Conn
	// exit code returned by ExecInspect.
	exitCode int
	running  bool

	resizes   []client.ExecResizeOptions
	resizeErr error

	execTty bool

	logsReader io.ReadCloser
	logsErr    error

	volumes         []volume.Volume
	volumeRemErr    error
	removedVols     []string
	volumeCreateErr error
	createdVolOpts  client.VolumeCreateOptions
	diskUsageErr    error
	diskUsageOpts   []client.DiskUsageOptions

	// listCalls counts ContainerList calls, so a test can assert that a refused
	// enumeration never reached Docker in the first place.
	listCalls atomic.Int64

	// Event subscription, for the container-event watch. nil channels mean "no
	// events", which is what every other test expects.
	eventCh    chan events.Message
	eventErrCh chan error
	eventOpts  []client.EventsListOptions

	// port-forward sidecar state
	createContainerErr error
	startErr           error
	createdConfig      *container.Config
	created            int
	createdHostConfig  *container.HostConfig
	removedContainers  []string
}

// inspectCalls reports how often ContainerInspect was called, so a test can
// assert that per-container limits are read once and then cached.
func (f *fakeDocker) inspectCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inspects
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{inspect: map[string]container.InspectResponse{}, attachedCh: make(chan net.Conn, 1)}
}

// waitAttach blocks until the agent attaches and returns the container side.
func (f *fakeDocker) waitAttach() net.Conn { return <-f.attachedCh }

func (f *fakeDocker) ContainerList(_ context.Context, _ client.ContainerListOptions) (client.ContainerListResult, error) {
	f.listCalls.Add(1)
	if f.listErr != nil {
		return client.ContainerListResult{}, f.listErr
	}
	return client.ContainerListResult{Items: f.containers}, nil
}

func (f *fakeDocker) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	f.inspects++
	f.mu.Unlock()
	if c, ok := f.inspect[id]; ok {
		return client.ContainerInspectResult{Container: c}, nil
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{ID: id, Config: &container.Config{}}}, nil
}

func (f *fakeDocker) ExecCreate(_ context.Context, _ string, opts client.ExecCreateOptions) (client.ExecCreateResult, error) {
	if f.createErr != nil {
		return client.ExecCreateResult{}, f.createErr
	}
	f.execTty = opts.TTY
	return client.ExecCreateResult{ID: "exec-123"}, nil
}

// ExecAttach returns a HijackedResponse wired to one end of a net.Pipe.
// The test holds the other end via TestConn().
func (f *fakeDocker) ExecAttach(_ context.Context, _ string, _ client.ExecAttachOptions) (client.ExecAttachResult, error) {
	if f.attachErr != nil {
		return client.ExecAttachResult{}, f.attachErr
	}
	agentSide, containerSide := newHalfDuplex()
	f.containerConn = containerSide
	f.attachedCh <- containerSide
	return client.ExecAttachResult{HijackedResponse: client.HijackedResponse{Conn: agentSide, Reader: bufio.NewReader(agentSide)}}, nil
}

// --- port-forward sidecar surface ---

func (f *fakeDocker) ContainerCreate(_ context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createContainerErr != nil {
		return client.ContainerCreateResult{}, f.createContainerErr
	}
	f.createdConfig, f.createdHostConfig = opts.Config, opts.HostConfig
	f.created++
	return client.ContainerCreateResult{ID: "sidecar-1"}, nil
}

// createdCount reports how many sidecar containers were created.
func (f *fakeDocker) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

func (f *fakeDocker) ContainerStart(_ context.Context, _ string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	return client.ContainerStartResult{}, f.startErr
}

// ContainerAttach mirrors ExecAttach: the test drives the sidecar side of the
// pipe, writing stdcopy-framed control lines and payload.
func (f *fakeDocker) ContainerAttach(_ context.Context, _ string, _ client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	if f.attachErr != nil {
		return client.ContainerAttachResult{}, f.attachErr
	}
	agentSide, containerSide := newHalfDuplex()
	f.containerConn = containerSide
	f.attachedCh <- containerSide
	return client.ContainerAttachResult{HijackedResponse: client.HijackedResponse{Conn: agentSide, Reader: bufio.NewReader(agentSide)}}, nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedContainers = append(f.removedContainers, id)
	return client.ContainerRemoveResult{}, nil
}

// RemovedContainers reports sidecars the agent cleaned up.
func (f *fakeDocker) RemovedContainers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removedContainers...)
}

func (f *fakeDocker) ExecResize(_ context.Context, _ string, opts client.ExecResizeOptions) (client.ExecResizeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, opts)
	return client.ExecResizeResult{}, f.resizeErr
}

func (f *fakeDocker) ExecInspect(_ context.Context, _ string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	return client.ExecInspectResult{ExitCode: f.exitCode, Running: f.running}, nil
}

func (f *fakeDocker) ContainerLogs(_ context.Context, _ string, _ client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	if f.logsReader != nil {
		return f.logsReader, nil
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeDocker) VolumeList(_ context.Context, _ client.VolumeListOptions) (client.VolumeListResult, error) {
	return client.VolumeListResult{Items: f.volumes}, nil
}

func (f *fakeDocker) VolumeCreate(_ context.Context, opts client.VolumeCreateOptions) (client.VolumeCreateResult, error) {
	if f.volumeCreateErr != nil {
		return client.VolumeCreateResult{}, f.volumeCreateErr
	}
	f.createdVolOpts = opts
	return client.VolumeCreateResult{Volume: volume.Volume{
		Name:       opts.Name,
		Driver:     opts.Driver,
		Mountpoint: "/var/lib/docker/volumes/" + opts.Name + "/_data",
		Scope:      "local",
		Labels:     opts.Labels,
	}}, nil
}

func (f *fakeDocker) VolumeRemove(_ context.Context, name string, _ client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
	if f.volumeRemErr != nil {
		return client.VolumeRemoveResult{}, f.volumeRemErr
	}
	f.removedVols = append(f.removedVols, name)
	return client.VolumeRemoveResult{}, nil
}

// statsFrames are the docker stats JSON frames the fake serves, keyed by
// container id, and statsIdx tracks which one each container is on — so a test
// can hand out a second reading and exercise the CPU delta. statsOpts records
// every call's options, so a test can prove each read is a one-shot.
func (f *fakeDocker) ContainerStats(_ context.Context, id string, opts client.ContainerStatsOptions) (client.ContainerStatsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsOpts = append(f.statsOpts, opts)
	if f.statsErr != nil {
		return client.ContainerStatsResult{}, f.statsErr
	}
	frames := f.statsFrames[id]
	if len(frames) == 0 {
		return client.ContainerStatsResult{}, errors.New("no stats for " + id)
	}
	i := f.statsIdx[id]
	if i >= len(frames) {
		i = len(frames) - 1 // keep serving the last frame
	}
	if f.statsIdx == nil {
		f.statsIdx = map[string]int{}
	}
	f.statsIdx[id] = i + 1
	body, err := json.Marshal(frames[i])
	if err != nil {
		return client.ContainerStatsResult{}, err
	}
	return client.ContainerStatsResult{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

// statsCallOpts returns the options of every ContainerStats call so far.
func (f *fakeDocker) statsCallOpts() []client.ContainerStatsOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]client.ContainerStatsOptions(nil), f.statsOpts...)
}

func (f *fakeDocker) ImageList(_ context.Context, _ client.ImageListOptions) (client.ImageListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return client.ImageListResult{Items: f.images}, f.imageListErr
}

// ImagePrune records what it was asked for, so a test can prove the safe mode
// really is the safe one.
func (f *fakeDocker) ImagePrune(_ context.Context, opts client.ImagePruneOptions) (client.ImagePruneResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneFilters = append(f.pruneFilters, opts.Filters)
	if f.pruneErr != nil {
		return client.ImagePruneResult{}, f.pruneErr
	}
	return client.ImagePruneResult{Report: f.pruneReport}, nil
}

func (f *fakeDocker) Info(_ context.Context, _ client.InfoOptions) (client.SystemInfoResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.infoErr != nil {
		return client.SystemInfoResult{}, f.infoErr
	}
	return client.SystemInfoResult{Info: f.info}, nil
}

// DiskUsage mirrors the real client's shape on API >= 1.52: only the requested
// object types are filled, and their per-object Items only when Verbose is set
// (totals alone otherwise). A caller that forgets Verbose therefore sees no
// images / no volume sizes here, exactly as it would against a real daemon.
func (f *fakeDocker) DiskUsage(_ context.Context, opts client.DiskUsageOptions) (client.DiskUsageResult, error) {
	f.mu.Lock()
	f.diskUsageOpts = append(f.diskUsageOpts, opts)
	f.mu.Unlock()
	if f.diskUsageErr != nil {
		return client.DiskUsageResult{}, f.diskUsageErr
	}
	var r client.DiskUsageResult
	if opts.Images {
		r.Images = f.imageDiskUsage
		if !opts.Verbose {
			r.Images.Items = nil
		}
	}
	if opts.Volumes {
		r.Volumes.TotalCount = int64(len(f.volumes))
		if opts.Verbose {
			r.Volumes.Items = f.volumes
		}
	}
	return r, nil
}

func (f *fakeDocker) Events(_ context.Context, opts client.EventsListOptions) client.EventsResult {
	f.mu.Lock()
	f.eventOpts = append(f.eventOpts, opts)
	ch, errCh := f.eventCh, f.eventErrCh
	f.mu.Unlock()
	if ch == nil {
		// Default: no events, as the volume-size cache's tests expect.
		return client.EventsResult{Messages: make(chan events.Message), Err: make(chan error)}
	}
	return client.EventsResult{Messages: ch, Err: errCh}
}

// eventFilters returns the filters the last Events subscription asked for, so a
// test can assert that narrowing happens at the DAEMON rather than in the agent
// — the difference between "we do not forward other containers' events" and "we
// receive them and mean to drop them".
func (f *fakeDocker) eventFilters() client.Filters {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.eventOpts) == 0 {
		return client.Filters{}
	}
	return f.eventOpts[len(f.eventOpts)-1].Filters
}

func (f *fakeDocker) Resizes() []client.ExecResizeOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]client.ExecResizeOptions, len(f.resizes))
	copy(out, f.resizes)
	return out
}

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
// reader. Implements the moby client.CloseWriter interface.
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

// stdWriter frames writes the way the daemon multiplexes a non-TTY stream: an
// 8-byte header (stream id, 3 zero bytes, big-endian uint32 payload length)
// followed by the payload — what stdcopy.StdCopy demultiplexes. moby/moby/api
// only ships the reader half, so the tests carry this writer half themselves.
type stdWriter struct {
	w      io.Writer
	stream stdcopy.StdType
}

func newStdWriter(w io.Writer, stream stdcopy.StdType) io.Writer {
	return &stdWriter{w: w, stream: stream}
}

func (s *stdWriter) Write(p []byte) (int, error) {
	frame := make([]byte, 8+len(p))
	frame[0] = byte(s.stream)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(p)))
	copy(frame[8:], p)
	n, err := s.w.Write(frame)
	n -= 8
	if n < 0 {
		n = 0
	}
	return n, err
}

// filterValues returns the values set for term in f, sorted — what the old
// filters.Args.Get offered and client.Filters (a plain map) no longer does.
func filterValues(f client.Filters, term string) []string {
	var out []string
	for v, on := range f[term] {
		if on {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
