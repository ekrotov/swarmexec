// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	cliconfig "github.com/docker/cli/cli/config"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/session"
	cterm "swarmexec/client/internal/term"
)

const (
	defaultAgentImage = "docker.io/logleio/swarmexec-agent:latest"
	defaultServiceNm  = "swarmexec_agent"
	agentSecretName   = "swarmexec_agent_secret"
	agentRoleLabel    = "swarmexec.role"
	agentRoleValue    = "agent"
)

// errNoAgent is shown when an agent-needing command finds no agent deployed.
var errNoAgent = errors.New("no swarmexec agent found in this swarm — run `swarmexec init` to provision it")

// errAgentTooOld is shown when an agent rejects an RPC with Unimplemented.
var errAgentTooOld = errors.New("agent is older than this client (missing RPC) — update it with `swarmexec init --force`")

// errSecretRejected explains an Unauthenticated status, which since v1.18.0 has
// a new and far more likely cause than a wrong secret: this client sends a proof
// bound to the connection instead of the secret itself, and an agent predating
// that does not recognise it. The bare "invalid or missing agent secret" the
// agent returns is actively misleading in that case — the secret is fine.
var errSecretRejected = errors.New("agent rejected the shared secret.\n" +
	"  Most likely the agents predate connection-bound authentication (v1.18.0):\n" +
	"  update them with `swarmexec init --force`, or set `legacy_secret: true` in\n" +
	"  the client config to send the raw secret meanwhile.\n" +
	"  Otherwise the configured secret does not match the one the agents hold.")

type initFlags struct {
	image          string
	secret         string
	serviceName    string
	port           int
	force          bool
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
	fl.BoolVar(&f.saveConfig, "save-config", true, "write the client config (~/.config/swarmexec/config.yaml)")
	fl.BoolVar(&f.registryAuth, "registry-auth", true, "pass local registry credentials so nodes can pull a private image")
	fl.BoolVar(&f.wait, "wait", true, "wait for the agents to come up and report progress")
	fl.DurationVar(&f.rolloutTimeout, "rollout-timeout", 90*time.Second, "how long to wait for agents to start")
	return cmd
}

