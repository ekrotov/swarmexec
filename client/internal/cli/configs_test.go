package cli

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

// svcWithConfigs builds a service mounting the given config references.
func svcWithConfigs(name string, refs ...*swarm.ConfigReference) swarm.Service {
	var s swarm.Service
	s.Spec.Name = name
	s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Configs: refs}
	return s
}

func TestServiceConfigMembership(t *testing.T) {
	svcs := []swarm.Service{
		svcWithConfigs("web",
			&swarm.ConfigReference{ConfigID: "c1", ConfigName: "nginx.conf"},
			&swarm.ConfigReference{ConfigID: "c2", ConfigName: "mime.types"}),
		svcWithConfigs("api", &swarm.ConfigReference{ConfigID: "c1", ConfigName: "nginx.conf"}),
		svcWithConfigs("lonely"),
	}
	m := serviceConfigMembership(svcs)

	// Keyed by BOTH id and name — a spec may reference either.
	for _, key := range []string{"c1", "nginx.conf"} {
		got := m[key]
		if len(got) != 2 {
			t.Errorf("%s: %v, want web and api", key, got)
		}
	}
	if len(m["c2"]) != 1 || m["c2"][0] != "web" {
		t.Errorf("c2: %v, want [web]", m["c2"])
	}
	if len(m["nope"]) != 0 {
		t.Errorf("unknown config should have no services: %v", m["nope"])
	}

	// A nil reference and a nil container spec must not panic.
	var bare swarm.Service
	bare.Spec.Name = "bare"
	withNil := svcWithConfigs("withnil", nil, &swarm.ConfigReference{ConfigID: "c9"})
	m2 := serviceConfigMembership([]swarm.Service{bare, withNil})
	if len(m2["c9"]) != 1 {
		t.Errorf("c9: %v, want [withnil]", m2["c9"])
	}
}

// A service must be listed once per config even if it mounts it twice.
func TestServiceConfigMembershipDeduplicates(t *testing.T) {
	svcs := []swarm.Service{
		svcWithConfigs("web",
			&swarm.ConfigReference{ConfigID: "c1", ConfigName: "app.conf"},
			&swarm.ConfigReference{ConfigID: "c1", ConfigName: "app.conf"}),
	}
	if got := serviceConfigMembership(svcs)["c1"]; len(got) != 1 {
		t.Errorf("c1: %v, want web once", got)
	}
}

func TestIndentConfigContent(t *testing.T) {
	got := indentConfigContent([]byte("server {\n  listen 80;\n}\n"))
	for _, want := range []string{"    server {", "    }"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	// Content is operator-supplied and lands in a dynamic-colour TextView, so a
	// bracket run must not be parsed as a colour tag.
	esc := indentConfigContent([]byte("[red]not a tag"))
	if strings.Contains(esc, "[red]not") {
		t.Errorf("colour tag survived unescaped:\n%s", esc)
	}

	// Binary content is reported, not dumped into the overlay.
	bin := indentConfigContent([]byte{0x00, 0x01, 0x02})
	if !strings.Contains(bin, "binary content") {
		t.Errorf("binary payload should be reported:\n%s", bin)
	}

	// Oversized content is truncated with a note rather than silently cut, and
	// the RENDERED output is bounded too — many short lines each gain
	// indentation, so a byte cap alone does not bound the row count.
	big := indentConfigContent([]byte(strings.Repeat("a\n", configContentLimit)))
	if !strings.Contains(big, "truncated") {
		t.Errorf("large payload should say it was truncated")
	}
	if n := strings.Count(big, "\n"); n > configContentMaxLines+1 {
		t.Errorf("rendered %d lines, want at most %d", n, configContentMaxLines+1)
	}

	// A single enormous line is bounded by the byte cap instead.
	wide := indentConfigContent([]byte(strings.Repeat("x", 3*configContentLimit)))
	if !strings.Contains(wide, "truncated") || len(wide) > configContentLimit+256 {
		t.Errorf("one long line not bounded: %d bytes", len(wide))
	}
}

func TestIsProbablyText(t *testing.T) {
	if !isProbablyText([]byte("plain text\n")) {
		t.Error("text misdetected as binary")
	}
	if isProbablyText([]byte{'a', 0x00, 'b'}) {
		t.Error("NUL byte should mark content binary")
	}
	if !isProbablyText(nil) {
		t.Error("empty content should not be treated as binary")
	}
	// A NUL past the sampled head is not detected — documented behaviour, and
	// harmless: the render is capped anyway.
	long := append([]byte(strings.Repeat("a", 9<<10)), 0x00)
	if !isProbablyText(long) {
		t.Error("only the head is sampled, so a late NUL should not flip the verdict")
	}
}
