// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// fakeRotateAPI is an in-memory manager: secrets, services and their tasks,
// with swarm's own rules where a rotation depends on them — a referenced
// secret cannot be removed, a name is unique, an update must carry the
// version it read.
type fakeRotateAPI struct {
	mu       sync.Mutex
	secrets  map[string]*swarm.Secret // by ID
	data     map[string][]byte        // by secret ID
	services map[string]*swarm.Service
	tasks    map[string][]swarm.Task // by service ID
	nextID   int
	clock    time.Time

	// failOnce makes the first call of an operation on a name fail
	// ("create:x", "remove:x", "update:svc").
	failOnce map[string]bool
	// conflicts is how many "update out of sequence" a service answers with
	// before an update goes through.
	conflicts map[string]int
	// stuck services take the update but pause it, as swarm does when the
	// new tasks fail.
	stuck map[string]bool

	mutations []string // "create x", "remove x", "update svc"
}

func newFakeRotate() *fakeRotateAPI {
	return &fakeRotateAPI{
		secrets: map[string]*swarm.Secret{}, data: map[string][]byte{},
		services: map[string]*swarm.Service{}, tasks: map[string][]swarm.Task{},
		clock:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		failOnce: map[string]bool{}, conflicts: map[string]int{}, stuck: map[string]bool{},
	}
}

func (f *fakeRotateAPI) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s%03d", prefix, f.nextID)
}

func (f *fakeRotateAPI) injected(op string) error {
	if f.failOnce[op] {
		delete(f.failOnce, op)
		return fmt.Errorf("injected failure: %s", op)
	}
	return nil
}

func (f *fakeRotateAPI) addSecret(name string, data string, labels map[string]string) *swarm.Secret {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createLocked(name, []byte(data), labels)
}

func (f *fakeRotateAPI) createLocked(name string, data []byte, labels map[string]string) *swarm.Secret {
	f.clock = f.clock.Add(time.Second)
	s := &swarm.Secret{ID: f.id("sec"), Meta: swarm.Meta{CreatedAt: f.clock},
		Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: name, Labels: maps.Clone(labels)}}}
	f.secrets[s.ID] = s
	f.data[s.ID] = bytes.Clone(data)
	return s
}

// addService creates a service with the given refs and replicas (-1: global,
// one task).
func (f *fakeRotateAPI) addService(name string, replicas int, refs ...*swarm.SecretReference) *swarm.Service {
	f.mu.Lock()
	defer f.mu.Unlock()
	spec := swarm.ServiceSpec{Annotations: swarm.Annotations{Name: name},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "busybox", Secrets: refs}}}
	if replicas < 0 {
		spec.Mode.Global = &swarm.GlobalService{}
	} else {
		r := uint64(replicas)
		spec.Mode.Replicated = &swarm.ReplicatedService{Replicas: &r}
	}
	s := &swarm.Service{ID: f.id("svc"), Spec: spec, Meta: swarm.Meta{Version: swarm.Version{Index: 1}}}
	f.services[s.ID] = s
	f.tasks[s.ID] = f.tasksFor(s)
	return s
}

func (f *fakeRotateAPI) tasksFor(s *swarm.Service) []swarm.Task {
	n := 1
	if r := s.Spec.Mode.Replicated; r != nil {
		n = int(*r.Replicas)
	}
	var out []swarm.Task
	for i := 0; i < n; i++ {
		out = append(out, swarm.Task{ID: f.id("task"), ServiceID: s.ID, Spec: cloneTaskSpec(s.Spec.TaskTemplate),
			DesiredState: swarm.TaskStateRunning, Status: swarm.TaskStatus{State: swarm.TaskStateRunning}})
	}
	return out
}

func cloneTaskSpec(t swarm.TaskSpec) swarm.TaskSpec {
	if t.ContainerSpec != nil {
		cs := *t.ContainerSpec
		cs.Secrets = nil
		for _, r := range t.ContainerSpec.Secrets {
			c := *r
			cs.Secrets = append(cs.Secrets, &c)
		}
		t.ContainerSpec = &cs
	}
	return t
}

func (f *fakeRotateAPI) byName(name string) *swarm.Secret {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.secrets {
		if s.Spec.Name == name {
			return s
		}
	}
	return nil
}

func (f *fakeRotateAPI) service(name string) swarm.Service {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.services {
		if s.Spec.Name == name {
			return *s
		}
	}
	return swarm.Service{}
}

