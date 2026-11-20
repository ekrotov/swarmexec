// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"
)

func ev(kind events.Type, action events.Action, attrs map[string]string) events.Message {
	return events.Message{
		Type:     kind,
		Action:   action,
		Actor:    events.Actor{ID: "0123456789abcdef0123", Attributes: attrs},
		TimeNano: time.Now().UnixNano(),
	}
}

func TestActivityLogKeepsTheNewestFirst(t *testing.T) {
	var a activityLog
	for _, name := range []string{"first", "second", "third"} {
		a.record(ev(events.ServiceEventType, events.ActionUpdate, map[string]string{"name": name}))
	}
	got, dropped := a.list()
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	if len(got) != 3 || got[0].Name != "third" || got[2].Name != "first" {
		t.Fatalf("entries = %v, want third, second, first", entryNames(got))
	}
}

// The ring is bounded, and it says how much it threw away: a view that quietly
// starts in the middle would read as "nothing happened before this".
func TestActivityLogDropsTheOldestAndCountsIt(t *testing.T) {
	var a activityLog
	for i := range activityCapacity + 7 {
		a.record(ev(events.ServiceEventType, events.ActionUpdate, map[string]string{"name": fmt.Sprintf("svc-%d", i)}))
	}
	got, dropped := a.list()
	if len(got) != activityCapacity {
		t.Errorf("kept %d, want %d", len(got), activityCapacity)
	}
	if dropped != 7 {
		t.Errorf("dropped = %d, want 7", dropped)
	}
	if got[len(got)-1].Name != "svc-7" {
		t.Errorf("oldest kept = %s, want svc-7", got[len(got)-1].Name)
	}
}

// Docker hangs every label on a service event. Labels are strings their
// deployer chose, so only attributes whose meaning the Engine defines are
// rendered — the same rule CONTRACT.md §3.5 states for container events.
func TestActivityEntryTakesOnlyDefinedAttributes(t *testing.T) {
	var a activityLog
	a.record(ev(events.ServiceEventType, events.ActionUpdate, map[string]string{
		"name":                     "shop_web",
		"updatestate.new":          "completed",
		"com.docker.stack.image":   "nginx:1",
		"my.label":                 "[red]surprise[white]",
		"swarmexec.mcp.enable":     "true",
		"org.opencontainers.title": "web",
	}))
	got, _ := a.list()
	if len(got) != 1 {
		t.Fatal("no entry recorded")
	}
	e := got[0]
	if e.Name != "shop_web" || e.Detail != "updatestate: completed" {
		t.Errorf("entry = %+v, want shop_web with the update state", e)
	}
	joined := e.Name + e.Detail + e.Kind + e.Action
	for _, leaked := range []string{"surprise", "nginx:1", "opencontainers"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("a label reached the entry: %q in %q", leaked, joined)
		}
	}
}

func TestActivityEntryFallsBackToTheShortID(t *testing.T) {
	var a activityLog
	a.record(ev(events.SecretEventType, events.ActionCreate, nil))
	got, _ := a.list()
	if got[0].Name != "0123456789ab" {
		t.Errorf("name = %q, want the 12-character id", got[0].Name)
	}
}

func TestEventTimeFallsBackWhenTheDaemonSentNone(t *testing.T) {
	before := time.Now()
	got := eventTime(events.Message{})
	if got.Before(before) {
		// The zero time would sort to 1970 and read as a bug in the view.
		t.Errorf("eventTime of a message without a timestamp = %v, want about now", got)
	}
	ts := time.Now().Add(-time.Hour).Unix()
	if got := eventTime(events.Message{Time: ts}); got.Unix() != ts {
		t.Errorf("eventTime = %v, want the seconds the daemon sent", got)
	}
}

// A nil ring (a cluster the UI never connected to) must not panic.
func TestActivityLogNilIsSafe(t *testing.T) {
	var a *activityLog
	a.record(ev(events.NodeEventType, events.ActionUpdate, nil))
	if got, dropped := a.list(); got != nil || dropped != 0 {
		t.Errorf("list of a nil ring = %v, %d", got, dropped)
	}
}

// The split the activity view is built on: everything subscribed is recorded,
// only what the tree shows triggers a re-list. A secret changing must not cost
// a manager call.
func TestOnlyTreeKindsRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msgs, errs := make(chan events.Message), make(chan error)
	var refreshes atomic.Int64
	var seen activityLog

	go consumeTopology(ctx, msgs, errs, fastTopology, seen.record, func() { refreshes.Add(1) })

	msgs <- ev(events.SecretEventType, events.ActionCreate, map[string]string{"name": "db_password"})
	msgs <- ev(events.ConfigEventType, events.ActionCreate, map[string]string{"name": "nginx_conf"})
	msgs <- ev(events.NetworkEventType, events.ActionCreate, map[string]string{"name": "shop_net"})
	waitFor(t, "the three events to be recorded", func() bool {
		got, _ := seen.list()
		return len(got) == 3
	})
	// Wait out several debounce windows: a refresh would have happened by now.
	time.Sleep(4 * fastTopology.debounce)
	if got := refreshes.Load(); got != 0 {
		t.Fatalf("secret/config/network events caused %d tree refresh(es), want 0", got)
	}

	msgs <- ev(events.ServiceEventType, events.ActionUpdate, map[string]string{"name": "shop_web"})
	waitFor(t, "the service event to refresh the tree", func() bool { return refreshes.Load() == 1 })
	waitFor(t, "all four events to be recorded", func() bool {
		got, _ := seen.list()
		return len(got) == 4
	})
}

func entryNames(es []activityEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Name
	}
	return out
}

// Recorded from a real swarm: `docker events` while one service was created
// and scaled. The shapes here are the daemon's, not invented ones.
func TestActivityRecordsWhatARealSwarmSends(t *testing.T) {
	var a activityLog
	for _, m := range []events.Message{
		{Type: events.SecretEventType, Action: events.ActionCreate,
			Actor: events.Actor{Attributes: map[string]string{"name": "a6test"}}},
		{Type: events.NetworkEventType, Action: events.ActionCreate,
			Actor: events.Actor{Attributes: map[string]string{"name": "a6net"}}},
		{Type: events.ServiceEventType, Action: events.ActionCreate,
			Actor: events.Actor{Attributes: map[string]string{"name": "a6svc"}}},
		// Every container joining a network emits this. On a node with any
		// churn it is most of the stream, and it says nothing about the shape
		// of the cluster changing.
		{Type: events.NetworkEventType, Action: events.ActionConnect,
			Actor: events.Actor{Attributes: map[string]string{"container": "ba2047ebbc69", "name": "bridge", "type": "bridge"}}},
		{Type: events.ServiceEventType, Action: events.ActionUpdate,
			Actor: events.Actor{Attributes: map[string]string{"name": "a6svc", "replicas.new": "2", "replicas.old": "1"}}},
	} {
		a.record(m)
	}

	got, _ := a.list()
	if len(got) != 4 {
		t.Fatalf("recorded %v, want the connect dropped", entryNames(got))
	}
	for _, e := range got {
		if e.Kind == "network" && e.Action == "connect" {
			t.Error("a network connect reached the ring")
		}
	}
	if got[0].Name != "a6svc" || got[0].Detail != "replicas: 2" {
		t.Errorf("newest = %+v, want the scale to 2", got[0])
	}
}
