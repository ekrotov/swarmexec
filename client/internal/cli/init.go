// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	cliconfig "github.com/docker/cli/cli/config"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/session"
	"swarmexec/client/internal/swarmops"
	cterm "swarmexec/client/internal/term"
	"swarmexec/internal/deploy"
	"swarmexec/internal/policy"
	"swarmexec/internal/prunesched"
)

// The deployment vocabulary lives in internal/deploy, which the agent imports
// too; these are local names for it, so the call sites below stay readable.
const (
	defaultAgentImage = "docker.io/logleio/swarmexec-agent:latest"
	defaultServiceNm  = deploy.ServiceName
	agentSecretName   = deploy.SecretName
	agentRoleLabel    = deploy.RoleLabel
	agentRoleValue    = deploy.RoleAgent
)

// The agent-error explanations live in agenterr; these are local names for
// them, so the many call sites in this package stay short.
var (
	errNoAgent        = agenterr.ErrNoAgent
	errAgentTooOld    = agenterr.ErrAgentTooOld
	errSecretRejected = agenterr.ErrSecretRejected
)

type initFlags struct {
	image          string
	secret         string
	serviceName    string
	port           int
	force          bool
	allowLegacy    bool
	metricsPort    int
	policyFile     string
	saveConfig     bool
	registryAuth   bool
	wait           bool
	rolloutTimeout time.Duration
}

func newInitCmd(g *globalFlags) *cobra.Command {
	f := &initFlags{}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Provision the agent on every node of the swarm (self-signed + shared secret)",
		Long: "init deploys the swarmexec agent as a global Swarm service via the\n" +
			"manager API: it creates a shared-secret Docker secret, runs the agent in\n" +
			"self-signed mode on every node (host port 9443), and writes the matching\n" +
			"client config so exec/logs/volume work immediately.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, g, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.image, "image", defaultAgentImage, "agent image to deploy")
	fl.StringVar(&f.secret, "secret", "", "shared secret to use (default: generate a random one)")
	fl.StringVar(&f.serviceName, "service-name", defaultServiceNm, "name for the agent service")
	fl.IntVar(&f.port, "port", config.DefaultPort, "host port the agent publishes")
	fl.BoolVar(&f.force, "force", false, "update the service if it already exists")
	// Default true to match the agent's own default, so `init` never silently
	// locks out a client the operator has not upgraded yet. Passing false is the
	// deprecation lever: once every client sends the connection-bound proof, the
	// agent should stop accepting the raw secret at all.
	fl.BoolVar(&f.allowLegacy, "allow-legacy-secret", true,
		"accept the raw shared secret from clients predating connection-bound auth; pass=false once all clients are upgraded")
	fl.IntVar(&f.metricsPort, "metrics-port", 0,
		"publish the agent's Prometheus metrics on this host port (0 = off); plain HTTP, counts only")
	fl.StringVar(&f.policyFile, "policy-file", "",
		"authorization rule file for the agents (YAML, checked before deploying); without it every authenticated request is allowed")
	fl.BoolVar(&f.saveConfig, "save-config", true, "write the client config (~/.config/swarmexec/config.yaml)")
	fl.BoolVar(&f.registryAuth, "registry-auth", true, "pass local registry credentials so nodes can pull a private image")
	fl.BoolVar(&f.wait, "wait", true, "wait for the agents to come up and report progress")
	fl.DurationVar(&f.rolloutTimeout, "rollout-timeout", 90*time.Second, "how long to wait for agents to start")
	return cmd
}

