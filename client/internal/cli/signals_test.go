// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// blockingRoot is a command that waits for its context and, like the real
// ones, cleans up on the way out with a defer.
func blockingRoot(started chan<- struct{}, cleaned *atomic.Bool, ret error) *cobra.Command {
	root := &cobra.Command{
		Use: "x", SilenceErrors: true, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			defer cleaned.Store(true) // stands for ac.Close(), fw.Close(), the askpass socket
			close(started)
			<-cmd.Context().Done()
			return ret
		},
	}
	root.SetArgs([]string{})
	return root
}

// CLI-02: Ctrl-C used to be process death, so no deferred cleanup ran and a
// `wait` stopped without a word. It now cancels the command's context, lets
// the command unwind and exits 128+signal — whatever the command returned.
func TestSignalCancelsContextRunsCleanupAndExits(t *testing.T) {
	for _, c := range []struct {
		sig  os.Signal
		want int
		ret  error
	}{
		{syscall.SIGINT, exitInterrupted, nil},
		{syscall.SIGINT, exitInterrupted, &cliError{code: exitNo, err: context.Canceled}}, // wait's branch
		{syscall.SIGTERM, exitTerminated, nil},
		{syscall.SIGHUP, exitHangup, nil},
	} {
		started := make(chan struct{})
		var cleaned atomic.Bool
		sigs := make(chan os.Signal, 2)
		done := make(chan int, 1)
		go func() {
			done <- runWithSignals(blockingRoot(started, &cleaned, c.ret), &globalFlags{}, sigs, io.Discard)
		}()
		<-started
		sigs <- c.sig
		select {
		case code := <-done:
			if code != c.want {
				t.Errorf("%v: exit %d, want %d", c.sig, code, c.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%v: the command was not cancelled", c.sig)
		}
		if !cleaned.Load() {
			t.Errorf("%v: the deferred cleanup did not run", c.sig)
		}
	}
}

func TestNoSignalKeepsTheCommandsCode(t *testing.T) {
	root := &cobra.Command{Use: "x", SilenceErrors: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error { return &cliError{code: exitNo, silent: true} }}
	root.SetArgs([]string{})
	if code := runWithSignals(root, &globalFlags{}, make(chan os.Signal), io.Discard); code != exitNo {
		t.Errorf("exit %d, want %d", code, exitNo)
	}
}

// A second Ctrl-C still kills at once, and so does a command that never
// notices its context (a blocking read): after the grace period it is cut off.
func TestSecondSignalAndGraceEndTheProcess(t *testing.T) {
	prevExit, prevGrace := hardExit, stopGrace
	t.Cleanup(func() { hardExit, stopGrace = prevExit, prevGrace })

	for _, name := range []string{"second signal", "grace period"} {
		release := make(chan struct{})
		exited := make(chan int, 1)
		hardExit = func(code int) {
			exited <- code
			close(release) // let the stuck command return so the test ends
		}
		stopGrace = time.Hour
		if name == "grace period" {
			stopGrace = 50 * time.Millisecond
		}
		started := make(chan struct{})
		root := &cobra.Command{Use: "x", SilenceErrors: true, SilenceUsage: true,
			RunE: func(*cobra.Command, []string) error { close(started); <-release; return nil }}
		root.SetArgs([]string{})
		sigs := make(chan os.Signal, 2)
		done := make(chan int, 1)
		go func() { done <- runWithSignals(root, &globalFlags{}, sigs, io.Discard) }()
		<-started
		sigs <- syscall.SIGINT
		if name == "second signal" {
			select {
			case code := <-exited:
				t.Fatalf("exited with %d on the first signal", code)
			case <-time.After(100 * time.Millisecond):
			}
			sigs <- syscall.SIGINT
		}
		select {
		case code := <-exited:
			if code != exitInterrupted {
				t.Errorf("%s: hard exit %d, want %d", name, code, exitInterrupted)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the process was not ended", name)
		}
		<-done
	}
}