func (f *fakeRotateAPI) SecretList(context.Context, client.SecretListOptions) (client.SecretListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out client.SecretListResult
	for _, s := range f.secrets {
		c := *s
		c.Spec.Labels = maps.Clone(s.Spec.Labels)
		out.Items = append(out.Items, c)
	}
	return out, nil
}

func (f *fakeRotateAPI) SecretCreate(_ context.Context, o client.SecretCreateOptions) (client.SecretCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.injected("create:" + o.Spec.Name); err != nil {
		return client.SecretCreateResult{}, err
	}
	for _, s := range f.secrets {
		if s.Spec.Name == o.Spec.Name {
			return client.SecretCreateResult{}, fmt.Errorf("secret %s already exists", o.Spec.Name)
		}
	}
	s := f.createLocked(o.Spec.Name, o.Spec.Data, o.Spec.Labels)
	f.mutations = append(f.mutations, "create "+o.Spec.Name)
	return client.SecretCreateResult{ID: s.ID}, nil
}

func (f *fakeRotateAPI) SecretRemove(_ context.Context, id string, _ client.SecretRemoveOptions) (client.SecretRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.secrets[id]
	if !ok {
		return client.SecretRemoveResult{}, fmt.Errorf("no such secret %s", id)
	}
	if err := f.injected("remove:" + s.Spec.Name); err != nil {
		return client.SecretRemoveResult{}, err
	}
	for _, svc := range f.services {
		for _, r := range svc.Spec.TaskTemplate.ContainerSpec.Secrets {
			if r.SecretID == id {
				return client.SecretRemoveResult{}, fmt.Errorf("secret %s is in use by service %s", s.Spec.Name, svc.Spec.Name)
			}
		}
	}
	delete(f.secrets, id)
	f.mutations = append(f.mutations, "remove "+s.Spec.Name)
	return client.SecretRemoveResult{}, nil
}

func (f *fakeRotateAPI) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out client.ServiceListResult
	for _, s := range f.services {
		out.Items = append(out.Items, *s)
	}
	return out, nil
}

func (f *fakeRotateAPI) ServiceInspect(_ context.Context, id string, _ client.ServiceInspectOptions) (client.ServiceInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.services[id]
	if !ok {
		return client.ServiceInspectResult{}, fmt.Errorf("no such service %s", id)
	}
	return client.ServiceInspectResult{Service: *s}, nil
}

func (f *fakeRotateAPI) ServiceUpdate(_ context.Context, id string, o client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.services[id]
	if !ok {
		return client.ServiceUpdateResult{}, fmt.Errorf("no such service %s", id)
	}
	if err := f.injected("update:" + s.Spec.Name); err != nil {
		return client.ServiceUpdateResult{}, err
	}
	if f.conflicts[s.Spec.Name] > 0 {
		f.conflicts[s.Spec.Name]--
		s.Version.Index++ // somebody else got there first
		return client.ServiceUpdateResult{}, errors.New("Error response from daemon: rpc error: code = Unknown desc = update out of sequence")
	}
	if o.Version != s.Version {
		return client.ServiceUpdateResult{}, errors.New("Error response from daemon: rpc error: code = Unknown desc = update out of sequence")
	}
	for _, r := range o.Spec.TaskTemplate.ContainerSpec.Secrets {
		if sec, ok := f.secrets[r.SecretID]; !ok || sec.Spec.Name != r.SecretName {
			return client.ServiceUpdateResult{}, fmt.Errorf("secret not found: %s", r.SecretName)
		}
	}
	s.PreviousSpec = &swarm.ServiceSpec{}
	*s.PreviousSpec = s.Spec
	s.Spec = o.Spec
	s.Version.Index++
	f.mutations = append(f.mutations, "update "+s.Spec.Name)
	if f.stuck[s.Spec.Name] {
		s.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStatePaused, Message: "update paused due to failure or early termination of task x"}
		return client.ServiceUpdateResult{}, nil
	}
	s.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStateCompleted}
	f.tasks[id] = f.tasksFor(s)
	return client.ServiceUpdateResult{}, nil
}

func (f *fakeRotateAPI) TaskList(_ context.Context, o client.TaskListOptions) (client.TaskListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out client.TaskListResult
	for id := range o.Filters["service"] {
		out.Items = append(out.Items, f.tasks[id]...)
	}
	return out, nil
}

// fixture: secret "db" (stack-labelled) used by "web" (custom target, uid,
// gid, mode) and by "worker", which also mounts a second secret "api".
type rotateFixture struct {
	f        *fakeRotateAPI
	labels   map[string]string
	webRef   swarm.SecretReference
	apiRefID string
}