// runInit deploys the agent: connect to a manager, plan (every check that can
// refuse, before anything is written), then apply the plan step by step.
//
// It was one 190-line function holding all of that (review Q-01). The order
// of the writes is unchanged and is part of the behaviour: the secret is
// created before the registry credentials are read, so a re-run after a
// failed step 2 reuses the secret step 1 made.
func runInit(cmd *cobra.Command, g *globalFlags, f *initFlags) error {
	ctx := cmdContext(cmd)
	out := cmd.OutOrStdout()
	dcli, ctxName, err := connectInitManager(ctx, out, g)
	if err != nil {
		return err
	}
	plan, err := planInit(ctx, dcli, f)
	if err != nil {
		return err
	}
	d := &initDeploy{ctx: ctx, dcli: dcli, out: out, f: f, plan: plan}

	secret, err := d.sharedSecret() // [1/3]
	if err != nil {
		return err
	}
	serviceID, err := d.agentService(secret.id) // [2/3]
	if err != nil {
		return err
	}
	if img := inspectServiceImage(ctx, dcli, serviceID); img != "" {
		fmt.Fprintf(out, "        image: %s\n", img)
	}
	if f.wait { // [3/3]
		d.step(3, "starting agents")
		fmt.Fprintln(out)
		waitRollout(ctx, dcli, serviceID, out, f.rolloutTimeout)
	}
	if f.saveConfig {
		d.saveClientConfig(g.configPath, ctxName, secret)
	}

	fmt.Fprintf(out, "\n✓ ready — try:\n  swarmexec ps\n  swarmexec ui\n")
	return nil
}

// connectInitManager picks the Docker context, connects to it and makes sure
// it is a Swarm manager, saying which as it goes.
func connectInitManager(ctx context.Context, out io.Writer, g *globalFlags) (*client.Client, string, error) {
	tctx, cerr := resolveContext(out, g)
	if cerr != nil {
		return nil, "", cerr
	}
	dcli, err := newDockerClient(ctx, tctx.Name)
	if err != nil {
		return nil, "", &cliError{code: usageExitCode, err: err}
	}
	fmt.Fprintf(out, "deploying agents into Docker context %q (%s)\n", tctx.Name, hostOrDefault(tctx.Host))

	infoRes, err := dcli.Info(ctx, client.InfoOptions{})
	info := infoRes.Info
	if err != nil {
		return nil, "", &cliError{code: session.TransportFailure, err: fmt.Errorf("query Docker manager: %w", err)}
	}
	if !info.Swarm.ControlAvailable {
		return nil, "", &cliError{code: usageExitCode, err: fmt.Errorf("context %q is not a Swarm manager (pick a manager context, or set --context)", tctx.Name)}
	}
	fmt.Fprintf(out, "manager: %s (%s)\n", info.Name, plural(info.Swarm.Nodes, "node"))
	return dcli, tctx.Name, nil
}

// initPlan is what init settles before it writes anything. Everything that
// can refuse the flags is checked while planning, so a refusal leaves the
// cluster as it was.
type initPlan struct {
	policy    []byte // the --policy-file contents, already checked; nil without one
	newSecret string // the value the shared secret gets if init has to create it
}

func planInit(ctx context.Context, dcli swarmops.ServiceLister, f *initFlags) (initPlan, error) {
	var p initPlan
	// Pre-flight: a policy that the agents would refuse must fail HERE, on this
	// machine, not as an agent that will not start on every node. The agents
	// init deploys use a shared secret and no client CA, so they cannot verify
	// identities — checked exactly as they will check it.
	if f.policyFile != "" {
		b, err := os.ReadFile(f.policyFile)
		if err != nil {
			return p, &cliError{code: usageExitCode, err: fmt.Errorf("read --policy-file: %w", err)}
		}
		if _, err := policy.Parse(b, false); err != nil {
			return p, &cliError{code: usageExitCode, err: fmt.Errorf("--policy-file %s: %w", f.policyFile, err)}
		}
		p.policy = b
	}
	if err := checkInitPorts(ctx, dcli, f); err != nil {
		return p, err
	}
	p.newSecret = f.secret
	if p.newSecret == "" {
		p.newSecret = generateSecret()
	}
	return p, nil
}

