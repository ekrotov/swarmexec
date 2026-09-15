// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The report names every service, network and secret in the cluster and every
// weakness found in it. That is a map of where to attack, and it has no
// business being readable by every account on the machine.
func TestReportFileIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "report.md")
	if err := writeReportFile(path, "# report\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("report mode = %04o, want 0600", perm)
	}
	// The parent it had to create must not be traversable by others either.
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("created directory mode = %04o, want no access for group or other", perm)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "# report\n" {
		t.Errorf("content = %q, %v", body, err)
	}
}

// A docker context name is user-chosen and may contain a separator. It goes
// into a filename, so it must not be able to walk out of the directory.
func TestSafeFileNameCannotEscapeTheDirectory(t *testing.T) {
	for _, in := range []string{"../../etc/passwd", "a/b", "..", "with space", "ok-name_1"} {
		got := safeFileName(in)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("safeFileName(%q) = %q, still contains a separator", in, got)
		}
		if got == ".." || strings.Contains(got, "..") {
			t.Errorf("safeFileName(%q) = %q, still walks up", in, got)
		}
	}
	if got := safeFileName("ok-name_1"); got != "ok-name_1" {
		t.Errorf("a plain name should survive unchanged, got %q", got)
	}
}

// Two clusters' reports land in the same directory often enough that the
// default name has to say which cluster it describes, and not collide with the
// previous run.
func TestDefaultReportPathNamesTheCluster(t *testing.T) {
	got := defaultReportPath("prod")
	if !strings.Contains(got, "prod") {
		t.Errorf("default name %q does not name the cluster", got)
	}
	if !strings.HasSuffix(got, ".md") {
		t.Errorf("default name %q is not a markdown file", got)
	}
	// No context configured still yields a usable, non-empty name.
	if bare := defaultReportPath(""); bare == "" || !strings.HasSuffix(bare, ".md") {
		t.Errorf("nameless context yielded %q", bare)
	}
	// A hostile context name must not reach the path.
	if hostile := defaultReportPath("../../etc"); strings.Contains(hostile, "..") {
		t.Errorf("default name %q walks up", hostile)
	}
}