func runInit(cmd *cobra.Command, g *globalFlags, f *initFlags) error {
	ctx := cmdContext(cmd)
	out := cmd.OutOrStdout()
	tctx, cerr := resolveContext(out, g)
	if cerr != nil {
		return cerr
	}
	dcli, err := newDockerClient(ctx, tctx.Name)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	fmt.Fprintf(out, "deploying agents into Docker context %q (%s)\n", tctx.Name, hostOrDefault(tctx.Host))

	info, err := dcli.Info(ctx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("query Docker manager: %w", err)}
	}
	if !info.Swarm.ControlAvailable {
		return &cliError{code: usageExitCode, err: fmt.Errorf("context %q is not a Swarm manager (pick a manager context, or set --context)", tctx.Name)}
	}

	steps := 3 // secret, service, rollout/config
	step := func(n int, label string) {
		fmt.Fprintf(out, "[%d/%d] %s … ", n, steps, label)
	}

	fmt.Fprintf(out, "manager: %s (%d nodes)\n", info.Name, info.Swarm.Nodes)

	// Pre-flight: fail fast on a host-port conflict before creating anything.
	if other, ok := portConflict(ctx, dcli, f.port, f.serviceName); ok {
		return &cliError{code: usageExitCode, err: fmt.Errorf(
			"host port %d is already published by service %q — remove it (`docker service rm %s`, or `swarmexec down` if it's an old swarmexec agent) or pick another --port",
			f.port, other, other)}
	}

	// [1/3] shared secret --------------------------------------------------------
	step(1, "shared secret")
	createWith := f.secret
	if createWith == "" {
		createWith = generateSecret()
	}
	secretID, created, err := ensureSecret(ctx, dcli, agentSecretName, createWith)
	if err != nil {
		fmt.Fprintln(out, "failed")
		return &cliError{code: session.TransportFailure, err: err}
	}
	// We know the value to write into the client config only if we just created
	// the secret, or the operator passed it explicitly (Docker secrets are
	// write-only, so an existing one's value cannot be read back).
	knownSecret := created || f.secret != ""
	secretVal := createWith
	if created {
		fmt.Fprintln(out, "created")
	} else {
		fmt.Fprintln(out, "reusing existing")
		if f.secret != "" {
			// The existing secret is kept (Docker secrets are immutable), so the
			// --secret value is NOT applied to the cluster — yet it IS written to the
			// client config below. If it isn't the existing secret's actual value the
			// agent will reject the client, so warn loudly.
			fmt.Fprintf(out, "warning: secret %q already exists and is kept as-is — your --secret value was NOT applied to the cluster.\n"+
				"         It will be written to the client config, so it must equal the existing secret's value or the agent will reject you.\n"+
				"         To change the cluster secret, remove it first (no service may reference it) and re-run init.\n", agentSecretName)
		}
	}

	// [2/3] agent service --------------------------------------------------------
	step(2, fmt.Sprintf("agent service on port %d", f.port))
	spec := agentServiceSpec(f, secretID)
	var encodedAuth string
	if f.registryAuth {
		auth, aerr := encodedRegistryAuth(f.image)
		host := registryHost(f.image)
		switch {
		case aerr != nil:
			// Don't deploy blind: without distributed credentials the manager may
			// pull fine (image cached / logged in) while every worker reports the
			// image as unavailable.
			fmt.Fprintln(out, "failed")
			return &cliError{code: usageExitCode, err: fmt.Errorf("read local registry credentials for %s: %w — run `docker login %s` on THIS machine (where you run swarmexec, not the manager); init distributes them to the swarm nodes. Or pass --registry-auth=false for a public image", host, aerr, host)}
		case auth == "" && host != "docker.io":
			fmt.Fprintln(out, "failed")
			return &cliError{code: usageExitCode, err: fmt.Errorf("no registry credentials for %s found in this machine's Docker config — init reads your LOCAL `docker login` (not the manager's) and distributes it to the swarm nodes so they can pull the private image %q. Run `docker login %s` here, or pass --registry-auth=false for a public image", host, f.image, host)}
		default:
			encodedAuth = auth
		}
	}

	existing, err := serviceByName(ctx, dcli, f.serviceName)
	if err != nil {
		fmt.Fprintln(out, "failed")
		return &cliError{code: session.TransportFailure, err: err}
	}
	var serviceID string
	switch {
	case existing == nil:
		resp, cerr := dcli.ServiceCreate(ctx, spec, types.ServiceCreateOptions{EncodedRegistryAuth: encodedAuth, QueryRegistry: true})
		if cerr != nil {
			fmt.Fprintln(out, "failed")
			return &cliError{code: session.TransportFailure, err: fmt.Errorf("create agent service: %w", cerr)}
		}
		serviceID = resp.ID
		fmt.Fprintf(out, "created (%q, global)\n", f.serviceName)
	case f.force:
		if _, uerr := dcli.ServiceUpdate(ctx, existing.ID, existing.Version, spec, types.ServiceUpdateOptions{EncodedRegistryAuth: encodedAuth, QueryRegistry: true}); uerr != nil {
			fmt.Fprintln(out, "failed")
			return &cliError{code: session.TransportFailure, err: fmt.Errorf("update agent service: %w", uerr)}
		}
		serviceID = existing.ID
		fmt.Fprintf(out, "updated (%q)\n", f.serviceName)
	default:
		fmt.Fprintln(out, "already exists")
		return &cliError{code: usageExitCode, err: fmt.Errorf("service %q already exists; re-run with --force to update it", f.serviceName)}
	}

	if img := inspectServiceImage(ctx, dcli, serviceID); img != "" {
		fmt.Fprintf(out, "        image: %s\n", img)
	}

	// [3/3] wait for rollout -----------------------------------------------------
	if f.wait {
		step(3, "starting agents")
		fmt.Fprintln(out)
		waitRollout(ctx, dcli, serviceID, out, f.rolloutTimeout)
	}

	// client config --------------------------------------------------------------
	if f.saveConfig {
		if !knownSecret {
			fmt.Fprintln(out, "note: reused an existing secret whose value is unknown — set `agent_secret` in your config manually, or re-run with --secret")
		} else {
			cfg, _ := config.Load(g.configPath)
			cfg.AgentSecret = secretVal
			cfg.Insecure = true
			cfg.AddrMode = config.AddrModeIP
			cfg.Port = f.port
			if path, serr := cfg.Save(g.configPath); serr != nil {
				fmt.Fprintf(out, "warning: could not write client config: %v\n", serr)
			} else {
				fmt.Fprintf(out, "client config: %s\n", path)
			}
		}
	}

	fmt.Fprintf(out, "\n✓ ready — try:\n  swarmexec ps\n  swarmexec ui\n")
	return nil
}

