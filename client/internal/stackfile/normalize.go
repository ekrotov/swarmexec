// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
)

// ServiceFromSpec reduces a swarm service spec to the compose shape. It is the
// ONLY path from a spec to the model, and both a deployed service and a loaded
// stack file go through it — that is what makes the two comparable. Changing
// what a field means here changes it for both sides at once, which is the
// property worth protecting.
// Names turns the identifiers a service spec carries into the names a stack
// file uses.
//
// Two traps live here, both found by exporting a real stack and reading it back.
//
// First, a deployed service records a network by ID, while a file names it.
// Without NetworkName every service would differ on every network.
//
// Second — and far easier to get wrong — the "<stack>_" prefix may only be
// stripped from objects that BELONG to the stack. A cluster can hold a secret
// called "postgres_password" that nobody created as part of the "postgres"
// stack; blindly trimming the prefix renames it to "password", and the exported
// file then references a secret that does not exist. Owned therefore lists the
// FULL names the stack actually owns, and nothing else is touched.
type Names struct {
	Namespace   string
	NetworkName func(id string) string // deployed side only; nil when names are already names
	Owned       map[string]bool        // full names of objects this stack created
}

// short drops the stack prefix, but only from something the stack owns.
func (n Names) short(full string) string {
	if n.Owned[full] {
		return strings.TrimPrefix(full, n.Namespace+"_")
	}
	return full
}

// network resolves an attachment target (an id on the deployed side) to the
// name a file would use.
func (n Names) network(target string) string {
	if n.NetworkName != nil {
		if name := n.NetworkName(target); name != "" {
			target = name
		}
	}
	return n.short(target)
}

func ServiceFromSpec(ns string, spec swarm.ServiceSpec, names Names) *Service {
	cs := spec.TaskTemplate.ContainerSpec
	if cs == nil {
		cs = &swarm.ContainerSpec{}
	}
	s := &Service{
		// The digest is dropped on purpose. The daemon resolves an image to
		// "nginx:1.27@sha256:…" at deploy time, and no stack file carries that,
		// so keeping it would make every service differ, every time, forever.
		// What the operator wants to know is whether the TAG changed.
		Image:       stripDigest(cs.Image),
		Entrypoint:  cs.Command, // swarm calls the entrypoint "Command"
		Command:     cs.Args,    // …and the command "Args"
		Environment: envMap(cs.Env),
		User:        cs.User,
		WorkingDir:  cs.Dir,
		Hostname:    cs.Hostname,
		ReadOnly:    cs.ReadOnly,
		StopSignal:  cs.StopSignal,
		StopGrace:   durPtr(cs.StopGracePeriod),
		Isolation:   isolation(cs.Isolation),
		CapAdd:      sortedCopy(cs.CapabilityAdd),
		CapDrop:     sortedCopy(cs.CapabilityDrop),
		Sysctls:     copyMap(cs.Sysctls),
		ExtraHosts:  extraHosts(cs.Hosts),
		Groups:      sortedCopy(cs.Groups),
		Labels:      stackLabels(cs.Labels),
	}
	if cs.Init != nil {
		s.Init = cs.Init
	}
	if cs.DNSConfig != nil {
		s.DNS = sortedCopy(addrStrings(cs.DNSConfig.Nameservers))
		s.DNSSearch = sortedCopy(cs.DNSConfig.Search)
	}
	s.Ports = portStrings(spec.EndpointSpec)
	s.Volumes = mountStrings(names, cs.Mounts)
	s.Networks = networkAttachments(spec, names, stripNamespace(ns, spec.Name))
	s.Secrets = secretRefs(names, cs.Secrets)
	s.Configs = configRefs(names, cs.Configs)
	s.Healthcheck = healthcheck(cs.Healthcheck)
	s.Logging = logging(spec.TaskTemplate.LogDriver)
	s.Deploy = deploySection(spec)
	return s
}

// extraHosts flips docker's /etc/hosts spelling into compose's.
//
// The engine stores "IP hostname [alias...]" — the literal line it writes into
// the container — while compose writes "hostname:IP". Exporting the engine's
// form produced a file whose extra_hosts silently vanished on reload, which the
// round-trip caught. One entry per hostname, since a single engine line can
// name several.
func extraHosts(hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	var out []string
	for _, h := range hosts {
		parts := strings.Fields(h)
		if len(parts) < 2 {
			continue
		}
		for _, name := range parts[1:] {
			out = append(out, name+":"+parts[0])
		}
	}
	return sortedCopy(out)
}

