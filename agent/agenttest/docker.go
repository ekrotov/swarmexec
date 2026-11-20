// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"

	"swarmexec/agent/internal/server"
)

// swarmServiceLabel is the label Docker puts on a swarm task container.
const swarmServiceLabel = "com.docker.swarm.service.name"

// Container describes one container of the fake node.
type Container struct {
	ID      string
	Name    string // without the leading slash; defaults to ID
	Service string // swarm service name, "" for a plain container
	TTY     bool   // its logs are raw rather than stdcopy-framed
	Volumes []string
	Ports   []uint16 // container-side TCP ports
	Stopped bool
}

// ExecFunc is the process of an exec: what the container does with the
// command. It reads stdin until EOF (the agent half-closes it when the client
// does), writes to stdout/stderr and returns the exit code. The default echoes
// stdin to stdout and exits 0.
type ExecFunc func(cmd []string, stdin io.Reader, stdout, stderr io.Writer) int

// Docker is an in-memory Docker daemon for the agent to talk to: a scriptable
// stand-in for the handful of Engine calls the handlers make. Every call is
// recorded (see Calls) and any method can be made to fail (see Fail).
//
// It implements only what the RPCs in the harness's reach use — containers,
// exec, logs, volumes, info, events. A call outside that set (images, prune,
// stats, port-forward sidecars) panics on the nil embedded interface, which the
// agent's recovery interceptor turns into an Internal status; extend Docker
// when a test needs one.
type Docker struct {
	server.DockerClient // nil: unimplemented methods panic, loudly

	mu         sync.Mutex
	containers []Container
	logs       map[string]string // container -> stdout text
	volumes    []volume.Volume
	failures   map[string]error
	calls      []string
	execs      map[string]*execState
	execSeq    int
	info       system.Info
	execFn     ExecFunc
	logCalls   []client.ContainerLogsOptions
	logStreams map[string]func() io.ReadCloser
	// files is the archive endpoint's filesystem, per container (copy.go).
	files map[string]map[string]fakeFile
}

type execState struct {
	cmd  []string
	tty  bool
	code int
}

// NewDocker returns an empty fake node with the default (echo) exec process.
func NewDocker() *Docker {
	return &Docker{
		logs:     map[string]string{},
		failures: map[string]error{},
		execs:    map[string]*execState{},
		info:     system.Info{NCPU: 4, MemTotal: 8 << 30},
		execFn:   echoExec,
	}
}

func echoExec(_ []string, stdin io.Reader, stdout, _ io.Writer) int {
	_, _ = io.Copy(stdout, stdin)
	return 0
}

// AddContainer adds a running container to the node.
func (d *Docker) AddContainer(c Container) *Docker {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c.Name == "" {
		c.Name = c.ID
	}
	d.containers = append(d.containers, c)
	return d
}

// SetLogs sets the stdout text ContainerLogs serves for a container.
func (d *Docker) SetLogs(containerID, text string) *Docker {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logs[containerID] = text
	return d
}

// SetLogStream makes ContainerLogs serve what open returns for a container,
// byte for byte (a non-TTY container's stream must already be stdcopy-framed,
// see Frame). It lets a test feed lines over time or serve a large stream.
func (d *Docker) SetLogStream(containerID string, open func() io.ReadCloser) *Docker {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.logStreams == nil {
		d.logStreams = map[string]func() io.ReadCloser{}
	}
	d.logStreams[containerID] = open
	return d
}

// Frame builds one stdcopy frame (stream 1 = stdout, 2 = stderr) for a
// SetLogStream reader.
func Frame(stream byte, p []byte) []byte { return frame(stream, p) }

// AddVolume adds a local volume to the node.
func (d *Docker) AddVolume(name string) *Docker {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.volumes = append(d.volumes, volume.Volume{Name: name, Driver: "local", Scope: "local"})
	return d
}

// SetExec replaces the exec process (nil restores the echo default).
func (d *Docker) SetExec(fn ExecFunc) *Docker {
	d.mu.Lock()
	defer d.mu.Unlock()
	if fn == nil {
		fn = echoExec
	}
	d.execFn = fn
	return d
}

