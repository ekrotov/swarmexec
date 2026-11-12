// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"swarmexec/internal/pb"
	"swarmexec/internal/prunesched"
)

const (
	pcContainers = pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS
	pcImages     = pb.PruneCategory_PRUNE_CATEGORY_IMAGES
	pcDangling   = pb.PruneCategory_PRUNE_CATEGORY_DANGLING_IMAGES
	pcBuild      = pb.PruneCategory_PRUNE_CATEGORY_BUILD_CACHE
	pcNetworks   = pb.PruneCategory_PRUNE_CATEGORY_NETWORKS
	pcVolumes    = pb.PruneCategory_PRUNE_CATEGORY_VOLUMES
)

// The dialog opens on the safe sweeps. Volumes (data) and every unused image
// (removes what a scaled-to-zero service needs) are never ticked for you.
func TestPruneDialogDefaults(t *testing.T) {
	c := defaultPruneChoice()
	if c.ticked[pcVolumes] {
		t.Error("volumes must start unticked")
	}
	if c.ticked[pcImages] {
		t.Error("every unused image must start unticked")
	}
	want := []pb.PruneCategory{pcContainers, pcDangling, pcBuild, pcNetworks}
	if got := c.categories(); !slices.Equal(got, want) {
		t.Errorf("default categories = %v, want %v", got, want)
	}
	if c.request().GetContainersUntil() != "" {
		t.Error("the default age filter is the agent's (24h), sent as empty")
	}
}

// "all" is everything except volumes, whatever state volumes are in.
func TestPruneSelectAllNeverTouchesVolumes(t *testing.T) {
	c := defaultPruneChoice()
	c.selectAll()
	if c.ticked[pcVolumes] {
		t.Fatal("all ticked volumes")
	}
	for _, cat := range []pb.PruneCategory{pcContainers, pcImages, pcBuild, pcNetworks} {
		if !c.ticked[cat] {
			t.Errorf("all left %v unticked", cat)
		}
	}
	if c.ticked[pcDangling] {
		t.Error("every unused image already covers untagged; both must not be ticked")
	}
	// Ticked volumes stay ticked through all-off and all-on: all does not own them.
	c.toggle(pcVolumes)
	c.selectAll() // everything was on → off
	if !c.ticked[pcVolumes] || c.ticked[pcContainers] {
		t.Errorf("second a should clear all but volumes: %v", c.ticked)
	}
}

func TestPruneImageChoicesExcludeEachOther(t *testing.T) {
	c := defaultPruneChoice()
	c.toggle(pcImages)
	if !c.ticked[pcImages] || c.ticked[pcDangling] {
		t.Errorf("ticking every unused image must untick untagged: %v", c.ticked)
	}
	c.toggle(pcDangling)
	if c.ticked[pcImages] || !c.ticked[pcDangling] {
		t.Errorf("and the reverse: %v", c.ticked)
	}
}

func statsFixture() *pb.ReclaimStatsResponse {
	return &pb.ReclaimStatsResponse{
		MeasuredAt:      time.Now().Add(-23 * time.Minute).UTC().Format(time.RFC3339),
		ContainersUntil: "24h0m0s",
		Categories: []*pb.ReclaimCategory{
			{Category: pcContainers, Count: 3, Bytes: 3 << 20, BytesKnown: true, HeldBack: 2},
			{Category: pcImages, Count: 5, Bytes: 2 << 30, BytesKnown: true},
			{Category: pcDangling, Count: 2, Bytes: 300 << 20, BytesKnown: true},
			{Category: pcBuild, Count: 9, Bytes: 1 << 30, BytesKnown: true},
			{Category: pcNetworks, Count: 2},
			{Category: pcVolumes, Count: 4, Bytes: 800 << 20, BytesKnown: true},
		},
	}
}

