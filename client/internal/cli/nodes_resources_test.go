package cli

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

func nodeWithCapacity(id, host string, cpus, mem int64) swarm.Node {
	var n swarm.Node
	n.ID = id
	n.Description.Hostname = host
	n.Description.Resources.NanoCPUs = cpus
	n.Description.Resources.MemoryBytes = mem
	n.Spec.Role = swarm.NodeRoleWorker
	return n
}

// reservedTask is a task on node with the given reservation, in a state that
// still occupies capacity.
func reservedTask(nodeID string, cpus, mem int64) swarm.Task {
	t := swarm.Task{NodeID: nodeID}
	t.DesiredState = swarm.TaskStateRunning
	t.Status.State = swarm.TaskStateRunning
	t.Spec.Resources = &swarm.ResourceRequirements{
		Reservations: &swarm.Resources{NanoCPUs: cpus, MemoryBytes: mem},
	}
	return t
}

const oneCPU = 1_000_000_000

func TestBuildNodeInfosSumsReservations(t *testing.T) {
	nodes := []swarm.Node{nodeWithCapacity("n1", "host-a", 4*oneCPU, 8<<30)}
	tasks := []swarm.Task{
		reservedTask("n1", oneCPU, 1<<30),
		reservedTask("n1", oneCPU/2, 512<<20),
	}
	got := buildNodeInfos(nodes, tasks)
	if len(got) != 1 {
		t.Fatalf("got %d nodes, want 1", len(got))
	}
	n := got[0]
	if want := oneCPU + oneCPU/2; n.ReservedNanoCPUs != int64(want) {
		t.Errorf("ReservedNanoCPUs = %d, want %d", n.ReservedNanoCPUs, want)
	}
	if want := int64(1<<30 + 512<<20); n.ReservedMemoryBytes != want {
		t.Errorf("ReservedMemoryBytes = %d, want %d", n.ReservedMemoryBytes, want)
	}
	if n.TasksWithoutReservation != 0 {
		t.Errorf("TasksWithoutReservation = %d, want 0", n.TasksWithoutReservation)
	}
}

// Terminal tasks stay in the task list as history — counting them would inflate
// every node's booked figure and make a healthy node look full.
func TestBuildNodeInfosIgnoresTerminalTasks(t *testing.T) {
	nodes := []swarm.Node{nodeWithCapacity("n1", "host-a", 4*oneCPU, 8<<30)}

	shutdownDesired := reservedTask("n1", oneCPU, 1<<30)
	shutdownDesired.DesiredState = swarm.TaskStateShutdown

	failed := reservedTask("n1", oneCPU, 1<<30)
	failed.Status.State = swarm.TaskStateFailed

	complete := reservedTask("n1", oneCPU, 1<<30)
	complete.Status.State = swarm.TaskStateComplete

	live := reservedTask("n1", oneCPU, 1<<30)

	got := buildNodeInfos(nodes, []swarm.Task{shutdownDesired, failed, complete, live})[0]
	if got.ReservedNanoCPUs != oneCPU {
		t.Errorf("ReservedNanoCPUs = %d, want %d (only the live task counts)", got.ReservedNanoCPUs, int64(oneCPU))
	}
}

// A task that is assigned but not yet running already holds its reservation —
// counting only "running" would understate a node mid-deploy.
func TestBuildNodeInfosCountsNotYetRunningTasks(t *testing.T) {
	nodes := []swarm.Node{nodeWithCapacity("n1", "host-a", 4*oneCPU, 8<<30)}
	starting := reservedTask("n1", oneCPU, 1<<30)
	starting.Status.State = swarm.TaskStatePreparing

	got := buildNodeInfos(nodes, []swarm.Task{starting})[0]
	if got.ReservedNanoCPUs != oneCPU {
		t.Errorf("ReservedNanoCPUs = %d, want %d (a preparing task holds its reservation)", got.ReservedNanoCPUs, int64(oneCPU))
	}
	if got.Tasks != 0 {
		t.Errorf("Tasks = %d, want 0 — only running tasks count as running", got.Tasks)
	}
}

