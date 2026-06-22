package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/hinshun/vt10x"
	"github.com/rivo/tview"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dial"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	pb "swarmexec/internal/pb"
)

// terminalView is a tview primitive that embeds a vt10x terminal emulator and
// bridges it to an agent Exec stream: the exec output is fed into the emulator
// and rendered into the pane, key events are encoded and sent to the exec
// stdin, and pane resizes are forwarded to the remote TTY.
type terminalView struct {
	*tview.Box
	app *tview.Application
	vt  vt10x.Terminal

	mu         sync.Mutex
	cols, rows int
	stdin      io.Writer // exec stdin; nil until the session starts

	resizeCh chan os.Signal
	detach   func() // close the modal (set by the caller)
}

func newTerminalView(app *tview.Application) *terminalView {
	v := &terminalView{
		Box:      tview.NewBox(),
		app:      app,
		cols:     80,
		rows:     24,
		resizeCh: make(chan os.Signal, 1),
	}
	v.Box.SetBorder(true)
	// The emulator's reply writer (cursor-position reports etc.) goes to stdin.
	v.vt = vt10x.New(vt10x.WithSize(v.cols, v.rows), vt10x.WithWriter(replyWriter{v}))
	return v
}

// run dials the agent and runs the exec session, feeding output into the
// emulator. onDone is invoked (off the UI goroutine) when the session ends.
func (v *terminalView) run(ctx context.Context, cfg config.Config, ep resolve.Endpoint, command []string, tty bool, connectTimeout time.Duration, onDone func(int, error)) {
	pr, pw := io.Pipe()
	v.mu.Lock()
	v.stdin = pw
	cols, rows := v.cols, v.rows
	v.mu.Unlock()

	go func() {
		dctx, dcancel := context.WithTimeout(ctx, connectTimeout)
		conn, err := dial.Dial(dctx, ep.DialHost, cfg.Port, cfg)
		dcancel()
		if err != nil {
			onDone(session.TransportFailure, err)
			return
		}
		defer conn.Close()

		stream, err := pb.NewAgentClient(conn).Exec(ctx)
		if err != nil {
			onDone(session.TransportFailure, fmt.Errorf("open exec stream: %w", err))
			return
		}

		code, runErr := session.Run(ctx, stream, session.Options{
			Start: &pb.StartExec{
				ContainerId: ep.ContainerID,
				Cmd:         command,
				Tty:         tty,
				Width:       uint32(cols),
				Height:      uint32(rows),
			},
			Stdin:        pr,
			Stdout:       vtSink{v},
			Stderr:       vtSink{v},
			ResizeEvents: v.resizeCh,
			SizeFn:       v.size,
		})
		onDone(code, runErr)
	}()
}

func (v *terminalView) size() (uint32, uint32, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return uint32(v.cols), uint32(v.rows), nil
}

// Draw renders the emulator's screen into the pane, resizing the emulator (and
// the remote TTY) when the pane geometry changes.
func (v *terminalView) Draw(screen tcell.Screen) {
	v.Box.DrawForSubclass(screen, v)
	x, y, w, h := v.GetInnerRect()
	if w <= 0 || h <= 0 {
		return
	}

	v.mu.Lock()
	if w != v.cols || h != v.rows {
		v.cols, v.rows = w, h
		v.vt.Resize(w, h)
		select {
		case v.resizeCh <- resizeSignal{}:
		default:
		}
	}
	v.mu.Unlock()

	v.vt.Lock()
	defer v.vt.Unlock()
	cols, rows := v.vt.Size()
	cur := v.vt.Cursor()
	curVisible := v.vt.CursorVisible()
	for row := 0; row < h && row < rows; row++ {
		for col := 0; col < w && col < cols; col++ {
			g := v.vt.Cell(col, row)
			ch := g.Char
			if ch == 0 {
				ch = ' '
			}
			st := tcell.StyleDefault.
				Foreground(vtColor(g.FG)).
				Background(vtColor(g.BG))
			if curVisible && col == cur.X && row == cur.Y {
				st = st.Reverse(true)
			}
			screen.SetContent(x+col, y+row, ch, nil, st)
		}
	}
}