// checkInitPorts fails fast on a host-port conflict before anything is
// created: the global agent could not bind a port another service publishes.
func checkInitPorts(ctx context.Context, dcli swarmops.ServiceLister, f *initFlags) error {
	if f.metricsPort == f.port {
		return &cliError{code: usageExitCode, err: fmt.Errorf("--metrics-port %d is the agent port; pick another", f.metricsPort)}
	}
	if other, ok := portConflict(ctx, dcli, f.port, f.serviceName); ok {
		return &cliError{code: usageExitCode, err: fmt.Errorf(
			"host port %d is already published by service %q — remove it (`docker service rm %s`, or `swarmexec down` if it's an old swarmexec agent) or pick another --port",
			f.port, other, other)}
	}
	if f.metricsPort > 0 {
		if other, ok := portConflict(ctx, dcli, f.metricsPort, f.serviceName); ok {
			return &cliError{code: usageExitCode, err: fmt.Errorf(
				"host port %d is already published by service %q — pick another --metrics-port", f.metricsPort, other)}
		}
	}
	return nil
}

// initDeploy applies an initPlan to the cluster, one numbered step at a time.
type initDeploy struct {
	ctx  context.Context
	dcli *client.Client
	out  io.Writer
	f    *initFlags
	plan initPlan
}

const initSteps = 3 // secret, service, rollout/config

// step opens a step's line; its outcome is printed at the end of it.
func (d *initDeploy) step(n int, label string) {
	fmt.Fprintf(d.out, "[%d/%d] %s … ", n, initSteps, label)
}

// failed ends the current step's line and makes err init's result.
func (d *initDeploy) failed(code int, err error) error {
	fmt.Fprintln(d.out, "failed")
	return &cliError{code: code, err: err}
}

// initSecret is the shared secret the agents are deployed with.
type initSecret struct {
	id    string
	value string
	// known: value is the cluster's secret. Only if init just created it, or
	// the operator passed it explicitly (Docker secrets are write-only, so an
	// existing one's value cannot be read back).
	known bool
}

// sharedSecret is step 1: create the shared secret, or reuse the one there.
func (d *initDeploy) sharedSecret() (initSecret, error) {
	d.step(1, "shared secret")
	id, created, err := ensureSecret(d.ctx, d.dcli, agentSecretName, d.plan.newSecret)
	if err != nil {
		return initSecret{}, d.failed(session.TransportFailure, err)
	}
	s := initSecret{id: id, value: d.plan.newSecret, known: created || d.f.secret != ""}
	if created {
		fmt.Fprintln(d.out, "created")
		return s, nil
	}
	fmt.Fprintln(d.out, "reusing existing")
	if d.f.secret != "" {
		// The existing secret is kept (Docker secrets are immutable), so the
		// --secret value is NOT applied to the cluster — yet it IS written to the
		// client config below. If it isn't the existing secret's actual value the
		// agent will reject the client, so warn loudly.
		fmt.Fprintf(d.out, "warning: secret %q already exists and is kept as-is — your --secret value was NOT applied to the cluster.\n"+
			"         It will be written to the client config, so it must equal the existing secret's value or the agent will reject you.\n"+
			"         To change the cluster secret, remove it first (no service may reference it) and re-run init.\n", agentSecretName)
	}
	return s, nil
}

// agentService is step 2: create the agent service, or with --force replace
// the existing one's spec. Returns the service's id.
func (d *initDeploy) agentService(secretID string) (string, error) {
	d.step(2, fmt.Sprintf("agent service on port %d", d.f.port))
	spec := agentServiceSpec(d.f, secretID)
	if d.plan.policy != nil {
		ref, err := ensurePolicyConfig(d.ctx, d.dcli, d.plan.policy)
		if err != nil {
			return "", d.failed(session.TransportFailure, err)
		}
		spec.TaskTemplate.ContainerSpec.Configs = []*swarm.ConfigReference{ref}
	}
	auth, err := d.registryAuth()
	if err != nil {
		return "", err
	}
	existing, err := swarmops.ServiceByName(d.ctx, d.dcli, d.f.serviceName)
	if err != nil {
		return "", d.failed(session.TransportFailure, err)
	}
	switch {
	case existing == nil:
		resp, err := d.dcli.ServiceCreate(d.ctx, client.ServiceCreateOptions{Spec: spec, EncodedRegistryAuth: auth, QueryRegistry: true})
		if err != nil {
			return "", d.failed(session.TransportFailure, fmt.Errorf("create agent service: %w", err))
		}
		fmt.Fprintf(d.out, "created (%q, global)\n", d.f.serviceName)
		return resp.ID, nil
	case d.f.force:
		if carryPruneSchedule(&spec, existing) {
			fmt.Fprint(d.out, "(keeping the prune schedule) ")
		}
		if _, err := d.dcli.ServiceUpdate(d.ctx, existing.ID, client.ServiceUpdateOptions{Version: existing.Version, Spec: spec, EncodedRegistryAuth: auth, QueryRegistry: true}); err != nil {
			return "", d.failed(session.TransportFailure, fmt.Errorf("update agent service: %w", err))
		}
		fmt.Fprintf(d.out, "updated (%q)\n", d.f.serviceName)
		return existing.ID, nil
	default:
		fmt.Fprintln(d.out, "already exists")
		return "", &cliError{code: usageExitCode, err: fmt.Errorf("service %q already exists; re-run with --force to update it", d.f.serviceName)}
	}
}