// isolation drops docker's "default", which the daemon fills in on every
// deployed service and which no stack file ever spells out.
func isolation(i container.Isolation) string {
	if i == "" || i.IsDefault() {
		return ""
	}
	return string(i)
}

// stripDigest removes an "@sha256:…" suffix, keeping the tag.
func stripDigest(image string) string {
	if i := strings.Index(image, "@sha256:"); i > 0 {
		return image[:i]
	}
	return image
}

// stackLabels drops the bookkeeping labels docker puts on everything in a
// stack. They are present on every deployed object and in no stack file, so
// they would otherwise be a difference on every single service.
func stackLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range in {
		if k == "com.docker.stack.namespace" || k == "com.docker.stack.image" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// sortedCopy orders a list the daemon returns in arbitrary order, and drops
// duplicates. Without the ordering a diff would flap between two runs that
// changed nothing; without the de-duplication an exported stack would not
// round-trip, because docker's converter appends the service's own name to its
// network aliases whether or not the file already lists it. A repeated alias,
// capability or nameserver means nothing anyway, so collapsing them cannot hide
// a real difference.
func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// portStrings renders published ports in compose's long-ish short form. Sorted
// by what is published, so adding one port does not rewrite the whole block.
// portStrings renders published ports. The short "8080:80" form is used where
// it suffices and compose's long mapping where it does not — host-mode
// publishing has no short spelling, and writing one anyway produced a file that
// would not load ("invalid containerPort: 5432 (host)"). Sorted by the rendered
// text so adding one port does not rewrite the block.
func portStrings(ep *swarm.EndpointSpec) []any {
	if ep == nil || len(ep.Ports) == 0 {
		return nil
	}
	type entry struct {
		key string
		val any
	}
	entries := make([]entry, 0, len(ep.Ports))
	for _, p := range ep.Ports {
		proto := string(p.Protocol)
		if p.Protocol == network.TCP {
			proto = "" // the default; spelling it on one side only is a phantom
		}
		if p.PublishMode == swarm.PortConfigPublishModeHost {
			entries = append(entries, entry{
				key: fmt.Sprintf("%010d:%010d/host", p.PublishedPort, p.TargetPort),
				val: &Port{Target: p.TargetPort, Published: p.PublishedPort, Protocol: proto, Mode: "host"},
			})
			continue
		}
		s := fmt.Sprintf("%d:%d", p.PublishedPort, p.TargetPort)
		if p.PublishedPort == 0 {
			s = fmt.Sprintf("%d", p.TargetPort)
		}
		if proto != "" {
			s += "/" + proto
		}
		entries = append(entries, entry{key: fmt.Sprintf("%010d:%010d/%s", p.PublishedPort, p.TargetPort, proto), val: s})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.val)
	}
	return out
}

