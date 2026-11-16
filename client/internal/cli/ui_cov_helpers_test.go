// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/internal/pb"
)

// Shared helpers of the TUI tests that drive whole dialogs: a running UI with
// the chrome the dialogs touch, keys sent the way the event loop sends them,
// and an overlay read back as the text it draws.

// covChromeUI is runningUI plus the pieces of runUI's chrome that the dialogs
// reach for: the keymap, the status line, the search bar, the forwards
// registry and the cluster map.
func covChromeUI(t *testing.T) *ui {
	t.Helper()
	u := runningUI(t)
	onLoop(t, u, func() {
		u.km = defaultKeybinds()
		u.forwards = newForwardRegistry()
		u.status = tview.NewTextView()
		u.search = tview.NewInputField()
		u.root = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(u.search, 0, 0, false)
		u.clusters = map[string]*clusterState{"a": u.clusterState}
		u.activeCtx = "a"
	})
	return u
}

// sendKey hands ev to the root the way the event loop does, synchronously on
// the UI goroutine: every container on the way to the focused widget sees it,
// so the dialogs' own input captures run.
func sendKey(t *testing.T, u *ui, ev *tcell.EventKey) {
	t.Helper()
	onLoop(t, u, func() {
		u.pages.InputHandler()(ev, func(p tview.Primitive) { u.app.SetFocus(p) })
	})
}

func sendRune(t *testing.T, u *ui, r rune) { t.Helper(); sendKey(t, u, rune_(r)) }

func sendSpecial(t *testing.T, u *ui, k tcell.Key) {
	t.Helper()
	sendKey(t, u, tcell.NewEventKey(k, 0, tcell.ModNone))
}

// typeText types s into whatever has focus, one rune at a time.
func typeText(t *testing.T, u *ui, s string) {
	t.Helper()
	for _, r := range s {
		sendRune(t, u, r)
	}
}

// setFocusedText sets the text of the focused input field without typing it,
// so no autocomplete drop-down opens to take the next Enter.
func setFocusedText(t *testing.T, u *ui, s string) {
	t.Helper()
	onLoop(t, u, func() {
		in, ok := u.app.GetFocus().(*tview.InputField)
		if !ok {
			t.Errorf("focus is on %T, not an input field", u.app.GetFocus())
			return
		}
		in.SetText(s)
	})
}

var colorTags = regexp.MustCompile(`\[[a-zA-Z:#-]*\]`)

var spaces = regexp.MustCompile(`[\s│─┌┐└┘├┤║═╔╗╚╝]+`)

// drawnText is what p draws on a w×h screen, whitespace and box borders
// collapsed to single spaces, so a wrapped modal still reads as its message.
func drawnText(p tview.Primitive, w, h int) string {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		return ""
	}
	defer screen.Fini()
	screen.SetSize(w, h)
	p.SetRect(0, 0, w, h)
	p.Draw(screen)
	screen.Show()
	cells, sw, _ := screen.GetContents()
	var b strings.Builder
	for i, c := range cells {
		if i > 0 && i%sw == 0 {
			b.WriteByte('\n')
		}
		if len(c.Runes) == 0 {
			b.WriteByte(' ')
			continue
		}
		b.WriteString(string(c.Runes))
	}
	return strings.TrimSpace(spaces.ReplaceAllString(b.String(), " "))
}

// pageText is the named overlay as drawn on a large screen ("" when the page
// is not open).
func pageText(t *testing.T, u *ui, name string) string {
	t.Helper()
	var text string
	onLoop(t, u, func() {
		if p := u.pages.GetPage(name); p != nil {
			text = drawnText(p, 200, 60)
		}
	})
	return text
}