func newRotateFixture(t *testing.T) *rotateFixture {
	t.Helper()
	f := newFakeRotate()
	labels := map[string]string{stackLabelKey: "shop", "team": "core"}
	db := f.addSecret("db", "old", labels)
	api := f.addSecret("api", "token", nil)
	web := swarm.SecretReference{SecretID: db.ID, SecretName: "db",
		File: &swarm.SecretReferenceFileTarget{Name: "db_password", UID: "1000", GID: "2000", Mode: 0o400}}
	wr := web
	f.addService("web", 2, &wr)
	f.addService("worker", 1,
		&swarm.SecretReference{SecretID: api.ID, SecretName: "api", File: &swarm.SecretReferenceFileTarget{Name: "api", UID: "0", GID: "0", Mode: 0o444}},
		&swarm.SecretReference{SecretID: db.ID, SecretName: "db", File: &swarm.SecretReferenceFileTarget{Name: "db", UID: "0", GID: "0", Mode: 0o444}},
	)
	return &rotateFixture{f: f, labels: labels, webRef: web, apiRefID: api.ID}
}

func fastRotate(events *[]rotateEvent) rotateOptions {
	return rotateOptions{Timeout: 200 * time.Millisecond, Poll: time.Millisecond,
		Progress: func(e rotateEvent) {
			if events != nil {
				*events = append(*events, e)
			}
		}}
}

// checkRotated asserts the end state every successful rotation must reach.
func (x *rotateFixture) checkRotated(t *testing.T, want string) {
	t.Helper()
	f := x.f
	db := f.byName("db")
	if db == nil {
		t.Fatal("db is gone")
	}
	if got := string(f.data[db.ID]); got != want {
		t.Errorf("db content = %q, want %q", got, want)
	}
	if !maps.Equal(db.Spec.Labels, x.labels) {
		t.Errorf("db labels = %v, want %v", db.Spec.Labels, x.labels)
	}
	if tmp := f.byName("db_rotating"); tmp != nil {
		t.Errorf("db_rotating still exists")
	}
	web := f.service("web").Spec.TaskTemplate.ContainerSpec.Secrets
	if len(web) != 1 || web[0].SecretID != db.ID || web[0].SecretName != "db" || *web[0].File != *x.webRef.File {
		t.Errorf("web ref = %+v / %+v, want db %s with %+v", *web[0], *web[0].File, db.ID, *x.webRef.File)
	}
	worker := f.service("worker").Spec.TaskTemplate.ContainerSpec.Secrets
	if len(worker) != 2 || worker[0].SecretID != x.apiRefID || worker[0].SecretName != "api" || worker[0].File.Name != "api" {
		t.Errorf("worker's other secret changed: %+v", *worker[0])
	}
	if worker[1].SecretID != db.ID || worker[1].File.Name != "db" || worker[1].File.Mode != 0o444 {
		t.Errorf("worker db ref = %+v", *worker[1])
	}
}

func TestRotateSecret_HappyPath(t *testing.T) {
	x := newRotateFixture(t)
	var events []rotateEvent
	if err := rotateSecret(context.Background(), x.f, "db", []byte("new\n"), fastRotate(&events)); err != nil {
		t.Fatal(err)
	}
	x.checkRotated(t, "new\n")
	want := []string{"create db_rotating", "update web", "update worker", "remove db", "create db", "update web", "update worker", "remove db_rotating"}
	if strings.Join(x.f.mutations, ",") != strings.Join(want, ",") {
		t.Errorf("mutations:\n got %v\nwant %v", x.f.mutations, want)
	}
	// While parked, the stand-in carried the labels plus the marker — checked
	// through the order of steps: every step reported, 1 to 6.
	seen := map[int]bool{}
	for _, e := range events {
		seen[e.Step] = true
		if strings.Contains(e.Msg, "new\n") {
			t.Errorf("progress leaked the value: %q", e.Msg)
		}
	}
	for s := 1; s <= rotateSteps; s++ {
		if !seen[s] {
			t.Errorf("no progress for step %d: %v", s, events)
		}
	}
}

