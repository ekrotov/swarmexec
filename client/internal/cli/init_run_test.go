// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cliconfig "github.com/docker/cli/cli/config"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/system"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/session"
	"swarmexec/client/internal/swarmfake"
	"swarmexec/internal/deploy"
	"swarmexec/internal/prunesched"
)

// These pin `swarmexec init` end to end — flags in, Docker API writes and
// printed text out — against swarmfake, so the command can be restructured
// without a cluster to check it on. The expected texts are what init printed
// when they were written; a change to them is a change of behaviour.

// initRun is one init invocation's outcome.
type initRun struct {
	out     string // stdout, with the fake's address replaced by HOST
	err     error
	cfgPath string // where the client config would be written
}

// code is the exit code the run maps to (0 on success).
func (r initRun) code(t *testing.T) int {
	t.Helper()
	if r.err == nil {
		return 0
	}
	var ce *cliError
	if !errors.As(r.err, &ce) {
		t.Fatalf("init returned %T (%v), want a *cliError", r.err, r.err)
	}
	return ce.code
}

// clientConfig is the client config init wrote, or false if it wrote none.
func (r initRun) clientConfig(t *testing.T) (config.Config, bool) {
	t.Helper()
	if _, err := os.Stat(r.cfgPath); err != nil {
		return config.Config{}, false
	}
	cfg, err := config.Load(r.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// init stores its keys under the context it deployed to: DOCKER_HOST makes
	// that "default".
	return cfg.ForContext("default"), true
}

// runInitOn runs `init args...` against f (nil: a manager nobody answers on).
// dockerConfig, when not empty, is this machine's Docker config.json — where
// init reads registry credentials from.
func runInitOn(t *testing.T, f *swarmfake.Swarm, dockerConfig string, args ...string) initRun {
	t.Helper()
	return runInitSeeded(t, f, dockerConfig, "", args...)
}

// runInitSeeded is runInitOn with a client config file already in place
// (seed, when not empty, written 0644 on purpose: init must tighten it).
func runInitSeeded(t *testing.T, f *swarmfake.Swarm, dockerConfig, seed string, args ...string) initRun {
	t.Helper()
	var host string
	if f != nil {
		api := swarmfake.Serve(t, f)
		host = "tcp://" + strings.TrimPrefix(api.URL, "http://")
	} else {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		host = "tcp://" + l.Addr().String()
		l.Close() // closed: the address refuses connections
	}
	t.Setenv("DOCKER_HOST", host)
	t.Setenv("DOCKER_CONTEXT", "")

	// The docker CLI caches its config directory for the process, so the
	// environment alone would not reach it: set it, and put it back.
	dir := t.TempDir()
	if dockerConfig != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(dockerConfig), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prev := cliconfig.Dir()
	cliconfig.SetDir(dir)
	t.Cleanup(func() { cliconfig.SetDir(prev) })
	t.Setenv("DOCKER_CONFIG", dir)

	g := &globalFlags{configPath: filepath.Join(t.TempDir(), "config.yaml")}
	if seed != "" {
		if err := os.WriteFile(g.configPath, []byte(seed), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(g.configPath, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := newInitCmd(g)
	cmd.SetArgs(args)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return initRun{out: strings.ReplaceAll(out.String(), host, "HOST"), err: err, cfgPath: g.configPath}
}

// manager is a swarm whose /info says it is a manager of three nodes.
func manager() *swarmfake.Swarm {
	return &swarmfake.Swarm{Info: &system.Info{Name: "manager-1", Swarm: swarm.Info{ControlAvailable: true, Nodes: 3}}}
}

// defaultInitFlags are the flag values init runs with when none are given.
func defaultInitFlags() *initFlags {
	return &initFlags{image: defaultAgentImage, serviceName: defaultServiceNm, port: config.DefaultPort, allowLegacy: true}
}

// wantSpec checks a service spec the fake received against the one expected,
// compared as the JSON the API carries. The Docker client sends an image
// reference in its familiar form, so the default image arrives without its
// docker.io/ prefix.
func wantSpec(t *testing.T, got, want swarm.ServiceSpec) {
	t.Helper()
	if cs := want.TaskTemplate.ContainerSpec; cs != nil && cs.Image == defaultAgentImage {
		cs.Image = strings.TrimPrefix(defaultAgentImage, "docker.io/")
	}
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if !bytes.Equal(g, w) {
		t.Errorf("service spec differs:\n got %s\nwant %s", g, w)
	}
}

func wantOut(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("output differs:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// existingAgent is an agent init deployed earlier, edited by hand since: an
// env var and a prune schedule (which the TUI sets as labels).
func existingAgent(f *swarmfake.Swarm) {
	spec := agentServiceSpec(defaultInitFlags(), "secret-old")
	spec.TaskTemplate.ContainerSpec.Env = []string{"HAND_EDIT=1"}
	spec.TaskTemplate.ContainerSpec.Labels = prunesched.Default().Labels()
	var svc swarm.Service
	svc.ID, svc.Version.Index, svc.Spec = "agent1", 5, spec
	f.Services = append(f.Services, svc)
	var sec swarm.Secret
	sec.ID, sec.Spec.Name, sec.Spec.Data = "secret-old", agentSecretName, []byte("old")
	f.Secrets = append(f.Secrets, sec)
}

const initReady = "\n✓ ready — try:\n  swarmexec ps\n  swarmexec ui\n"

// A first init: a generated secret, the agent service as agentServiceSpec
// builds it from the defaults, and a client config holding that secret.
func TestInitFreshDeploy(t *testing.T) {
	f := manager()
	r := runInitOn(t, f, "", "--wait=false")
	if r.err != nil {
		t.Fatalf("init: %v\n%s", r.err, r.out)
	}
	wantOut(t, r.out, "using $DOCKER_HOST (HOST); unset it or set --context to pick a context\n"+
		"deploying agents into Docker context \"default\" (HOST)\n"+
		"manager: manager-1 (3 nodes)\n"+
		"[1/3] shared secret … created\n"+
		"[2/3] agent service on port 9443 … created (\"swarmexec_agent\", global)\n"+
		"        image: logleio/swarmexec-agent:latest\n"+
		"client config: "+r.cfgPath+"\n"+initReady)

	if len(f.Secrets) != 1 || f.Secrets[0].Spec.Labels[agentRoleLabel] != agentRoleValue {
		t.Fatalf("secrets = %+v, want one with the role label", f.Secrets)
	}
	if len(f.Services) != 1 {
		t.Fatalf("services = %+v", f.Services)
	}
	wantSpec(t, f.Services[0].Spec, agentServiceSpec(defaultInitFlags(), f.Secrets[0].ID))
	if len(f.RegistryAuth) != 1 || f.RegistryAuth[0] != "" {
		t.Errorf("registry auth = %q, want none for a public image without credentials", f.RegistryAuth)
	}
	cfg, ok := r.clientConfig(t)
	if !ok {
		t.Fatal("no client config written")
	}
	if cfg.AgentSecret != string(f.Secrets[0].Spec.Data) || cfg.AgentSecret == "" {
		t.Errorf("config secret %q, cluster secret %q", cfg.AgentSecret, f.Secrets[0].Spec.Data)
	}
	if !cfg.Insecure || cfg.AddrMode != config.AddrModeIP || cfg.Port != 9443 {
		t.Errorf("config = insecure %v, addr mode %q, port %d", cfg.Insecure, cfg.AddrMode, cfg.Port)
	}
}

// With --wait (the default) init follows the rollout until every agent runs.
func TestInitWaitsForTheRollout(t *testing.T) {
	f := manager()
	for _, id := range []string{"t1", "t2"} {
		var task swarm.Task
		task.ID, task.ServiceID = id, "service-swarmexec_agent" // the fake's id for the service init creates
		task.DesiredState, task.Status.State = swarm.TaskStateRunning, swarm.TaskStateRunning
		f.Tasks = append(f.Tasks, task)
	}
	r := runInitOn(t, f, "", "--secret", "s3cret")
	if r.err != nil {
		t.Fatalf("init: %v\n%s", r.err, r.out)
	}
	wantOut(t, r.out, "using $DOCKER_HOST (HOST); unset it or set --context to pick a context\n"+
		"deploying agents into Docker context \"default\" (HOST)\n"+
		"manager: manager-1 (3 nodes)\n"+
		"[1/3] shared secret … created\n"+
		"[2/3] agent service on port 9443 … created (\"swarmexec_agent\", global)\n"+
		"        image: logleio/swarmexec-agent:latest\n"+
		"[3/3] starting agents … \n"+
		"      agents: 2/2 running\n"+
		"client config: "+r.cfgPath+"\n"+initReady)
	if string(f.Secrets[0].Spec.Data) != "s3cret" {
		t.Errorf("--secret not used for the new secret: %q", f.Secrets[0].Spec.Data)
	}
	if cfg, _ := r.clientConfig(t); cfg.AgentSecret != "s3cret" {
		t.Errorf("config secret = %q", cfg.AgentSecret)
	}
}

// An agent already deployed is left alone without --force: the secret is
// reused, the service refused with a usage error, and nothing is written.
func TestInitRefusesAnExistingAgentWithoutForce(t *testing.T) {
	f := manager()
	existingAgent(f)
	r := runInitOn(t, f, "", "--wait=false")
	if c := r.code(t); c != exitUsage {
		t.Errorf("exit code %d, want %d", c, exitUsage)
	}
	if want := `service "swarmexec_agent" already exists; re-run with --force to update it`; r.err.Error() != want {
		t.Errorf("error = %q, want %q", r.err, want)
	}
	wantOut(t, r.out, "using $DOCKER_HOST (HOST); unset it or set --context to pick a context\n"+
		"deploying agents into Docker context \"default\" (HOST)\n"+
		"manager: manager-1 (3 nodes)\n"+
		"[1/3] shared secret … reusing existing\n"+
		"[2/3] agent service on port 9443 … already exists\n")
	if len(f.Secrets) != 1 || len(f.Services) != 1 || f.Services[0].Version.Index != 5 {
		t.Errorf("init without --force changed the cluster: %d secrets, %d services, version %d",
			len(f.Secrets), len(f.Services), f.Services[0].Version.Index)
	}
	if _, ok := r.clientConfig(t); ok {
		t.Error("a refused init wrote the client config")
	}
}

// --force rebuilds the spec from the flags: hand edits are dropped (as
// agent-update advice says), the prune schedule is carried over, and the
// rollout reports a failing task's reason, then gives up at the timeout.
func TestInitForceReplacesTheSpec(t *testing.T) {
	f := manager()
	existingAgent(f)
	var ok, bad swarm.Task
	ok.ID, ok.ServiceID, ok.DesiredState, ok.Status.State = "t1", "agent1", swarm.TaskStateRunning, swarm.TaskStateRunning
	bad.ID, bad.ServiceID, bad.DesiredState, bad.Status.State = "t2", "agent1", swarm.TaskStateRunning, swarm.TaskStateRejected
	bad.Status.Err = "port already in use"
	f.Tasks = []swarm.Task{ok, bad}

	r := runInitOn(t, f, "", "--force", "--rollout-timeout=0")
	if r.err != nil {
		t.Fatalf("init: %v\n%s", r.err, r.out)
	}
	wantOut(t, r.out, "using $DOCKER_HOST (HOST); unset it or set --context to pick a context\n"+
		"deploying agents into Docker context \"default\" (HOST)\n"+
		"manager: manager-1 (3 nodes)\n"+
		"[1/3] shared secret … reusing existing\n"+
		"[2/3] agent service on port 9443 … (keeping the prune schedule) updated (\"swarmexec_agent\")\n"+
		"        image: logleio/swarmexec-agent:latest\n"+
		"[3/3] starting agents … \n"+
		"      agents: 1/2 running — port already in use\n"+
		"      (timeout; check `docker service ps agent1`)\n"+
		"note: reused an existing secret whose value is unknown — set `agent_secret` in your config manually, or re-run with --secret\n"+
		initReady)

	want := agentServiceSpec(defaultInitFlags(), "secret-old")
	want.TaskTemplate.ContainerSpec.Labels = prunesched.Default().Labels()
	wantSpec(t, f.Services[0].Spec, want)
	if f.Services[0].Version.Index != 6 {
		t.Errorf("version = %d, want the update to have landed on 5", f.Services[0].Version.Index)
	}
	if len(f.Secrets) != 1 {
		t.Errorf("secrets = %d, want the existing one reused", len(f.Secrets))
	}
	if _, ok := r.clientConfig(t); ok {
		t.Error("init wrote a client config without knowing the secret")
	}
}

// --secret on a cluster that already has one: the cluster keeps its secret,
// the operator's value goes into the client config, and init says so.
func TestInitWarnsWhenSecretIsNotApplied(t *testing.T) {
	f := manager()
	existingAgent(f)
	f.Services[0].Spec.TaskTemplate.ContainerSpec.Labels = nil // no schedule this time
	r := runInitOn(t, f, "", "--force", "--wait=false", "--secret", "mine")
	if r.err != nil {
		t.Fatalf("init: %v\n%s", r.err, r.out)
	}
	wantOut(t, r.out, "using $DOCKER_HOST (HOST); unset it or set --context to pick a context\n"+
		"deploying agents into Docker context \"default\" (HOST)\n"+
		"manager: manager-1 (3 nodes)\n"+
		"[1/3] shared secret … reusing existing\n"+
		"warning: secret \"swarmexec_agent_secret\" already exists and is kept as-is — your --secret value was NOT applied to the cluster.\n"+
		"         It will be written to the client config, so it must equal the existing secret's value or the agent will reject you.\n"+
		"         To change the cluster secret, remove it first (no service may reference it) and re-run init.\n"+
		"[2/3] agent service on port 9443 … updated (\"swarmexec_agent\")\n"+
		"        image: logleio/swarmexec-agent:latest\n"+
		"client config: "+r.cfgPath+"\n"+initReady)
	if string(f.Secrets[0].Spec.Data) != "old" {
		t.Error("the cluster secret was changed")
	}
	if cfg, _ := r.clientConfig(t); cfg.AgentSecret != "mine" {
		t.Errorf("config secret = %q, want the --secret value", cfg.AgentSecret)
	}
}

// Every flag that reaches the spec, together: image, port, name, metrics,
// the legacy-secret lever and a policy, which becomes a config the service
// mounts. A second run with the same policy reuses that config.
func TestInitFlagsShapeTheSpec(t *testing.T) {
	policyFile := filepath.Join(t.TempDir(), "policy.yaml")
	policyData := []byte("default: allow\nrules:\n  - deny: [exec]\n")
	if err := os.WriteFile(policyFile, policyData, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--image", "reg.example/agent:2", "--registry-auth=false", "--port", "9555",
		"--metrics-port", "9464", "--service-name", "myagent", "--allow-legacy-secret=false",
		"--policy-file", policyFile, "--save-config=false", "--wait=false"}
	f := manager()
	r := runInitOn(t, f, "", args...)
	if r.err != nil {
		t.Fatalf("init: %v\n%s", r.err, r.out)
	}
	wantOut(t, r.out, "using $DOCKER_HOST (HOST); unset it or set --context to pick a context\n"+
		"deploying agents into Docker context \"default\" (HOST)\n"+
		"manager: manager-1 (3 nodes)\n"+
		"[1/3] shared secret … created\n"+
		"[2/3] agent service on port 9555 … created (\"myagent\", global)\n"+
		"        image: reg.example/agent:2\n"+
		initReady)

	sum := sha256.Sum256(policyData)
	cfgName := policyConfigPrefix + hex.EncodeToString(sum[:])[:12]
	if len(f.Configs) != 1 || f.Configs[0].Spec.Name != cfgName || !bytes.Equal(f.Configs[0].Spec.Data, policyData) {
		t.Fatalf("configs = %+v, want the policy as %s", f.Configs, cfgName)
	}
	want := agentServiceSpec(&initFlags{image: "reg.example/agent:2", serviceName: "myagent", port: 9555,
		metricsPort: 9464, policyFile: policyFile}, f.Secrets[0].ID)
	want.TaskTemplate.ContainerSpec.Configs = []*swarm.ConfigReference{{
		ConfigID: f.Configs[0].ID, ConfigName: cfgName,
		File: &swarm.ConfigReferenceFileTarget{Name: deploy.PolicyPath, UID: "0", GID: "0", Mode: 0o444},
	}}
	wantSpec(t, f.Services[0].Spec, want)
	if _, ok := r.clientConfig(t); ok {
		t.Error("--save-config=false wrote the client config")
	}

	// Same policy again: the content-named config is found, not re-created.
	r = runInitOn(t, f, "", append(args, "--force")...)
	if r.err != nil {
		t.Fatalf("second init: %v\n%s", r.err, r.out)
	}
	if len(f.Configs) != 1 {
		t.Errorf("configs = %d after a re-run with the same policy, want 1", len(f.Configs))
	}
	wantSpec(t, f.Services[0].Spec, want)
}

// Registry credentials: init distributes this machine's `docker login` for
// the image's registry, refuses a private image it has none for, and sends
// nothing with --registry-auth=false.
func TestInitRegistryAuth(t *testing.T) {
	const image = "reg.example/agent:2"

	r := runInitOn(t, manager(), "", "--image", image, "--wait=false")
	if c := r.code(t); c != exitUsage {
		t.Errorf("no credentials: exit %d, want %d", c, exitUsage)
	}
	if !strings.Contains(r.err.Error(), "no registry credentials for reg.example found") ||
		!strings.HasSuffix(r.out, "[2/3] agent service on port 9443 … failed\n") {
		t.Errorf("no credentials: %v\n%s", r.err, r.out)
	}

	login := base64.StdEncoding.EncodeToString([]byte("bob:pw"))
	f := manager()
	r = runInitOn(t, f, `{"auths":{"reg.example":{"auth":"`+login+`"}}}`, "--image", image, "--wait=false")
	if r.err != nil {
		t.Fatalf("with credentials: %v\n%s", r.err, r.out)
	}
	if len(f.RegistryAuth) != 1 {
		t.Fatalf("registry auth = %q", f.RegistryAuth)
	}
	raw, err := base64.URLEncoding.DecodeString(f.RegistryAuth[0])
	if err != nil || !strings.Contains(string(raw), `"username":"bob"`) {
		t.Errorf("registry auth header = %s (%v), want bob's login", raw, err)
	}

	f = manager()
	if r = runInitOn(t, f, "", "--image", image, "--registry-auth=false", "--wait=false"); r.err != nil {
		t.Fatalf("--registry-auth=false: %v", r.err)
	}
	if len(f.RegistryAuth) != 1 || f.RegistryAuth[0] != "" {
		t.Errorf("--registry-auth=false sent %q", f.RegistryAuth)
	}
}

// The checks before anything is written: each refusal is a usage error, and
// the cluster is untouched.
func TestInitPreflightRefusals(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("default: maybe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	publishing := func(name string, port uint32) swarm.Service {
		var s swarm.Service
		s.ID, s.Spec.Name = name, name
		s.Spec.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{{PublishedPort: port, PublishMode: swarm.PortConfigPublishModeHost}}}
		return s
	}
	cases := []struct {
		name    string
		info    bool // the fake answers as a manager
		taken   uint32
		args    []string
		wantErr string
	}{
		{"not a manager", false, 0, nil, `context "default" is not a Swarm manager (pick a manager context, or set --context)`},
		{"unreadable policy", true, 0, []string{"--policy-file", "/nonexistent/policy.yaml"}, "read --policy-file: "},
		{"invalid policy", true, 0, []string{"--policy-file", bad}, "--policy-file " + bad + ": "},
		{"metrics on the agent port", true, 0, []string{"--metrics-port", "9443"}, "--metrics-port 9443 is the agent port; pick another"},
		{"agent port taken", true, 9443, nil, `host port 9443 is already published by service "web" — remove it`},
		{"metrics port taken", true, 9464, []string{"--metrics-port", "9464"}, `host port 9464 is already published by service "web" — pick another --metrics-port`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &swarmfake.Swarm{}
			if c.info {
				f = manager()
			}
			if c.taken > 0 {
				f.Services = []swarm.Service{publishing("web", c.taken)}
			}
			r := runInitOn(t, f, "", append([]string{"--wait=false"}, c.args...)...)
			if code := r.code(t); code != exitUsage {
				t.Errorf("exit %d, want %d", code, exitUsage)
			}
			if !strings.HasPrefix(r.err.Error(), c.wantErr) {
				t.Errorf("error = %q, want it to start %q", r.err, c.wantErr)
			}
			if strings.Contains(r.out, "[1/3]") || len(f.Secrets) != 0 || len(f.Configs) != 0 || len(f.Services) > 1 {
				t.Errorf("a refused init started writing:\n%s", r.out)
			}
		})
	}
}

// A manager that cannot be reached, and writes the manager refuses: each is
// a transport failure, reported on the step it hit.
func TestInitFailures(t *testing.T) {
	r := runInitOn(t, nil, "")
	if c := r.code(t); c != session.TransportFailure {
		t.Errorf("unreachable: exit %d, want %d", c, session.TransportFailure)
	}
	if !strings.HasPrefix(r.err.Error(), "query Docker manager: ") {
		t.Errorf("unreachable: %v", r.err)
	}

	policyFile := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(policyFile, []byte("default: allow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		existing bool
		fail     string
		args     []string
		wantErr  string
		wantTail string
	}{
		{"secret create", false, "POST /secrets/create", nil, "create secret: ", "[1/3] shared secret … failed\n"},
		{"secret list", false, "GET /secrets", nil, "list secrets: ", "[1/3] shared secret … failed\n"},
		{"policy config", false, "POST /configs/create", []string{"--policy-file", policyFile}, "create policy config: ",
			"[2/3] agent service on port 9443 … failed\n"},
		{"service create", false, "POST /services/create", nil, "create agent service: ", "[2/3] agent service on port 9443 … failed\n"},
		{"service update", true, "POST /services/agent1/update", []string{"--force"}, "update agent service: ",
			"[2/3] agent service on port 9443 … (keeping the prune schedule) failed\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := manager()
			if c.existing {
				existingAgent(f)
			}
			f.Fail = map[string]bool{c.fail: true}
			r := runInitOn(t, f, "", append([]string{"--wait=false"}, c.args...)...)
			if code := r.code(t); code != session.TransportFailure {
				t.Errorf("exit %d, want %d", code, session.TransportFailure)
			}
			if !strings.HasPrefix(r.err.Error(), c.wantErr) {
				t.Errorf("error = %q, want it to start %q", r.err, c.wantErr)
			}
			if !strings.HasSuffix(r.out, c.wantTail) {
				t.Errorf("output must end %q:\n%s", c.wantTail, r.out)
			}
			if _, ok := r.clientConfig(t); ok {
				t.Error("a failed init wrote the client config")
			}
		})
	}
}

// The SEC-06 regression: init against a second cluster used to replace the
// one global agent_secret, so the first cluster rejected every call after it.
func TestInitSecondClusterKeepsTheFirstClustersSecret(t *testing.T) {
	const seed = "# my config\nagent_secret: global-made-up\ncontexts:\n  prod:\n    agent_secret: prod-made-up\n    insecure: true\n"
	t.Setenv("SWARMEXEC_CA", "/made/up/ca.pem") // env value: must not reach the file
	t.Setenv("SWARMEXEC_OPERATOR", "env-operator")
	r := runInitSeeded(t, manager(), "", seed, "--wait=false", "--secret", "staging-made-up")
	if r.err != nil {
		t.Fatalf("init: %v\n%s", r.err, r.out)
	}
	raw, err := os.ReadFile(r.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"/made/up/ca.pem", "env-operator"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("config file persisted %q:\n%s", bad, raw)
		}
	}
	if !strings.Contains(string(raw), "# my config") {
		t.Errorf("comment lost:\n%s", raw)
	}
	if fi, err := os.Stat(r.cfgPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v; want 0600 on the pre-existing file", fi.Mode().Perm(), err)
	}
	cfg, err := config.Load(r.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ForContext("prod").AgentSecret; got != "prod-made-up" {
		t.Errorf("first cluster's secret = %q, want it untouched", got)
	}
	if got := cfg.ForContext("default").AgentSecret; got != "staging-made-up" {
		t.Errorf("second cluster's secret = %q", got)
	}
	if cfg.AgentSecret != "global-made-up" {
		t.Errorf("global secret = %q, want it untouched", cfg.AgentSecret)
	}
}
