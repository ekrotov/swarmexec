// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// Rotating a secret's content while it keeps its name.
//
// Swarm secrets are immutable, and one a service references cannot be
// removed. Renaming (NAME_v2) is the usual way round that, and the one thing an
// operator with a stack file does not want: the file names the secret, so the
// file would have to change too. Instead the services are parked on a
// temporary secret while NAME is replaced underneath them:
//
//  1. create NAME_rotating with the new content (NAME's labels + a marker)
//  2. switch every service from NAME to NAME_rotating      — rolling update
//  3. remove NAME, which nothing references any more
//  4. create NAME again, new content, NAME's original labels
//  5. switch every service from NAME_rotating back to NAME — rolling update
//  6. remove NAME_rotating
//
// Only the reference's secret changes in steps 2 and 5: the file target, uid,
// gid and mode stay as they were, so inside the container the same path holds
// the new bytes.
//
// Every step reads the cluster again before it acts, so a run that died
// anywhere is finished by running it again: the state on the cluster says
// where it stopped (see rotateFrom). The state in between — services on
// NAME_rotating — is a working one: it holds the new content.
//
// An unused secret goes through the same six steps, with nothing to switch in
// 2 and 5. Deleting and re-creating it directly would be two calls fewer, but
// would leave a window in which NAME is gone and its labels exist only in this
// process; with the temporary secret holding them a crash there is resumable
// like any other.

const (
	// rotateSuffix names the temporary secret.
	rotateSuffix = "_rotating"
	// rotateMarkerLabel is set on the temporary secret, valued with the name
	// it stands in for. A NAME_rotating without it is someone else's secret,
	// and is left alone.
	rotateMarkerLabel = "swarmexec.rotating-for"
	// rotateSteps is the number of steps progress is counted in.
	rotateSteps = 6
	// maxSecretNameLen is swarm's limit on an object name.
	maxSecretNameLen = 64
	// defaultRotateTimeout bounds each convergence wait.
	defaultRotateTimeout = 5 * time.Minute
	// rotateUpdateAttempts is how often a service update is tried when the
	// service changed between reading and writing it.
	rotateUpdateAttempts = 5
)

// secretRotateAPI is the slice of the Docker client a rotation needs.
// *client.Client satisfies it.
type secretRotateAPI interface {
	SecretList(context.Context, client.SecretListOptions) (client.SecretListResult, error)
	SecretCreate(context.Context, client.SecretCreateOptions) (client.SecretCreateResult, error)
	SecretRemove(context.Context, string, client.SecretRemoveOptions) (client.SecretRemoveResult, error)
	ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error)
	ServiceInspect(context.Context, string, client.ServiceInspectOptions) (client.ServiceInspectResult, error)
	ServiceUpdate(context.Context, string, client.ServiceUpdateOptions) (client.ServiceUpdateResult, error)
	TaskList(context.Context, client.TaskListOptions) (client.TaskListResult, error)
}

var _ secretRotateAPI = (*client.Client)(nil)

// rotateEvent is one progress line. It never carries the secret's value.
type rotateEvent struct {
	Step    int    // 1..rotateSteps
	Service string // the service the line is about, "" for a secret step
	Msg     string
}

func (e rotateEvent) String() string {
	return fmt.Sprintf("[%d/%d] %s", e.Step, rotateSteps, e.Msg)
}

// rotateOptions tunes a rotation. The zero value is usable.
type rotateOptions struct {
	Timeout  time.Duration     // per convergence wait; 0 means defaultRotateTimeout
	Poll     time.Duration     // convergence poll interval; 0 means 1s
	Progress func(rotateEvent) // may be nil; called from the rotating goroutine
}

// rotateError is a failed rotation: the step it failed in, and whether the
// cluster was left in the resumable in-between state (NAME_rotating exists).
type rotateError struct {
	Name      string
	Step      int
	Resumable bool
	Err       error
}

func (e *rotateError) Error() string {
	return fmt.Sprintf("rotate secret %s: step %d/%d: %v", e.Name, e.Step, rotateSteps, e.Err)
}

func (e *rotateError) Unwrap() error { return e.Err }

// rotateUser is a service that mounts the secret (or its stand-in), with the
// file targets it mounts it at.
type rotateUser struct {
	ID      string
	Name    string
	Targets []string
	OnTemp  bool // already switched to NAME_rotating
}

