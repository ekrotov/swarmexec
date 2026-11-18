package swarmops

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/swarmapi"
	"swarmexec/client/internal/swarmfake"
)

// stackService is a service as `docker stack deploy` writes it, at version 5.
func stackService() swarm.Service {
	svc := swarm.Service{ID: "s1"}
	svc.Version.Index = 5
	svc.Spec.Name = "shop_api"
	svc.Spec.Labels = map[string]string{"com.docker.stack.namespace": "shop"}
	svc.Spec.Mode.Replicated = &swarm.ReplicatedService{Replicas: new(uint64)}
	svc.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{
		Image: "api:1", Env: []string{"A=1"},
		Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: "shop_data", Target: "/data"}},
	}
	svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "net1"}}
	svc.Spec.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{{Protocol: network.TCP, TargetPort: 80, PublishedPort: 80}}}
	return svc
}

// KOR-05: every service editor reads (Current*) when it opens and writes (Set*)
// minutes later. A change in between — a CI deploy, a second operator — must be
// refused, not silently rolled back to what the editor loaded.
func TestServiceEditsRefuseStaleLoad(t *testing.T) {
	type editor struct {
		name  string
		load  func(ctx context.Context, d swarmapi.Client) error
		apply func(ctx context.Context, d swarmapi.Client) error
	}
	const svcName = "shop_api"
	editors := []editor{
		{"env",
			func(ctx context.Context, d swarmapi.Client) error {
				_, err := CurrentServiceEnv(ctx, d, svcName)
				return err
			},
			func(ctx context.Context, d swarmapi.Client) error {
				return SetServiceEnv(ctx, d, svcName, []string{"A=1", "B=2"})
			}},
		{"labels",
			func(ctx context.Context, d swarmapi.Client) error {
				_, err := CurrentServiceLabels(ctx, d, svcName)
				return err
			},
			func(ctx context.Context, d swarmapi.Client) error {
				return SetServiceLabels(ctx, d, svcName, map[string]string{"x": "y"})
			}},
		{"ports",
			func(ctx context.Context, d swarmapi.Client) error {
				_, err := CurrentServicePorts(ctx, d, svcName)
				return err
			},
			func(ctx context.Context, d swarmapi.Client) error {
				return SetServicePorts(ctx, d, svcName, []swarm.PortConfig{{Protocol: network.TCP, TargetPort: 81, PublishedPort: 81}})
			}},
		{"mounts",
			func(ctx context.Context, d swarmapi.Client) error {
				_, err := CurrentServiceMountSpecs(ctx, d, svcName)
				return err
			},
			func(ctx context.Context, d swarmapi.Client) error { return SetServiceMounts(ctx, d, svcName, nil) }},
		{"placement",
			func(ctx context.Context, d swarmapi.Client) error {
				_, err := CurrentServicePlacement(ctx, d, svcName)
				return err
			},
			func(ctx context.Context, d swarmapi.Client) error {
				return SetServicePlacement(ctx, d, svcName, []string{"node.role==worker"})
			}},
		{"resources",
			func(ctx context.Context, d swarmapi.Client) error {
				_, err := CurrentServiceResources(ctx, d, svcName)
				return err
			},
			func(ctx context.Context, d swarmapi.Client) error {
				return SetServiceResources(ctx, d, svcName, 0, 1<<28, 0, 0)
			}},
		{"scale",
			func(ctx context.Context, d swarmapi.Client) error {
				_, _, err := CurrentServiceReplicas(ctx, d, svcName)
				return err
			},
			func(ctx context.Context, d swarmapi.Client) error { return ScaleService(ctx, d, svcName, 3) }},
		{"networks",
			func(ctx context.Context, d swarmapi.Client) error {
				_, err := CurrentServiceNetworks(ctx, d, svcName, map[string]string{"net1": "shop_backend"})
				return err
			},
			func(ctx context.Context, d swarmapi.Client) error {
				return SetServiceNetworks(ctx, d, svcName, []string{"net1"})
			}},
	}
	// "another operator" adds an env var, which also bumps the version.
	concurrent := func(f *swarmfake.Swarm) {
		cs := f.Services[0].Spec.TaskTemplate.ContainerSpec
		cs.Env = append(cs.Env, "FROM_CI=1")
		f.Services[0].Version.Index++
	}
	for _, e := range editors {
		t.Run(e.name+"/concurrent change refused", func(t *testing.T) {
			f := &swarmfake.Swarm{Services: []swarm.Service{stackService()},
				Networks: []network.Summary{{Network: network.Network{ID: "net1", Name: "shop_backend"}}}}
			d := swarmfake.Client(t, f)
			ctx := WithBaseline(context.Background())
			if err := e.load(ctx, d); err != nil {
				t.Fatal(err)
			}
			concurrent(f)
			want := f.Services[0].Spec.TaskTemplate.ContainerSpec.Env
			err := e.apply(ctx, d)
			var ce *ConflictError
			if !errors.As(err, &ce) || !errors.Is(err, ErrConflict) {
				t.Fatalf("err = %v, want a *ConflictError", err)
			}
			if ce.Kind != "service" || ce.Name != svcName || ce.Loaded != 5 || ce.Current != 6 {
				t.Errorf("conflict = %+v", ce)
			}
			if f.Services[0].Version.Index != 6 || !reflect.DeepEqual(f.Services[0].Spec.TaskTemplate.ContainerSpec.Env, want) {
				t.Errorf("a refused edit wrote: version %d env %v", f.Services[0].Version.Index, f.Services[0].Spec.TaskTemplate.ContainerSpec.Env)
			}
		})
		t.Run(e.name+"/unchanged applies", func(t *testing.T) {
			f := &swarmfake.Swarm{Services: []swarm.Service{stackService()},
				Networks: []network.Summary{{Network: network.Network{ID: "net1", Name: "shop_backend"}}}}
			d := swarmfake.Client(t, f)
			ctx := WithBaseline(context.Background())
			if err := e.load(ctx, d); err != nil {
				t.Fatal(err)
			}
			if err := e.apply(ctx, d); err != nil {
				t.Fatalf("unchanged service refused: %v", err)
			}
			if f.Services[0].Version.Index != 6 {
				t.Errorf("version = %d, want 6 (one update)", f.Services[0].Version.Index)
			}
		})
	}
}

