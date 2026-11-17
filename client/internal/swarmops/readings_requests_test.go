package swarmops

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmfake"
)

// PRF-01: manager API requests of ONE tree refresh (ServicesAndContainers, the
// read behind the TUI tree, `ps`, stats and MCP). Before the fix every running
// container cost a GET /nodes/{id} (333 requests for 300 containers on 5
// nodes); now the nodes are one list, however many containers run.
func TestTopologyReadRequestCount(t *testing.T) {
	for _, n := range []int{30, 300} {
		f := &swarmfake.Swarm{}
		for i := 0; i < 5; i++ {
			nd := swarm.Node{ID: fmt.Sprintf("n%d", i)}
			nd.Description.Hostname = fmt.Sprintf("node-%d", i)
			f.Nodes = append(f.Nodes, nd)
		}
		one := uint64(1)
		for s := 0; s < n/10; s++ {
			svc := swarm.Service{ID: fmt.Sprintf("s%d", s)}
			svc.Spec.Name = fmt.Sprintf("svc%d", s)
			svc.Spec.Mode.Replicated = &swarm.ReplicatedService{Replicas: &one}
			f.Services = append(f.Services, svc)
			for k := 0; k < 10; k++ {
				tk := swarm.Task{ID: fmt.Sprintf("t%d-%d", s, k), ServiceID: svc.ID, Slot: k + 1, NodeID: fmt.Sprintf("n%d", k%5), DesiredState: swarm.TaskStateRunning}
				tk.Status.State = swarm.TaskStateRunning
				tk.Status.Timestamp = time.Now()
				tk.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: fmt.Sprintf("c%d-%d", s, k)}
				f.Tasks = append(f.Tasks, tk)
			}
		}
		var mu sync.Mutex
		counts := map[string]int{}
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if i := strings.Index(p[1:], "/"); i >= 0 && strings.HasPrefix(p, "/v") {
				p = p[i+1:]
			}
			if strings.HasPrefix(p, "/nodes/") {
				p = "/nodes/{id}"
			}
			mu.Lock()
			counts[r.Method+" "+p]++
			mu.Unlock()
			f.ServeHTTP(w, r)
		}))
		dcli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
		if err != nil {
			t.Fatal(err)
		}
		r := resolve.New(dcli, resolve.AddrHostname)
		// Warm-up call (API version negotiation), then count one refresh.
		if _, _, err := ServicesAndContainers(context.Background(), r, "review"); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		counts = map[string]int{}
		mu.Unlock()
		start := time.Now()
		_, cands, err := ServicesAndContainers(context.Background(), r, "review")
		if err != nil {
			t.Fatal(err)
		}
		dur := time.Since(start)
		mu.Lock()
		total, perTask := 0, 0
		var keys []string
		for k, v := range counts {
			total += v
			if k == "GET /tasks" {
				perTask = v // the fake fills no ServiceStatus, so Services() lists tasks
			}
			keys = append(keys, fmt.Sprintf("%s x%d", k, v))
		}
		inspects, lists := counts["GET /nodes/{id}"], counts["GET /nodes"]
		mu.Unlock()
		sort.Strings(keys)
		if inspects != 0 || lists != 1 {
			t.Errorf("%d containers: %d node inspects and %d node lists, want 0 and 1", len(cands), inspects, lists)
		}
		// Everything but the fake's per-service task listing is a handful of
		// requests, independent of the number of containers.
		if rest := total - perTask; rest > 4 {
			t.Errorf("%d containers: %d manager requests besides GET /tasks, want at most 4 (%s)", len(cands), rest, strings.Join(keys, ", "))
		}
		t.Logf("%3d running containers on 5 nodes: %d manager requests per refresh (%s) in %s on loopback", len(cands), total, strings.Join(keys, ", "), dur.Round(time.Millisecond))
		dcli.Close()
		api.Close()
	}
}
