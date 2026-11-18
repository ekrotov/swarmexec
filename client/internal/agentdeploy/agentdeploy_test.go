// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentdeploy

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"swarmexec/client/internal/swarmfake"
	"swarmexec/internal/deploy"
	"swarmexec/internal/prunesched"
)

// fakeServiceLister answers ServiceList from a fixed set, so the port check
// can be exercised without a daemon.
type fakeServiceLister struct {
	all []swarm.Service
	err error
}

func (f fakeServiceLister) ServiceList(_ context.Context, _ client.ServiceListOptions) (client.ServiceListResult, error) {
	if f.err != nil {
		return client.ServiceListResult{}, f.err
	}
	return client.ServiceListResult{Items: f.all}, nil
}

func svcPublishingHost(name string, port uint32) swarm.Service {
	var s swarm.Service
	s.Spec.Name = name
	s.Spec.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{
		{PublishMode: swarm.PortConfigPublishModeHost, PublishedPort: port},
	}}
	return s
}

func TestRegistryHost(t *testing.T) {
	cases := map[string]string{
		"registry.logle.io/internal-tools/x/agent:latest": "registry.logle.io",
		"localhost:5000/agent:1.0":                        "localhost:5000",
		"nginx":                                           "docker.io",
		"library/nginx:latest":                            "docker.io",
		"gcr.io/proj/img":                                 "gcr.io",
	}
	for image, want := range cases {
		if got := RegistryHost(image); got != want {
			t.Errorf("RegistryHost(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestGenerateSecret(t *testing.T) {
	a, b := GenerateSecret(), GenerateSecret()
	if a == "" || a == b {
		t.Fatalf("expected two distinct non-empty secrets, got %q and %q", a, b)
	}
	if len(a) < 32 {
		t.Errorf("secret too short: %d chars", len(a))
	}
}

func TestAgentServiceSpec(t *testing.T) {
	o := Options{Image: "reg/agent:1", ServiceName: "swarmexec_agent", Port: 9443}
	spec := Spec(o, "sec-123")

	if spec.Mode.Global == nil {
		t.Error("service must be global")
	}
	if spec.Annotations.Labels[deploy.RoleLabel] != deploy.RoleAgent {
		t.Error("missing role label")
	}
	cs := spec.TaskTemplate.ContainerSpec
	if cs.Image != "reg/agent:1" {
		t.Errorf("image = %q", cs.Image)
	}
	args := strings.Join(cs.Args, " ")
	for _, want := range []string{"-port=9443", "-self-signed", "/run/secrets/" + deploy.SecretName} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	if len(cs.Secrets) != 1 || cs.Secrets[0].SecretID != "sec-123" {
		t.Errorf("secret ref wrong: %+v", cs.Secrets)
	}
	if len(cs.Mounts) != 1 || cs.Mounts[0].Source != "/var/run/docker.sock" {
		t.Errorf("socket mount wrong: %+v", cs.Mounts)
	}
	// Health timings, but the command left to the image: a spec naming
	// `-healthcheck` would restart-loop an agent rolled back to an image
	// without the probe.
	if hc := cs.Healthcheck; hc == nil || len(hc.Test) != 0 || hc.Interval != deploy.HealthInterval ||
		hc.Timeout != deploy.HealthTimeout || hc.StartPeriod != deploy.HealthStartPeriod || hc.Retries != deploy.HealthRetries {
		t.Errorf("healthcheck = %+v, want the deploy timings and no Test", hc)
	}
	if spec.UpdateConfig.Order != swarm.UpdateOrderStopFirst {
		t.Errorf("update order = %q, want stop-first", spec.UpdateConfig.Order)
	}
	p := spec.EndpointSpec.Ports
	if len(p) != 1 || p[0].PublishedPort != 9443 || p[0].PublishMode != swarm.PortConfigPublishModeHost {
		t.Errorf("port config wrong: %+v", p)
	}
}

func TestAgentServiceSpec_LegacySecretSwitch(t *testing.T) {
	args := func(allowLegacy bool) []string {
		o := Options{Image: "reg/agent:1", ServiceName: "swarmexec_agent", Port: 9443, AllowLegacySecret: allowLegacy}
		return Spec(o, "sec-123").TaskTemplate.ContainerSpec.Args
	}

	// Default: nothing emitted, so a re-roll produces the same command earlier
	// versions did and no client is locked out by surprise.
	for _, a := range args(true) {
		if strings.Contains(a, "allow-legacy-secret") {
			t.Errorf("default must not emit the flag, got %q", a)
		}
	}

	var found bool
	for _, a := range args(false) {
		if a == "-allow-legacy-secret=false" {
			found = true
		}
	}
	if !found {
		t.Errorf("--allow-legacy-secret=false must reach the agent command, got %v", args(false))
	}
}

// With --metrics-port the agent both listens and publishes: a metrics flag
// without a published port is an endpoint nothing outside the container can
// reach, which is exactly how the metrics went unused before.
func TestAgentServiceSpec_MetricsPort(t *testing.T) {
	o := Options{Image: "reg/agent:1", ServiceName: "swarmexec_agent", Port: 9443, AllowLegacySecret: true}
	if ports := Spec(o, "s").EndpointSpec.Ports; len(ports) != 1 {
		t.Fatalf("no metrics: want only the agent port, got %+v", ports)
	}

	o.MetricsPort = 9464
	spec := Spec(o, "s")
	ports := spec.EndpointSpec.Ports
	if len(ports) != 2 || ports[1].PublishedPort != 9464 || ports[1].TargetPort != 9464 || ports[1].PublishMode != swarm.PortConfigPublishModeHost {
		t.Errorf("metrics port not published in host mode: %+v", ports)
	}
	args := strings.Join(spec.TaskTemplate.ContainerSpec.Args, " ")
	if !strings.Contains(args, "-metrics-addr=:9464") {
		t.Errorf("agent not told to listen: %s", args)
	}
}

func TestPortConflict(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		services []swarm.Service
		wantSvc  string
		wantOk   bool
	}{
		{"conflict", []swarm.Service{svcPublishingHost("agent_agent", 9443)}, "agent_agent", true},
		{"own-service-ignored", []swarm.Service{svcPublishingHost("swarmexec_agent", 9443)}, "", false},
		{"different-port", []swarm.Service{svcPublishingHost("other", 8080)}, "", false},
		{"none", nil, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotSvc, gotOk := PortConflict(ctx, fakeServiceLister{all: c.services}, 9443, "swarmexec_agent")
			if gotSvc != c.wantSvc || gotOk != c.wantOk {
				t.Errorf("PortConflict = (%q, %v), want (%q, %v)", gotSvc, gotOk, c.wantSvc, c.wantOk)
			}
		})
	}
}

func TestRolloutLine(t *testing.T) {
	task := func(desired, state swarm.TaskState, errMsg, msg string) swarm.Task {
		var tk swarm.Task
		tk.DesiredState, tk.Status.State, tk.Status.Err, tk.Status.Message = desired, state, errMsg, msg
		return tk
	}
	run, shut := swarm.TaskStateRunning, swarm.TaskStateShutdown
	for _, tc := range []struct {
		name     string
		tasks    []swarm.Task
		wantMsg  string
		wantDone bool
	}{
		{"nothing yet", nil, "0/0 running", false},
		{"all running", []swarm.Task{task(run, run, "", ""), task(run, run, "", "")}, "2/2 running", true},
		{"failed task's error", []swarm.Task{task(run, run, "", ""), task(run, swarm.TaskStateRejected, "port in use", "x")},
			"1/2 running — port in use", false},
		{"message when no error", []swarm.Task{task(run, swarm.TaskStateFailed, "", "exit 1")}, "0/1 running — exit 1", false},
		// A failure of a task no longer wanted says nothing once the rest run.
		{"old failure, all running", []swarm.Task{task(run, run, "", ""), task(shut, swarm.TaskStateFailed, "boom", "")},
			"1/1 running", true},
	} {
		msg, done := RolloutLine(tc.tasks)
		if msg != tc.wantMsg || done != tc.wantDone {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, msg, done, tc.wantMsg, tc.wantDone)
		}
	}
}

