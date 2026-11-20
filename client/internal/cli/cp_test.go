// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"swarmexec/client/internal/cptar"
	pb "swarmexec/internal/pb"
)

// cpAgent is an agent that serves one file and one directory, and keeps what a
// CopyTo wrote. Paths it does not know report exists=false.
type cpAgent struct {
	pb.UnimplementedAgentServer
	mu       sync.Mutex
	dirs     map[string]bool // paths that are directories
	files    map[string]string
	received []byte
	destDir  string
	uidgid   bool
}

func newCpAgent() *cpAgent {
	return &cpAgent{
		dirs:  map[string]bool{"/tmp": true, "/etc/nginx": true},
		files: map[string]string{"/var/log/app.log": "the log line"},
	}
}

func (a *cpAgent) StatPath(_ context.Context, req *pb.StatPathRequest) (*pb.StatPathResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := req.GetPath()
	switch {
	case a.dirs[p]:
		return &pb.StatPathResponse{Exists: true, Stat: &pb.CopyStat{Name: filepath.Base(p), IsDir: true}}, nil
	case a.files[p] != "":
		return &pb.StatPathResponse{Exists: true, Stat: &pb.CopyStat{Name: filepath.Base(p), Size: int64(len(a.files[p]))}}, nil
	}
	return &pb.StatPathResponse{Exists: false}, nil
}

func (a *cpAgent) CopyFrom(req *pb.CopyFromRequest, stream pb.Agent_CopyFromServer) error {
	a.mu.Lock()
	body, ok := a.files[req.GetPath()]
	isDir := a.dirs[req.GetPath()]
	a.mu.Unlock()
	base := filepath.Base(req.GetPath())
	if err := stream.Send(&pb.CopyChunk{Payload: &pb.CopyChunk_Stat{
		Stat: &pb.CopyStat{Name: base, Size: int64(len(body)), IsDir: isDir},
	}}); err != nil {
		return err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name, content string) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			return err
		}
		_, err := tw.Write([]byte(content))
		return err
	}
	var err error
	switch {
	case isDir:
		err = write(base+"/nginx.conf", "listen 80;")
	case ok:
		err = write(base, body)
	}
	if err == nil {
		err = tw.Close()
	}
	if err != nil {
		return err
	}
	return stream.Send(&pb.CopyChunk{Payload: &pb.CopyChunk_Data{Data: buf.Bytes()}})
}

func (a *cpAgent) CopyTo(stream pb.Agent_CopyToServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	var body bytes.Buffer
	for {
		m, rerr := stream.Recv()
		if rerr != nil {
			break
		}
		body.Write(m.GetData())
	}
	a.mu.Lock()
	a.received, a.destDir, a.uidgid = body.Bytes(), start.GetDestPath(), start.GetCopyUidGid()
	a.mu.Unlock()
	return stream.SendAndClose(&pb.CopyToResponse{BytesWritten: int64(body.Len())})
}

func (a *cpAgent) got() ([]byte, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.received, a.destDir, a.uidgid
}

// `cp` end to end through the command: the target is resolved on the manager,
// the right side of the wire is used for each direction, and the destination
// rules match docker cp's.
func TestCpRun(t *testing.T) {
	agent := newCpAgent()
	g := cliCluster(t, execSwarm(), agent)
	run := func(args ...string) (string, string, error) {
		t.Helper()
		return runIO(t, newCpCmd(g), "", append([]string{"--connect-timeout", "2s"}, args...)...)
	}

	t.Run("a file out of a container, renamed", func(t *testing.T) {
		dir := t.TempDir()
		dst := filepath.Join(dir, "mine.log")
		if _, _, err := run("shop_web:/var/log/app.log", dst); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(dst)
		if err != nil || string(got) != "the log line" {
			t.Errorf("%s = %q, %v", dst, got, err)
		}
	})

	t.Run("a file into an existing local directory keeps its name", func(t *testing.T) {
		dir := t.TempDir()
		if _, _, err := run("shop_web:/var/log/app.log", dir); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "app.log")); err != nil {
			t.Errorf("app.log: %v", err)
		}
	})

	t.Run("a directory out of a container becomes the named directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "conf")
		if _, _, err := run("shop_web:/etc/nginx", dir); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "nginx.conf")); err != nil {
			t.Errorf("conf/nginx.conf: %v", err)
		}
	})

	t.Run("a file into a container's directory keeps its name", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "patch.sql")
		if err := os.WriteFile(src, []byte("SELECT 1"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := run(src, "shop_web:/tmp"); err != nil {
			t.Fatal(err)
		}
		body, dest, uidgid := agent.got()
		if dest != "/tmp" || uidgid {
			t.Errorf("dest = %q uidgid = %v, want /tmp and no -a", dest, uidgid)
		}
		if name := firstEntry(t, body); name != "patch.sql" {
			t.Errorf("entry = %q, want patch.sql", name)
		}
	})

	t.Run("a file onto a path that does not exist takes that name", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "patch.sql")
		if err := os.WriteFile(src, []byte("SELECT 1"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := run("-a", src, "shop_web:/tmp/renamed.sql"); err != nil {
			t.Fatal(err)
		}
		body, dest, uidgid := agent.got()
		if dest != "/tmp" || !uidgid {
			t.Errorf("dest = %q uidgid = %v, want /tmp and -a honoured", dest, uidgid)
		}
		if name := firstEntry(t, body); name != "renamed.sql" {
			t.Errorf("entry = %q, want renamed.sql", name)
		}
	})

	t.Run("between two containers, renamed in flight", func(t *testing.T) {
		if _, _, err := run("shop_web:/var/log/app.log", "shop_api.2:/tmp/copied.log"); err != nil {
			t.Fatal(err)
		}
		body, dest, _ := agent.got()
		if dest != "/tmp" {
			t.Errorf("dest = %q, want /tmp", dest)
		}
		if name := firstEntry(t, body); name != "copied.log" {
			t.Errorf("entry = %q, want copied.log", name)
		}
	})

	t.Run("a path the container does not have fails, and writes nothing", func(t *testing.T) {
		dir := t.TempDir()
		_, _, err := run("shop_web:/nope", filepath.Join(dir, "x"))
		if err == nil {
			t.Fatal("a missing source succeeded")
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("the destination directory holds %v", entries)
		}
	})
}