// rotatePlan is what a rotation of Name would do, for the confirm.
type rotatePlan struct {
	Name   string
	Temp   string
	Labels map[string]string // the labels NAME keeps
	Users  []rotateUser
	Stack  string // com.docker.stack.namespace, "" when not from a stack
	// Resume is set when an interrupted rotation is found; StartStep is
	// where this run picks it up.
	Resume    bool
	StartStep int
}

// rotateState is the cluster as a rotation of name finds it.
type rotateState struct {
	name     string
	orig     *swarm.Secret // the secret called name; nil once removed
	temp     *swarm.Secret // name_rotating; nil before step 1 / after step 6
	services []swarm.Service
}

// origIsNew reports whether NAME is the one this rotation re-created (step 4
// ran): the original is always older than the temporary secret.
func (s rotateState) origIsNew() bool {
	return s.orig != nil && s.temp != nil && s.orig.Meta.CreatedAt.After(s.temp.Meta.CreatedAt)
}

// startStep is where a run on this state begins.
func (s rotateState) startStep() int {
	switch {
	case s.temp == nil:
		return 1
	case s.orig == nil:
		return 4
	case s.origIsNew() && len(usersOf(s.services, s.orig.ID)) > 0:
		return 5
	case s.origIsNew():
		// Re-created but not in use yet: replacing it costs nothing and makes
		// the content given now the one that ends up in NAME.
		return 3
	default:
		return 2
	}
}

// rotateTempName is the stand-in's name.
func rotateTempName(name string) string { return name + rotateSuffix }

// validateRotateName checks the name before anything is read.
func validateRotateName(name string) error {
	if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
		return errors.New("secret name is required (and has no surrounding spaces)")
	}
	if n := len(rotateTempName(name)); n > maxSecretNameLen {
		return fmt.Errorf("secret name %q is too long to rotate: the temporary %q would be %d characters, over swarm's %d",
			name, rotateTempName(name), n, maxSecretNameLen)
	}
	return nil
}

// validateRotateValue holds the new content to the editor's rules: not empty,
// not over swarm's 500 KB. Nothing is trimmed — the bytes are sent as given.
func validateRotateValue(data []byte) error {
	if len(data) == 0 {
		return errors.New("the new value is empty")
	}
	if len(data) > maxSecretSize {
		return fmt.Errorf("the new value is %d bytes — over the swarm limit of %d (500 KB)", len(data), maxSecretSize)
	}
	return nil
}

// loadRotateState reads the two secrets and every service. A NAME_rotating
// this tool did not create for NAME is refused, as is asking to rotate the
// stand-in itself.
func loadRotateState(ctx context.Context, api secretRotateAPI, name string) (rotateState, error) {
	st := rotateState{name: name}
	secs, err := api.SecretList(ctx, client.SecretListOptions{})
	if err != nil {
		return st, fmt.Errorf("list secrets: %w", err)
	}
	temp := rotateTempName(name)
	for i := range secs.Items {
		s := secs.Items[i]
		switch s.Spec.Name {
		case name:
			st.orig = &s
		case temp:
			st.temp = &s
		}
	}
	if st.orig != nil {
		if of, ok := st.orig.Spec.Labels[rotateMarkerLabel]; ok {
			return st, fmt.Errorf("%q is the temporary secret of an interrupted rotation of %q — rotate %q to finish it", name, of, of)
		}
	}
	if st.temp != nil && st.temp.Spec.Labels[rotateMarkerLabel] != name {
		return st, fmt.Errorf("a secret %q exists that swarmexec did not create for this rotation (no %s=%s label) — refusing to touch it; rename or remove it first",
			temp, rotateMarkerLabel, name)
	}
	if st.orig == nil && st.temp == nil {
		return st, fmt.Errorf("no secret named %q", name)
	}
	svcs, err := api.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return st, fmt.Errorf("list services: %w", err)
	}
	st.services = svcs.Items
	return st, nil
}