// dockerTo builds a manager client pointed at a fake swarm.
func dockerTo(t *testing.T, f *swarmfake.Swarm) *client.Client {
	t.Helper()
	api := swarmfake.Serve(t, f)
	c, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// Docker secrets are immutable: an existing one is reused as-is, and that is
// what makes `init --force` safe to re-run. Creating a second secret with the
// same name would fail; silently replacing it would lock out every agent that
// already mounted the old one.
func TestEnsureSecretReusesAnExistingOne(t *testing.T) {
	f := &swarmfake.Swarm{}
	dcli := dockerTo(t, f)
	ctx := context.Background()

	id, created, err := EnsureSecret(ctx, dcli, deploy.SecretName, "first")
	if err != nil || !created || id == "" {
		t.Fatalf("create: id=%q created=%v err=%v", id, created, err)
	}
	again, created, err := EnsureSecret(ctx, dcli, deploy.SecretName, "second")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("an existing secret was reported as created")
	}
	if again != id {
		t.Errorf("id = %q, want the existing %q", again, id)
	}
	if n := len(f.Secrets); n != 1 {
		t.Errorf("%d secrets on the cluster, want 1", n)
	}
}

// A policy config is named by the hash of its content, because Docker configs
// are immutable: the same file reuses the config, a changed file becomes a new
// one, and the service update is what points the agents at it.
func TestEnsurePolicyConfigIsContentAddressed(t *testing.T) {
	f := &swarmfake.Swarm{}
	dcli := dockerTo(t, f)
	ctx := context.Background()

	ref, err := EnsurePolicyConfig(ctx, dcli, []byte("rules: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref.ConfigName, PolicyConfigPrefix) {
		t.Errorf("config name %q does not carry the prefix", ref.ConfigName)
	}
	if ref.File == nil || ref.File.Name != deploy.PolicyPath {
		t.Errorf("policy mounted at %+v, want %s", ref.File, deploy.PolicyPath)
	}

	same, err := EnsurePolicyConfig(ctx, dcli, []byte("rules: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if same.ConfigID != ref.ConfigID {
		t.Error("the same policy created a second config")
	}

	other, err := EnsurePolicyConfig(ctx, dcli, []byte("rules: [{action: exec}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if other.ConfigName == ref.ConfigName {
		t.Error("a different policy reused the same config name")
	}
	if n := len(f.Configs); n != 2 {
		t.Errorf("%d configs, want 2", n)
	}
}

// The prune schedule is set from the TUI or `prune-schedule set`, never from
// init's flags. A re-init that rebuilt the spec from flags alone would switch
// it off without a word, which is how it was lost before.
func TestCarryPruneScheduleKeepsWhatInitDoesNotKnow(t *testing.T) {
	o := Options{Image: "reg/agent:1", ServiceName: "swarmexec_agent", Port: 9443}
	sched := prunesched.Default()
	sched.Enabled = true
	var existing swarm.Service
	existing.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Labels: sched.Labels()}

	spec := Spec(o, "sec")
	if !CarryPruneSchedule(&spec, &existing) {
		t.Fatal("an existing schedule must be carried into the new spec")
	}
	if !prunesched.HasLabels(spec.TaskTemplate.ContainerSpec.Labels) {
		t.Error("the carried spec has no schedule labels")
	}

	fresh := Spec(o, "sec")
	if CarryPruneSchedule(&fresh, nil) {
		t.Error("there is nothing to carry from a service that does not exist")
	}
	var bare swarm.Service
	bare.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Labels: map[string]string{"com.example": "x"}}
	if CarryPruneSchedule(&fresh, &bare) {
		t.Error("a service without a schedule must not be reported as carrying one")
	}
}

// Docker pins the @sha256 digest on deploy when QueryRegistry is set, and init
// reports that digest. A service it cannot inspect must come back empty rather
// than with a guess.
func TestInspectServiceImageReadsBackTheResolvedImage(t *testing.T) {
	var svc swarm.Service
	svc.ID = "svc1"
	svc.Spec.Name = "swarmexec_agent"
	svc.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "reg/agent:1@sha256:abc"}
	dcli := dockerTo(t, &swarmfake.Swarm{Services: []swarm.Service{svc}})

	if got := InspectServiceImage(context.Background(), dcli, "svc1"); got != "reg/agent:1@sha256:abc" {
		t.Errorf("image = %q, want the resolved one", got)
	}
	if got := InspectServiceImage(context.Background(), dcli, "nope"); got != "" {
		t.Errorf("an unknown service reported %q, want empty", got)
	}
}

