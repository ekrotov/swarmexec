// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
	"swarmexec/internal/prunesched"
)

// denyActions refuses the listed actions and allows the rest, and remembers
// every request it saw.
type denyActions struct {
	deny map[string]bool
	seen *[]auth.Request
}

func (d denyActions) Authorize(_ context.Context, r auth.Request) auth.Decision {
	if d.seen != nil {
		*d.seen = append(*d.seen, r)
	}
	if d.deny[r.Action] {
		return auth.Decision{Allow: false, Reason: "denied " + r.Action}
	}
	return auth.Decision{Allow: true, Reason: "ok"}
}

const (
	catContainers = pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS
	catImages     = pb.PruneCategory_PRUNE_CATEGORY_IMAGES
	catDangling   = pb.PruneCategory_PRUNE_CATEGORY_DANGLING_IMAGES
	catBuild      = pb.PruneCategory_PRUNE_CATEGORY_BUILD_CACHE
	catNetworks   = pb.PruneCategory_PRUNE_CATEGORY_NETWORKS
	catVolumes    = pb.PruneCategory_PRUNE_CATEGORY_VOLUMES
)

var everyCategory = []pb.PruneCategory{catContainers, catImages, catDangling, catBuild, catNetworks, catVolumes}

// stopped builds a stopped container that finished `ago` before now and was
// created `age` before now.
func stopped(d *fakeDocker, id string, size int64, age, ago time.Duration, labels map[string]string) {
	now := time.Now()
	d.containers = append(d.containers, container.Summary{
		ID: id, State: container.StateExited, SizeRw: size, Created: now.Add(-age).Unix(), Labels: labels,
	})
	d.inspect[id] = container.InspectResponse{ID: id, Config: &container.Config{},
		State: &container.State{FinishedAt: now.Add(-ago).Format(time.RFC3339Nano)}}
}

func resultFor(t *testing.T, rs []*pb.PruneCategoryResult, c pb.PruneCategory) *pb.PruneCategoryResult {
	t.Helper()
	for _, r := range rs {
		if r.GetCategory() == c {
			return r
		}
	}
	t.Fatalf("no result for %v in %v", c, rs)
	return nil
}

// The containers guard counts from when a container STOPPED, not when it was
// created: a month-old service that crashed a minute ago is the one whose logs
// someone is about to read.
func TestPruneContainersKeepsRecentlyStopped(t *testing.T) {
	d := newFakeDocker()
	stopped(d, "old", 100, 72*time.Hour, 48*time.Hour, nil)
	stopped(d, "young", 10, 2*time.Hour, time.Hour, nil)
	stopped(d, "longrun-crash", 20, 30*24*time.Hour, 10*time.Minute, nil)
	d.containers = append(d.containers, container.Summary{ID: "running", State: container.StateRunning})
	s := imageTestServer(d)

	resp, err := s.Prune(context.Background(), &pb.PruneRequest{Categories: []pb.PruneCategory{catContainers}})
	if err != nil {
		t.Fatal(err)
	}
	r := resultFor(t, resp.GetResults(), catContainers)
	if r.GetCount() != 1 || r.GetFreedBytes() != 100 {
		t.Errorf("default 24h: result %+v, want only the container that stopped 48h ago", r)
	}
	if got := d.RemovedContainers(); !slices.Equal(got, []string{"old"}) {
		t.Errorf("removed %v, want [old]", got)
	}

	// until=0 takes every stopped container — and still never a running one.
	d.removedContainers = nil
	d.containers = slices.DeleteFunc(d.containers, func(c container.Summary) bool { return c.ID == "old" })
	if _, err := s.Prune(context.Background(), &pb.PruneRequest{Categories: []pb.PruneCategory{catContainers}, ContainersUntil: "0s"}); err != nil {
		t.Fatal(err)
	}
	got := d.RemovedContainers()
	slices.Sort(got)
	if !slices.Equal(got, []string{"longrun-crash", "young"}) {
		t.Errorf("until=0 removed %v, want every stopped container and no running one", got)
	}
	for _, o := range d.listOpts {
		if o.All && len(filterValues(o.Filters, "status")) > 0 &&
			!slices.Equal(filterValues(o.Filters, "status"), []string{"created", "dead", "exited"}) {
			t.Errorf("stopped-container list asked for status %v", filterValues(o.Filters, "status"))
		}
	}
}

