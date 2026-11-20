// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package cptar packs and unpacks the tar streams `swarmexec cp` moves between
// the workstation and a container.
//
// Unpacking is the dangerous half, and it is dangerous on the workstation, not
// in the container: the archive comes from a container's filesystem, which a
// compromised image controls, and a tar entry named "../../.ssh/authorized_keys"
// is a write outside the destination. `kubectl cp` shipped that bug more than
// once. Extract therefore refuses, rather than sanitizes-and-continues, every
// entry whose resolved target leaves the destination root — including one that
// would get there through a symlink the same archive just created, which is
// why each path component is checked with Lstat instead of trusting a prefix
// comparison on the name alone.
package cptar

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ExtractOptions shapes the destination names, which is how the `docker cp`
// rules ("into this directory" vs "as this name") are expressed.
type ExtractOptions struct {
	// Rename replaces the first path element of every entry, turning
	// "app.log" into the name the operator asked for. Empty leaves names alone.
	Rename string
	// StripRoot drops the first path element, turning "nginx/nginx.conf" into
	// "nginx.conf" — "copy the directory's contents here".
	StripRoot bool
	// MaxBytes caps the payload written; 0 means no cap. The agent enforces its
	// own; this one bounds what a misreporting agent could still write here.
	MaxBytes int64
}

// Report is what an extraction did, for the summary line and for tests.
type Report struct {
	Files, Dirs, Links int
	Bytes              int64
	// Refused names entries that were not written and why: an escape attempt,
	// an unsupported entry type. It is shown to the operator — a file silently
	// missing from a copy is worse than a noisy one.
	Refused []string
}

// ErrEscape is the refusal of an entry that would be written outside the
// destination root.
var ErrEscape = errors.New("tar entry escapes the destination")

// Extract writes the tar stream r below dstRoot, which must exist and be a
// directory. It returns what it wrote even when it fails, so a partial copy
// can be reported honestly.
func Extract(r io.Reader, dstRoot string, opts ExtractOptions) (Report, error) {
	var rep Report
	root, err := filepath.Abs(dstRoot)
	if err != nil {
		return rep, err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return rep, nil
		}
		if err != nil {
			return rep, fmt.Errorf("read archive: %w", err)
		}
		name, keep := rename(h.Name, opts)
		if !keep {
			continue
		}
		target, err := safeJoin(root, name)
		if err != nil {
			rep.Refused = append(rep.Refused, fmt.Sprintf("%s (%v)", h.Name, err))
			continue
		}
		if err := writeEntry(tr, h, root, target, &rep, opts); err != nil {
			return rep, err
		}
	}
}

// writeEntry puts one tar entry on disk. Everything that is not a regular
// file, a directory or a safe symlink is refused by name: a device node or a
// socket out of a container has no meaning on the operator's machine, and
// quietly dropping it would be the kind of surprise a copy must not have.
func writeEntry(tr io.Reader, h *tar.Header, root, target string, rep *Report, opts ExtractOptions) error {
	switch h.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		rep.Dirs++
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		n, err := writeFile(tr, target, perm(h.FileInfo().Mode()), opts.MaxBytes-rep.Bytes)
		rep.Bytes += n
		if err != nil {
			return err
		}
		rep.Files++
	case tar.TypeSymlink:
		if err := linkTargetInside(root, target, h.Linkname); err != nil {
			rep.Refused = append(rep.Refused, fmt.Sprintf("%s -> %s (%v)", h.Name, h.Linkname, err))
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		_ = os.Remove(target)
		if err := os.Symlink(h.Linkname, target); err != nil {
			return err
		}
		rep.Links++
	default:
		rep.Refused = append(rep.Refused, fmt.Sprintf("%s (unsupported entry type %q)", h.Name, string(rune(h.Typeflag))))
	}
	return nil
}

// writeFile writes one regular file, stopping at left bytes when a cap is in
// force (left <= 0 with a cap set means the cap is already spent).
func writeFile(r io.Reader, target string, mode fs.FileMode, left int64) (int64, error) {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var n int64
	if left > 0 {
		// One byte over the cap is enough to notice it was exceeded.
		n, err = io.Copy(f, io.LimitReader(r, left+1))
		if err == nil && n > left {
			return n, fmt.Errorf("archive exceeds the size limit")
		}
	} else {
		n, err = io.Copy(f, r)
	}
	if err != nil {
		return n, err
	}
	return n, f.Close()
}

// perm keeps the ordinary permission bits and drops setuid, setgid and the
// sticky bit: a file arriving from a container has no business being
// privileged on the operator's machine.
func perm(m fs.FileMode) fs.FileMode { return m.Perm() }

