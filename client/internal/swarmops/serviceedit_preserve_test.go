package swarmops

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/swarmfake"
)

// KOR-01 / TST-04: the TUI editors' data layer must leave the entries the
// operator did NOT touch exactly as they were. A stack-deployed service, shaped as `docker stack
// deploy` writes it; each editor is driven exactly as ui_serviceedit.go does:
// Current*() strings -> add one entry -> *FromStrings -> Set*().
func TestEditorsPreserveUntouchedEntries(t *testing.T) {
	svc := swarm.Service{ID: "s1"}
	svc.Spec.Name = "shop_api"
	svc.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{
		Image: "api:1",
		Secrets: []*swarm.SecretReference{{
			SecretID: "sec1", SecretName: "shop_db_password",
			File: &swarm.SecretReferenceFileTarget{Name: "db_password", UID: "1000", GID: "1000", Mode: os.FileMode(0o400)},
		}},
		Mounts: []mount.Mount{{
			Type: mount.TypeVolume, Source: "shop_data", Target: "/data",
			VolumeOptions: &mount.VolumeOptions{
				Labels:       map[string]string{"com.docker.stack.namespace": "shop"},
				DriverConfig: &mount.Driver{Name: "local", Options: map[string]string{"type": "nfs", "o": "addr=10.0.0.9,rw", "device": ":/export/shop"}},
			},
		}, {
			Type: mount.TypeTmpfs, Target: "/scratch",
			TmpfsOptions: &mount.TmpfsOptions{SizeBytes: 1 << 20},
		}},
	}
	svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "net1", Aliases: []string{"api"}}}
	svc.Spec.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{{
		Protocol: network.TCP, TargetPort: 9443, PublishedPort: 9443, PublishMode: swarm.PortConfigPublishModeHost,
	}}}
	f := &swarmfake.Swarm{
		Services: []swarm.Service{svc},
		Secrets: []swarm.Secret{
			{ID: "sec1", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "shop_db_password"}}},
			{ID: "sec2", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "shop_api_key"}}},
		},
		Networks: []network.Summary{{Network: network.Network{ID: "net1", Name: "shop_backend"}}, {Network: network.Network{ID: "net2", Name: "monitoring"}}},
	}
	dcli := swarmfake.Client(t, f)
	ctx := context.Background()
	spec := func() swarm.ServiceSpec { return f.Services[0].Spec }
	wantSecret := *svc.Spec.TaskTemplate.ContainerSpec.Secrets[0]
	wantSecretFile := *wantSecret.File
	wantMounts := append([]mount.Mount(nil), svc.Spec.TaskTemplate.ContainerSpec.Mounts...)
	wantNet := svc.Spec.TaskTemplate.Networks[0]
	wantPort := svc.Spec.EndpointSpec.Ports[0]

	// Secrets editor: add ONE secret.
	names, err := CurrentServiceSecrets(ctx, dcli, "shop_api")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetServiceSecrets(ctx, dcli, "shop_api", append(names, "shop_api_key")); err != nil {
		t.Fatal(err)
	}
	secs := spec().TaskTemplate.ContainerSpec.Secrets
	if len(secs) != 2 {
		t.Fatalf("secrets = %d, want 2", len(secs))
	}
	if got := *secs[0]; got.SecretID != wantSecret.SecretID || !reflect.DeepEqual(*got.File, wantSecretFile) {
		t.Errorf("untouched secret rewritten: %+v file %+v, want file %+v", got, *got.File, wantSecretFile)
	}
	if secs[1].SecretName != "shop_api_key" || secs[1].File.Name != "shop_api_key" {
		t.Errorf("new secret = %+v", secs[1])
	}
	// Networks editor: attach ONE more network.
	if err := SetServiceNetworks(ctx, dcli, "shop_api", []string{"net1", "net2"}); err != nil {
		t.Fatal(err)
	}
	nets := spec().TaskTemplate.Networks
	if len(nets) != 2 || !reflect.DeepEqual(nets[0], wantNet) || nets[1].Target != "net2" {
		t.Errorf("networks = %+v, want %+v kept and net2 added", nets, wantNet)
	}

	// Ports editor: add ONE port.
	ports, err := CurrentServicePorts(ctx, dcli, "shop_api")
	if err != nil {
		t.Fatal(err)
	}
	pc, err := PortsFromStrings(append(ports, "8080:80/tcp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := SetServicePorts(ctx, dcli, "shop_api", pc); err != nil {
		t.Fatal(err)
	}
	ps := spec().EndpointSpec.Ports
	if len(ps) != 2 || !reflect.DeepEqual(ps[0], wantPort) || ps[1].PublishMode != swarm.PortConfigPublishModeIngress {
		t.Errorf("ports = %+v, want %+v kept (host mode) and one ingress port added", ps, wantPort)
	}

	// Mounts editor: add ONE mount.
	ms, err := CurrentServiceMountSpecs(ctx, dcli, "shop_api")
	if err != nil {
		t.Fatal(err)
	}
	mm, err := MountsFromStrings(append(ms, "bind:/etc/ssl:/ssl:ro"))
	if err != nil {
		t.Fatal(err)
	}
	if err := SetServiceMounts(ctx, dcli, "shop_api", mm); err != nil {
		t.Fatal(err)
	}
	got := spec().TaskTemplate.ContainerSpec.Mounts
	if len(got) != 3 || !reflect.DeepEqual(got[:2], wantMounts) || got[2].Target != "/ssl" {
		t.Errorf("mounts = %+v, want %+v kept and /ssl added", got, wantMounts)
	}
}

func TestPortHostModeRoundTrips(t *testing.T) {
	p := swarm.PortConfig{Protocol: network.TCP, TargetPort: 80, PublishedPort: 8080, PublishMode: swarm.PortConfigPublishModeHost}
	s := FormatServicePort(p)
	if s != "8080:80/tcp host" {
		t.Fatalf("format = %q", s)
	}
	got, err := ParseServicePort(s)
	if err != nil || got != p {
		t.Fatalf("parse = %+v, %v", got, err)
	}
}

func TestTmpfsMountRoundTrips(t *testing.T) {
	m := mount.Mount{Type: mount.TypeTmpfs, Target: "/tmp"}
	got, err := ParseServiceMount(FormatServiceMount(m))
	if err != nil || got.Type != mount.TypeTmpfs || got.Target != "/tmp" {
		t.Fatalf("parse = %+v, %v", got, err)
	}
	if _, err := ParseServiceMount("image:x:/y"); err == nil {
		t.Fatal("unknown type accepted")
	}
}