func TestPruneContainersUntilIsConfigurable(t *testing.T) {
	d := newFakeDocker()
	stopped(d, "a", 1, 5*time.Hour, 3*time.Hour, nil)
	stopped(d, "b", 1, 2*time.Hour, time.Hour, nil)
	s := imageTestServer(d)
	if _, err := s.Prune(context.Background(), &pb.PruneRequest{Categories: []pb.PruneCategory{catContainers}, ContainersUntil: "2h"}); err != nil {
		t.Fatal(err)
	}
	if got := d.RemovedContainers(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("until=2h removed %v, want [a]", got)
	}
	if _, err := s.Prune(context.Background(), &pb.PruneRequest{Categories: []pb.PruneCategory{catContainers}, ContainersUntil: "soon"}); statusCode(err) != codes.InvalidArgument {
		t.Errorf("a bad duration must be InvalidArgument, got %v", err)
	}
}

// The network guard. The Engine's networks/prune is not even reachable — the
// client interface has no such method — and the hand-rolled selection removes
// only local, non-predefined networks nothing references.
func TestNetworkGuard(t *testing.T) {
	if _, ok := reflect.TypeOf((*DockerClient)(nil)).Elem().MethodByName("NetworksPrune"); ok {
		t.Fatal("DockerClient must not expose NetworksPrune: on a manager it removes unused swarm overlay networks cluster-wide")
	}
	d := newFakeDocker()
	net := func(id, name, scope, driver string) network.Summary {
		return network.Summary{Network: network.Network{ID: id, Name: name, Scope: scope, Driver: driver}}
	}
	d.networks = []network.Summary{
		net("n-bridge", "bridge", "local", "bridge"),
		net("n-host", "host", "local", "host"),
		net("n-none", "none", "local", "null"),
		net("n-gw", "docker_gwbridge", "local", "bridge"),
		net("n-ingress", "ingress", "swarm", "overlay"),
		net("n-overlay", "app_backend", "swarm", "overlay"),
		net("n-localoverlay", "odd", "local", "overlay"),
		net("n-stopped", "used_by_stopped", "local", "bridge"),
		net("n-attached", "attached", "local", "bridge"),
		net("n-junk", "junk", "local", "bridge"),
	}
	cfgOnly := net("n-cfg", "macvlan_cfg", "local", "null")
	cfgOnly.ConfigOnly = true
	d.networks = append(d.networks, cfgOnly)
	d.netInspect = map[string]network.Inspect{
		"n-attached": {Network: d.networks[8].Network, Containers: map[string]network.EndpointResource{"x": {}}},
	}
	// A STOPPED container still referencing a network keeps it: starting that
	// container again must not fail on a network we removed.
	d.containers = append(d.containers, container.Summary{ID: "c1", State: container.StateExited,
		NetworkSettings: &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{
			"used_by_stopped": {NetworkID: "n-stopped"},
		}}})
	s := imageTestServer(d)

	resp, err := s.Prune(context.Background(), &pb.PruneRequest{Categories: []pb.PruneCategory{catNetworks}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.removedNetworks, []string{"n-junk"}) {
		t.Errorf("removed %v, want only the unused local network", d.removedNetworks)
	}
	if r := resultFor(t, resp.GetResults(), catNetworks); r.GetCount() != 1 {
		t.Errorf("result %+v", r)
	}
	for _, o := range d.networkListOpts {
		if got := filterValues(o.Filters, "scope"); !slices.Equal(got, []string{"local"}) {
			t.Errorf("network list scope filter = %v, want local", got)
		}
	}
}

