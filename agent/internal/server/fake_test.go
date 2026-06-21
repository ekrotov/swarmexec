package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
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

func (f *fakeDocker) ContainerExecResize(_ context.Context, _ string, opts container.ResizeOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, opts)
	return f.resizeErr
}

func (f *fakeDocker) ContainerExecInspect(_ context.Context, _ string) (container.ExecInspect, error) {
	return container.ExecInspect{ExitCode: f.exitCode, Running: f.running}, nil
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