// Tasks with no reservation are counted separately: they book nothing yet can
// still consume the node, so the figure must not silently pretend they are free.
func TestBuildNodeInfosTracksUnreservedTasks(t *testing.T) {
	nodes := []swarm.Node{nodeWithCapacity("n1", "host-a", 4*oneCPU, 8<<30)}
	bare := swarm.Task{NodeID: "n1"}
	bare.DesiredState = swarm.TaskStateRunning
	bare.Status.State = swarm.TaskStateRunning

	zeroRes := reservedTask("n1", 0, 0) // Reservations present but empty

	got := buildNodeInfos(nodes, []swarm.Task{bare, zeroRes, reservedTask("n1", oneCPU, 1<<30)})[0]
	if got.TasksWithoutReservation != 2 {
		t.Errorf("TasksWithoutReservation = %d, want 2", got.TasksWithoutReservation)
	}
	if got.ReservedNanoCPUs != oneCPU {
		t.Errorf("ReservedNanoCPUs = %d, want %d", got.ReservedNanoCPUs, int64(oneCPU))
	}
}

// Reservations must land on the node the task runs on, not be pooled.
func TestBuildNodeInfosAttributesPerNode(t *testing.T) {
	nodes := []swarm.Node{
		nodeWithCapacity("n1", "host-a", 4*oneCPU, 8<<30),
		nodeWithCapacity("n2", "host-b", 4*oneCPU, 8<<30),
	}
	tasks := []swarm.Task{
		reservedTask("n1", oneCPU, 1<<30),
		reservedTask("n2", 2*oneCPU, 2<<30),
		reservedTask("", oneCPU, 1<<30), // unassigned: belongs to no node
	}
	by := map[string]swarmNodeInfo{}
	for _, n := range buildNodeInfos(nodes, tasks) {
		by[n.Hostname] = n
	}
	if by["host-a"].ReservedNanoCPUs != oneCPU {
		t.Errorf("host-a cpu = %d, want %d", by["host-a"].ReservedNanoCPUs, int64(oneCPU))
	}
	if by["host-b"].ReservedNanoCPUs != 2*oneCPU {
		t.Errorf("host-b cpu = %d, want %d", by["host-b"].ReservedNanoCPUs, int64(2*oneCPU))
	}
}

func TestNodeResourceSection(t *testing.T) {
	n := swarmNodeInfo{
		Hostname: "host-a", NanoCPUs: 4 * oneCPU, MemoryBytes: 8 << 30,
		ReservedNanoCPUs: 2 * oneCPU, ReservedMemoryBytes: 4 << 30,
	}
	got := nodeResourceSection(n)
	if !strings.Contains(got, "free") {
		t.Errorf("section should report what is left:\n%s", got)
	}
	// Half booked → neither the empty nor the full bar.
	if !strings.Contains(got, "█") || !strings.Contains(got, "░") {
		t.Errorf("bar should be partially filled:\n%s", got)
	}

	// A node reporting no capacity must say so rather than divide by zero.
	empty := nodeResourceSection(swarmNodeInfo{Hostname: "x"})
	if !strings.Contains(empty, "no capacity") {
		t.Errorf("a capacity-less node should be called out:\n%s", empty)
	}

	// Over-commitment must be visible, and "free" must not go negative.
	over := nodeResourceSection(swarmNodeInfo{
		Hostname: "host-b", NanoCPUs: oneCPU, MemoryBytes: 1 << 30,
		ReservedNanoCPUs: 3 * oneCPU, ReservedMemoryBytes: 3 << 30,
	})
	if !strings.Contains(over, "over-committed") {
		t.Errorf("over-commitment should be flagged:\n%s", over)
	}
	if strings.Contains(over, "-") && strings.Contains(over, "(-") {
		t.Errorf("free must not render negative:\n%s", over)
	}

	// Unreserved tasks are called out, so 0%% does not read as "idle".
	withBare := nodeResourceSection(swarmNodeInfo{
		Hostname: "host-c", NanoCPUs: 4 * oneCPU, MemoryBytes: 8 << 30,
		TasksWithoutReservation: 3,
	})
	if !strings.Contains(withBare, "no reservation") {
		t.Errorf("unreserved tasks should be called out:\n%s", withBare)
	}
}

func TestResourceBarBounds(t *testing.T) {
	if got := resourceBar(0, 100); strings.Count(got, "█") != 0 {
		t.Errorf("empty bar has %d filled cells, want 0", strings.Count(got, "█"))
	}
	if got := resourceBar(100, 100); strings.Count(got, "█") != resourceBarWidth {
		t.Errorf("full bar has %d filled cells, want %d", strings.Count(got, "█"), resourceBarWidth)
	}
	// Over-committed must clamp, not overflow the bar.
	if got := resourceBar(500, 100); strings.Count(got, "█") != resourceBarWidth {
		t.Errorf("over-committed bar has %d filled cells, want %d", strings.Count(got, "█"), resourceBarWidth)
	}
	if got := resourceBar(1, 0); strings.Count(got, "█") != 0 {
		t.Errorf("zero capacity must not panic or fill: %q", got)
	}
}