// The confirm is the last thing read before an irreversible act.
func TestPruneConfirmBody(t *testing.T) {
	stats := statsFixture()
	c := defaultPruneChoice()
	body := pruneConfirmBody("node-1", c, stats)
	for _, want := range []string{"node-1", "stopped containers (stopped > 24h)", "untagged images", "build cache", "unused local networks", "Frees about"} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "VOLUMES") || strings.Contains(body, "unused volumes") {
		t.Errorf("volumes mentioned though not ticked:\n%s", body)
	}
	// 3 MiB + 300 MiB + 1 GiB; networks have no size.
	if want := diskSize(3<<20 + 300<<20 + 1<<30); !strings.Contains(body, want) {
		t.Errorf("expected savings %s missing:\n%s", want, body)
	}

	c.toggle(pcVolumes)
	c.toggle(pcImages)
	c.until = "2h"
	body = pruneConfirmBody("node-1", c, stats)
	for _, want := range []string{"VOLUMES", "DELETED with its data", "named volumes included", "cannot be undone", "pull again", "stopped > 2h"} {
		if !strings.Contains(body, want) {
			t.Errorf("with volumes + every image ticked, confirm lacks %q:\n%s", want, body)
		}
	}
}

// An agent that predates the RPC says so instead of showing zeros.
func TestReclaimSectionOldAgent(t *testing.T) {
	got := reclaimSection(nil, errAgentTooOld, false, time.Now())
	if !strings.Contains(got, "agent too old for prune stats") {
		t.Errorf("old agent:\n%s", got)
	}
	if strings.Contains(got, "0 B") {
		t.Errorf("old agent must not show zeros:\n%s", got)
	}
}

func TestReclaimSectionShowsFiguresAgeAndSchedule(t *testing.T) {
	stats := statsFixture()
	stats.Schedule = &pb.PruneSchedule{Enabled: true, Days: []string{"mon", "tue"}, Start: "03:00", TimesPerDay: 2, Tz: "UTC",
		Categories: []pb.PruneCategory{pcContainers, pcImages}, NextRun: "2026-11-09T03:00:00Z"}
	stats.LastRun = &pb.PruneRun{StartedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		Results: []*pb.PruneCategoryResult{{Category: pcContainers, Count: 4, FreedBytes: 1 << 20}, {Category: pcImages, Error: "denied"}},
		Error:   "every unused image: denied"}
	got := reclaimSection(stats, nil, false, time.Now())
	for _, want := range []string{"stopped > 24h", "(2 more kept, stopped recently)", "measured 23 min ago",
		"every unused image", "unused local networks", "mon,tue, 2× a day from 03:00 UTC", "next run Mon 2026-11-09 03:00",
		"last run 2h ago", "freed 1MiB", "failed", "every unused image: denied"} {
		if !strings.Contains(got, want) {
			t.Errorf("section lacks %q:\n%s", want, got)
		}
	}
	stats.Schedule = &pb.PruneSchedule{Error: "swarmexec.prune.schedule.times=\"x\": bad"}
	if got := reclaimSection(stats, nil, false, time.Now()); !strings.Contains(got, "invalid labels, schedule off") {
		t.Errorf("invalid schedule not shown:\n%s", got)
	}
}

func TestPruneResultText(t *testing.T) {
	got := pruneResultText("n1", &pb.PruneResponse{Results: []*pb.PruneCategoryResult{
		{Category: pcContainers, Count: 2, FreedBytes: 2048},
		{Category: pcNetworks, Count: 1},
		{Category: pcBuild, Skipped: true},
		{Category: pcVolumes, Error: "authorization denied: no"},
	}})
	for _, want := range []string{"freed 2KiB", "stopped containers: 2 removed", "unused local networks: 1 removed", "build cache: skipped", "authorization denied"} {
		if !strings.Contains(got, want) {
			t.Errorf("result lacks %q:\n%s", want, got)
		}
	}
}

// fakeAgentSvc is the manager API slice the schedule editor uses.
type fakeAgentSvc struct {
	svcs    []swarm.Service
	updated []client.ServiceUpdateOptions
}