// registryAuth is the encoded credentials the nodes pull the image with: this
// machine's `docker login` for the image's registry, or "" with
// --registry-auth=false and for a public Docker Hub image.
func (d *initDeploy) registryAuth() (string, error) {
	if !d.f.registryAuth {
		return "", nil
	}
	auth, err := encodedRegistryAuth(d.f.image)
	host := registryHost(d.f.image)
	switch {
	case err != nil:
		// Don't deploy blind: without distributed credentials the manager may
		// pull fine (image cached / logged in) while every worker reports the
		// image as unavailable.
		return "", d.failed(usageExitCode, fmt.Errorf("read local registry credentials for %s: %w — run `docker login %s` on THIS machine (where you run swarmexec, not the manager); init distributes them to the swarm nodes. Or pass --registry-auth=false for a public image", host, err, host))
	case auth == "" && host != "docker.io":
		return "", d.failed(usageExitCode, fmt.Errorf("no registry credentials for %s found in this machine's Docker config — init reads your LOCAL `docker login` (not the manager's) and distributes it to the swarm nodes so they can pull the private image %q. Run `docker login %s` here, or pass --registry-auth=false for a public image", host, d.f.image, host))
	}
	return auth, nil
}

// saveClientConfig records what the deployed agents accept under
// contexts.<ctxName> — if init knows the secret's value. It writes only the
// keys init is responsible for (secret, insecure, addr_mode, port), only into
// that context's block, and never anything that merely came from the
// environment or the rest of the file: a second cluster no longer replaces the
// first one's secret (SEC-06).
func (d *initDeploy) saveClientConfig(path, ctxName string, s initSecret) {
	if !s.known {
		fmt.Fprintln(d.out, "note: reused an existing secret whose value is unknown — set `agent_secret` in your config manually, or re-run with --secret")
		return
	}
	keys := config.ContextKeys{AgentSecret: s.value, Insecure: true, AddrMode: config.AddrModeIP, Port: d.f.port}
	if written, err := config.SetContextKeys(path, ctxName, keys); err != nil {
		fmt.Fprintf(d.out, "warning: could not write client config: %v\n", err)
	} else {
		fmt.Fprintf(d.out, "client config: %s\n", written)
	}
}

// resolveContext decides which Docker context to act on, informing the user and
// — when several exist and stdin is a terminal — prompting them to choose.
func resolveContext(out io.Writer, g *globalFlags) (dockerctx.Context, error) {
	// An explicit --context / $DOCKER_CONTEXT wins; no prompt. Same order as
	// dockerctx.ResolveHost, so the context named here is the daemon dialled.
	if explicit := firstNonEmpty(g.dockerContext, os.Getenv("DOCKER_CONTEXT")); explicit != "" {
		host, _ := dockerctx.ResolveHost(explicit)
		return dockerctx.Context{Name: explicit, Host: host, Current: true}, nil
	}
	// Then $DOCKER_HOST, as for every other command and for the docker CLI: it
	// overrides the active context. Skipping it here sent init/down to the
	// active context — a production cluster — while the operator had pointed
	// DOCKER_HOST at a test daemon. "default" is the context that resolves to it.
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		fmt.Fprintf(out, "using $DOCKER_HOST (%s); unset it or set --context to pick a context\n", h)
		return dockerctx.Context{Name: "default", Host: h, Current: true}, nil
	}

	contexts, err := dockerctx.List()
	if err != nil || len(contexts) == 0 {
		host, _ := dockerctx.ResolveHost("")
		return dockerctx.Context{Name: dockerctx.Current(), Host: host}, nil
	}

	current := dockerctx.Current()
	if len(contexts) == 1 {
		return contexts[0], nil // single context: just use it (announced by caller)
	}

	// Several contexts: prompt when interactive, else fall back to the active one
	// so scripts keep working.
	if !cterm.IsTerminal(os.Stdin.Fd()) {
		for _, c := range contexts {
			if c.Name == current {
				fmt.Fprintf(out, "multiple Docker contexts; using the active one %q (set --context to override)\n", c.Name)
				return c, nil
			}
		}
		return contexts[0], nil
	}
	return chooseContext(out, contexts, current)
}

