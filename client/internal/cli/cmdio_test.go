// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// syncBuf is a buffer a test may read while a command is still writing to it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// usePrompts plays the operator for the prompts of one test: they read
// answers from input, ask through the returned buffer, and see a terminal (or
// not). The seam is restored at cleanup.
func usePrompts(t *testing.T, input string, terminal bool) *syncBuf {
	t.Helper()
	asked := &syncBuf{}
	prev := prompts
	prompts = promptSeam{in: bufio.NewReader(strings.NewReader(input)), out: asked, interactive: func() bool { return terminal }}
	t.Cleanup(func() { prompts = prev })
	return asked
}

// runIO runs cmd with args, stdin as its input, and returns what it wrote to
// its own stdout and stderr. Unlike runCmd alone, it fails the test when the
// command wrote to the process streams instead of the command's: that is the
// seam, and a stray fmt.Println would otherwise go unnoticed.
func runIO(t *testing.T, cmd *cobra.Command, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut syncBuf
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	leakedOut, leakedErr, err := runCmd(t, cmd, args...)
	if leakedOut != "" || leakedErr != "" {
		t.Errorf("the command bypassed its streams: process stdout %q, process stderr %q", leakedOut, leakedErr)
	}
	return out.String(), errOut.String(), err
}