// usersOf returns the services whose container spec references secretID,
// sorted by name, with the targets they mount it at.
func usersOf(svcs []swarm.Service, secretID string) []rotateUser {
	var out []rotateUser
	for _, s := range svcs {
		cs := s.Spec.TaskTemplate.ContainerSpec
		if cs == nil || secretID == "" {
			continue
		}
		var targets []string
		for _, r := range cs.Secrets {
			if r != nil && r.SecretID == secretID {
				t := r.SecretName
				if r.File != nil && r.File.Name != "" {
					t = r.File.Name
				}
				targets = append(targets, t)
			}
		}
		if len(targets) > 0 {
			out = append(out, rotateUser{ID: s.ID, Name: s.Spec.Name, Targets: targets})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// planRotation describes what rotating name would do, without doing it.
func planRotation(ctx context.Context, api secretRotateAPI, name string) (rotatePlan, error) {
	if err := validateRotateName(name); err != nil {
		return rotatePlan{}, err
	}
	st, err := loadRotateState(ctx, api, name)
	if err != nil {
		return rotatePlan{}, err
	}
	p := rotatePlan{Name: name, Temp: rotateTempName(name), Labels: st.keptLabels(), StartStep: st.startStep()}
	p.Resume = st.temp != nil
	p.Stack = p.Labels[stackLabelKey]
	if st.orig != nil {
		p.Users = append(p.Users, usersOf(st.services, st.orig.ID)...)
	}
	if st.temp != nil {
		for _, u := range usersOf(st.services, st.temp.ID) {
			u.OnTemp = true
			p.Users = append(p.Users, u)
		}
	}
	sort.SliceStable(p.Users, func(i, j int) bool { return p.Users[i].Name < p.Users[j].Name })
	return p, nil
}

// keptLabels are the labels NAME has, and gets back in step 4. Once the
// stand-in exists it is the record of them — step 1 copied the original's —
// so they survive the original's removal; before that, the original's own.
func (s rotateState) keptLabels() map[string]string {
	src := map[string]string(nil)
	switch {
	case s.temp != nil:
		src = s.temp.Spec.Labels
	case s.orig != nil:
		src = s.orig.Spec.Labels
	}
	out := map[string]string{}
	for k, v := range src {
		if k != rotateMarkerLabel {
			out[k] = v
		}
	}
	return out
}

// rotateSecret replaces the content of the secret called name with data,
// keeping its name, labels and every service's mount of it. See the comment
// at the top of this file for the steps; on a re-run after a failure it
// continues where the cluster says the last run stopped.
func rotateSecret(ctx context.Context, api secretRotateAPI, name string, data []byte, opt rotateOptions) error {
	if err := validateRotateName(name); err != nil {
		return err
	}
	if err := validateRotateValue(data); err != nil {
		return err
	}
	if opt.Timeout <= 0 {
		opt.Timeout = defaultRotateTimeout
	}
	if opt.Poll <= 0 {
		opt.Poll = time.Second
	}
	r := &rotation{api: api, name: name, temp: rotateTempName(name), data: data, opt: opt}
	return r.run(ctx)
}

type rotation struct {
	api  secretRotateAPI
	name string
	temp string
	data []byte
	opt  rotateOptions
	step int
	// resumable turns true once NAME_rotating exists: from then on, a failure
	// leaves a state a re-run can finish.
	resumable bool
}

func (r *rotation) progress(service, format string, args ...any) {
	if r.opt.Progress != nil {
		r.opt.Progress(rotateEvent{Step: r.step, Service: service, Msg: fmt.Sprintf(format, args...)})
	}
}

func (r *rotation) fail(err error) error {
	return &rotateError{Name: r.name, Step: r.step, Resumable: r.resumable, Err: err}
}

func (r *rotation) run(ctx context.Context) error {
	st, err := loadRotateState(ctx, r.api, r.name)
	if err != nil {
		return err
	}
	start := st.startStep()
	r.resumable = st.temp != nil
	if st.temp != nil {
		r.step = start
		r.progress("", "resuming the interrupted rotation of %s at step %d", r.name, start)
		if start == 5 {
			r.progress("", "%s was already re-created by that run; its content stays (the value given now is not used)", r.name)
		}
	}

	// 1: the stand-in, with the new content and NAME's labels.
	if st.temp == nil {
		r.step = 1
		labels := maps.Clone(st.orig.Spec.Labels)
		if labels == nil {
			labels = map[string]string{}
		}
		labels[rotateMarkerLabel] = r.name
		r.progress("", "creating %s", r.temp)
		temp, err := r.create(ctx, r.temp, labels)
		if err != nil {
			return r.fail(err)
		}
		st.temp = temp
		r.resumable = true
	}
	keep := st.keptLabels()

	// 2 + 3: off the original, then remove it — unless it is the one step 4
	// already made and services are on it (then go straight to 5).
	if st.orig != nil && start != 5 {
		r.step = 2
		if err := r.switchAll(ctx, st.orig.ID, st.temp); err != nil {
			return r.fail(err)
		}
		r.step = 3
		r.progress("", "removing the old %s", r.name)
		if err := r.removeUnused(ctx, st.orig); err != nil {
			return r.fail(err)
		}
		st.orig = nil
	}

	// 4: NAME again, new content, original labels.
	if st.orig == nil {
		r.step = 4
		r.progress("", "creating %s with the new content", r.name)
		orig, err := r.create(ctx, r.name, keep)
		if err != nil {
			return r.fail(err)
		}
		st.orig = orig
	}

	// 5: back from the stand-in.
	r.step = 5
	if err := r.switchAll(ctx, st.temp.ID, st.orig); err != nil {
		return r.fail(err)
	}

	// 6: the stand-in goes.
	r.step = 6
	r.progress("", "removing %s", r.temp)
	if err := r.removeUnused(ctx, st.temp); err != nil {
		return r.fail(err)
	}
	r.resumable = false
	r.progress("", "done: %s has the new content", r.name)
	return nil
}

// create makes a secret with the rotation's content.
func (r *rotation) create(ctx context.Context, name string, labels map[string]string) (*swarm.Secret, error) {
	spec := swarm.SecretSpec{Annotations: swarm.Annotations{Name: name, Labels: labels}, Data: r.data}
	res, err := r.api.SecretCreate(ctx, client.SecretCreateOptions{Spec: spec})
	if err != nil {
		return nil, fmt.Errorf("create secret %s: %w", name, err)
	}
	spec.Data = nil // only the reference is kept around
	return &swarm.Secret{ID: res.ID, Spec: spec}, nil
}

// removeUnused removes a secret after checking — on a fresh service list —
// that nothing references it; a service attached meanwhile stops the run
// rather than having docker refuse with a less useful message.
func (r *rotation) removeUnused(ctx context.Context, s *swarm.Secret) error {
	svcs, err := r.api.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return fmt.Errorf("list services: %w", err)
	}
	if users := usersOf(svcs.Items, s.ID); len(users) > 0 {
		names := make([]string, len(users))
		for i, u := range users {
			names[i] = u.Name
		}
		return fmt.Errorf("%s is still used by %s", s.Spec.Name, strings.Join(names, ", "))
	}
	if _, err := r.api.SecretRemove(ctx, s.ID, client.SecretRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove secret %s: %w", s.Spec.Name, err)
	}
	return nil
}

// switchAll moves every service that references from onto to, one service at
// a time, each converged before the next starts. The list is read fresh, so
// a service already switched by an earlier run is simply not on it.
func (r *rotation) switchAll(ctx context.Context, fromID string, to *swarm.Secret) error {
	svcs, err := r.api.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return fmt.Errorf("list services: %w", err)
	}
	users := usersOf(svcs.Items, fromID)
	if len(users) == 0 {
		r.progress("", "no service to switch to %s", to.Spec.Name)
		return nil
	}
	for i, u := range users {
		r.progress(u.Name, "switching %s to %s (%d/%d)", u.Name, to.Spec.Name, i+1, len(users))
		changed, err := r.switchService(ctx, u, fromID, to)
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		if err := r.waitConverged(ctx, u, to.ID, to.Spec.Name); err != nil {
			return err
		}
	}
	return nil
}

// switchService rewrites one service's references from fromID to to — the
// secret only; target, uid, gid and mode stay — and updates the service at
// the version it was read at. A version conflict (someone else updated it in
// between) is retried on a fresh read. changed is false when no reference
// pointed at fromID any more.
func (r *rotation) switchService(ctx context.Context, u rotateUser, fromID string, to *swarm.Secret) (changed bool, err error) {
	for attempt := 1; ; attempt++ {
		res, err := r.api.ServiceInspect(ctx, u.ID, client.ServiceInspectOptions{})
		if err != nil {
			return false, fmt.Errorf("inspect service %s: %w", u.Name, err)
		}
		svc := res.Service
		spec, n := switchSecretRefs(svc.Spec, fromID, to.ID, to.Spec.Name)
		if n == 0 {
			return false, nil
		}
		_, err = r.api.ServiceUpdate(ctx, svc.ID, client.ServiceUpdateOptions{Version: svc.Version, Spec: spec})
		if err == nil {
			return true, nil
		}
		if !isVersionConflict(err) || attempt >= rotateUpdateAttempts {
			return false, fmt.Errorf("update service %s: %w", u.Name, err)
		}
		r.progress(u.Name, "%s changed while being updated — retrying (%d/%d)", u.Name, attempt+1, rotateUpdateAttempts)
	}
}

// switchSecretRefs returns spec with every secret reference to fromID pointed
// at (toID, toName), and how many it changed. Everything else — the other
// references, the file targets — is copied unchanged; the input is not
// modified.
func switchSecretRefs(spec swarm.ServiceSpec, fromID, toID, toName string) (swarm.ServiceSpec, int) {
	cs := spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return spec, 0
	}
	refs := make([]*swarm.SecretReference, len(cs.Secrets))
	n := 0
	for i, ref := range cs.Secrets {
		if ref == nil {
			continue
		}
		c := *ref
		if c.File != nil {
			f := *c.File
			c.File = &f
		}
		if c.SecretID == fromID {
			c.SecretID, c.SecretName = toID, toName
			n++
		}
		refs[i] = &c
	}
	csCopy := *cs
	csCopy.Secrets = refs
	spec.TaskTemplate.ContainerSpec = &csCopy
	return spec, n
}