func (f *fakeAgentSvc) ServiceList(_ context.Context, o client.ServiceListOptions) (client.ServiceListResult, error) {
	var out []swarm.Service
	for _, s := range f.svcs {
		if labels := o.Filters["label"]; len(labels) > 0 && s.Spec.Labels[agentRoleLabel] != agentRoleValue {
			continue
		}
		out = append(out, s)
	}
	return client.ServiceListResult{Items: out}, nil
}

func (f *fakeAgentSvc) ServiceUpdate(_ context.Context, _ string, o client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
	f.updated = append(f.updated, o)
	return client.ServiceUpdateResult{}, nil
}

func agentSvc(labels map[string]string) swarm.Service {
	return swarm.Service{ID: "svc1", Meta: swarm.Meta{Version: swarm.Version{Index: 42}}, Spec: swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "swarmexec_agent", Labels: map[string]string{agentRoleLabel: agentRoleValue}},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image: "logleio/swarmexec-agent:latest@sha256:abc", Args: []string{"-port=9443"}, Labels: labels,
		}},
	}}
}

// Saving the schedule changes the container labels and nothing else: other
// labels survive, the image digest is untouched, and the version guards
// against a concurrent edit.
func TestSaveScheduleRoundTripKeepsEverythingElse(t *testing.T) {
	f := &fakeAgentSvc{svcs: []swarm.Service{agentSvc(map[string]string{"team": "ops", prunesched.LabelTimes: "9"})}}
	svc, err := findAgentService(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := agentSchedule(svc)
	if err != nil {
		t.Fatal(err)
	}
	cur.Enabled = true
	cur.Times = 3
	cur.Days, _ = prunesched.ParseDays("mon-fri")
	setCat(&cur, prunesched.Volumes, true)
	if err := saveAgentSchedule(context.Background(), f, svc, cur); err != nil {
		t.Fatal(err)
	}
	if len(f.updated) != 1 {
		t.Fatalf("updates = %d", len(f.updated))
	}
	up := f.updated[0]
	if up.Version.Index != 42 {
		t.Errorf("version = %d, want the inspected one", up.Version.Index)
	}
	cs := up.Spec.TaskTemplate.ContainerSpec
	if cs.Labels["team"] != "ops" || cs.Image != "logleio/swarmexec-agent:latest@sha256:abc" || cs.Args[0] != "-port=9443" {
		t.Errorf("save changed more than the schedule: %+v", cs)
	}
	if up.QueryRegistry {
		t.Error("a schedule save must not re-resolve the image")
	}
	back, err := prunesched.Parse(cs.Labels)
	if err != nil || !back.Enabled || back.Times != 3 || prunesched.FormatDays(back.Days) != "mon,tue,wed,thu,fri" ||
		!slices.Contains(back.Categories, prunesched.Volumes) {
		t.Errorf("round trip = %+v, %v", back, err)
	}
	if svc.Spec.TaskTemplate.ContainerSpec.Labels[prunesched.LabelTimes] != "9" {
		t.Error("the inspected service must not be modified in place")
	}
}

func TestApplyScheduleFlagsOnlyChangesWhatWasGiven(t *testing.T) {
	cur := prunesched.Default()
	cur.TZ = "Europe/Berlin"
	cur.LabelFilters = []string{"a=b"}
	given := map[string]bool{"times": true, "start": true, "label-filter": true}
	f := &pruneScheduleSetFlags{times: 2, start: "06:30", labelFilter: "-"}
	s, err := applyScheduleFlags(cur, f, func(n string) bool { return given[n] })
	if err != nil {
		t.Fatal(err)
	}
	if !s.Enabled || s.Times != 2 || s.Start != 390 || s.TZ != "Europe/Berlin" || len(s.LabelFilters) != 0 {
		t.Errorf("schedule = %+v", s)
	}
	f = &pruneScheduleSetFlags{disable: true}
	if s, _ := applyScheduleFlags(cur, f, func(string) bool { return false }); s.Enabled {
		t.Error("--disable must turn it off")
	}
	f = &pruneScheduleSetFlags{categories: "containers,secrets"}
	if _, err := applyScheduleFlags(cur, f, func(n string) bool { return n == "categories" }); err == nil {
		t.Error("an unknown category must be refused")
	}
}

func TestScheduleEditorImagesAndPreview(t *testing.T) {
	s := prunesched.Default()
	if imagesMode(s) != "every unused" {
		t.Fatalf("default images mode = %s", imagesMode(s))
	}
	cycleImages(&s)
	if imagesMode(s) != "off" {
		t.Errorf("after every unused comes off, got %s", imagesMode(s))
	}
	cycleImages(&s)
	if imagesMode(s) != "untagged only" || !slices.Contains(s.Categories, prunesched.DanglingImages) {
		t.Errorf("then untagged: %s %v", imagesMode(s), s.Categories)
	}
	setCat(&s, prunesched.Containers, false)
	setCat(&s, prunesched.Containers, true)
	if s.Categories[0] != prunesched.Containers {
		t.Errorf("categories keep their canonical order: %v", s.Categories)
	}
	// The volumes row is red and says it is an option.
	_, v, danger := schedRowText(s, schedRow{kind: rowCat, cat: prunesched.Volumes})
	if !danger || !strings.Contains(v, "off by default") || strings.HasPrefix(v, "[x]") {
		t.Errorf("volumes row = %q danger=%v", v, danger)
	}

	s.Enabled, s.Times, s.Start = true, 3, 0
	p := schedPreview(s, time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
	if !strings.Contains(p, "00:00, 08:00, 16:00 UTC") || !strings.Contains(p, "next: Fri 16:00 · Sat 00:00 · Sat 08:00") {
		t.Errorf("preview:\n%s", p)
	}
	s.Times = 0
	if p := schedPreview(s, time.Now()); !strings.Contains(p, "cannot save") {
		t.Errorf("an invalid schedule must say it cannot be saved:\n%s", p)
	}
}

// init --force rebuilds the spec from flags; the schedule, which no init flag
// carries, has to survive it.
func TestInitForceCarriesTheSchedule(t *testing.T) {
	f := &initFlags{image: "reg/agent:2", serviceName: "swarmexec_agent", port: 9443}
	spec := agentServiceSpec(f, "sec")
	existing := agentSvc(map[string]string{prunesched.LabelEnabled: "true", prunesched.LabelStart: "03:00", "handmade": "x"})
	if !carryPruneSchedule(&spec, &existing) {
		t.Fatal("schedule labels not carried")
	}
	got := spec.TaskTemplate.ContainerSpec.Labels
	if got[prunesched.LabelEnabled] != "true" || got[prunesched.LabelStart] != "03:00" {
		t.Errorf("labels = %v", got)
	}
	if _, ok := got["handmade"]; ok {
		t.Error("init writes its own spec; only the schedule is carried")
	}
	fresh := agentServiceSpec(f, "sec")
	if carryPruneSchedule(&fresh, nil) || carryPruneSchedule(&fresh, &swarm.Service{Spec: agentSvc(nil).Spec}) {
		t.Error("nothing to carry must report false")
	}
	if fresh.TaskTemplate.ContainerSpec.Labels != nil {
		t.Error("a spec with no schedule must stay label-free")
	}
}

func TestKeymapOldPruneNameStillBinds(t *testing.T) {
	km, warns := loadKeybinds(writeKeys(t, "node_prune_images: Z\n"))
	if len(warns) != 0 || km.NodePrune != 'Z' {
		t.Errorf("node_prune_images alias: prune=%q warns=%v", km.NodePrune, warns)
	}
	if d := defaultKeybinds(); d.NodePrune != 'P' || d.NodePruneSched != 'S' {
		t.Errorf("defaults = %q %q", d.NodePrune, d.NodePruneSched)
	}
}