func TestCpRejectsArgumentsItCannotHonour(t *testing.T) {
	g := cliCluster(t, execSwarm(), newCpAgent())
	for name, args := range map[string][]string{
		"two local paths": {"./a", "./b"},
		"stdin":           {"-", "shop_web:/tmp/x"},
		"stdout":          {"shop_web:/tmp/x", "-"},
		"no size":         {"--max-size", "huge", "shop_web:/tmp/x", "./x"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := runIO(t, newCpCmd(g), "", args...); err == nil {
				t.Errorf("%v was accepted", args)
			}
		})
	}
}

// The split between "a container and a path" and "a local path" has to cope
// with Windows drive letters and with local paths that contain a colon.
func TestParseCpSpec(t *testing.T) {
	for arg, want := range map[string]cpSpec{
		"web:/tmp/x":      {target: "web", path: "/tmp/x"},
		"web.2:/tmp/x":    {target: "web.2", path: "/tmp/x"},
		"./local:file":    {path: "./local:file"},
		"/abs/path":       {path: "/abs/path"},
		`C:\tmp\file`:     {path: `C:\tmp\file`},
		"relative.txt":    {path: "relative.txt"},
		"c-abc123:/x":     {target: "c-abc123", path: "/x"},
		"dir/sub:notaref": {path: "dir/sub:notaref"},
	} {
		if got := parseCpSpec(arg); got != want {
			t.Errorf("parseCpSpec(%q) = %+v, want %+v", arg, got, want)
		}
	}
}

func TestLocalDestinationFollowsDockerCpRules(t *testing.T) {
	dir := t.TempDir()
	existingDir := filepath.Join(dir, "sub")
	if err := os.Mkdir(existingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existingFile := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(existingFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("an existing directory receives the source under its own name", func(t *testing.T) {
		got, opts, err := localDestination(existingDir, false)
		if err != nil || got != existingDir || opts.Rename != "" || opts.StripRoot {
			t.Errorf("= %q, %+v, %v", got, opts, err)
		}
	})
	t.Run("a new path renames a file", func(t *testing.T) {
		got, opts, err := localDestination(filepath.Join(dir, "new.txt"), false)
		if err != nil || got != dir || opts.Rename != "new.txt" {
			t.Errorf("= %q, %+v, %v", got, opts, err)
		}
	})
	t.Run("a new path becomes the directory for a directory source", func(t *testing.T) {
		target := filepath.Join(dir, "tree")
		got, opts, err := localDestination(target, true)
		if err != nil || got != target || !opts.StripRoot {
			t.Errorf("= %q, %+v, %v", got, opts, err)
		}
	})
	t.Run("a directory cannot be copied onto a file", func(t *testing.T) {
		if _, _, err := localDestination(existingFile, true); err == nil {
			t.Errorf("copying a directory onto a file was allowed")
		}
	})
}

// firstEntry is the name of an archive's first member.
func firstEntry(t *testing.T, archive []byte) string {
	t.Helper()
	h, err := tar.NewReader(bytes.NewReader(archive)).Next()
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	return strings.TrimSuffix(h.Name, "/")
}

// Guard against the package's own helper drifting from cptar's: the CLI
// decides names, cptar applies them, and a test that only used one of the two
// would not notice them disagreeing.
func TestExtractOptionsFromLocalDestinationAreTheOnesCptarApplies(t *testing.T) {
	dir := t.TempDir()
	_, opts, err := localDestination(filepath.Join(dir, "renamed.log"), false)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "app.log", Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cptar.Extract(io.Reader(bytes.NewReader(buf.Bytes())), dir, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "renamed.log")); err != nil {
		t.Errorf("renamed.log: %v", err)
	}
}
