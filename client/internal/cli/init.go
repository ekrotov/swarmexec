package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	cliconfig "github.com/docker/cli/cli/config"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/session"
)

const (
	defaultAgentImage = "registry.logle.io/internal-tools/swarm-remote-exec/agent:latest"
	defaultServiceNm  = "swarmexec_agent"
	agentSecretName   = "swarmexec_agent_secret"
	agentRoleLabel    = "swarmexec.role"
	agentRoleValue    = "agent"
)

// errNoAgent is shown when an agent-needing command finds no agent deployed.
var errNoAgent = errors.New("no swarmexec agent found in this swarm — run `swarmexec init` to provision it")

type initFlags struct {
	image        string
	secret       string
	serviceName  string
	port         int
	force        bool
	saveConfig   bool
	registryAuth bool
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
	return cmd
}

func runInit(cmd *cobra.Command, g *globalFlags, f *initFlags) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	dcli, err := newDockerClient(g.dockerContext)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}

	info, err := dcli.Info(ctx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("query Docker manager: %w", err)}
	}
	if !info.Swarm.ControlAvailable {
		return &cliError{code: usageExitCode, err: fmt.Errorf("the selected Docker endpoint is not a Swarm manager (point --context/$DOCKER_CONTEXT at a manager node)")}
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "swarm manager: %s (%d nodes)\n", info.Name, info.Swarm.Nodes)

	// 1) shared secret -----------------------------------------------------------
	createWith := f.secret
	if createWith == "" {
		createWith = generateSecret()
	}
	secretID, created, err := ensureSecret(ctx, dcli, agentSecretName, createWith)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	// We know the value to write into the client config only if we just created
	// the secret, or the operator passed it explicitly (Docker secrets are
	// write-only, so an existing one's value cannot be read back).
	knownSecret := created || f.secret != ""
	secretVal := createWith
	if created {
		fmt.Fprintf(out, "secret %q: created\n", agentSecretName)
	} else {
		fmt.Fprintf(out, "secret %q: reusing existing\n", agentSecretName)
	}

	// 2) agent service -----------------------------------------------------------
	spec := agentServiceSpec(f, secretID)
	var encodedAuth string
	if f.registryAuth {
		if auth, aerr := encodedRegistryAuth(f.image); aerr == nil {
			encodedAuth = auth
		}
	}

	existing, err := serviceByName(ctx, dcli, f.serviceName)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	switch {
	case existing == nil:
		if _, err := dcli.ServiceCreate(ctx, spec, types.ServiceCreateOptions{EncodedRegistryAuth: encodedAuth}); err != nil {
			return &cliError{code: session.TransportFailure, err: fmt.Errorf("create agent service: %w", err)}
		}
		fmt.Fprintf(out, "service %q: created (global, host port %d)\n", f.serviceName, f.port)
	case f.force:
		if _, err := dcli.ServiceUpdate(ctx, existing.ID, existing.Version, spec, types.ServiceUpdateOptions{EncodedRegistryAuth: encodedAuth}); err != nil {
			return &cliError{code: session.TransportFailure, err: fmt.Errorf("update agent service: %w", err)}
		}
		fmt.Fprintf(out, "service %q: updated\n", f.serviceName)
	default:
		return &cliError{code: usageExitCode, err: fmt.Errorf("service %q already exists; re-run with --force to update it", f.serviceName)}
	}

	// 3) client config -----------------------------------------------------------
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
				fmt.Fprintf(out, "client config written: %s\n", path)
			}
		}
	}

	fmt.Fprintf(out, "\nDone. The agent is rolling out on every node. Try:\n  swarmexec ps\n  swarmexec ui\n")
	return nil
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
func agentDeployed(ctx context.Context, dcli *client.Client) bool {
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
		if cs := s.Spec.TaskTemplate.ContainerSpec; cs != nil && strings.Contains(cs.Image, "swarm-remote-exec/agent") {
			return true
		}
	}
	return false
}

// enrichAgentError replaces a transport failure with the "run init" hint when no
// agent is deployed; otherwise it returns the original error unchanged.
func enrichAgentError(ctx context.Context, dcli *client.Client, err error) error {
	if err == nil {
		return nil
	}
	if !agentDeployed(ctx, dcli) {
		return errNoAgent
	}
	return err
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