func TestServiceEditConflictMessageAndRecovery(t *testing.T) {
	f := &swarmfake.Swarm{Services: []swarm.Service{stackService()}}
	d := swarmfake.Client(t, f)
	ctx := WithBaseline(context.Background())
	if _, err := CurrentServiceEnv(ctx, d, "shop_api"); err != nil {
		t.Fatal(err)
	}
	f.Services[0].Version.Index = 9
	err := SetServiceEnv(ctx, d, "shop_api", []string{"A=1"})
	if err == nil || !strings.Contains(err.Error(), `service "shop_api" was changed by someone else since you opened this editor (version 5, now 9)`) ||
		!strings.Contains(err.Error(), "nothing was written") || !strings.Contains(err.Error(), "reopen") {
		t.Fatalf("message = %v", err)
	}
	// Reloading (a fresh Current*) re-bases the editor; the same apply then goes through.
	if _, err := CurrentServiceEnv(ctx, d, "shop_api"); err != nil {
		t.Fatal(err)
	}
	if err := SetServiceEnv(ctx, d, "shop_api", []string{"A=1"}); err != nil {
		t.Fatalf("after reload: %v", err)
	}
	// Applied: the baseline is consumed, so a later apply that no editor
	// loaded for is not blamed on a long-gone load.
	f.Services[0].Version.Index = 20
	if err := SetServiceEnv(ctx, d, "shop_api", []string{"A=2"}); err != nil {
		t.Errorf("apply without a fresh load: %v", err)
	}
}

// An action no editor loaded for (force update, image update) is not held to a
// baseline left behind by an editor that was cancelled earlier.
func TestActionsIgnoreAbandonedBaseline(t *testing.T) {
	f := &swarmfake.Swarm{Services: []swarm.Service{stackService()}}
	d := swarmfake.Client(t, f)
	ctx := WithBaseline(context.Background())
	if _, err := CurrentServiceEnv(ctx, d, "shop_api"); err != nil {
		t.Fatal(err)
	}
	f.Services[0].Version.Index = 9
	if err := ForceUpdateService(ctx, d, "shop_api"); err != nil {
		t.Errorf("force update: %v", err)
	}
	if err := UpdateServiceImage(ctx, d, "shop_api", "api:2"); err != nil {
		t.Errorf("image update: %v", err)
	}
}

func TestAsConflictRecognisesSwarmsOwnRefusal(t *testing.T) {
	err := AsConflict("service", "web", errors.New("rpc error: update out of sequence"))
	if !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v", err)
	}
	other := errors.New("boom")
	if AsConflict("service", "web", other) != other {
		t.Error("an unrelated error was rewritten")
	}
}
