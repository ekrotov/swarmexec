// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cptar

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// entry is one tar member a test writes.
type entry struct {
	name string
	body string
	mode int64
	typ  byte
	link string
}

func archive(t *testing.T, entries ...entry) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Mode: mode, Typeflag: typ, Linkname: e.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

// The attack the whole package exists for: an archive out of a container
// naming a path above the destination. `kubectl cp` shipped this bug twice.
func TestExtractRefusesAnEntryAboveTheDestination(t *testing.T) {
	dst := t.TempDir()
	outside := filepath.Join(filepath.Dir(dst), "pwned")

	rep, err := Extract(archive(t, entry{name: "../pwned", body: "owned"}), dst, ExtractOptions{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("%s was written outside the destination", outside)
	}
	if len(rep.Refused) != 1 || !strings.Contains(rep.Refused[0], "../pwned") {
		t.Errorf("Refused = %v, want the entry named", rep.Refused)
	}
}

// Deeper nesting, and the absolute form, are the same attack.
func TestExtractRefusesEveryShapeOfEscape(t *testing.T) {
	for _, name := range []string{
		"a/../../escape", "./../escape", "/etc/passwd", "a/b/../../../escape",
	} {
		t.Run(name, func(t *testing.T) {
			dst := t.TempDir()
			rep, err := Extract(archive(t, entry{name: name, body: "x"}), dst, ExtractOptions{})
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if len(rep.Refused) == 0 && rep.Files > 0 {
				t.Errorf("%q was written; it must be refused", name)
			}
			assertNothingOutside(t, dst)
		})
	}
}

// The second half of the same attack: a symlink out of the destination,
// followed by an entry that writes "through" it. The link is refused, so the
// write lands inside — but the check is on the link, not on the later name,
// which is the part a prefix comparison alone would miss.
func TestExtractRefusesASymlinkOutOfTheDestination(t *testing.T) {
	dst := t.TempDir()
	rep, err := Extract(archive(t,
		entry{name: "escape", typ: tar.TypeSymlink, link: "../.."},
		entry{name: "escape/evil", body: "owned"},
	), dst, ExtractOptions{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(rep.Refused) == 0 {
		t.Fatalf("the escaping symlink was accepted")
	}
	// The later entry may have created an ordinary directory of that name
	// inside the destination; what must not exist is the link out of it.
	if fi, err := os.Lstat(filepath.Join(dst, "escape")); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		t.Errorf("the escaping symlink was created")
	}
	assertNothingOutside(t, dst)
}

// A symlink that stays inside is ordinary content and is kept.
func TestExtractKeepsASymlinkInsideTheDestination(t *testing.T) {
	dst := t.TempDir()
	rep, err := Extract(archive(t,
		entry{name: "data/real", body: "hello"},
		entry{name: "data/alias", typ: tar.TypeSymlink, link: "real"},
	), dst, ExtractOptions{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rep.Links != 1 || len(rep.Refused) != 0 {
		t.Fatalf("report = %+v, want one link kept", rep)
	}
	got, err := os.ReadFile(filepath.Join(dst, "data", "alias"))
	if err != nil || string(got) != "hello" {
		t.Errorf("reading through the link gave %q, %v", got, err)
	}
}

// setuid out of a container must not become setuid on the operator's machine.
func TestExtractDropsSetuidAndFriends(t *testing.T) {
	dst := t.TempDir()
	if _, err := Extract(archive(t, entry{name: "tool", body: "x", mode: 0o4755}), dst, ExtractOptions{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dst, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		t.Errorf("mode = %v, want the privileged bits dropped", fi.Mode())
	}
}

// Anything that is neither a file, a directory nor a symlink is named rather
// than silently dropped: a copy that quietly loses part of a tree is worse
// than one that says so.
func TestExtractNamesWhatItCannotWrite(t *testing.T) {
	dst := t.TempDir()
	rep, err := Extract(archive(t, entry{name: "dev/null", typ: tar.TypeChar}), dst, ExtractOptions{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(rep.Refused) != 1 || !strings.Contains(rep.Refused[0], "dev/null") {
		t.Errorf("Refused = %v, want the device named", rep.Refused)
	}
}

func TestExtractRenamesAndStripsTheRoot(t *testing.T) {
	t.Run("rename a single file", func(t *testing.T) {
		dst := t.TempDir()
		if _, err := Extract(archive(t, entry{name: "app.log", body: "L"}), dst, ExtractOptions{Rename: "mine.log"}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dst, "mine.log")); err != nil {
			t.Errorf("mine.log: %v", err)
		}
	})
	t.Run("rename a tree's root", func(t *testing.T) {
		dst := t.TempDir()
		if _, err := Extract(archive(t, entry{name: "nginx/nginx.conf", body: "c"}), dst, ExtractOptions{Rename: "conf"}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dst, "conf", "nginx.conf")); err != nil {
			t.Errorf("conf/nginx.conf: %v", err)
		}
	})
	t.Run("strip the root", func(t *testing.T) {
		dst := t.TempDir()
		if _, err := Extract(archive(t,
			entry{name: "nginx", typ: tar.TypeDir, mode: 0o755},
			entry{name: "nginx/nginx.conf", body: "c"},
		), dst, ExtractOptions{StripRoot: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dst, "nginx.conf")); err != nil {
			t.Errorf("nginx.conf: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dst, "nginx")); err == nil {
			t.Errorf("the stripped root was created anyway")
		}
	})
}

func TestExtractStopsAtTheSizeCap(t *testing.T) {
	dst := t.TempDir()
	_, err := Extract(archive(t, entry{name: "big", body: strings.Repeat("x", 100)}), dst, ExtractOptions{MaxBytes: 10})
	if err == nil {
		t.Fatalf("a 100-byte entry passed a 10-byte cap")
	}
}

// Pack and Extract are each other's inverse, which is what a copy through two
// containers relies on.
func TestPackAndExtractRoundTrip(t *testing.T) {
	src := t.TempDir()
	mkdir(t, filepath.Join(src, "sub"))
	write(t, filepath.Join(src, "top.txt"), "top")
	write(t, filepath.Join(src, "sub", "nested.txt"), "nested")

	var buf bytes.Buffer
	if _, err := Pack(&buf, src, "tree"); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	dst := t.TempDir()
	rep, err := Extract(bytes.NewReader(buf.Bytes()), dst, ExtractOptions{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rep.Files != 2 {
		t.Errorf("Files = %d, want 2", rep.Files)
	}
	for name, want := range map[string]string{
		filepath.Join(dst, "tree", "top.txt"):           "top",
		filepath.Join(dst, "tree", "sub", "nested.txt"): "nested",
	} {
		got, err := os.ReadFile(name)
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestPackNamesASingleFileAfterTheDestination(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "patch.sql"), "SELECT 1")

	var buf bytes.Buffer
	if _, err := Pack(&buf, filepath.Join(src, "patch.sql"), "renamed.sql"); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	names := entryNames(t, buf.Bytes())
	if len(names) != 1 || names[0] != "renamed.sql" {
		t.Errorf("entries = %v, want [renamed.sql]", names)
	}
}

// Rename re-frames an archive in flight: the container-to-container path, which
// never writes to this machine.
func TestRenameRewritesTheRootWithoutTouchingTheContent(t *testing.T) {
	in := archive(t,
		entry{name: "nginx", typ: tar.TypeDir, mode: 0o755},
		entry{name: "nginx/nginx.conf", body: "listen 80;"},
	)
	var out bytes.Buffer
	if err := Rename(in, &out, ExtractOptions{Rename: "conf"}); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	names := entryNames(t, out.Bytes())
	want := []string{"conf/", "conf/nginx.conf"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	dst := t.TempDir()
	if _, err := Extract(bytes.NewReader(out.Bytes()), dst, ExtractOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "conf", "nginx.conf"))
	if err != nil || string(got) != "listen 80;" {
		t.Errorf("content = %q, %v; want it unchanged", got, err)
	}
}

func entryNames(t *testing.T, b []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(b))
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			return names
		}
		names = append(names, h.Name)
	}
}

// assertNothingOutside fails when anything was written next to the destination.
func assertNothingOutside(t *testing.T, dst string) {
	t.Helper()
	parent := filepath.Dir(dst)
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Join(parent, e.Name()) != dst && strings.Contains(e.Name(), "escape") {
			t.Errorf("%s was written outside the destination", e.Name())
		}
	}
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

// A link target that is textually inside the destination but resolves outside
// it, because the destination already holds a symlink of its own. Cleaning the
// joined name once would accept this; resolving it component by component, as
// the OS does, does not.
func TestExtractRefusesALinkThroughAnExistingSymlink(t *testing.T) {
	dst := t.TempDir()
	if err := os.Symlink("/etc", filepath.Join(dst, "old")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rep, err := Extract(archive(t, entry{name: "creds", typ: tar.TypeSymlink, link: "old/passwd"}), dst, ExtractOptions{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(rep.Refused) != 1 || !strings.Contains(rep.Refused[0], "creds") {
		t.Fatalf("Refused = %v, want the link named", rep.Refused)
	}
	if _, err := os.Lstat(filepath.Join(dst, "creds")); err == nil {
		t.Errorf("the link into /etc was created")
	}
}

// A link that walks up and back down inside the destination is ordinary
// content: refusing every ".." would break a tree that uses relative links.
func TestExtractKeepsALinkThatLeavesAndReturnsInside(t *testing.T) {
	dst := t.TempDir()
	rep, err := Extract(archive(t,
		entry{name: "a/real", body: "hello"},
		entry{name: "b/alias", typ: tar.TypeSymlink, link: "../a/real"},
	), dst, ExtractOptions{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rep.Links != 1 || len(rep.Refused) != 0 {
		t.Fatalf("report = %+v, want the link kept", rep)
	}
	got, err := os.ReadFile(filepath.Join(dst, "b", "alias"))
	if err != nil || string(got) != "hello" {
		t.Errorf("reading through the link gave %q, %v", got, err)
	}
}

// An absolute target means something inside a container and nothing in the
// operator's destination directory, so it is refused rather than recreated.
func TestExtractRefusesAnAbsoluteLinkTarget(t *testing.T) {
	dst := t.TempDir()
	rep, err := Extract(archive(t, entry{name: "certs", typ: tar.TypeSymlink, link: "/etc/ssl/certs"}), dst, ExtractOptions{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(rep.Refused) != 1 {
		t.Errorf("Refused = %v, want the absolute link named", rep.Refused)
	}
}

// The ordering attack: a link whose target walks through a path that is
// nothing YET, and a later entry of the same archive turns that path into a
// symlink. Every single step looked safe when it was taken; the finished tree
// still holds a link out of the destination. The second pass catches it.
func TestExtractRemovesALinkThatOnlyEscapesOnceTheArchiveIsComplete(t *testing.T) {
	dst := t.TempDir()
	rep, err := Extract(archive(t,
		// "q" is written first, while "p" does not exist.
		entry{name: "q", typ: tar.TypeSymlink, link: "p/../etc"},
		// "p" then becomes a link to the destination itself, so "p/.." is its
		// parent — outside.
		entry{name: "p", typ: tar.TypeSymlink, link: "."},
	), dst, ExtractOptions{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "q")); err == nil {
		t.Errorf("the escaping link survived the extraction")
	}
	if len(rep.Refused) == 0 || !strings.Contains(strings.Join(rep.Refused, " "), "q") {
		t.Errorf("Refused = %v, want the removed link named", rep.Refused)
	}
	if rep.Links != 1 {
		t.Errorf("Links = %d, want 1 (only the honest link counts)", rep.Links)
	}
}