// InputHandler encodes key events into terminal input bytes and writes them to
// the exec stdin. Ctrl-] detaches (closes the modal) without ending the shell.
func (v *terminalView) InputHandler() func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
	return v.WrapInputHandler(func(ev *tcell.EventKey, _ func(tview.Primitive)) {
		if ev.Key() == tcell.KeyCtrlRightSq { // Ctrl-]
			if v.detach != nil {
				v.detach()
			}
			return
		}
		v.mu.Lock()
		w := v.stdin
		v.mu.Unlock()
		if w == nil {
			return
		}
		if b := encodeKey(ev); b != nil {
			_, _ = w.Write(b)
		}
	})
}

// resizeSignal is a cross-platform os.Signal placeholder pushed onto the resize
// channel (session.pumpResize ignores the value and just re-reads the size).
// Avoids syscall.SIGWINCH, which does not exist on Windows.
type resizeSignal struct{}

func (resizeSignal) String() string { return "resize" }
func (resizeSignal) Signal()        {}

// vtSink feeds exec output into the emulator and requests a redraw.
type vtSink struct{ v *terminalView }

func (s vtSink) Write(p []byte) (int, error) {
	_, _ = s.v.vt.Write(p)
	s.v.app.QueueUpdateDraw(func() {})
	return len(p), nil
}

// replyWriter forwards emulator replies to the exec stdin.
type replyWriter struct{ v *terminalView }

func (r replyWriter) Write(p []byte) (int, error) {
	r.v.mu.Lock()
	w := r.v.stdin
	r.v.mu.Unlock()
	if w == nil {
		return len(p), nil
	}
	return w.Write(p)
}

// vtColor maps a vt10x color to a tcell color (default, 256-palette, or 24-bit).
func vtColor(c vt10x.Color) tcell.Color {
	switch {
	case c == vt10x.DefaultFG || c == vt10x.DefaultBG:
		return tcell.ColorDefault
	case c < 256:
		return tcell.PaletteColor(int(c))
	case c < (1 << 24):
		return tcell.NewRGBColor(int32((c>>16)&0xff), int32((c>>8)&0xff), int32(c&0xff))
	}
	return tcell.ColorDefault
}

// encodeKey turns a tcell key event into the bytes a terminal would send.
func encodeKey(ev *tcell.EventKey) []byte {
	switch ev.Key() {
	case tcell.KeyRune:
		if ev.Modifiers()&tcell.ModCtrl != 0 {
			return []byte{byte(ev.Rune()) & 0x1f}
		}
		r := []byte(string(ev.Rune()))
		if ev.Modifiers()&tcell.ModAlt != 0 {
			return append([]byte{0x1b}, r...)
		}
		return r
	case tcell.KeyEnter:
		return []byte{'\r'}
	case tcell.KeyTab:
		return []byte{'\t'}
	case tcell.KeyBacktab:
		return []byte("\x1b[Z")
	case tcell.KeyEsc:
		return []byte{0x1b}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		return []byte{0x7f}
	case tcell.KeyDelete:
		return []byte("\x1b[3~")
	case tcell.KeyInsert:
		return []byte("\x1b[2~")
	case tcell.KeyUp:
		return []byte("\x1b[A")
	case tcell.KeyDown:
		return []byte("\x1b[B")
	case tcell.KeyRight:
		return []byte("\x1b[C")
	case tcell.KeyLeft:
		return []byte("\x1b[D")
	case tcell.KeyHome:
		return []byte("\x1b[H")
	case tcell.KeyEnd:
		return []byte("\x1b[F")
	case tcell.KeyPgUp:
		return []byte("\x1b[5~")
	case tcell.KeyPgDn:
		return []byte("\x1b[6~")
	}
	// Control combinations: tcell maps Ctrl-A..Ctrl-_ to a contiguous range of
	// key codes; the control byte is the offset from KeyCtrlA plus one (^A = 1).
	if k := ev.Key(); k >= tcell.KeyCtrlA && k <= tcell.KeyCtrlUnderscore {
		return []byte{byte(k-tcell.KeyCtrlA) + 1}
	}
	return nil
}
