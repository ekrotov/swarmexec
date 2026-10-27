// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcpaccess

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
)

func TestEvaluateDefaults(t *testing.T) {
	cases := []struct {
		name   string
		def    Default
		labels map[string]string
		want   Access
	}{
		{"unlabelled, default none", DefaultNone, nil, Access{}},
		{"unlabelled, default read", DefaultRead, nil, Access{Visible: true}},
		{"enabled", DefaultNone, map[string]string{KeyEnable: "true"}, Access{Visible: true}},
		{"enabled with logs and exec", DefaultNone,
			map[string]string{KeyEnable: "true", KeyLogs: "true", KeyExec: "true"},
			Access{Visible: true, Logs: true, Exec: true}},
		{"explicit false beats default read", DefaultRead, map[string]string{KeyEnable: "false"}, Access{}},
		{"explicit false hides even with logs/exec", DefaultRead,
			map[string]string{KeyEnable: "false", KeyLogs: "true", KeyExec: "true"}, Access{}},
		{"logs without enable under default none stays hidden", DefaultNone,
			map[string]string{KeyLogs: "true"}, Access{}},
		{"logs without enable under default read", DefaultRead,
			map[string]string{KeyLogs: "true"}, Access{Visible: true, Logs: true}},
		{"explicit logs false", DefaultNone,
			map[string]string{KeyEnable: "true", KeyLogs: "false", KeyExec: "true"},
			Access{Visible: true, Exec: true}},
		{"default read never grants exec", DefaultRead, map[string]string{"other": "x"}, Access{Visible: true}},
		{"ParseBool spellings", DefaultNone,
			map[string]string{KeyEnable: "TRUE", KeyLogs: "1", KeyExec: "0"},
			Access{Visible: true, Logs: true}},
		{"garbled enable fails closed", DefaultRead, map[string]string{KeyEnable: "yes please"}, Access{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := Policy{Default: c.def}.Evaluate(c.labels)
			if got != c.want {
				t.Errorf("Evaluate = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestEvaluateWarnsAboutUnknownKeys(t *testing.T) {
	a, warns := Policy{}.Evaluate(map[string]string{
		KeyEnable:            "true",
		"swarmexec.mcp.exce": "true", // the typo the warning exists for
		"Swarmexec.MCP.logs": "true", // wrong case: not honoured either
		"traefik.enable":     "true",
	})
	if a.Exec || a.Logs {
		t.Fatalf("a typo or a mis-cased key granted something: %+v", a)
	}
	joined := strings.Join(warns, "\n")
	for _, want := range []string{"swarmexec.mcp.exce", "Swarmexec.MCP.logs"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning for %s; got %q", want, joined)
		}
	}
	if strings.Contains(joined, "traefik") {
		t.Errorf("a foreign label was warned about: %q", joined)
	}
}

func TestEvaluateWarnsAboutGarbledValue(t *testing.T) {
	_, warns := Policy{}.Evaluate(map[string]string{KeyExec: "maybe"})
	if len(warns) != 1 || !strings.Contains(warns[0], KeyExec) {
		t.Errorf("warnings = %q, want one about %s", warns, KeyExec)
	}
}

func TestParseDefault(t *testing.T) {
	for in, want := range map[string]Default{"": DefaultNone, "none": DefaultNone, "READ": DefaultRead} {
		got, err := ParseDefault(in)
		if err != nil || got != want {
			t.Errorf("ParseDefault(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseDefault("all"); err == nil {
		t.Error(`ParseDefault("all") must fail: there is no default that grants logs or exec`)
	}
}

func TestStrip(t *testing.T) {
	in := map[string]string{
		KeyEnable:                    "true",
		"SWARMEXEC.MCP.whatever":     "x",
		"swarmexec.mcp":              "x",
		"com.docker.stack.namespace": "web",
	}
	out := Strip(in)
	if len(out) != 1 || out["com.docker.stack.namespace"] != "web" {
		t.Errorf("Strip = %v", out)
	}
	if len(in) != 4 {
		t.Error("Strip modified its input")
	}
	if Strip(nil) != nil {
		t.Error("Strip(nil) should stay nil")
	}
}

func TestScrub(t *testing.T) {
	in := "keep 1\nlabel swarmexec.mcp.enable=true\nkeep 2\nSWARMEXEC.MCP.X\nlast"
	got := Scrub(in)
	if got != "keep 1\nkeep 2\nlast" {
		t.Errorf("Scrub = %q", got)
	}
	if Scrub("nothing here") != "nothing here" {
		t.Error("Scrub changed clean text")
	}
}

func TestMaskEnv(t *testing.T) {
	got := MaskEnv([]string{
		"DB_PASSWORD=hunter2",
		"API_KEY=abc",
		"DB_PASSWORD_FILE=/run/secrets/db", // a reference, not a secret: same rule as the scan
		"COMPASS=north",                    // whole-token match: not PASS
		"NOVALUE",
		"TOKEN=", // empty still masked: the key decides, not the value
	})
	want := []string{
		"DB_PASSWORD=" + Masked,
		"API_KEY=" + Masked,
		"DB_PASSWORD_FILE=/run/secrets/db",
		"COMPASS=north",
		"NOVALUE",
		"TOKEN=" + Masked,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("MaskEnv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	m := MaskEnvMap(map[string]string{"SECRET": "s", "PORT": "80"})
	if m["SECRET"] != Masked || m["PORT"] != "80" {
		t.Errorf("MaskEnvMap = %v", m)
	}
}

func specWithMounts(ms ...mount.Mount) swarm.ServiceSpec {
	var s swarm.ServiceSpec
	s.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Mounts: ms}
	return s
}

func TestExposesDockerSocket(t *testing.T) {
	bind := func(src string) mount.Mount { return mount.Mount{Type: mount.TypeBind, Source: src, Target: "/x"} }
	cases := []struct {
		name string
		spec swarm.ServiceSpec
		want bool
	}{
		{"the socket itself", specWithMounts(bind("/var/run/docker.sock")), true},
		{"read-only socket", specWithMounts(mount.Mount{Type: mount.TypeBind, Source: "/run/docker.sock", ReadOnly: true}), true},
		{"rootless socket path", specWithMounts(bind("/run/user/1000/docker.sock")), true},
		{"host root", specWithMounts(bind("/")), true},
		{"the directory the socket lives in", specWithMounts(bind("/var/run/")), true},
		{"windows pipe", specWithMounts(mount.Mount{Type: mount.TypeNamedPipe, Source: `\\.\pipe\docker_engine`}), true},
		{"a volume named like the socket is not a bind", specWithMounts(mount.Mount{Type: mount.TypeVolume, Source: "docker.sock"}), false},
		{"an ordinary bind", specWithMounts(bind("/srv/data")), false},
		{"no container spec", swarm.ServiceSpec{}, false},
	}
	for _, c := range cases {
		if got := ExposesDockerSocket(c.spec); got != c.want {
			t.Errorf("%s: ExposesDockerSocket = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGuardSpecChange(t *testing.T) {
	base := func() swarm.ServiceSpec {
		var s swarm.ServiceSpec
		s.Labels = map[string]string{KeyEnable: "true", "app": "web"}
		s.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "nginx", Labels: map[string]string{"c": "1"}}
		return s
	}
	mutate := func(f func(*swarm.ServiceSpec)) swarm.ServiceSpec {
		s := base()
		// Fresh maps, so a mutation never reaches back into "before".
		s.Labels = Strip(s.Labels)
		s.Labels[KeyEnable] = "true"
		cs := *s.TaskTemplate.ContainerSpec
		cs.Labels = map[string]string{"c": "1"}
		s.TaskTemplate.ContainerSpec = &cs
		f(&s)
		return s
	}

	allowed := map[string]swarm.ServiceSpec{
		"unchanged":                  base(),
		"other label changed":        mutate(func(s *swarm.ServiceSpec) { s.Labels["app"] = "api" }),
		"other label removed":        mutate(func(s *swarm.ServiceSpec) { delete(s.Labels, "app") }),
		"image changed":              mutate(func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec.Image = "nginx:2" }),
		"container label added":      mutate(func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec.Labels["d"] = "2" }),
		"whole label map rewritten ": mutate(func(s *swarm.ServiceSpec) { s.Labels = map[string]string{KeyEnable: "true"} }),
	}
	for name, after := range allowed {
		if err := GuardSpecChange(base(), after); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}

	refused := map[string]swarm.ServiceSpec{
		"access label added":   mutate(func(s *swarm.ServiceSpec) { s.Labels[KeyExec] = "true" }),
		"access label changed": mutate(func(s *swarm.ServiceSpec) { s.Labels[KeyEnable] = "false" }),
		"access label removed": mutate(func(s *swarm.ServiceSpec) { delete(s.Labels, KeyEnable) }),
		"all labels dropped":   mutate(func(s *swarm.ServiceSpec) { s.Labels = nil }),
		"typo key added":       mutate(func(s *swarm.ServiceSpec) { s.Labels["swarmexec.mcp.exce"] = "true" }),
		"mis-cased key added":  mutate(func(s *swarm.ServiceSpec) { s.Labels["SWARMEXEC.MCP.EXEC"] = "true" }),
		"on the container":     mutate(func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec.Labels[KeyExec] = "true" }),
		"container spec gone with a label on it": func() swarm.ServiceSpec {
			s := base()
			s.TaskTemplate.ContainerSpec.Labels[KeyLogs] = "true"
			return s
		}(),
	}
	for name, after := range refused {
		before := base()
		if name == "container spec gone with a label on it" {
			// Reverse direction: the label exists before and vanishes after.
			before, after = after, base()
		}
		err := GuardSpecChange(before, after)
		if err == nil {
			t.Errorf("%s: allowed", name)
			continue
		}
		if strings.Contains(strings.ToLower(err.Error()), "swarmexec.mcp") {
			t.Errorf("%s: the refusal names the label namespace: %v", name, err)
		}
	}
}
