// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// The Engine's archive endpoint, as a fake node: a flat file store per
// container, served as tar by CopyFromContainer and filled by CopyToContainer.
// Enough for the copy RPCs (agent/internal/server/copy.go) to be exercised
// end-to-end over real gRPC, which is where the client's tar handling is worth
// testing — that is the half with the historical CVEs.

// SetFile puts a regular file into a container's filesystem.
func (d *Docker) SetFile(containerID, p string, content []byte) *Docker {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ensureFiles(containerID)[path.Clean(p)] = fakeFile{data: content, mode: 0o644}
	return d
}

// SetSymlink puts a symlink into a container's filesystem, so a test can
// exercise CopyFrom's follow_link.
func (d *Docker) SetSymlink(containerID, p, target string) *Docker {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ensureFiles(containerID)[path.Clean(p)] = fakeFile{mode: os.ModeSymlink | 0o777, link: target}
	return d
}

// SetDir marks a path as a directory holding the given files, each name
// relative to it. Serving a directory is how a client's "copy a tree out"
// path gets tested.
func (d *Docker) SetDir(containerID, p string, files map[string][]byte) *Docker {
	d.mu.Lock()
	defer d.mu.Unlock()
	store := d.ensureFiles(containerID)
	store[path.Clean(p)] = fakeFile{mode: os.ModeDir | 0o755, dir: files}
	return d
}

// Files returns a copy of what a container's filesystem holds, including
// everything a CopyTo extracted into it.
func (d *Docker) Files(containerID string) map[string][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string][]byte{}
	for name, f := range d.files[containerID] {
		if f.mode.IsDir() {
			continue
		}
		out[name] = f.data
	}
	return out
}

type fakeFile struct {
	data []byte
	mode os.FileMode
	link string
	dir  map[string][]byte
}

// ensureFiles returns the container's store, creating it. Callers hold d.mu.
func (d *Docker) ensureFiles(containerID string) map[string]fakeFile {
	if d.files == nil {
		d.files = map[string]map[string]fakeFile{}
	}
	if d.files[containerID] == nil {
		d.files[containerID] = map[string]fakeFile{}
	}
	return d.files[containerID]
}

func (d *Docker) stat(containerID, p string) (fakeFile, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.files[containerID][path.Clean(p)]
	return f, ok
}

func noSuchPath(p string) error {
	return fmt.Errorf("could not find the file %s in the container: %w", p, cerrdefs.ErrNotFound)
}

func (f fakeFile) pathStat(p string) container.PathStat {
	return container.PathStat{
		Name:       path.Base(p),
		Size:       int64(len(f.data)),
		Mode:       f.mode,
		Mtime:      time.Unix(0, 0).UTC(),
		LinkTarget: f.link,
	}
}

func (d *Docker) ContainerStatPath(_ context.Context, id string, opts client.ContainerStatPathOptions) (client.ContainerStatPathResult, error) {
	if err := d.enter("ContainerStatPath"); err != nil {
		return client.ContainerStatPathResult{}, err
	}
	f, ok := d.stat(id, opts.Path)
	if !ok {
		return client.ContainerStatPathResult{}, noSuchPath(opts.Path)
	}
	return client.ContainerStatPathResult{Stat: f.pathStat(opts.Path)}, nil
}

func (d *Docker) CopyFromContainer(_ context.Context, id string, opts client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	if err := d.enter("CopyFromContainer"); err != nil {
		return client.CopyFromContainerResult{}, err
	}
	f, ok := d.stat(id, opts.SourcePath)
	if !ok {
		return client.CopyFromContainerResult{}, noSuchPath(opts.SourcePath)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	base := path.Base(path.Clean(opts.SourcePath))
	write := func(name string, mode int64, body []byte, link string) error {
		h := &tar.Header{Name: name, Mode: mode, Size: int64(len(body)), ModTime: time.Unix(0, 0).UTC()}
		if link != "" {
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, link, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}
	var err error
	switch {
	case f.mode.IsDir():
		for name, body := range f.dir {
			if err = write(path.Join(base, name), 0o644, body, ""); err != nil {
				break
			}
		}
	case f.link != "":
		err = write(base, 0o777, nil, f.link)
	default:
		err = write(base, 0o644, f.data, "")
	}
	if err == nil {
		err = tw.Close()
	}
	if err != nil {
		return client.CopyFromContainerResult{}, err
	}
	return client.CopyFromContainerResult{
		Content: io.NopCloser(bytes.NewReader(buf.Bytes())),
		Stat:    f.pathStat(opts.SourcePath),
	}, nil
}

func (d *Docker) CopyToContainer(_ context.Context, id string, opts client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	if err := d.enter("CopyToContainer"); err != nil {
		return client.CopyToContainerResult{}, err
	}
	// Read before locking: Content is the agent's live gRPC stream.
	tr := tar.NewReader(opts.Content)
	extracted := map[string]fakeFile{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return client.CopyToContainerResult{}, err
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return client.CopyToContainerResult{}, err
		}
		extracted[path.Join(opts.DestinationPath, h.Name)] = fakeFile{
			data: body,
			mode: os.FileMode(h.Mode), //nolint:gosec // test fake: the mode is the one the test wrote
			link: h.Linkname,
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	store := d.ensureFiles(id)
	for name, f := range extracted {
		store[name] = f
	}
	return client.CopyToContainerResult{}, nil
}