// chooseContext prompts the operator to pick a context, defaulting to the active
// one on an empty answer.
func chooseContext(out io.Writer, contexts []dockerctx.Context, current string) (dockerctx.Context, error) {
	def := 0
	fmt.Fprintln(out, "Multiple Docker contexts found — choose which one to use:")
	for i, c := range contexts {
		marker := " "
		if c.Name == current {
			marker = "*"
			def = i
		}
		fmt.Fprintf(out, "  [%d]%s %-16s %s\n", i+1, marker, c.Name, hostOrDefault(c.Host))
	}
	fmt.Fprintf(out, "select [1-%d] (default %d=%s): ", len(contexts), def+1, contexts[def].Name)

	line, ok := promptLine("")
	if !ok {
		return contexts[def], nil
	}
	s := strings.TrimSpace(line)
	if s == "" {
		return contexts[def], nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > len(contexts) {
		return dockerctx.Context{}, &cliError{code: usageExitCode, err: fmt.Errorf("invalid selection %q", s)}
	}
	return contexts[n-1], nil
}

func hostOrDefault(h string) string {
	if h == "" {
		return "local socket"
	}
	return h
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// waitRollout polls the service's tasks until every desired task is running (or
// the timeout elapses), printing the running/desired count as it changes.
func waitRollout(ctx context.Context, dcli *client.Client, serviceID string, out io.Writer, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		tasksRes, err := dcli.TaskList(ctx, client.TaskListOptions{
			Filters: make(client.Filters).Add("service", serviceID),
		})
		tasks := tasksRes.Items
		if err != nil {
			fmt.Fprintf(out, "      (could not query tasks: %v)\n", err)
			return
		}
		msg, done := rolloutLine(tasks)
		if msg != last {
			fmt.Fprintf(out, "      agents: %s\n", msg)
			last = msg
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(out, "      (timeout; check `docker service ps %s`)\n", serviceID)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

// rolloutLine is the rollout's progress line — running/desired, and while it
// is short, the latest failed task's reason — and whether every desired task
// runs.
func rolloutLine(tasks []swarm.Task) (msg string, done bool) {
	desired, running := 0, 0
	var taskErr string
	for _, t := range tasks {
		if t.DesiredState == swarm.TaskStateRunning {
			desired++
		}
		switch t.Status.State {
		case swarm.TaskStateRunning:
			running++
		case swarm.TaskStateRejected, swarm.TaskStateFailed:
			if t.Status.Err != "" {
				taskErr = t.Status.Err
			} else if t.Status.Message != "" {
				taskErr = t.Status.Message
			}
		}
	}
	msg = fmt.Sprintf("%d/%d running", running, desired)
	if taskErr != "" && running < desired {
		msg += " — " + taskErr // surface e.g. "port already in use" instead of a silent timeout
	}
	return msg, desired > 0 && running >= desired
}

// ensureSecret returns the id of the named secret, creating it with createWith
// if it does not exist (Docker secrets are immutable, so an existing one is
// reused as-is).
func ensureSecret(ctx context.Context, dcli *client.Client, name, createWith string) (id string, created bool, err error) {
	listRes, err := dcli.SecretList(ctx, client.SecretListOptions{
		Filters: make(client.Filters).Add("name", name),
	})
	list := listRes.Items
	if err != nil {
		return "", false, fmt.Errorf("list secrets: %w", err)
	}
	for _, s := range list {
		if s.Spec.Name == name {
			return s.ID, false, nil
		}
	}
	resp, err := dcli.SecretCreate(ctx, client.SecretCreateOptions{Spec: swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: name, Labels: map[string]string{agentRoleLabel: agentRoleValue}},
		Data:        []byte(createWith),
	}})
	if err != nil {
		return "", false, fmt.Errorf("create secret: %w", err)
	}
	return resp.ID, true, nil
}

// policyConfigPrefix names the Docker configs holding agent policies. Configs
// are immutable, so each distinct policy is its own config, named by its
// content hash: re-running init with the same file reuses it, a changed file
// becomes a new one, and the service update points the agents at it.
const policyConfigPrefix = "swarmexec_agent_policy_"

func ensurePolicyConfig(ctx context.Context, dcli *client.Client, data []byte) (*swarm.ConfigReference, error) {
	sum := sha256.Sum256(data)
	name := policyConfigPrefix + hex.EncodeToString(sum[:])[:12]
	listRes, err := dcli.ConfigList(ctx, client.ConfigListOptions{Filters: make(client.Filters).Add("name", name)})
	list := listRes.Items
	if err != nil {
		return nil, fmt.Errorf("list configs: %w", err)
	}
	id := ""
	for _, c := range list {
		if c.Spec.Name == name {
			id = c.ID
		}
	}
	if id == "" {
		resp, err := dcli.ConfigCreate(ctx, client.ConfigCreateOptions{Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: name, Labels: map[string]string{agentRoleLabel: agentRoleValue}},
			Data:        data,
		}})
		if err != nil {
			return nil, fmt.Errorf("create policy config: %w", err)
		}
		id = resp.ID
	}
	return &swarm.ConfigReference{
		ConfigID:   id,
		ConfigName: name,
		File:       &swarm.ConfigReferenceFileTarget{Name: deploy.PolicyPath, UID: "0", GID: "0", Mode: 0o444},
	}, nil
}