func TestRotateSecret_TempCarriesLabelsAndMarker(t *testing.T) {
	x := newRotateFixture(t)
	x.f.failOnce["remove:db"] = true // stop after step 2
	err := rotateSecret(context.Background(), x.f, "db", []byte("new"), fastRotate(nil))
	var re *rotateError
	if !errors.As(err, &re) || re.Step != 3 || !re.Resumable {
		t.Fatalf("err = %v, want a resumable step-3 failure", err)
	}
	tmp := x.f.byName("db_rotating")
	want := maps.Clone(x.labels)
	want[rotateMarkerLabel] = "db"
	if tmp == nil || !maps.Equal(tmp.Spec.Labels, want) {
		t.Fatalf("db_rotating labels = %v, want %v", tmp, want)
	}
	if string(x.f.data[tmp.ID]) != "new" {
		t.Errorf("db_rotating content = %q", x.f.data[tmp.ID])
	}
	if h := rotateResumeHint(err, "re-run"); !strings.Contains(h, "re-run to resume") {
		t.Errorf("hint = %q", h)
	}
}

// Every way a run can stop, followed by a clean re-run: the second run must
// finish the job from wherever the first one left the cluster.
func TestRotateSecret_ResumeFromEveryFailurePoint(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(f *fakeRotateAPI)
		step      int
		resumable bool
		startAt   int    // where the re-run says it resumes; 0: a fresh start
		final     string // content db ends with
	}{
		{"create temp", func(f *fakeRotateAPI) { f.failOnce["create:db_rotating"] = true }, 1, false, 0, "second"},
		{"step 2, second service", func(f *fakeRotateAPI) { f.failOnce["update:worker"] = true }, 2, true, 2, "second"},
		{"step 2, update paused", func(f *fakeRotateAPI) { f.stuck["web"] = true }, 2, true, 2, "second"},
		{"remove old", func(f *fakeRotateAPI) { f.failOnce["remove:db"] = true }, 3, true, 2, "second"},
		{"re-create", func(f *fakeRotateAPI) { f.failOnce["create:db"] = true }, 4, true, 4, "second"},
		// From 5 on, db holds the first run's content and keeps it: it is in
		// use already, and replacing it would take another round of restarts.
		{"remove temp", func(f *fakeRotateAPI) { f.failOnce["remove:db_rotating"] = true }, 6, true, 5, "first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newRotateFixture(t)
			tc.setup(x.f)
			err := rotateSecret(context.Background(), x.f, "db", []byte("first"), fastRotate(nil))
			var re *rotateError
			if !errors.As(err, &re) || re.Step != tc.step || re.Resumable != tc.resumable {
				t.Fatalf("first run: err = %v, want step %d resumable=%v", err, tc.step, tc.resumable)
			}
			x.f.stuck = map[string]bool{}
			if tc.startAt > 0 {
				p, err := planRotation(context.Background(), x.f, "db")
				if err != nil || !p.Resume || p.StartStep != tc.startAt {
					t.Fatalf("plan = %+v, %v; want resume at %d", p, err, tc.startAt)
				}
				if len(p.Users) != 2 {
					t.Errorf("plan users = %+v, want web and worker", p.Users)
				}
				if p.Stack != "shop" || p.Labels[rotateMarkerLabel] != "" {
					t.Errorf("plan labels = %v stack %q", p.Labels, p.Stack)
				}
			}
			if err := rotateSecret(context.Background(), x.f, "db", []byte("second"), fastRotate(nil)); err != nil {
				t.Fatalf("re-run: %v", err)
			}
			x.checkRotated(t, tc.final)
		})
	}
}

// A failure half-way through step 5: web is back on the new db, worker still
// on the stand-in. The re-run finishes the switch and keeps db's content.
func TestRotateSecret_ResumeInStep5(t *testing.T) {
	x := newRotateFixture(t)
	// worker's first update (step 2) goes through; its second (step 5) fails.
	run := 0
	api := &stepFailAPI{fakeRotateAPI: x.f, fail: func(op string) bool {
		if op == "update:worker" {
			run++
			return run == 2
		}
		return false
	}}
	err := rotateSecret(context.Background(), api, "db", []byte("first"), fastRotate(nil))
	var re *rotateError
	if !errors.As(err, &re) || re.Step != 5 || !re.Resumable {
		t.Fatalf("first run: err = %v, want a resumable step-5 failure", err)
	}
	p, err := planRotation(context.Background(), x.f, "db")
	if err != nil || p.StartStep != 5 {
		t.Fatalf("plan = %+v, %v; want resume at 5", p, err)
	}
	var events []rotateEvent
	if err := rotateSecret(context.Background(), x.f, "db", []byte("second"), fastRotate(&events)); err != nil {
		t.Fatal(err)
	}
	x.checkRotated(t, "first")
	found := false
	for _, e := range events {
		found = found || strings.Contains(e.Msg, "not used")
	}
	if !found {
		t.Errorf("the re-run did not say the new value is not used: %v", events)
	}
}