// isVersionConflict recognises the manager's "update out of sequence": the
// service changed between our read and our write.
func isVersionConflict(err error) bool {
	return err != nil && (cerrdefs.IsConflict(err) || strings.Contains(err.Error(), "out of sequence"))
}

// waitConverged waits until the service's rolling update is through: no task
// meant to run still uses the old secret, every one that does use the new
// one is running, and swarm does not report the update as still in progress.
// A paused or rolled-back update is a failure; so is the timeout.
//
// A service scaled to 0 (or a global one with no eligible node) has no tasks
// and converges at once — its spec is what had to change.
func (r *rotation) waitConverged(ctx context.Context, u rotateUser, secretID, secretName string) error {
	deadline := time.Now().Add(r.opt.Timeout)
	last := ""
	for {
		res, err := r.api.ServiceInspect(ctx, u.ID, client.ServiceInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspect service %s: %w", u.Name, err)
		}
		updating := false
		if us := res.Service.UpdateStatus; us != nil {
			switch us.State {
			case swarm.UpdateStatePaused, swarm.UpdateStateRollbackStarted,
				swarm.UpdateStateRollbackPaused, swarm.UpdateStateRollbackCompleted:
				msg := us.Message
				if msg == "" {
					msg = "no reason given"
				}
				return fmt.Errorf("rolling update of %s %s: %s", u.Name, us.State, msg)
			case swarm.UpdateStateUpdating:
				updating = true
			}
		}
		tasks, err := r.api.TaskList(ctx, client.TaskListOptions{Filters: make(client.Filters).Add("service", u.ID)})
		if err != nil {
			return fmt.Errorf("list tasks of %s: %w", u.Name, err)
		}
		var stale, pending, running int
		for _, t := range tasks.Items {
			if t.DesiredState != swarm.TaskStateRunning {
				continue
			}
			switch {
			case !taskUsesSecret(t, secretID):
				stale++
			case t.Status.State == swarm.TaskStateRunning:
				running++
			default:
				pending++
			}
		}
		if !updating && stale == 0 && pending == 0 {
			if running == 0 {
				r.progress(u.Name, "%s updated (no running tasks)", u.Name)
			} else {
				r.progress(u.Name, "%s converged: %d task(s) running", u.Name, running)
			}
			return nil
		}
		if line := fmt.Sprintf("%s converging: %d/%d task(s) running on %s", u.Name, running, running+pending+stale, secretName); line != last {
			r.progress(u.Name, "%s", line)
			last = line
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not converge within %s (%d running, %d starting, %d still on the old secret)",
				u.Name, r.opt.Timeout, running, pending, stale)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.opt.Poll):
		}
	}
}

// taskUsesSecret reports whether a task was created from a spec that mounts
// secretID.
func taskUsesSecret(t swarm.Task, secretID string) bool {
	cs := t.Spec.ContainerSpec
	if cs == nil {
		return false
	}
	for _, ref := range cs.Secrets {
		if ref != nil && ref.SecretID == secretID {
			return true
		}
	}
	return false
}

// rotateResumeHint is the line a failure ends with.
func rotateResumeHint(err error, rerun string) string {
	var re *rotateError
	if errors.As(err, &re) && re.Resumable {
		return fmt.Sprintf("the rotation stopped half-way; the services keep running (%s holds the new content) — %s to resume", rotateTempName(re.Name), rerun)
	}
	return ""
}
