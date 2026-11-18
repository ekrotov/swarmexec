// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/session"
	"swarmexec/client/internal/swarmfake"
)

// agentSwarm is a cluster with the agent deployed: its service, shared
// secret, two policy configs (labelled for the agent), a config that merely
// shares the label, one that is the user's own, and an application service.
func agentSwarm() *swarmfake.Swarm {
	roleLabels := map[string]string{agentRoleLabel: agentRoleValue}
	cfg := func(id, name string, labels map[string]string) swarm.Config {
		return swarm.Config{ID: id, Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: name, Labels: labels}}}
	}
	return &swarmfake.Swarm{
		Services: []swarm.Service{agentService(), stackedService("s-web", "shop_web", "shop", 1)},
		Secrets: []swarm.Secret{
			{ID: "sec-agent", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: agentSecretName}}},
			{ID: "sec-app", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "app_secret"}}},
		},
		Configs: []swarm.Config{
			cfg("cfg-p1", policyConfigPrefix+"a", roleLabels),
			cfg("cfg-p2", policyConfigPrefix+"b", roleLabels),
			cfg("cfg-other", "labelled_but_not_policy", roleLabels),
			cfg("cfg-user", "app_config", nil),
		},
		Nodes: []swarm.Node{readyNode("n1", agentNodeHost)},
	}
}

func names[T any](items []T, name func(T) string) []string {
	var out []string
	for _, it := range items {
		out = append(out, name(it))
	}
	return out
}

func serviceNames(f *swarmfake.Swarm) string {
	return strings.Join(names(f.Services, func(s swarm.Service) string { return s.Spec.Name }), ",")
}
func secretNames(f *swarmfake.Swarm) string {
	return strings.Join(names(f.Secrets, func(s swarm.Secret) string { return s.Spec.Name }), ",")
}
func configNames(f *swarmfake.Swarm) string {
	return strings.Join(names(f.Configs, func(c swarm.Config) string { return c.Spec.Name }), ",")
}