// resolveContext decides which Docker context to act on, informing the user and
// — when several exist and stdin is a terminal — prompting them to choose.
func resolveContext(out io.Writer, g *globalFlags) (dockerctx.Context, error) {
	// An explicit --context / $DOCKER_CONTEXT wins; no prompt.
	if explicit := firstNonEmpty(g.dockerContext, os.Getenv("DOCKER_CONTEXT")); explicit != "" {
		host, _ := dockerctx.ResolveHost(explicit)
		return dockerctx.Context{Name: explicit, Host: host, Current: true}, nil
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
		tasks, err := dcli.TaskList(ctx, types.TaskListOptions{
			Filters: filters.NewArgs(filters.Arg("service", serviceID)),
		})
		if err != nil {
			fmt.Fprintf(out, "      (could not query tasks: %v)\n", err)
			return
		}
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
		msg := fmt.Sprintf("%d/%d running", running, desired)
		if taskErr != "" && running < desired {
			msg += " — " + taskErr // surface e.g. "port already in use" instead of a silent timeout
		}
		if msg != last {
			fmt.Fprintf(out, "      agents: %s\n", msg)
			last = msg
		}
		if desired > 0 && running >= desired {
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

// ensureSecret returns the id of the named secret, creating it with createWith
// if it does not exist (Docker secrets are immutable, so an existing one is
// reused as-is).
func ensureSecret(ctx context.Context, dcli *client.Client, name, createWith string) (id string, created bool, err error) {
	list, err := dcli.SecretList(ctx, types.SecretListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return "", false, fmt.Errorf("list secrets: %w", err)
	}
	for _, s := range list {
		if s.Spec.Name == name {
			return s.ID, false, nil
		}
	}
	resp, err := dcli.SecretCreate(ctx, swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: name, Labels: map[string]string{agentRoleLabel: agentRoleValue}},
		Data:        []byte(createWith),
	})
	if err != nil {
		return "", false, fmt.Errorf("create secret: %w", err)
	}
	return resp.ID, true, nil
}

// agentServiceSpec mirrors deploy/agent-stack-selfsigned.yml.
func agentServiceSpec(f *initFlags, secretID string) swarm.ServiceSpec {
	args := []string{
		fmt.Sprintf("-port=%d", f.port),
		"-self-signed",
		"-agent-secret-file=/run/secrets/" + agentSecretName,
		"-docker-host=unix:///var/run/docker.sock",
		"-drain-timeout=5s",
		"-log-format=json",
		"-audit-dest=stdout",
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
				Mounts: []mount.Mount{{
					Type:   mount.TypeBind,
					Source: "/var/run/docker.sock",
					Target: "/var/run/docker.sock",
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
		EndpointSpec: &swarm.EndpointSpec{
			Ports: []swarm.PortConfig{{
				Protocol:      swarm.PortConfigProtocolTCP,
				TargetPort:    uint32(f.port),
				PublishedPort: uint32(f.port),
				PublishMode:   swarm.PortConfigPublishModeHost,
			}},
		},
	}
}

func generateSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}

// serviceByName returns the service with the given name, or nil.
func serviceByName(ctx context.Context, dcli *client.Client, name string) (*swarm.Service, error) {
	list, err := dcli.ServiceList(ctx, types.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	for i := range list {
		if list[i].Spec.Name == name {
			return &list[i], nil
		}
	}
	return nil, nil
}

// agentDeployed reports whether a swarmexec agent service exists in the swarm —
// either tagged with our role label or recognizable by its image. Used to turn
// "can't reach an agent" into the actionable "run swarmexec init" hint.
// serviceLister is the slice of the Docker manager API agentDeployed needs (so
// it is unit-testable without a real client).
type serviceLister interface {
	ServiceList(context.Context, types.ServiceListOptions) ([]swarm.Service, error)
}

func agentDeployed(ctx context.Context, dcli serviceLister) bool {
	if list, err := dcli.ServiceList(ctx, types.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", agentRoleLabel+"="+agentRoleValue)),
	}); err == nil && len(list) > 0 {
		return true
	}
	list, err := dcli.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return true // can't tell — don't claim the agent is missing
	}
	for _, s := range list {
		if cs := s.Spec.TaskTemplate.ContainerSpec; cs != nil && isAgentImage(cs.Image) {
			return true
		}
	}
	return false
}

// isAgentImage reports whether an image reference looks like the swarmexec
// agent. Label matching is the primary signal (agentServiceSpec always sets
// swarmexec.role=agent); this is the fallback for services deployed without
// that label. It matches both the Docker Hub name (logleio/swarmexec-agent)
// and the older GitLab-registry name (…/swarm-remote-exec/agent), so a cluster
// mid-migration is still recognized whichever image it runs.
func isAgentImage(image string) bool {
	return strings.Contains(image, "swarmexec-agent") ||
		strings.Contains(image, "swarm-remote-exec/agent")
}

// inspectServiceImage reads back a service's resolved image (Docker pins the
// @sha256 digest on deploy when QueryRegistry is set).
func inspectServiceImage(ctx context.Context, dcli *client.Client, serviceID string) string {
	svc, _, err := dcli.ServiceInspectWithRaw(ctx, serviceID, types.ServiceInspectOptions{})
	if err != nil || svc.Spec.TaskTemplate.ContainerSpec == nil {
		return ""
	}
	return svc.Spec.TaskTemplate.ContainerSpec.Image
}

// agentServiceImage returns the image (including any pinned @sha256 digest) of
// the deployed agent service, or "" if none is found.
func agentServiceImage(ctx context.Context, dcli serviceLister) string {
	if list, err := dcli.ServiceList(ctx, types.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", agentRoleLabel+"="+agentRoleValue)),
	}); err == nil && len(list) > 0 && list[0].Spec.TaskTemplate.ContainerSpec != nil {
		return list[0].Spec.TaskTemplate.ContainerSpec.Image
	}
	list, err := dcli.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return ""
	}
	for _, s := range list {
		if cs := s.Spec.TaskTemplate.ContainerSpec; cs != nil && isAgentImage(cs.Image) {
			return cs.Image
		}
	}
	return ""
}

// portConflict reports whether a service other than ownName already publishes
// the given host port, which would stop the global agent from binding it.
func portConflict(ctx context.Context, dcli serviceLister, port int, ownName string) (string, bool) {
	list, err := dcli.ServiceList(ctx, types.ServiceListOptions{})
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

// enrichAgentError replaces a transport failure with the "run init" hint when no
// agent is deployed; otherwise it returns the original error unchanged.
func enrichAgentError(ctx context.Context, dcli *client.Client, err error) error {
	if err == nil {
		return nil
	}
	if agentTooOld(err) {
		return errAgentTooOld
	}
	if status.Code(err) == codes.Unauthenticated {
		return errSecretRejected
	}
	if !agentDeployed(ctx, dcli) {
		return errNoAgent
	}
	return err
}

// agentTooOld reports whether err is a gRPC Unimplemented status, i.e. the agent
// predates an RPC this cli uses.
func agentTooOld(err error) bool {
	return status.Code(err) == codes.Unimplemented
}

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