// stepFailAPI fails ServiceUpdate when fail says so.
type stepFailAPI struct {
	*fakeRotateAPI
	fail func(op string) bool
}

func (s *stepFailAPI) ServiceUpdate(ctx context.Context, id string, o client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
	s.mu.Lock()
	name := s.services[id].Spec.Name
	s.mu.Unlock()
	if s.fail("update:" + name) {
		return client.ServiceUpdateResult{}, errors.New("injected failure")
	}
	return s.fakeRotateAPI.ServiceUpdate(ctx, id, o)
}

func TestRotateSecret_RefusesForeignTemp(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"no marker":        {"owner": "someone"},
		"other rotation":   {rotateMarkerLabel: "other"},
		"no labels at all": nil,
	} {
		t.Run(name, func(t *testing.T) {
			x := newRotateFixture(t)
			x.f.addSecret("db_rotating", "theirs", labels)
			x.f.mutations = nil
			err := rotateSecret(context.Background(), x.f, "db", []byte("new"), fastRotate(nil))
			if err == nil || !strings.Contains(err.Error(), "did not create") {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if len(x.f.mutations) != 0 {
				t.Errorf("touched the cluster: %v", x.f.mutations)
			}
			if _, err := planRotation(context.Background(), x.f, "db"); err == nil {
				t.Error("plan accepted a foreign stand-in")
			}
		})
	}
}

func TestRotateSecret_RefusesRotatingTheStandIn(t *testing.T) {
	f := newFakeRotate()
	f.addSecret("db_rotating", "x", map[string]string{rotateMarkerLabel: "db"})
	err := rotateSecret(context.Background(), f, "db_rotating", []byte("new"), fastRotate(nil))
	if err == nil || !strings.Contains(err.Error(), `rotate "db"`) {
		t.Fatalf("err = %v", err)
	}
}

// An unused secret takes the same path with nothing to switch: the stand-in
// still holds the labels while NAME is briefly gone, so a crash there is
// resumable like any other — and no service is touched.
func TestRotateSecret_UnusedSecret(t *testing.T) {
	f := newFakeRotate()
	labels := map[string]string{"a": "b"}
	f.addSecret("lonely", "old", labels)
	if err := rotateSecret(context.Background(), f, "lonely", []byte("new"), fastRotate(nil)); err != nil {
		t.Fatal(err)
	}
	want := "create lonely_rotating,remove lonely,create lonely,remove lonely_rotating"
	if got := strings.Join(f.mutations, ","); got != want {
		t.Errorf("mutations = %s, want %s", got, want)
	}
	s := f.byName("lonely")
	if string(f.data[s.ID]) != "new" || !maps.Equal(s.Spec.Labels, labels) {
		t.Errorf("lonely = %q %v", f.data[s.ID], s.Spec.Labels)
	}

	// The crash window: lonely removed, not re-created.
	f2 := newFakeRotate()
	f2.addSecret("lonely", "old", labels)
	f2.failOnce["create:lonely"] = true
	if err := rotateSecret(context.Background(), f2, "lonely", []byte("new"), fastRotate(nil)); err == nil {
		t.Fatal("expected the injected failure")
	}
	if f2.byName("lonely") != nil {
		t.Fatal("setup: lonely should be gone")
	}
	if err := rotateSecret(context.Background(), f2, "lonely", []byte("new"), fastRotate(nil)); err != nil {
		t.Fatal(err)
	}
	if s := f2.byName("lonely"); s == nil || !maps.Equal(s.Spec.Labels, labels) {
		t.Errorf("labels lost across the crash: %v", s)
	}
}

func TestRotateSecret_VersionConflictRetry(t *testing.T) {
	x := newRotateFixture(t)
	x.f.conflicts["web"] = 2
	var events []rotateEvent
	if err := rotateSecret(context.Background(), x.f, "db", []byte("new"), fastRotate(&events)); err != nil {
		t.Fatal(err)
	}
	x.checkRotated(t, "new")
	retries := 0
	for _, e := range events {
		if strings.Contains(e.Msg, "retrying") {
			retries++
		}
	}
	if retries != 2 {
		t.Errorf("retries reported = %d, want 2", retries)
	}

	y := newRotateFixture(t)
	y.f.conflicts["web"] = rotateUpdateAttempts
	err := rotateSecret(context.Background(), y.f, "db", []byte("new"), fastRotate(nil))
	if err == nil || !strings.Contains(err.Error(), "out of sequence") {
		t.Fatalf("err = %v, want the conflict after %d attempts", err, rotateUpdateAttempts)
	}
}