// mountStrings renders mounts as compose's "source:target[:ro]". Named volumes
// lose the stack prefix, since that is how the file spells them.
func mountStrings(names Names, mounts []mount.Mount) []string {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]string, 0, len(mounts))
	for _, m := range mounts {
		src := m.Source
		if m.Type == mount.TypeVolume {
			src = names.short(src)
		}
		s := m.Target
		if src != "" {
			s = src + ":" + m.Target
		}
		if m.ReadOnly {
			s += ":ro"
		}
		if m.Type != "" && m.Type != mount.TypeVolume && m.Type != mount.TypeBind {
			s += " (" + string(m.Type) + ")"
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// networkAttachments reads the task template's networks, falling back to the
// deprecated top-level field — older daemons and older specs put them there,
// and a stack deployed years ago must still compare correctly.
//
// selfName is dropped from the aliases. Docker's converter appends the
// service's own short name to every network it joins, so the alias carries no
// information — but a service deployed by some other route may not have it
// recorded, and then the same stack would differ from itself depending on how
// it was created.
func networkAttachments(spec swarm.ServiceSpec, names Names, selfName string) map[string]*NetAttach {
	// Only TaskTemplate.Networks: the top-level Spec.Networks of services
	// created before API 1.44 is gone from the API types, and daemons since
	// Docker 25 move it into the task template themselves.
	nets := spec.TaskTemplate.Networks
	if len(nets) == 0 {
		return nil
	}
	out := make(map[string]*NetAttach, len(nets))
	for _, n := range nets {
		var aliases []string
		for _, a := range n.Aliases {
			if a != selfName {
				aliases = append(aliases, a)
			}
		}
		out[names.network(n.Target)] = &NetAttach{Aliases: sortedCopy(aliases)}
	}
	return out
}

func secretRefs(names Names, refs []*swarm.SecretReference) []*Mounted {
	out := make([]*Mounted, 0, len(refs))
	for _, r := range refs {
		if r == nil {
			continue
		}
		m := &Mounted{Source: names.short(r.SecretName)}
		fillFile(m, r.File)
		out = append(out, m)
	}
	return sortMounted(out)
}

func configRefs(names Names, refs []*swarm.ConfigReference) []*Mounted {
	out := make([]*Mounted, 0, len(refs))
	for _, r := range refs {
		if r == nil {
			continue
		}
		m := &Mounted{Source: names.short(r.ConfigName)}
		if r.File != nil {
			m.Target, m.UID, m.GID = r.File.Name, r.File.UID, r.File.GID
			m.Mode = uint32(r.File.Mode)
		}
		out = append(out, m)
	}
	return sortMounted(out)
}

func fillFile(m *Mounted, f *swarm.SecretReferenceFileTarget) {
	if f == nil {
		return
	}
	m.Target, m.UID, m.GID = f.Name, f.UID, f.GID
	m.Mode = uint32(f.Mode)
}

func sortMounted(in []*Mounted) []*Mounted {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool { return in[i].Source < in[j].Source })
	return in
}

func healthcheck(h *container.HealthConfig) *Healthcheck {
	if h == nil {
		return nil
	}
	// A single "NONE" is how docker spells a disabled healthcheck.
	if len(h.Test) == 1 && h.Test[0] == "NONE" {
		return &Healthcheck{Disable: true}
	}
	if len(h.Test) == 0 && h.Interval == 0 && h.Timeout == 0 && h.Retries == 0 && h.StartPeriod == 0 {
		return nil
	}
	return &Healthcheck{
		Test:        append([]string(nil), h.Test...),
		Interval:    dur(h.Interval),
		Timeout:     dur(h.Timeout),
		Retries:     uint64(h.Retries),
		StartPeriod: dur(h.StartPeriod),
	}
}

func logging(d *swarm.Driver) *Logging {
	if d == nil || (d.Name == "" && len(d.Options) == 0) {
		return nil
	}
	return &Logging{Driver: d.Name, Options: copyMap(d.Options)}
}

// deploySection renders everything compose puts under "deploy:". Daemon
// defaults are elided here; see swarmDefaults for why.
func deploySection(spec swarm.ServiceSpec) *Deploy {
	d := &Deploy{Labels: stackLabels(spec.Labels)}

	switch {
	case spec.Mode.Global != nil:
		d.Mode = "global"
	case spec.Mode.Replicated != nil && spec.Mode.Replicated.Replicas != nil:
		n := *spec.Mode.Replicated.Replicas
		d.Replicas = &n
	}

	if spec.EndpointSpec != nil && string(spec.EndpointSpec.Mode) != "" &&
		string(spec.EndpointSpec.Mode) != defaultEndpointMode {
		d.EndpointMode = string(spec.EndpointSpec.Mode)
	}

	if p := spec.TaskTemplate.Placement; p != nil {
		pl := &Placement{Constraints: sortedCopy(p.Constraints), MaxReplicas: p.MaxReplicas}
		for _, pref := range p.Preferences {
			if pref.Spread != nil {
				pl.Preferences = append(pl.Preferences, &PlacementPreference{Spread: pref.Spread.SpreadDescriptor})
			}
		}
		// Order is significant for preferences — swarm applies them in turn — so
		// this one list is deliberately NOT sorted.

		if len(pl.Constraints) > 0 || len(pl.Preferences) > 0 || pl.MaxReplicas > 0 {
			d.Placement = pl
		}
	}

	if r := spec.TaskTemplate.Resources; r != nil {
		res := &Resources{}
		if l := r.Limits; l != nil {
			if set := resourceSet(l.NanoCPUs, l.MemoryBytes, l.Pids); set != nil {
				res.Limits = set
			}
		}
		if rr := r.Reservations; rr != nil {
			if set := resourceSet(rr.NanoCPUs, rr.MemoryBytes, 0); set != nil {
				res.Reservations = set
			}
		}
		if res.Limits != nil || res.Reservations != nil {
			d.Resources = res
		}
	}

	d.RestartPolicy = restartPolicy(spec.TaskTemplate.RestartPolicy)
	d.UpdateConfig = updateConfig(spec.UpdateConfig)
	d.Rollback = updateConfig(spec.RollbackConfig)

	if d.Mode == "" && d.Replicas == nil && d.Labels == nil && d.EndpointMode == "" &&
		d.Placement == nil && d.Resources == nil && d.RestartPolicy == nil &&
		d.UpdateConfig == nil && d.Rollback == nil {
		return nil
	}
	return d
}

func resourceSet(cpus, mem, pids int64) *ResourceSet {
	c, m := nanoCPUs(cpus), bytesValue(mem)
	if c == "" && m == "" && pids == 0 {
		return nil
	}
	return &ResourceSet{CPUs: c, Memory: m, PIDs: pids}
}

// restartPolicy elides the daemon's default so an unspecified policy on one
// side and the filled-in default on the other do not read as a change.
func restartPolicy(p *swarm.RestartPolicy) *RestartPolicy {
	if p == nil {
		return nil
	}
	out := &RestartPolicy{Window: durPtr(p.Window)}
	if c := string(p.Condition); c != "" && c != defaultRestartCondition {
		out.Condition = c
	}
	if p.Delay != nil && *p.Delay != defaultRestartDelay {
		out.Delay = dur(*p.Delay)
	}
	if p.MaxAttempts != nil && *p.MaxAttempts != 0 {
		n := *p.MaxAttempts
		out.MaxAttempts = &n
	}
	if out.Condition == "" && out.Delay == "" && out.MaxAttempts == nil && out.Window == "" {
		return nil
	}
	return out
}

func updateConfig(u *swarm.UpdateConfig) *UpdateConfig {
	if u == nil {
		return nil
	}
	out := &UpdateConfig{
		Delay:           dur(u.Delay),
		Monitor:         dur(u.Monitor),
		MaxFailureRatio: u.MaxFailureRatio,
	}
	if u.Parallelism != defaultUpdateParallel {
		n := u.Parallelism
		out.Parallelism = &n
	}
	if u.FailureAction != "" && string(u.FailureAction) != defaultUpdateFailure {
		out.FailureAction = string(u.FailureAction)
	}
	if u.Order != "" && string(u.Order) != defaultUpdateOrder {
		out.Order = string(u.Order)
	}
	if out.Parallelism == nil && out.Delay == "" && out.FailureAction == "" &&
		out.Monitor == "" && out.MaxFailureRatio == 0 && out.Order == "" {
		return nil
	}
	return out
}

// Unmodelled names the spec fields that are set but that this rendering does
// not carry. It exists because the dangerous failure of a comparison tool is
// not a wrong answer but a confident silence: without this, a stack differing
// only in a field the model ignores would diff clean. The caller puts these in
// the stack's notes, so "no differences" always comes with the caveat attached.
func Unmodelled(spec swarm.ServiceSpec) []string {
	var out []string
	cs := spec.TaskTemplate.ContainerSpec
	if cs != nil {
		if cs.Privileges != nil && (cs.Privileges.CredentialSpec != nil || cs.Privileges.SELinuxContext != nil ||
			cs.Privileges.Seccomp != nil || cs.Privileges.AppArmor != nil || cs.Privileges.NoNewPrivileges) {
			out = append(out, "privileges (seccomp, apparmor, selinux, credential spec)")
		}
		if len(cs.Ulimits) > 0 {
			out = append(out, "ulimits")
		}
		if cs.OomScoreAdj != 0 {
			out = append(out, "oom_score_adj")
		}
		if cs.TTY {
			out = append(out, "tty")
		}
		if cs.OpenStdin {
			out = append(out, "stdin_open")
		}
	}
	if spec.TaskTemplate.PluginSpec != nil {
		out = append(out, "plugin spec")
	}
	if spec.TaskTemplate.Runtime != "" && spec.TaskTemplate.Runtime != swarm.RuntimeContainer {
		out = append(out, "runtime "+string(spec.TaskTemplate.Runtime))
	}
	return out
}

// addrStrings spells DNS server addresses the way a stack file writes them.
func addrStrings(as []netip.Addr) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.String())
	}
	return out
}