// Volumes hold data: they are pruned when asked for by name and in no other
// case, and then with all=true so named volumes count.
func TestVolumesOnlyWhenAsked(t *testing.T) {
	d := newFakeDocker()
	s := imageTestServer(d)
	if _, err := s.Prune(context.Background(), &pb.PruneRequest{Categories: []pb.PruneCategory{catContainers, catImages, catDangling, catBuild, catNetworks}}); err != nil {
		t.Fatal(err)
	}
	if len(d.volumePruneOpts) != 0 {
		t.Fatal("volumes were pruned without being asked for")
	}
	d.volumePruneReport = volume.PruneReport{VolumesDeleted: []string{"v1", "v2"}, SpaceReclaimed: 300}
	resp, err := s.Prune(context.Background(), &pb.PruneRequest{Categories: []pb.PruneCategory{catVolumes}})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.volumePruneOpts) != 1 || !d.volumePruneOpts[0].All {
		t.Errorf("volume prune options = %+v, want one call with All", d.volumePruneOpts)
	}
	if r := resultFor(t, resp.GetResults(), catVolumes); r.GetCount() != 2 || r.GetFreedBytes() != 300 {
		t.Errorf("result %+v", r)
	}
}

// Every category is its own authorization, and a refused one neither stops
// the others nor reaches Docker.
func TestPruneAuthorizesEachCategory(t *testing.T) {
	d := newFakeDocker()
	var seen []auth.Request
	s, log := auditedTestServer(d, denyActions{deny: map[string]bool{"volume.prune": true, "image.prune.all": true}, seen: &seen})
	stopped(d, "old", 5, 72*time.Hour, 48*time.Hour, nil)

	resp, err := s.Prune(context.Background(), &pb.PruneRequest{Categories: everyCategory})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, r := range seen {
		actions = append(actions, r.Action)
	}
	// images subsumes dangling-images, so image.prune is never asked.
	want := []string{"container.prune", "image.prune.all", "buildcache.prune", "network.prune", "volume.prune"}
	if !slices.Equal(actions, want) {
		t.Errorf("authorized %v, want %v in that order", actions, want)
	}
	for _, c := range []pb.PruneCategory{catImages, catVolumes} {
		if r := resultFor(t, resp.GetResults(), c); !strings.Contains(r.GetError(), "authorization denied") {
			t.Errorf("%v: %+v, want a denial", c, r)
		}
	}
	if r := resultFor(t, resp.GetResults(), catContainers); r.GetError() != "" || r.GetCount() != 1 {
		t.Errorf("containers should have run: %+v", r)
	}
	if len(d.pruneFilters) != 0 || len(d.volumePruneOpts) != 0 {
		t.Error("a refused category reached Docker")
	}
	if len(d.buildPruneOpts) != 1 || !d.buildPruneOpts[0].All {
		t.Errorf("build cache prune = %+v", d.buildPruneOpts)
	}

	// One audit record per category, each saying what happened.
	got := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["event"] == "prune" {
			got[rec["category"].(string)] = rec
		}
	}
	if len(got) != 5 {
		t.Fatalf("prune audit records = %v", got)
	}
	wantFields(t, got["volumes"], map[string]any{"ok": false, "trigger": "manual", "identity": "test-user"})
	wantFields(t, got["containers"], map[string]any{"ok": true, "count": 1, "freed_bytes": 5})
}

func TestPruneRejectsAnEmptyRequest(t *testing.T) {
	s := imageTestServer(newFakeDocker())
	if _, err := s.Prune(context.Background(), &pb.PruneRequest{}); statusCode(err) != codes.InvalidArgument {
		t.Errorf("no categories must be InvalidArgument, got %v", err)
	}
	if _, err := s.Prune(context.Background(), &pb.PruneRequest{Categories: []pb.PruneCategory{99}}); statusCode(err) != codes.InvalidArgument {
		t.Errorf("unknown category must be InvalidArgument, got %v", err)
	}
}

