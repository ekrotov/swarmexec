// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/rivo/tview"
)

// gatedAPI is the fake swarm with a hand on the tap: service reads wait until
// the test opens the gate, so a switch can be made while one is in flight.
func gatedAPI(t *testing.T) (dcli *client.Client, entered <-chan struct{}, open func()) {
	t.Helper()
	fs := &fakeSwarm{services: []swarm.Service{{
		ID:   "s1",
		Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "web"}},
	}}}
	gate, in := make(chan struct{}), make(chan struct{}, 16)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/services") {
			in <- struct{}{}
			<-gate
		}
		fs.ServeHTTP(w, r)
	}))
	opened := false
	open = func() {
		if !opened {
			opened = true
			close(gate)
		}
	}
	t.Cleanup(func() { open(); api.Close() })
	dcli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dcli.Close() })
	return dcli, in, open
}

func frontPage(t *testing.T, u *ui) string {
	t.Helper()
	var name string
	onLoop(t, u, func() { name, _ = u.pages.GetFrontPage() })
	return name
}

// The bug A-03 named: the service editors had no staleness guard, so a load
// started on cluster A that finished after a switch opened its editor — or
// its error modal — over cluster B. Control first: without a switch the
// editor opens.
func TestServiceEditorOpensOnItsOwnCluster(t *testing.T) {
	u := runningUI(t)
	dcli, entered, open := gatedAPI(t)
	u.dcli = dcli
	onLoop(t, u, func() { u.openPortsEditor("web", tview.NewBox(), nil) })
	<-entered
	open()
	deadline := time.Now().Add(5 * time.Second)
	for frontPage(t, u) != pageListEdit {
		if time.Now().After(deadline) {
			t.Fatalf("the ports editor did not open (front page %q)", frontPage(t, u))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServiceEditorLoadIsDroppedAfterASwitch(t *testing.T) {
	r := captureLog(t)
	u := runningUI(t)
	dcli, entered, open := gatedAPI(t)
	u.dcli = dcli
	onLoop(t, u, func() { u.openPortsEditor("web", tview.NewBox(), nil) })
	<-entered
	// Switch without cancelling: the answer for cluster A still arrives, which
	// is the case only the generation check can catch.
	switchAway(t, u, false)
	open()
	waitForLog(t, r, "stale result dropped", "op=ui.serviceedit.ports.load")
	if p := frontPage(t, u); p != pageMain {
		t.Fatalf("cluster A's editor opened over cluster B (front page %q)", p)
	}
}