// Fail makes every following call of the named Engine method (for example
// "ContainerList", "ContainerLogs", "ExecCreate") return err; nil clears it.
// Wrap an errdefs class (cerrdefs.ErrNotFound, ErrConflict, ...) to see the
// agent's dockerStatus mapping end to end.
func (d *Docker) Fail(method string, err error) *Docker {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		delete(d.failures, method)
	} else {
		d.failures[method] = err
	}
	return d
}

// Calls returns the Engine methods called so far, in order.
func (d *Docker) Calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

// LogCalls returns the options of every ContainerLogs call: what the agent
// asked the daemon for after translating the client's LogsRequest.
func (d *Docker) LogCalls() []client.ContainerLogsOptions {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]client.ContainerLogsOptions(nil), d.logCalls...)
}

// Called reports how many times method was called.
func (d *Docker) Called(method string) int {
	n := 0
	for _, c := range d.Calls() {
		if c == method {
			n++
		}
	}
	return n
}

// enter records a call and returns its injected failure, if any.
func (d *Docker) enter(method string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, method)
	return d.failures[method]
}

func (d *Docker) find(id string) (Container, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.containers {
		if c.ID == id || c.Name == id {
			return c, true
		}
	}
	return Container{}, false
}

func notFound(id string) error {
	return fmt.Errorf("no such container: %s: %w", id, cerrdefs.ErrNotFound)
}

func (c Container) labels() map[string]string {
	if c.Service == "" {
		return nil
	}
	return map[string]string{swarmServiceLabel: c.Service}
}

func (d *Docker) ContainerList(_ context.Context, _ client.ContainerListOptions) (client.ContainerListResult, error) {
	if err := d.enter("ContainerList"); err != nil {
		return client.ContainerListResult{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []container.Summary
	for _, c := range d.containers {
		if c.Stopped {
			continue
		}
		s := container.Summary{ID: c.ID, Names: []string{"/" + c.Name}, Labels: c.labels()}
		for _, v := range c.Volumes {
			s.Mounts = append(s.Mounts, container.MountPoint{Type: mount.TypeVolume, Name: v})
		}
		for _, p := range c.Ports {
			s.Ports = append(s.Ports, container.PortSummary{PrivatePort: p, Type: "tcp"})
		}
		out = append(out, s)
	}
	return client.ContainerListResult{Items: out}, nil
}

func (d *Docker) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if err := d.enter("ContainerInspect"); err != nil {
		return client.ContainerInspectResult{}, err
	}
	c, ok := d.find(id)
	if !ok {
		return client.ContainerInspectResult{}, notFound(id)
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{
		ID:   c.ID,
		Name: "/" + c.Name,
		Config: &container.Config{
			Tty:    c.TTY,
			Labels: c.labels(),
		},
	}}, nil
}

func (d *Docker) ContainerLogs(_ context.Context, id string, opts client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	d.mu.Lock()
	d.logCalls = append(d.logCalls, opts)
	d.mu.Unlock()
	if err := d.enter("ContainerLogs"); err != nil {
		return nil, err
	}
	c, ok := d.find(id)
	if !ok {
		return nil, notFound(id)
	}
	d.mu.Lock()
	text := d.logs[id]
	open := d.logStreams[id]
	d.mu.Unlock()
	if open != nil {
		return open(), nil
	}
	if c.TTY {
		return io.NopCloser(strings.NewReader(text)), nil
	}
	// A non-TTY container's log stream is stdcopy-framed; a line beginning
	// "ERR " goes out on stderr, the rest on stdout.
	var framed strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		if line == "" {
			continue
		}
		stream := byte(1)
		if strings.HasPrefix(line, "ERR ") {
			stream, line = 2, strings.TrimPrefix(line, "ERR ")
		}
		framed.Write(frame(stream, []byte(line)))
	}
	return io.NopCloser(strings.NewReader(framed.String())), nil
}

// frame builds one stdcopy frame: stream id, three zero bytes, big-endian
// length, payload.
func frame(stream byte, p []byte) []byte {
	b := make([]byte, 8+len(p))
	b[0] = stream
	binary.BigEndian.PutUint32(b[4:8], uint32(len(p)))
	copy(b[8:], p)
	return b
}

type frameWriter struct {
	w      io.Writer
	stream byte
	mu     *sync.Mutex
}

