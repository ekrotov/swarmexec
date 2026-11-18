// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcpaccess

import (
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

// fakeJWT builds a JWT-shaped string from made-up parts at run time, so the
// source holds nothing that looks like a token.
func fakeJWT() string {
	return "eyJ" + "ZmFrZWhlYWRlcg" + ".eyJ" + "ZmFrZWJvZHk" + ".ZmFrZXNpZw"
}

func TestMaskValueShapes(t *testing.T) {
	cases := []struct{ name, key, val, want string }{
		{"url password only", "DATABASE_URL", "postgres://app:notasecret@db:5432/x?sslmode=disable",
			"postgres://app:" + Masked + "@db:5432/x?sslmode=disable"},
		{"redis url empty user", "CACHE_URI", "redis://:notasecret@cache:6379/0", "redis://:" + Masked + "@cache:6379/0"},
		{"url without credentials untouched", "API_URL", "https://api.example.com/v1", "https://api.example.com/v1"},
		{"ssh user without password untouched", "REPO_URL", "ssh://git@host/repo.git", "ssh://git@host/repo.git"},
		{"dsn key whole value", "DB_DSN", "host=db user=app dbname=x", Masked},
		{"dsn key url keeps host", "SENTRY_DSN", "https://u:notasecret@o.example/1", "https://u:" + Masked + "@o.example/1"},
		{"key by name", "APP_KEY", "base64:abc", Masked},
		{"auth key", "PROXY_AUTH", "user:x", Masked},
		{"reference stays", "APP_KEY_FILE", "/run/secrets/k", "/run/secrets/k"},
		{"jwt in plain key", "X_HEADER", "Bearer " + fakeJWT(), "Bearer " + Masked},
		{"pem in plain key", "X_BLOB", "-----BEGIN PRIVATE KEY-----\nZmFrZQ==\n-----END PRIVATE KEY-----", Masked},
		{"htpasswd label", "traefik.http.middlewares.a.basicauth.users", "admin:$apr1$fake$notahash", Masked},
		{"htpasswd in plain key", "SOMETHING", "admin:$apr1$fake$notahash", "admin:" + Masked},
		{"pair in text", "OPTS", "host=db password=notasecret port=1", "host=db password=" + Masked + " port=1"},
		{"harmless untouched", "LOG_LEVEL", "debug", "debug"},
		{"empty value of a credential key", "APP_KEY", "", Masked},
	}
	for _, c := range cases {
		if got := MaskValue(c.key, c.val); got != c.want {
			t.Errorf("%s: MaskValue(%q) = %q, want %q", c.name, c.key, got, c.want)
		}
	}
}

func TestMaskEnvURLAndStrip(t *testing.T) {
	got := MaskEnv([]string{"DATABASE_URL=postgres://app:notasecret@db/x", "NOEQ"})
	if got[0] != "DATABASE_URL=postgres://app:"+Masked+"@db/x" || got[1] != "NOEQ" {
		t.Errorf("MaskEnv = %v", got)
	}
	l := Strip(map[string]string{"traefik.x.basicauth.users": "a:$apr1$fake$h", "keep": "yes"})
	if l["traefik.x.basicauth.users"] != Masked || l["keep"] != "yes" {
		t.Errorf("Strip = %v", l)
	}
}

func TestMaskArgs(t *testing.T) {
	got := MaskArgs([]string{"redis-server", "--requirepass", "notasecret", "--password=notasecret", "--port", "6379",
		"--url=postgres://a:notasecret@h/d", "plain"})
	want := []string{"redis-server", "--requirepass", Masked, "--password=" + Masked, "--port", "6379",
		"--url=postgres://a:" + Masked + "@h/d", "plain"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("MaskArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if MaskArgs(nil) != nil {
		t.Error("MaskArgs(nil) should stay nil")
	}
}

func TestReachesDockerAPIAndProxy(t *testing.T) {
	with := func(env ...string) swarm.ServiceSpec {
		var s swarm.ServiceSpec
		s.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Env: env}
		return s
	}
	if !ReachesDockerAPI(with("A=1", "DOCKER_HOST=tcp://proxy:2375")) {
		t.Error("DOCKER_HOST should count")
	}
	if ReachesDockerAPI(with("DOCKER_HOST=")) || ReachesDockerAPI(with("DOCKER_HOSTNAME=x")) || ReachesDockerAPI(swarm.ServiceSpec{}) {
		t.Error("empty or unrelated env must not count")
	}
	proxy := swarm.Service{}
	proxy.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "example/docker-socket-proxy:1"}
	proxy.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "apinet"}}
	nets := SocketProxyNetworks([]swarm.Service{proxy, {}})
	var client swarm.ServiceSpec
	client.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "apinet"}}
	var other swarm.ServiceSpec
	other.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "web"}}
	if !SharesNetwork(client, nets) || SharesNetwork(other, nets) {
		t.Errorf("SharesNetwork wrong for %v", nets)
	}
}