// `down` is destructive: what it removes, what it must leave, and that
// nothing goes without a yes.
func TestDownRun(t *testing.T) {
	remaining := func(f *swarmfake.Swarm) string {
		return serviceNames(f) + " | " + secretNames(f) + " | " + configNames(f)
	}
	const untouched = "swarmexec_agent,shop_web | swarmexec_agent_secret,app_secret | swarmexec_agent_policy_a,swarmexec_agent_policy_b,labelled_but_not_policy,app_config"
	if got := remaining(agentSwarm()); got != untouched {
		t.Fatalf("fixture drifted: %s", got)
	}
	run := func(f *swarmfake.Swarm, stdin string, args ...string) (string, string, error) {
		t.Helper()
		g := cliCluster(t, f, nil)
		return runIO(t, newDownCmd(g), stdin, args...)
	}

	t.Run("--yes removes the service, its secret and its policy configs, and only those", func(t *testing.T) {
		f := agentSwarm()
		out, _, err := run(f, "", "--yes")
		if err != nil {
			t.Fatal(err)
		}
		if got := remaining(f); got != "shop_web | app_secret | labelled_but_not_policy,app_config" {
			t.Errorf("left on the cluster: %s", got)
		}
		for _, want := range []string{
			`removing agents from Docker context`,
			`service "swarmexec_agent": removed`,
			`secret "swarmexec_agent_secret": removed`,
			`policy configs: 2 removed`,
			"✓ done",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("--keep-secret leaves the secret", func(t *testing.T) {
		f := agentSwarm()
		out, _, err := run(f, "", "--yes", "--keep-secret")
		if err != nil {
			t.Fatal(err)
		}
		if got := secretNames(f); got != "swarmexec_agent_secret,app_secret" {
			t.Errorf("secrets = %s", got)
		}
		if strings.Contains(out, "secret ") {
			t.Errorf("output mentions the secret:\n%s", out)
		}
		if serviceNames(f) != "shop_web" {
			t.Errorf("services = %s", serviceNames(f))
		}
	})

	t.Run("the agent is found by its role label when the name differs", func(t *testing.T) {
		f := agentSwarm()
		f.Services[0].Spec.Name = "my-agent"
		out, _, err := run(f, "", "--yes", "--service-name", "nothing-by-this-name")
		if err != nil || serviceNames(f) != "shop_web" || !strings.Contains(out, `service "my-agent": removed`) {
			t.Errorf("err %v services %s out:\n%s", err, serviceNames(f), out)
		}
	})

	t.Run("no agent means nothing to do", func(t *testing.T) {
		f := agentSwarm()
		f.Services = f.Services[1:]
		out, _, err := run(f, "", "--yes")
		if err != nil || !strings.Contains(out, "nothing to do") {
			t.Errorf("err %v out:\n%s", err, out)
		}
		if got := remaining(f); !strings.Contains(got, "swarmexec_agent_secret") {
			t.Errorf("secret removed although there was no agent: %s", got)
		}
	})

	// ask runs `down` with no --yes while the operator (answer) sits at a
	// terminal or not.
	ask := func(f *swarmfake.Swarm, answer string, terminal bool) (out, asked string, err error) {
		t.Helper()
		q := usePrompts(t, answer, terminal)
		out, _, err = run(f, "")
		return out, q.String(), err
	}
	// A declined confirmation is the command's "no", not a transport failure
	// (review CLI-03): exit 1, and reportError prints "swarmexec: aborted".
	aborted := func(t *testing.T, err error) {
		t.Helper()
		var ce *cliError
		if !errors.As(err, &ce) || ce.code != exitNo || ce.err == nil || ce.err.Error() != "aborted" {
			t.Errorf("err = %#v, want exit %d with \"aborted\"", err, exitNo)
		}
	}

	t.Run("without --yes the operator is asked, and no removes nothing", func(t *testing.T) {
		f := agentSwarm()
		_, asked, err := ask(f, "n\n", true)
		aborted(t, err)
		if want := `Remove service "swarmexec_agent" and secret "swarmexec_agent_secret" on context`; !strings.Contains(asked, want) || !strings.HasSuffix(asked, "[y/N]: ") {
			t.Errorf("asked %q, want it to name %q", asked, want)
		}
		if got := remaining(f); got != untouched {
			t.Errorf("something was removed after \"n\": %s", got)
		}
	})

	t.Run("an empty answer is no", func(t *testing.T) {
		f := agentSwarm()
		_, _, err := ask(f, "\n", true)
		aborted(t, err)
		if remaining(f) != untouched {
			t.Error("removed on an empty answer")
		}
	})

	t.Run("yes removes", func(t *testing.T) {
		f := agentSwarm()
		_, asked, err := ask(f, "y\n", true)
		if err != nil || serviceNames(f) != "shop_web" {
			t.Errorf("err %v services %s", err, serviceNames(f))
		}
		if !strings.Contains(asked, "?") {
			t.Errorf("asked %q", asked)
		}
	})

	t.Run("with --keep-secret the question does not mention the secret", func(t *testing.T) {
		f := agentSwarm()
		q := usePrompts(t, "n\n", true)
		_, _, _ = run(f, "", "--keep-secret")
		if strings.Contains(q.String(), "secret") {
			t.Errorf("asked %q", q.String())
		}
	})

	t.Run("without a terminal it refuses rather than guess", func(t *testing.T) {
		f := agentSwarm()
		_, asked, err := ask(f, "y\n", false)
		aborted(t, err)
		if asked != "" || remaining(f) != untouched {
			t.Errorf("asked %q, left %s: a piped \"y\" must not remove anything", asked, remaining(f))
		}
	})

	t.Run("a refused service removal is an error and keeps the secret", func(t *testing.T) {
		f := agentSwarm()
		f.Fail = map[string]bool{"DELETE /services/svc-agent": true}
		_, _, err := run(f, "", "--yes")
		if exitCode(err) != session.TransportFailure || !strings.Contains(err.Error(), `remove service "swarmexec_agent"`) {
			t.Errorf("err = %v", err)
		}
		if got := remaining(f); got != untouched {
			t.Errorf("removed after the service failed to go: %s", got)
		}
	})

	t.Run("a secret still held is reported, not fatal", func(t *testing.T) {
		f := agentSwarm()
		f.Fail = map[string]bool{"DELETE /secrets/sec-agent": true}
		out, _, err := run(f, "", "--yes")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `secret "swarmexec_agent_secret": not removed`) || !strings.Contains(out, "remove later with `docker secret rm swarmexec_agent_secret`") {
			t.Errorf("output:\n%s", out)
		}
		if serviceNames(f) != "shop_web" || !strings.Contains(out, "✓ done") {
			t.Errorf("services %s, out:\n%s", serviceNames(f), out)
		}
	})

	t.Run("policy configs held by a draining task are reported", func(t *testing.T) {
		f := agentSwarm()
		f.Fail = map[string]bool{"DELETE /configs/cfg-p1": true}
		out, _, err := run(f, "", "--yes")
		if err != nil || !strings.Contains(out, "policy configs: not all removed") {
			t.Errorf("err %v out:\n%s", err, out)
		}
		if got := configNames(f); got != "swarmexec_agent_policy_a,labelled_but_not_policy,app_config" {
			t.Errorf("configs = %s", got)
		}
	})
}