func (f frameWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.w.Write(frame(f.stream, p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (d *Docker) ExecCreate(_ context.Context, id string, opts client.ExecCreateOptions) (client.ExecCreateResult, error) {
	if err := d.enter("ExecCreate"); err != nil {
		return client.ExecCreateResult{}, err
	}
	if _, ok := d.find(id); !ok {
		return client.ExecCreateResult{}, notFound(id)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.execSeq++
	eid := fmt.Sprintf("exec-%d", d.execSeq)
	d.execs[eid] = &execState{cmd: opts.Cmd, tty: opts.TTY}
	return client.ExecCreateResult{ID: eid}, nil
}

func (d *Docker) ExecAttach(_ context.Context, eid string, _ client.ExecAttachOptions) (client.ExecAttachResult, error) {
	if err := d.enter("ExecAttach"); err != nil {
		return client.ExecAttachResult{}, err
	}
	d.mu.Lock()
	st, fn := d.execs[eid], d.execFn
	d.mu.Unlock()
	if st == nil {
		return client.ExecAttachResult{}, fmt.Errorf("no such exec %s: %w", eid, cerrdefs.ErrNotFound)
	}
	agentSide, ctrSide := halfDuplex()
	go func() {
		var stdout, stderr io.Writer = ctrSide, ctrSide
		if !st.tty {
			var mu sync.Mutex
			stdout = frameWriter{w: ctrSide, stream: 1, mu: &mu}
			stderr = frameWriter{w: ctrSide, stream: 2, mu: &mu}
		}
		code := fn(st.cmd, ctrSide, stdout, stderr)
		d.mu.Lock()
		st.code = code
		d.mu.Unlock()
		_ = ctrSide.CloseWrite() // the process is gone: EOF on its output
	}()
	return client.ExecAttachResult{HijackedResponse: client.HijackedResponse{Conn: agentSide, Reader: bufio.NewReader(agentSide)}}, nil
}

func (d *Docker) ExecResize(context.Context, string, client.ExecResizeOptions) (client.ExecResizeResult, error) {
	return client.ExecResizeResult{}, d.enter("ExecResize")
}

func (d *Docker) ExecInspect(_ context.Context, eid string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	if err := d.enter("ExecInspect"); err != nil {
		return client.ExecInspectResult{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.execs[eid]
	if st == nil {
		return client.ExecInspectResult{}, fmt.Errorf("no such exec %s: %w", eid, cerrdefs.ErrNotFound)
	}
	return client.ExecInspectResult{ExitCode: st.code}, nil
}

func (d *Docker) VolumeList(_ context.Context, _ client.VolumeListOptions) (client.VolumeListResult, error) {
	if err := d.enter("VolumeList"); err != nil {
		return client.VolumeListResult{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return client.VolumeListResult{Items: append([]volume.Volume(nil), d.volumes...)}, nil
}

func (d *Docker) Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
	if err := d.enter("Info"); err != nil {
		return client.SystemInfoResult{}, err
	}
	return client.SystemInfoResult{Info: d.info}, nil
}

func (d *Docker) Events(ctx context.Context, _ client.EventsListOptions) client.EventsResult {
	_ = d.enter("Events")
	msgs := make(chan events.Message)
	errs := make(chan error)
	go func() { <-ctx.Done() }()
	return client.EventsResult{Messages: msgs, Err: errs}
}

var _ server.DockerClient = (*Docker)(nil)

// --- an in-memory connection with half-close, like a hijacked exec ---

type pipeConn struct {
	r *io.PipeReader
	w *io.PipeWriter
}

// halfDuplex returns the agent's end and the container's end of a connection
// whose CloseWrite delivers EOF without tearing down the other direction, as
// the real hijacked connection does (net.Pipe cannot half-close).
func halfDuplex() (agentSide, ctrSide *pipeConn) {
	a2cR, a2cW := io.Pipe()
	c2aR, c2aW := io.Pipe()
	return &pipeConn{r: c2aR, w: a2cW}, &pipeConn{r: a2cR, w: c2aW}
}

func (c *pipeConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *pipeConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *pipeConn) CloseWrite() error           { return c.w.Close() }
func (c *pipeConn) Close() error {
	_ = c.w.Close()
	return c.r.Close()
}
func (c *pipeConn) LocalAddr() net.Addr              { return pipeAddr{} }
func (c *pipeConn) RemoteAddr() net.Addr             { return pipeAddr{} }
func (c *pipeConn) SetDeadline(time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(time.Time) error { return nil }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }
