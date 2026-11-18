// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package agentdeploy is what `swarmexec init` writes to a cluster: the agent
// service's spec, the shared secret, the policy config, the registry
// credentials the nodes pull with, and the wait for the rollout.
//
// It is separate from the command because none of it is about a command line.
// The spec in particular is a contract with internal/deploy (which the agent
// parses its own flags from) and with agent/deploy/agent-stack-selfsigned.yml,
// and a contract is easier to keep when it can be read — and tested — without
// a cobra tree around it.
package agentdeploy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	cliconfig "github.com/docker/cli/cli/config"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"swarmexec/client/internal/swarmops"
	"swarmexec/internal/deploy"
	"swarmexec/internal/prunesched"
)

// Options is the agent deployment as the operator asked for it. It mirrors the
// flags of `swarmexec init`, but it is not those flags: the spec has to be
// buildable from a struct literal in a test.
type Options struct {
	// Image is the agent image the nodes pull.
	Image string
	// ServiceName is the global service's name.
	ServiceName string
	// Port is the host port the agent publishes.
	Port int
	// MetricsPort publishes the Prometheus endpoint; 0 leaves it off.
	MetricsPort int
	// AllowLegacySecret lets agents accept the raw shared secret from clients
	// that predate connection-bound auth.
	AllowLegacySecret bool
	// WithPolicy tells the agent to read a policy file; the config that holds
	// it is attached separately (EnsurePolicyConfig).
	WithPolicy bool
}

// WaitRollout polls the service's tasks until every desired task is running (or
// the timeout elapses), printing the running/desired count as it changes.
func WaitRollout(ctx context.Context, dcli *client.Client, serviceID string, out io.Writer, timeout time.Duration) {
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
		msg, done := RolloutLine(tasks)
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

// RolloutLine is the rollout's progress line — running/desired, and while it
// is short, the latest failed task's reason — and whether every desired task
// runs.
func RolloutLine(tasks []swarm.Task) (msg string, done bool) {
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

// EnsureSecret returns the id of the named secret, creating it with createWith
// if it does not exist (Docker secrets are immutable, so an existing one is
// reused as-is).
func EnsureSecret(ctx context.Context, dcli *client.Client, name, createWith string) (id string, created bool, err error) {
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
		Annotations: swarm.Annotations{Name: name, Labels: map[string]string{deploy.RoleLabel: deploy.RoleAgent}},
		Data:        []byte(createWith),
	}})
	if err != nil {
		return "", false, fmt.Errorf("create secret: %w", err)
	}
	return resp.ID, true, nil
}

// PolicyConfigPrefix names the Docker configs holding agent policies. Configs
// are immutable, so each distinct policy is its own config, named by its
// content hash: re-running init with the same file reuses it, a changed file
// becomes a new one, and the service update points the agents at it.
const PolicyConfigPrefix = "swarmexec_agent_policy_"

func EnsurePolicyConfig(ctx context.Context, dcli *client.Client, data []byte) (*swarm.ConfigReference, error) {
	sum := sha256.Sum256(data)
	name := PolicyConfigPrefix + hex.EncodeToString(sum[:])[:12]
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
			Annotations: swarm.Annotations{Name: name, Labels: map[string]string{deploy.RoleLabel: deploy.RoleAgent}},
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

// Spec is the service `swarmexec init` deploys.
//
// It is the same agent as agent/deploy/agent-stack-selfsigned.yml provisions,
// not the same spec: the stack file binds with -listen=:9443 and attaches an
// overlay network, this publishes -port in host mode. The command line comes
// from internal/deploy, which the agent parses its flags from — see there for
// why that is not decoration.
func Spec(o Options, secretID string) swarm.ServiceSpec {
	args := deploy.AgentArgs(deploy.AgentOptions{Port: o.Port, AllowLegacySecret: o.AllowLegacySecret, MetricsPort: o.MetricsPort, Policy: o.WithPolicy})
	ports := []swarm.PortConfig{{
		Protocol:      network.TCP,
		TargetPort:    uint32(o.Port),
		PublishedPort: uint32(o.Port),
		PublishMode:   swarm.PortConfigPublishModeHost,
	}}
	if o.MetricsPort > 0 {
		// Host mode like the agent port: there is no overlay network to scrape
		// over, so Prometheus reaches each node's agent at <node>:<port>.
		ports = append(ports, swarm.PortConfig{
			Protocol:      network.TCP,
			TargetPort:    uint32(o.MetricsPort),
			PublishedPort: uint32(o.MetricsPort),
			PublishMode:   swarm.PortConfigPublishModeHost,
		})
	}
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name:   o.ServiceName,
			Labels: map[string]string{deploy.RoleLabel: deploy.RoleAgent},
		},
		Mode:         swarm.ServiceMode{Global: &swarm.GlobalService{}},
		UpdateConfig: &swarm.UpdateConfig{Order: swarm.UpdateOrderStopFirst},
		TaskTemplate: swarm.TaskSpec{
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny},
			ContainerSpec: &swarm.ContainerSpec{
				Image: o.Image,
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
					SecretName: deploy.SecretName,
					File: &swarm.SecretReferenceFileTarget{
						Name: deploy.SecretName,
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

// CarryPruneSchedule copies the prune-schedule labels of the running agent
// into the spec init is about to write, and reports whether there were any.
// The schedule is set from the TUI or `prune-schedule set`, never from init's
// flags, so a re-init that rebuilt the spec from flags alone would switch it
// off without a word.
func CarryPruneSchedule(spec *swarm.ServiceSpec, existing *swarm.Service) bool {
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
func GenerateSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}

// InspectServiceImage reads back a service's resolved image (Docker pins the
// @sha256 digest on deploy when QueryRegistry is set).
func InspectServiceImage(ctx context.Context, dcli *client.Client, serviceID string) string {
	svcRes, err := dcli.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
	svc := svcRes.Service
	if err != nil || svc.Spec.TaskTemplate.ContainerSpec == nil {
		return ""
	}
	return svc.Spec.TaskTemplate.ContainerSpec.Image
}

// PortConflict reports whether a service other than ownName already publishes
// the given host port, which would stop the global agent from binding it.
func PortConflict(ctx context.Context, dcli swarmops.ServiceLister, port int, ownName string) (string, bool) {
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

// EncodedRegistryAuth resolves local credentials for the image's registry and
// encodes them for ServiceCreate (so nodes can pull a private image). Returns ""
// when no credentials are configured.
func EncodedRegistryAuth(image string) (string, error) {
	cf, err := cliconfig.Load("")
	if err != nil {
		return "", err
	}
	ac, err := cf.GetAuthConfig(RegistryHost(image))
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

// RegistryHost extracts the registry hostname from an image reference, or
// "docker.io" for Docker Hub images.
func RegistryHost(image string) string {
	parts := strings.SplitN(image, "/", 2)
	if len(parts) == 2 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		return parts[0]
	}
	return "docker.io"
}