// agentServiceSpec is the service `swarmexec init` deploys.
//
// It is the same agent as agent/deploy/agent-stack-selfsigned.yml provisions,
// not the same spec: the stack file binds with -listen=:9443 and attaches an
// overlay network, this publishes -port in host mode. The command line comes
// from internal/deploy, which the agent parses its flags from — see there for
// why that is not decoration.
func agentServiceSpec(f *initFlags, secretID string) swarm.ServiceSpec {
	args := deploy.AgentArgs(deploy.AgentOptions{Port: f.port, AllowLegacySecret: f.allowLegacy, MetricsPort: f.metricsPort, Policy: f.policyFile != ""})
	ports := []swarm.PortConfig{{
		Protocol:      network.TCP,
		TargetPort:    uint32(f.port),
		PublishedPort: uint32(f.port),
		PublishMode:   swarm.PortConfigPublishModeHost,
	}}
	if f.metricsPort > 0 {
		// Host mode like the agent port: there is no overlay network to scrape
		// over, so Prometheus reaches each node's agent at <node>:<port>.
		ports = append(ports, swarm.PortConfig{
			Protocol:      network.TCP,
			TargetPort:    uint32(f.metricsPort),
			PublishedPort: uint32(f.metricsPort),
			PublishMode:   swarm.PortConfigPublishModeHost,
		})
	}
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name:   f.serviceName,
			Labels: map[string]string{agentRoleLabel: agentRoleValue},
		},
		Mode:         swarm.ServiceMode{Global: &swarm.GlobalService{}},
		UpdateConfig: &swarm.UpdateConfig{Order: swarm.UpdateOrderStopFirst},
		TaskTemplate: swarm.TaskSpec{
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny},
			ContainerSpec: &swarm.ContainerSpec{
				Image: f.image,
				Args:  args,
				// Timings only: the probe command is the image's own HEALTHCHECK
				// (see deploy.HealthInterval for why the spec must not name it).
				Healthcheck: &container.HealthConfig{
					Interval:    deploy.HealthInterval,
					Timeout:     deploy.HealthTimeout,
					StartPeriod: deploy.HealthStartPeriod,
					Retries:     deploy.HealthRetries,
				},
				Mounts: []mount.Mount{{
					Type:   mount.TypeBind,
					Source: deploy.SocketPath,
					Target: deploy.SocketPath,
				}},
				Secrets: []*swarm.SecretReference{{
					SecretID:   secretID,
					SecretName: agentSecretName,
					File: &swarm.SecretReferenceFileTarget{
						Name: agentSecretName,
						UID:  "0",
						GID:  "0",
						Mode: 0o444,
					},
				}},
			},
		},
		EndpointSpec: &swarm.EndpointSpec{Ports: ports},
	}
}