func TestRotateSecret_ScaledToZeroAndGlobalAreSwitched(t *testing.T) {
	f := newFakeRotate()
	s := f.addSecret("s", "old", nil)
	f.addService("idle", 0, &swarm.SecretReference{SecretID: s.ID, SecretName: "s", File: &swarm.SecretReferenceFileTarget{Name: "s"}})
	f.addService("everywhere", -1, &swarm.SecretReference{SecretID: s.ID, SecretName: "s", File: &swarm.SecretReferenceFileTarget{Name: "s"}})
	if err := rotateSecret(context.Background(), f, "s", []byte("new"), fastRotate(nil)); err != nil {
		t.Fatal(err)
	}
	ns := f.byName("s")
	for _, name := range []string{"idle", "everywhere"} {
		if ref := f.service(name).Spec.TaskTemplate.ContainerSpec.Secrets[0]; ref.SecretID != ns.ID {
			t.Errorf("%s still on %s", name, ref.SecretID)
		}
	}
}

func TestRotateSecret_ConvergenceTimeout(t *testing.T) {
	x := newRotateFixture(t)
	// The update is accepted but the new task never starts.
	api := &pendingTasksAPI{fakeRotateAPI: x.f}
	err := rotateSecret(context.Background(), api, "db", []byte("new"), rotateOptions{Timeout: 20 * time.Millisecond, Poll: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "did not converge within") {
		t.Fatalf("err = %v", err)
	}
}

type pendingTasksAPI struct{ *fakeRotateAPI }

func (p *pendingTasksAPI) TaskList(ctx context.Context, o client.TaskListOptions) (client.TaskListResult, error) {
	res, err := p.fakeRotateAPI.TaskList(ctx, o)
	for i := range res.Items {
		res.Items[i].Status.State = swarm.TaskStatePending
	}
	return res, err
}

func TestRotateSecret_ValidatesBeforeTouching(t *testing.T) {
	for name, tc := range map[string]struct {
		secret string
		data   []byte
	}{
		"empty value":   {"db", nil},
		"too large":     {"db", bytes.Repeat([]byte("x"), maxSecretSize+1)},
		"name too long": {strings.Repeat("n", 56), []byte("x")},
		"no name":       {" ", []byte("x")},
		"missing":       {"nope", []byte("x")},
	} {
		t.Run(name, func(t *testing.T) {
			x := newRotateFixture(t)
			if err := rotateSecret(context.Background(), x.f, tc.secret, tc.data, fastRotate(nil)); err == nil {
				t.Fatal("accepted")
			}
			if len(x.f.mutations) != 0 {
				t.Errorf("touched the cluster: %v", x.f.mutations)
			}
		})
	}
	// Exactly 500 KB, and a name that just fits, are fine.
	f := newFakeRotate()
	name := strings.Repeat("n", maxSecretNameLen-len(rotateSuffix))
	f.addSecret(name, "x", nil)
	if err := rotateSecret(context.Background(), f, name, bytes.Repeat([]byte("x"), maxSecretSize), fastRotate(nil)); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchSecretRefs_LeavesInputAlone(t *testing.T) {
	spec := swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Secrets: []*swarm.SecretReference{
		{SecretID: "a", SecretName: "A", File: &swarm.SecretReferenceFileTarget{Name: "t", UID: "1", GID: "2", Mode: 0o400}},
		{SecretID: "b", SecretName: "B", File: &swarm.SecretReferenceFileTarget{Name: "b"}},
	}}}}
	out, n := switchSecretRefs(spec, "a", "c", "C")
	if n != 1 {
		t.Fatalf("n = %d", n)
	}
	if r := spec.TaskTemplate.ContainerSpec.Secrets[0]; r.SecretID != "a" {
		t.Error("input modified")
	}
	r := out.TaskTemplate.ContainerSpec.Secrets[0]
	if r.SecretID != "c" || r.SecretName != "C" || *r.File != (swarm.SecretReferenceFileTarget{Name: "t", UID: "1", GID: "2", Mode: 0o400}) {
		t.Errorf("switched ref = %+v %+v", *r, *r.File)
	}
	if b := out.TaskTemplate.ContainerSpec.Secrets[1]; b.SecretID != "b" {
		t.Errorf("other ref changed: %+v", *b)
	}
}