// dangling-images alone is the safe filter; with images the sweep covers it.
func TestPruneImagesCategories(t *testing.T) {
	d := newFakeDocker()
	d.pruneReport = image.PruneReport{SpaceReclaimed: 7, ImagesDeleted: []image.DeleteResponse{{Deleted: "sha256:x"}}}
	s := imageTestServer(d)
	if _, err := s.Prune(context.Background(), &pb.PruneRequest{Categories: []pb.PruneCategory{catDangling}}); err != nil {
		t.Fatal(err)
	}
	if got := filterValues(d.pruneFilters[0], "dangling"); !slices.Equal(got, []string{"true"}) {
		t.Errorf("dangling-images sent dangling=%v", got)
	}
	resp, err := s.Prune(context.Background(), &pb.PruneRequest{Categories: []pb.PruneCategory{catDangling, catImages}})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.pruneFilters) != 2 || !slices.Equal(filterValues(d.pruneFilters[1], "dangling"), []string{"false"}) {
		t.Errorf("images+dangling should be ONE sweep, got %v", d.pruneFilters)
	}
	if len(resp.GetResults()) != 1 || resp.GetResults()[0].GetFreedBytes() != 7 {
		t.Errorf("results %+v", resp.GetResults())
	}
}

// A label filter scopes what supports it and skips what cannot be scoped.
func TestPruneLabelFilters(t *testing.T) {
	d := newFakeDocker()
	stopped(d, "mine", 1, 72*time.Hour, 48*time.Hour, map[string]string{"t": "1"})
	stopped(d, "theirs", 1, 72*time.Hour, 48*time.Hour, nil)
	s := imageTestServer(d)
	resp, err := s.Prune(context.Background(), &pb.PruneRequest{Categories: everyCategory, LabelFilters: []string{"t=1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.RemovedContainers(); !slices.Equal(got, []string{"mine"}) {
		t.Errorf("removed %v, want only the labelled container", got)
	}
	if r := resultFor(t, resp.GetResults(), catBuild); !r.GetSkipped() || len(d.buildPruneOpts) != 0 {
		t.Errorf("build cache under a label filter must be skipped: %+v", r)
	}
	if got := filterValues(d.pruneFilters[0], "label"); !slices.Equal(got, []string{"t=1"}) {
		t.Errorf("image prune label filter = %v", got)
	}
	if got := filterValues(d.volumePruneOpts[0].Filters, "label"); !slices.Equal(got, []string{"t=1"}) {
		t.Errorf("volume prune label filter = %v", got)
	}
}

// A daemon that ignored the label filter must not turn a scoped prune into a
// node-wide one: containers and networks are checked again in the agent.
func TestLabelFilterHoldsWhenTheDaemonIgnoresIt(t *testing.T) {
	d := newFakeDocker()
	d.ignoreLabels = true
	stopped(d, "mine", 1, 72*time.Hour, 48*time.Hour, map[string]string{"t": "1"})
	stopped(d, "theirs", 1, 72*time.Hour, 48*time.Hour, nil)
	d.networks = []network.Summary{
		{Network: network.Network{ID: "m", Name: "mine", Scope: "local", Driver: "bridge", Labels: map[string]string{"t": "1"}}},
		{Network: network.Network{ID: "o", Name: "theirs", Scope: "local", Driver: "bridge"}},
	}
	s := imageTestServer(d)
	if _, err := s.Prune(context.Background(), &pb.PruneRequest{Categories: []pb.PruneCategory{catContainers, catNetworks}, LabelFilters: []string{"t=1"}}); err != nil {
		t.Fatal(err)
	}
	if got := d.RemovedContainers(); !slices.Equal(got, []string{"mine"}) {
		t.Errorf("containers removed %v", got)
	}
	if !slices.Equal(d.removedNetworks, []string{"m"}) {
		t.Errorf("networks removed %v", d.removedNetworks)
	}
}

// The measurement is the same selection a prune makes, category by category.
func TestMeasureReclaim(t *testing.T) {
	d := newFakeDocker()
	stopped(d, "old", 100, 72*time.Hour, 48*time.Hour, nil)
	stopped(d, "young", 7, time.Hour, time.Minute, nil)
	d.imageDiskUsage = du(300,
		img("sha256:used", 100, 60, 1, "app:1.2"),
		img("sha256:old", 200, 150, 0, "app:1.1"),
		img("sha256:dang", 50, 20, 0),
	)
	d.buildCacheDU = client.BuildCacheDiskUsage{TotalCount: 5, ActiveCount: 2, Reclaimable: 4096,
		Items: []build.CacheRecord{{ID: "x"}}}
	d.networks = []network.Summary{
		{Network: network.Network{ID: "j", Name: "junk", Scope: "local", Driver: "bridge"}},
		{Network: network.Network{ID: "o", Name: "ov", Scope: "swarm", Driver: "overlay"}},
	}
	d.volumes = []volume.Volume{
		{Name: "free", UsageData: &volume.UsageData{Size: 1000, RefCount: 0}},
		{Name: "busy", UsageData: &volume.UsageData{Size: 5000, RefCount: 1}},
		{Name: "anon", UsageData: &volume.UsageData{Size: 10, RefCount: 0}},
	}
	s := imageTestServer(d)
	s.sizeCache.Refresh(context.Background())

	cats, err := s.measureReclaim(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	by := map[pb.PruneCategory]*pb.ReclaimCategory{}
	var order []pb.PruneCategory
	for _, c := range cats {
		by[c.GetCategory()] = c
		order = append(order, c.GetCategory())
	}
	if !slices.Equal(order, everyCategory) {
		t.Errorf("order = %v", order)
	}
	check := func(c pb.PruneCategory, count, bytes int64, known bool) {
		t.Helper()
		g := by[c]
		if g.GetCount() != count || g.GetBytes() != bytes || g.GetBytesKnown() != known {
			t.Errorf("%v = count %d bytes %d known %v, want %d/%d/%v", c, g.GetCount(), g.GetBytes(), g.GetBytesKnown(), count, bytes, known)
		}
	}
	check(catContainers, 1, 100, true)
	if by[catContainers].GetHeldBack() != 1 {
		t.Errorf("held back = %d, want the container that stopped a minute ago", by[catContainers].GetHeldBack())
	}
	// 300 on disk, 40 held exclusively by the running image: 260 reclaimable,
	// the same figure ListImages reports. Dangling: the untagged image's 30.
	check(catImages, 2, 260, true)
	check(catDangling, 1, 30, true)
	check(catBuild, 3, 4096, true)
	check(catNetworks, 1, 0, false)
	check(catVolumes, 2, 1010, true)
}

// measure_now is rate-limited; inside the limit the cached figures come back
// marked throttled.
func TestReclaimStatsMeasureNowThrottles(t *testing.T) {
	d := newFakeDocker()
	s := imageTestServer(d)
	first, err := s.ReclaimStats(context.Background(), &pb.ReclaimStatsRequest{MeasureNow: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.GetMeasureThrottled() || first.GetMeasuredAt() == "" {
		t.Errorf("first measure_now: %+v", first)
	}
	second, _ := s.ReclaimStats(context.Background(), &pb.ReclaimStatsRequest{MeasureNow: true})
	if !second.GetMeasureThrottled() {
		t.Error("a second measure_now inside the limit must be throttled")
	}
	if _, err := newDenied(d).ReclaimStats(context.Background(), &pb.ReclaimStatsRequest{}); statusCode(err) != codes.PermissionDenied {
		t.Errorf("reclaim.read must be authorized, got %v", err)
	}
}

func newDenied(d *fakeDocker) *Server {
	s, _ := newTestServer(d, denyAuth{}, Options{})
	return s
}

// A failing category keeps its previous figure and reports the error.
func TestReclaimKeepsPreviousFigureOnError(t *testing.T) {
	d := newFakeDocker()
	d.imageDiskUsage = du(100, img("sha256:a", 100, 0, 0, "x:1"))
	s := imageTestServer(d)
	s.reclaim.Measure(context.Background(), false)
	d.diskUsageErr = errors.New("daemon busy")
	s.reclaim.Measure(context.Background(), false)
	cats, _, errMsg, _ := s.reclaim.Snapshot()
	if !strings.Contains(errMsg, "daemon busy") {
		t.Errorf("error = %q", errMsg)
	}
	for _, c := range cats {
		if c.GetCategory() == catImages && c.GetBytes() != 100 {
			t.Errorf("images figure lost on error: %+v", c)
		}
	}
}

// The scheduler: labels in, a run at the scheduled time, audited as the
// scheduler, and the result kept for ReclaimStats.
func TestScheduledRunIsAuditedAndKept(t *testing.T) {
	d := newFakeDocker()
	stopped(d, "old", 50, 72*time.Hour, 48*time.Hour, nil)
	var seen []auth.Request
	s, log := auditedTestServer(d, denyActions{seen: &seen})
	s.scheduler.load(map[string]string{
		prunesched.LabelEnabled:    "true",
		prunesched.LabelCategories: "containers,networks",
		prunesched.LabelStart:      "03:00",
		prunesched.LabelTimes:      "2",
	})
	sched, err, _ := s.scheduler.schedule()
	if err != nil || !sched.Enabled {
		t.Fatalf("schedule = %+v, %v", sched, err)
	}
	run := s.scheduler.runOnce(context.Background(), sched)
	if run.GetError() != "" || len(run.GetResults()) != 2 {
		t.Fatalf("run = %+v", run)
	}
	for _, r := range seen {
		if r.Identity != schedulerIdentity {
			t.Errorf("scheduled run authorized as %q", r.Identity)
		}
	}
	rec := findAuditEvent(t, log.String(), "prune")
	wantFields(t, rec, map[string]any{"trigger": "schedule", "identity": "scheduler", "category": "containers", "count": 1})

	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	st, last := s.scheduler.status(now)
	if !st.GetEnabled() || st.GetNextRun() != "2026-11-06T15:00:00Z" || st.GetTimesPerDay() != 2 {
		t.Errorf("status = %+v", st)
	}
	if last.GetStartedAt() == "" || len(last.GetResults()) != 2 {
		t.Errorf("last run = %+v", last)
	}
}

func TestInvalidScheduleLabelsDisable(t *testing.T) {
	s := imageTestServer(newFakeDocker())
	s.scheduler.load(map[string]string{prunesched.LabelEnabled: "true", prunesched.LabelTimes: "lots"})
	st, _ := s.scheduler.status(time.Now())
	if st.GetEnabled() || !strings.Contains(st.GetError(), prunesched.LabelTimes) || st.GetNextRun() != "" {
		t.Errorf("invalid labels must disable the schedule and say why: %+v", st)
	}
	// run returns at once for a disabled schedule instead of spinning.
	done := make(chan struct{})
	go func() { s.scheduler.run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("run kept going on an invalid schedule")
	}
}

func TestSchedulerWaitUntilUsesTheWallClock(t *testing.T) {
	s := imageTestServer(newFakeDocker())
	base := time.Now()
	calls := 0
	s.scheduler.maxSleep = time.Millisecond
	s.scheduler.now = func() time.Time { calls++; return base.Add(time.Duration(calls) * time.Hour) }
	if !s.scheduler.waitUntil(context.Background(), base.Add(90*time.Minute)) {
		t.Fatal("waitUntil gave up")
	}
	// It woke after one short sleep because the WALL clock passed the target,
	// not because the sleep was as long as the gap.
	if calls != 2 {
		t.Errorf("clock read %d times, want 2", calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.scheduler.now = time.Now
	if s.scheduler.waitUntil(ctx, time.Now().Add(time.Hour)) {
		t.Error("a cancelled context must stop the wait")
	}
}