// carryPruneSchedule copies the prune-schedule labels of the running agent
// into the spec init is about to write, and reports whether there were any.
// The schedule is set from the TUI or `prune-schedule set`, never from init's
// flags, so a re-init that rebuilt the spec from flags alone would switch it
// off without a word.
func carryPruneSchedule(spec *swarm.ServiceSpec, existing *swarm.Service) bool {
	if existing == nil || existing.Spec.TaskTemplate.ContainerSpec == nil || spec.TaskTemplate.ContainerSpec == nil {
		return false
	}
	old := existing.Spec.TaskTemplate.ContainerSpec.Labels
	if !prunesched.HasLabels(old) {
		return false
	}
	spec.TaskTemplate.ContainerSpec.Labels = prunesched.Carry(old, spec.TaskTemplate.ContainerSpec.Labels)
	return true
}

func generateSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}

// inspectServiceImage reads back a service's resolved image (Docker pins the
// @sha256 digest on deploy when QueryRegistry is set).
func inspectServiceImage(ctx context.Context, dcli *client.Client, serviceID string) string {
	svcRes, err := dcli.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
	svc := svcRes.Service
	if err != nil || svc.Spec.TaskTemplate.ContainerSpec == nil {
		return ""
	}
	return svc.Spec.TaskTemplate.ContainerSpec.Image
}

// portConflict reports whether a service other than ownName already publishes
// the given host port, which would stop the global agent from binding it.
func portConflict(ctx context.Context, dcli swarmops.ServiceLister, port int, ownName string) (string, bool) {
	listRes, err := dcli.ServiceList(ctx, client.ServiceListOptions{})
	list := listRes.Items
	if err != nil {
		return "", false // can't check — let the rollout surface any problem
	}
	for _, s := range list {
		if s.Spec.Name == ownName || s.Spec.EndpointSpec == nil {
			continue
		}
		for _, p := range s.Spec.EndpointSpec.Ports {
			if p.PublishMode == swarm.PortConfigPublishModeHost && int(p.PublishedPort) == port {
				return s.Spec.Name, true
			}
		}
	}
	return "", false
}

// agentTooOld reports whether err is a gRPC Unimplemented status, i.e. the agent
// predates an RPC this cli uses.
func agentTooOld(err error) bool { return agenterr.TooOld(err) }

// encodedRegistryAuth resolves local credentials for the image's registry and
// encodes them for ServiceCreate (so nodes can pull a private image). Returns ""
// when no credentials are configured.
func encodedRegistryAuth(image string) (string, error) {
	cf, err := cliconfig.Load("")
	if err != nil {
		return "", err
	}
	ac, err := cf.GetAuthConfig(registryHost(image))
	if err != nil {
		return "", err
	}
	if ac.Username == "" && ac.Password == "" && ac.Auth == "" && ac.IdentityToken == "" {
		return "", nil
	}
	b, err := json.Marshal(registry.AuthConfig{
		Username:      ac.Username,
		Password:      ac.Password,
		Auth:          ac.Auth,
		ServerAddress: ac.ServerAddress,
		IdentityToken: ac.IdentityToken,
		RegistryToken: ac.RegistryToken,
	})
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// registryHost extracts the registry hostname from an image reference, or
// "docker.io" for Docker Hub images.
func registryHost(image string) string {
	parts := strings.SplitN(image, "/", 2)
	if len(parts) == 2 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		return parts[0]
	}
	return "docker.io"
}
