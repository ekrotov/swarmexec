// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/swarmfake"
)

// fakeManager is a reachable manager for Dial to hand back.
func fakeManager(t *testing.T) *client.Client {
	t.Helper()
	api := httptest.NewServer(&swarmfake.Swarm{})
	t.Cleanup(api.Close)
	dcli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	return dcli
}

// The server's own context is dialled under the name it was started with;
// the agent identity it gets is marked as the assistant's; the connection is
// kept for the session.
func TestConnectUsesTheCallersDial(t *testing.T) {
	var dialled []string
	s := New(Options{
		ConnectTimeout: time.Second,
		DockerContext:  "staging",
		Dial: func(_ context.Context, name string) (Endpoint, error) {
			dialled = append(dialled, name)
			return Endpoint{Label: "staging", Docker: fakeManager(t), Config: config.Config{Port: 7000, Operator: "alice"}}, nil
		},
	})
	defer s.Close()

	c, err := s.cluster(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(dialled) != 1 || dialled[0] != "staging" {
		t.Errorf("dialled %q, want the server's own context by name", dialled)
	}
	if c.label != "staging" {
		t.Errorf("label = %q", c.label)
	}
	if op := c.agent.Config().Operator; op != "alice+mcp" {
		t.Errorf("operator = %q, want the assistant marked", op)
	}
	if _, err := s.cluster(context.Background(), "staging"); err != nil || len(dialled) != 1 {
		t.Errorf("second ask dialled again (%d dials, err %v); the session keeps its connection", len(dialled), err)
	}
}

// A dial failure and an unreachable manager are errors now — and not cached,
// so the retry an assistant makes after being told to load its key dials
// again.
func TestConnectFailuresAreNotCached(t *testing.T) {
	dials := 0
	var fail error = errors.New("ssh: Permission denied (publickey).")
	s := New(Options{
		ConnectTimeout: time.Second,
		Dial: func(context.Context, string) (Endpoint, error) {
			dials++
			if fail != nil {
				return Endpoint{}, fail
			}
			// A client whose manager is gone: Dial may forgive that, the server
			// must not.
			api := httptest.NewServer(&swarmfake.Swarm{})
			api.Close()
			dcli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
			if err != nil {
				t.Fatal(err)
			}
			return Endpoint{Docker: dcli}, nil
		},
	})
	defer s.Close()

	if _, err := s.cluster(context.Background(), ""); !errors.Is(err, fail) {
		t.Fatalf("err = %v, want the dial error", err)
	}
	fail = nil
	if _, err := s.cluster(context.Background(), ""); err == nil {
		t.Fatal("an unreachable manager connected")
	}
	if dials != 2 {
		t.Errorf("dials = %d, want 2 (failures are not cached)", dials)
	}
}

// A client that goes away is the end of a session, not an error.
func TestServeEndsCleanlyAtEOF(t *testing.T) {
	s := New(Options{ConnectTimeout: time.Second})
	defer s.Close()
	if err := s.Serve(context.Background(), io.NopCloser(strings.NewReader("")), io.Discard); err != nil {
		t.Errorf("Serve = %v, want nil at end of input", err)
	}
}