// waitPageText waits until the named overlay is open and draws every want.
func waitPageText(t *testing.T, u *ui, name string, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		text := pageText(t, u, name)
		ok := text != ""
		for _, w := range want {
			ok = ok && strings.Contains(text, w)
		}
		if ok {
			return text
		}
		if time.Now().After(deadline) {
			t.Fatalf("page %s never showed %q; it shows:\n%s", name, want, text)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitPageGone waits until the named overlay is closed.
func waitPageGone(t *testing.T, u *ui, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for pageOpen(t, u, name) {
		if time.Now().After(deadline) {
			t.Fatalf("page %s never closed", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// closeInfo dismisses an open notice with its only button.
func closeInfo(t *testing.T, u *ui) {
	t.Helper()
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageGone(t, u, pageInfo)
}

// helpLine is the footer line as it reads now, colour tags stripped.
func helpLine(t *testing.T, u *ui) string {
	t.Helper()
	var s string
	onLoop(t, u, func() { s = u.help.GetText(false) })
	return colorTags.ReplaceAllString(s, "")
}

// waitFooter waits for the footer (where flash writes) to say want.
func waitFooter(t *testing.T, u *ui, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(helpLine(t, u), want) {
		if time.Now().After(deadline) {
			t.Fatalf("footer never said %q; it says %q (notice: %q)", want, helpLine(t, u), pageText(t, u, pageInfo))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeAgent is an agent fake for the dialogs: volumes it lists and the
// containers that mount them, the volume writes it records, and a log stream
// that sends its lines and then holds the stream open like `logs -f`.
type fakeAgent struct {
	pb.UnimplementedAgentServer

	vols    []*pb.VolumeInfo
	ctrs    []*pb.ContainerInfo
	logs    []string
	stderr  []string
	failRm  bool
	failNew bool

	mu      sync.Mutex
	removed []string
	created []*pb.CreateVolumeRequest
	logReqs []*pb.LogsRequest
}

func (a *fakeAgent) ListVolumes(context.Context, *pb.ListVolumesRequest) (*pb.ListVolumesResponse, error) {
	return &pb.ListVolumesResponse{Volumes: a.vols}, nil
}

func (a *fakeAgent) ListContainers(context.Context, *pb.ListRequest) (*pb.ListResponse, error) {
	return &pb.ListResponse{Containers: a.ctrs}, nil
}

func (a *fakeAgent) RemoveVolume(_ context.Context, r *pb.RemoveVolumeRequest) (*pb.RemoveVolumeResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failRm {
		return nil, errVolumeInUse
	}
	a.removed = append(a.removed, r.GetName())
	return &pb.RemoveVolumeResponse{}, nil
}

func (a *fakeAgent) CreateVolume(_ context.Context, r *pb.CreateVolumeRequest) (*pb.CreateVolumeResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failNew {
		return nil, errVolumeInUse
	}
	a.created = append(a.created, r)
	return &pb.CreateVolumeResponse{Volume: &pb.VolumeInfo{Name: r.GetName(), Driver: r.GetDriver()}}, nil
}

func (a *fakeAgent) Logs(r *pb.LogsRequest, s pb.Agent_LogsServer) error {
	a.mu.Lock()
	a.logReqs = append(a.logReqs, r)
	a.mu.Unlock()
	for _, l := range a.logs {
		if err := s.Send(&pb.LogChunk{Payload: &pb.LogChunk_Stdout{Stdout: []byte(l + "\n")}}); err != nil {
			return err
		}
	}
	for _, l := range a.stderr {
		if err := s.Send(&pb.LogChunk{Payload: &pb.LogChunk_Stderr{Stderr: []byte(l + "\n")}}); err != nil {
			return err
		}
	}
	<-s.Context().Done()
	return nil
}

func (a *fakeAgent) removedNames() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.removed, ",")
}

func (a *fakeAgent) createdNames() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.created {
		out = append(out, r.GetName())
	}
	return strings.Join(out, ",")
}

func (a *fakeAgent) logCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.logReqs)
}

// errVolumeInUse is what a refusing agent answers a volume write with.
var errVolumeInUse = status.Error(codes.FailedPrecondition, "volume is in use")

// waitUntil polls cond (read off the UI goroutine) for up to ten seconds.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