// The rollout report is what an operator watches instead of `docker service
// ps`: it has to end as soon as every desired task runs, and it must print the
// count rather than wait silently.
func TestWaitRolloutReportsAndStopsWhenEveryTaskRuns(t *testing.T) {
	task := func(state swarm.TaskState) swarm.Task {
		var tk swarm.Task
		tk.ServiceID = "svc1"
		tk.DesiredState, tk.Status.State = swarm.TaskStateRunning, state
		return tk
	}
	dcli := dockerTo(t, &swarmfake.Swarm{
		Tasks: []swarm.Task{task(swarm.TaskStateRunning), task(swarm.TaskStateRunning)},
	})

	var out strings.Builder
	done := make(chan struct{})
	go func() {
		WaitRollout(context.Background(), dcli, "svc1", &out, 10*time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("WaitRollout kept polling although every task was running")
	}
	if got := out.String(); !strings.Contains(got, "2/2 running") {
		t.Errorf("report = %q, want the running/desired count", got)
	}
}

// init reads THIS machine's `docker login` and distributes it, so the nodes can
// pull a private image. Reading the wrong registry's entry — or none at all —
// deploys a service the workers cannot pull, which shows up as an image that is
// "unavailable" on every node but the manager.
func TestEncodedRegistryAuthReadsTheImagesRegistry(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"auths":{"reg.example":{"auth":"` +
		base64.StdEncoding.EncodeToString([]byte("robot:token")) + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)

	got, err := EncodedRegistryAuth("reg.example/team/agent:1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.URLEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("not the encoding ServiceCreate expects: %v", err)
	}
	if !strings.Contains(string(raw), "robot") {
		t.Errorf("encoded auth = %s, want the stored credentials", raw)
	}

	// A registry nothing is logged in to yields no credentials rather than an
	// error: the caller decides whether that is fatal (it is, for a private
	// registry; it is not for Docker Hub).
	other, err := EncodedRegistryAuth("other.example/agent:1")
	if err != nil || other != "" {
		t.Errorf("unknown registry: (%q, %v), want empty and no error", other, err)
	}
}