// rename applies StripRoot and Rename to an entry name, and reports whether
// anything is left to write (stripping the root leaves the root entry itself
// with an empty name).
//
// It deliberately does NOT clean the name: path.Clean("/"+name) would turn
// "../pwned" into "pwned" and write it, which is sanitize-and-continue — the
// very thing this package refuses to do. The name is passed on as it came and
// safeJoin decides whether it may be written at all.
func rename(name string, opts ExtractOptions) (string, bool) {
	name = strings.TrimSuffix(filepath.ToSlash(name), "/")
	if name == "." || name == "" {
		return "", false
	}
	switch {
	case opts.StripRoot:
		_, rest, found := strings.Cut(name, "/")
		if !found || rest == "" {
			return "", false
		}
		name = rest
	case opts.Rename != "":
		if _, rest, found := strings.Cut(name, "/"); found {
			name = path.Join(opts.Rename, rest)
		} else {
			name = opts.Rename
		}
	}
	return name, true
}

// safeJoin resolves name below root and refuses anything that leaves it. It
// walks the path component by component with Lstat rather than comparing the
// joined string, because an earlier entry of the same archive may have created
// a symlink that a later one would otherwise follow out of the destination.
func safeJoin(root, name string) (string, error) {
	if path.IsAbs(name) || filepath.IsAbs(name) {
		return "", fmt.Errorf("%w: absolute path", ErrEscape)
	}
	cur := root
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return "", fmt.Errorf("%w: %q", ErrEscape, name)
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // will be created below root
			}
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %q crosses a symlink", ErrEscape, name)
		}
	}
	if !within(root, cur) {
		return "", fmt.Errorf("%w: %q", ErrEscape, name)
	}
	return cur, nil
}

// linkTargetInside refuses a symlink that would point out of root.
//
// It walks the target the way the OS resolves it — component by component from
// the directory the link will live in — instead of cleaning the joined string
// once. The difference matters when the destination already holds a symlink of
// its own (`old -> /etc`, from an earlier copy or from the operator): a target
// of "old/passwd" is below root by any textual comparison and resolves outside
// it in fact. Such a link is refused, and so is one that leaves root at any
// step, even if a later ".." would bring it back.
//
// An absolute target is refused outright: inside a container "/etc/ssl/certs"
// means something, in the operator's destination directory it does not.
func linkTargetInside(root, linkPath, target string) error {
	if filepath.IsAbs(target) || path.IsAbs(target) {
		return fmt.Errorf("%w: absolute link target", ErrEscape)
	}
	cur := filepath.Dir(linkPath)
	for _, part := range strings.Split(filepath.ToSlash(target), "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
		default:
			cur = filepath.Join(cur, part)
		}
		if !within(root, cur) {
			return ErrEscape
		}
		// The final component usually does not exist yet (it may be a later
		// entry of the same archive); only one that exists AND is a symlink
		// can carry the resolution out of root.
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %q crosses a symlink", ErrEscape, target)
		}
	}
	return nil
}

// within reports whether p is root or below it.
func within(root, p string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// Pack writes src — a local file or directory — as a tar stream whose entries
// are rooted at name, which is what the destination inside the container will
// be called. Symlinks are stored as links; anything else that is not a regular
// file or a directory is skipped, and named in the returned report.
func Pack(w io.Writer, src, name string) (Report, error) {
	var rep Report
	tw := tar.NewWriter(w)
	root := filepath.Clean(src)
	err := filepath.Walk(root, func(p string, fi fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		entry := name
		if rel != "." {
			entry = path.Join(name, filepath.ToSlash(rel))
		}
		switch {
		case fi.IsDir():
			rep.Dirs++
			return writeHeader(tw, fi, entry, "")
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			rep.Links++
			return writeHeader(tw, fi, entry, target)
		case fi.Mode().IsRegular():
			if err := writeHeader(tw, fi, entry, ""); err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			n, err := io.Copy(tw, f)
			rep.Bytes += n
			rep.Files++
			return err
		default:
			rep.Refused = append(rep.Refused, fmt.Sprintf("%s (unsupported file type)", p))
			return nil
		}
	})
	if err != nil {
		return rep, err
	}
	return rep, tw.Close()
}

func writeHeader(tw *tar.Writer, fi fs.FileInfo, name, link string) error {
	h, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return err
	}
	h.Name = name
	if fi.IsDir() {
		h.Name += "/"
	}
	// Ownership is the destination's business: the agent passes copy_uid_gid
	// only when the operator asked for -a, and a uid from this machine means
	// nothing in the container otherwise.
	h.Uname, h.Gname = "", ""
	return tw.WriteHeader(h)
}

// Rename copies a tar stream through, rewriting entry names by the same rules
// Extract applies. It is how a container-to-container copy renames the source
// to what the destination should be called without ever touching this
// machine's disk — the archive is re-framed in flight, not stored.
func Rename(r io.Reader, w io.Writer, opts ExtractOptions) error {
	tr, tw := tar.NewReader(r), tar.NewWriter(w)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return tw.Close()
		}
		if err != nil {
			return err
		}
		name, keep := rename(h.Name, opts)
		if !keep {
			continue
		}
		out := *h
		out.Name = name
		if h.FileInfo().IsDir() {
			out.Name += "/"
		}
		if err := tw.WriteHeader(&out); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}
