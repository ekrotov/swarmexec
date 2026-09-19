# Shared Contract — swarmexec

This file is the **single source of truth** for the wire protocol between the
CLI client and the Swarm agent. Both `REQUIREMENTS-agent.md` and
`REQUIREMENTS-client.md` depend on this file. If anything here changes, both
components must be updated.

## 1. Overview

`swarmexec` lets an operator run an interactive `exec` (like `docker exec -it`)
against any container running in a Docker Swarm, from a single host, without
manually SSHing to the node that hosts the task.

Two binaries:

- **agent** — runs as a `global` Swarm service (one task per node). Has the
  local Docker socket mounted. Exposes a gRPC endpoint that creates and proxies
  an exec session into a container on *its own node*.
- **cli** — runs on the operator's machine. Resolves which node a task runs on
  via the Swarm manager API, connects directly to the agent on that node over
  mTLS gRPC, and bridges the operator's terminal to the exec session.

Routing model is **direct**: the CLI talks to the manager API to find the node,
then connects straight to that node's agent. There is no agent-to-agent mesh.

```
operator terminal ──gRPC/mTLS──> agent(nodeN) ──docker.sock──> container
        │
        └──Docker manager API (TaskList/NodeInspect) to resolve node + container id
```

## 2. Transport & Security

- Transport: **gRPC** over TLS with **mutual TLS** (both sides present certs).
- The agent authenticates the client via its client certificate. The client
  certificate's Common Name (CN) is the operator identity used for
  authorization and audit.
- The agent authorizes each exec request before starting it (authorization
  policy is defined in the agent requirements; default policy = allow any cert
  signed by the trusted CA).
- Default agent port: **9443** (gRPC, TLS).

### 2.1 Shared-secret authentication (self-signed agents)

When the agent runs with a shared secret instead of a client CA, every RPC
carries credentials in gRPC metadata:

| Key | Meaning |
|---|---|
| `x-swarmexec-binding` | **Current.** `base64url(HMAC-SHA256(secret, "swarmexec-channel-binding-v1" ‖ SHA-256(server leaf certificate DER)))`. |
| `x-swarmexec-secret` | **Legacy.** The raw secret. Accepted only while the agent runs with `-allow-legacy-secret`. |
| `x-swarmexec-operator` | Optional operator identity for audit when no client certificate is presented. Unauthenticated — see §2.2. |

Clients send the binding, never the raw secret. The binding is per-connection:
the agent recomputes it over its own certificate, so a proof collected from any
other connection — including one an on-path attacker terminated — does not
authenticate. The secret itself never travels.

This authenticates the *client* to the agent. It does not authenticate the agent
to the client: on a connection the client chose not to verify (`insecure`), the
peer still sees what that session sends it.

## 3. Protocol Definition (proto3)

This is the authoritative `.proto`. Both components generate Go code from it.

```proto
syntax = "proto3";
package swarmexec;
option go_package = "swarmexec/internal/pb";

service Agent {
  // List containers on THIS node. Optional helper for discovery/validation.
  rpc ListContainers(ListRequest) returns (ListResponse);

  // Bidirectional interactive exec stream.
  // The first ClientMessage on the stream MUST carry a StartExec payload.
  rpc Exec(stream ClientMessage) returns (stream ServerMessage);

  // Stream a container's logs. Server-streaming: the agent reads the container
  // logs on its own node and forwards them as LogChunks until the request is
  // satisfied (or, with follow=true, until the client cancels the stream).
  rpc Logs(LogsRequest) returns (stream LogChunk);

  // List volumes on THIS node. Swarm volumes are node-local, so the cli queries
  // every node and aggregates the results.
  rpc ListVolumes(ListVolumesRequest) returns (ListVolumesResponse);

  // Remove a volume on THIS node (authorized + audited). Fails if the volume is
  // in use unless force is set.
  rpc RemoveVolume(RemoveVolumeRequest) returns (RemoveVolumeResponse);

  // Create a volume on THIS node (authorized + audited). Volumes are node-local,
  // so the cli targets a specific node's agent.
  rpc CreateVolume(CreateVolumeRequest) returns (CreateVolumeResponse);

  // Forward a single TCP connection to a port inside a container on THIS node.
  // Bidirectional: one stream carries exactly one connection, so a local
  // listener opens a new stream per accepted conn (HTTP/2 multiplexes them over
  // the one transport). The first ClientMessage MUST carry a StartForward.
  rpc PortForward(stream ForwardClientMessage) returns (stream ForwardServerMessage);

  // Resource usage of the containers on THIS node. Unary on purpose: the agent
  // samples in the background and answers from memory, so a client polls this on
  // the refresh cycle it already has instead of holding a stream open per node.
  // It also solves the sampling problem — a CPU percentage needs two readings,
  // and the background sampler always has the previous one.
  rpc Stats(StatsRequest) returns (StatsResponse);

  // List the images on THIS node, with what each costs on disk and whether a
  // running container is using it. Images are node-local and the manager has no
  // view of them at all, so the cli asks each node in turn.
  rpc ListImages(ListImagesRequest) returns (ListImagesResponse);

  // Reclaim image disk space on THIS node (authorized + audited). Destructive:
  // see PruneImagesRequest.all for the two very different things it can mean.
  rpc PruneImages(PruneImagesRequest) returns (PruneImagesResponse);

  // Report the agent's build and protocol version. Cheap, low-privilege probe
  // used by `swarmexec doctor` and for client/agent skew detection. Calling it
  // on an agent that predates this RPC yields gRPC Unimplemented, which the cli
  // turns into an "agent too old — run init --force" hint.
  rpc Version(VersionRequest) returns (VersionResponse);
}

message ListRequest {
  string service_filter = 1; // optional substring/label filter; empty = all
}

message ListResponse {
  repeated ContainerInfo containers = 1;
}

message ContainerInfo {
  string id = 1;       // full container ID
  string name = 2;     // container name
  string service = 3;  // swarm service name if known, else empty
  repeated string volumes = 4;  // names of named volumes this container mounts
}

message ClientMessage {
  oneof payload {
    StartExec start = 1;  // MUST be the first message
    bytes stdin = 2;      // raw stdin bytes
    Resize resize = 3;    // terminal resize event
  }
}

message StartExec {
  string container_id = 1;     // full container ID to exec into
  repeated string cmd = 2;     // command + args, e.g. ["/bin/sh"]
  bool tty = 3;                // allocate a TTY
  uint32 width = 4;            // initial terminal width (columns)
  uint32 height = 5;           // initial terminal height (rows)
  repeated string env = 6;     // optional extra env, "KEY=VALUE"
  string working_dir = 7;      // optional working directory
  string user = 8;             // optional user, e.g. "1000:1000" or "root"
}

message Resize {
  uint32 width = 1;
  uint32 height = 2;
}

message ServerMessage {
  oneof payload {
    bytes stdout = 1;     // stdout bytes (also carries all output when tty=true)
    bytes stderr = 2;     // stderr bytes (only when tty=false)
    int32 exit_code = 3;  // sent once, as the final message before stream end
    string error = 4;     // terminal error; stream ends after this
  }
}

message LogsRequest {
  string container_id = 1;   // full container ID to read logs from
  bool follow = 2;           // keep streaming new log lines as they arrive
  uint32 tail = 3;           // last N lines to start from; 0 = all
  bool timestamps = 4;       // prefix each line with an RFC3339Nano timestamp
  uint32 since_seconds = 5;  // only logs newer than N seconds ago; 0 = no limit
}

message LogChunk {
  oneof payload {
    bytes stdout = 1;  // stdout bytes (also carries all output for TTY containers)
    bytes stderr = 2;  // stderr bytes (non-TTY containers only)
    string error = 3;  // terminal error; stream ends after this
  }
}

message ListVolumesRequest {
  // with_size asks the agent to also compute each volume's on-disk size via the
  // docker disk-usage endpoint (du-style; can be slow). Off by default so the
  // plain listing stays fast.
  bool with_size = 1;
}

message ListVolumesResponse {
  repeated VolumeInfo volumes = 1;
}

message VolumeInfo {
  string name = 1;
  string driver = 2;
  string mountpoint = 3;
  string created_at = 4;  // RFC3339, if known
  string scope = 5;       // "local" or "global"
  // size_bytes is the on-disk size, only meaningful when size_known is true.
  // -1 means "not available" (e.g. non-local driver).
  int64 size_bytes = 6;
  // size_known is true when the agent actually computed the size (with_size
  // requested and disk-usage succeeded). It lets the client tell "0 bytes" from
  // "an older agent that doesn't report sizes" (which would default size_bytes
  // to 0).
  bool size_known = 7;
  // labels are the volume's metadata labels, as set at creation time.
  map<string, string> labels = 8;
}

message RemoveVolumeRequest {
  string name = 1;
  bool force = 2;  // remove even with the "force" flag (does not override in-use)
}

message RemoveVolumeResponse {}

message CreateVolumeRequest {
  string name = 1;
  string driver = 2;                 // empty means "local"
  map<string, string> labels = 3;
  map<string, string> driver_opts = 4;
}

message CreateVolumeResponse {
  VolumeInfo volume = 1;             // the created volume
}

message StartForward {
  string container_id = 1;  // full container ID to forward into
  uint32 port = 2;          // TCP port inside the container
}

message ForwardClientMessage {
  oneof payload {
    StartForward start = 1;  // MUST be the first message
    bytes data = 2;          // raw bytes toward the container
  }
}

// ForwardReady is sent once, after the agent has established the connection to
// the target port and before any data. It lets the client distinguish "the
// target accepted" from "connected, but the peer has not spoken yet" — without
// it a forward pointing at a closed port looks healthy until the operator's
// own client times out.
message ForwardReady {}

message ForwardServerMessage {
  oneof payload {
    ForwardReady ready = 1;  // sent once, before any data
    bytes data = 2;          // raw bytes from the container
    string error = 3;        // terminal error; stream ends after this
  }
}

message VersionRequest {}

message VersionResponse {
  string version = 1;        // agent build version
  string proto_version = 2;  // wire protocol version
}

message StatsRequest {
  // container_ids narrows the answer to these containers; empty means every
  // container the agent is sampling. The cli sends the ids it is displaying so
  // a large node does not ship readings nobody looks at.
  repeated string container_ids = 1;
}

message StatsResponse {
  repeated ContainerStats stats = 1;

  // cpu_ready is false until the sampler has taken the TWO readings a CPU
  // percentage needs — it is a delta, unlike memory, which is valid from the
  // first sample. So a response can carry usable memory numbers with
  // cpu_ready=false, and the client renders "…" for CPU rather than a wrong 0%.
  bool cpu_ready = 2;

  // sampled_at is when the underlying sample was taken (RFC3339), so a client
  // can spot a stalled sampler rather than trusting stale numbers.
  string sampled_at = 3;

  // The node as a whole, for putting usage next to what the scheduler booked.
  // node_cpus is the online CPU count; usage percentages are relative to it
  // when a container has no CPU limit of its own.
  int64 node_cpus = 4;
  int64 node_memory_total_bytes = 5;
}

message ContainerStats {
  string container_id = 1;

  // cpu_percent is the same number `docker stats` prints: the share of ONE cpu,
  // so 250.0 means two and a half cores. Divide by cpu_limit_cores (or by
  // node_cpus when there is no limit) to get a 0-100 utilisation.
  double cpu_percent = 2;

  // cpu_limit_cores is the container's own CPU limit in cores, 0 when it has
  // none. With no limit the container may use the whole node, which is why the
  // client falls back to node_cpus for the ratio.
  double cpu_limit_cores = 3;

  int64 memory_bytes = 4;

  // memory_limit_bytes is the container's limit if it has one, otherwise the
  // node's total memory — the same substitution docker itself makes.
  // memory_limited says which of the two it is, so "80% of its limit" is never
  // confused with "80% of the node".
  int64 memory_limit_bytes = 5;
  bool memory_limited = 6;

  // health is the container's healthcheck verdict: "healthy", "unhealthy",
  // "starting", or "" when the container declares no healthcheck (and also when
  // the agent could not tell — the two are indistinguishable here, and both
  // mean "do not claim anything about this container's health").
  //
  // It rides along with the usage readings because the manager cannot supply
  // it: a swarm task reads "running" while its container fails every probe, so
  // health is only knowable on the node itself.
  string health = 7;
}

message ListImagesRequest {}

message ListImagesResponse {
  repeated ImageInfo images = 1;

  // Totals for the node, so a client can say what is at stake without summing a
  // long list — and can show the safe and the risky figure separately, because
  // they are what the operator is choosing between.
  int64 total_bytes = 2;
  int64 dangling_bytes = 3;  // untagged leftovers: nothing can start from these
  int64 unused_bytes = 4;    // tagged, but no RUNNING container uses them
}

message ImageInfo {
  string id = 1;
  repeated string tags = 2;  // empty for a dangling image
  int64 size_bytes = 3;
  int64 created_unix = 4;
  // in_use means a container that is RUNNING on this node right now uses it.
  // It is not the same as "needed": a service scaled to zero, or a task between
  // restarts, still needs its image and shows in_use=false.
  bool in_use = 5;
  bool dangling = 6;
}

message PruneImagesRequest {
  // all=false removes only DANGLING images — untagged leftovers of a rebuild,
  // which nothing can be about to start from. Always safe.
  //
  // all=true also removes TAGGED images that no running container uses. On a
  // swarm node that includes the image of any service currently scaled to zero
  // or between restarts: it will have to be pulled again, which is an outage if
  // the registry is unreachable. A client MUST present the two as separate
  // choices and MUST NOT default to this one.
  bool all = 1;
}

message PruneImagesResponse {
  int64 reclaimed_bytes = 1;
  repeated string deleted = 2;  // what the daemon reported removing
}
```

### 3.2 Port-forward lifecycle (normative)

1. Client opens the `PortForward` stream, one per accepted local TCP connection.
2. Client sends exactly one `StartForward` as the **first** message. Anything
   else first → `INVALID_ARGUMENT`. `port` outside 1–65535 → `INVALID_ARGUMENT`.
3. Agent authorizes with action `"portforward"`; on failure returns
   `PERMISSION_DENIED` and closes the stream.
4. Agent establishes a connection to the target port and sends exactly one
   `ready` **before any `data`**. A client MUST NOT treat the forward as usable
   until it has seen `ready`; this is what distinguishes "the target accepted"
   from "connected but silent", so a forward aimed at a closed port fails loudly
   instead of hanging.
5. Steady state: `data` in both directions, opaque bytes, no framing of any kind
   imposed by this protocol.
6. Client half-close (`CloseSend`) is close-write toward the target; the agent
   keeps forwarding the read direction until the target closes.
7. On any fatal error the agent sends one `error` message (or a gRPC status)
   and ends the stream.

The agent reaches the target port by joining its network namespace, because it
generally shares no network with it — see `DESIGN-port-forward.md` for the
constraint and the measurements behind it. That is an agent-side implementation
detail and no part of this wire contract.

The buffer-safety rule (§6) applies to `data` in both directions.

### 3.1 Logs framing (normative)

Like Exec, the stdout/stderr split depends on the container's TTY setting: for a
TTY container the Docker log stream is raw and the agent forwards everything as
`stdout`; for a non-TTY container the stream is `stdcopy`-multiplexed and the
agent demultiplexes it into `stdout`/`stderr`. The same buffer-safety rule (§6)
applies. The agent authorizes a Logs request before streaming, exactly as for
Exec.

### 3.3 Stats semantics (normative)

`Stats` is unary although the underlying data is continuous. The agent samples
its containers in the background and answers from memory; a client polls on
whatever refresh cycle it already has.

- `cpu_percent` is a **delta** between two readings, so it does not exist until
  the agent has taken two. `cpu_ready` says whether the CPU figures in this
  response are meaningful. Memory needs no delta and is valid from the first
  reading, so a response MAY carry usable memory with `cpu_ready=false`. A
  client MUST NOT render a `cpu_percent` of 0 as "idle" when `cpu_ready` is
  false.
- `cpu_percent` uses docker's own convention: the share of ONE cpu, so 250.0
  means two and a half cores.
- `memory_limit_bytes` is the container's own limit only when `memory_limited`
  is true; otherwise it is the node's total memory, which docker substitutes for
  a container with no limit. A client MUST consult `memory_limited` before
  describing a ratio as "of its limit".
- The agent MAY stop sampling while no client is asking, and MAY then report an
  empty `stats` list with `cpu_ready=false` until sampling resumes.
- `health` is the container's healthcheck verdict (`healthy` / `unhealthy` /
  `starting`), and is empty both when the container declares no healthcheck and
  when the agent could not determine one. A client MUST NOT read empty as
  healthy. The manager cannot supply this: a swarm task reads `running` while
  its container fails every probe, which is why it travels with the readings.
- An agent that predates this RPC answers `UNIMPLEMENTED`. Usage is
  supplementary, so a client MUST treat that as "no readings" rather than an
  error, and SHOULD back off rather than re-probing such a node every cycle.

### 3.4 Image accounting (normative)

`ListImages` reports disk figures, and how they are derived is part of the
contract because an operator deletes things by them.

- `total_bytes` is the layer store's real size, counting each layer once. It is
  NOT the sum of the images' `size_bytes`: an image's size includes every layer
  it is built from, and layers are shared, so that sum over-reports badly.
- `dangling_bytes + unused_bytes` is what removing every unused image would
  free: everything not held exclusively by an image a container is running.
- `dangling_bytes` alone is a LOWER bound on what the untagged-only prune
  frees. A layer shared by two untagged images belongs to neither one's unique
  size, and deriving the exact figure would need per-layer ownership the API
  does not expose. `unused_bytes` carries the remainder, so the two always sum
  to the exact total.
- `in_use` means a container is running from the image right now. It does NOT
  mean "needed": a service scaled to zero, or a task between restarts, still
  needs its image and reports `in_use=false`. This is why the two prune modes
  are not interchangeable.

`PruneImages` is destructive and authorized under two distinct actions,
`image.prune` for the untagged-only sweep and `image.prune.all` for the one
that also removes tagged images — so a policy can permit the first without the
second. A client MUST present them as separate choices and MUST NOT default to
`all`.

## 4. Session Lifecycle (normative)

1. Client opens the `Exec` stream.
2. Client sends exactly one `StartExec` as the **first** message. Sending stdin
   or resize before `StartExec` is a protocol violation → agent returns
   `INVALID_ARGUMENT`.
3. Agent authorizes; on failure returns a gRPC status error (`PERMISSION_DENIED`
   or `UNAUTHENTICATED`) and closes the stream.
4. Agent creates the exec, attaches, and applies the initial size if `tty=true`.
5. Steady state, concurrently:
   - Client → agent: `stdin` bytes and `resize` events.
   - Agent → client: `stdout`/`stderr` bytes.
6. Client signals end-of-input by half-closing the stream (`CloseSend`). The
   agent translates this to EOF on the exec's stdin (close-write only; the
   output direction stays open).
7. When the process exits, the agent sends one `exit_code` message, then ends
   the stream cleanly.
8. On any fatal error the agent sends an `error` message (or a gRPC status) and
   ends the stream.

## 5. TTY vs non-TTY framing (normative)

- **tty = true**: the underlying Docker attach is a single raw byte stream.
  The agent forwards everything as `stdout`. `stderr` is never used.
- **tty = false**: the Docker attach stream is multiplexed with an 8-byte frame
  header per chunk (stdcopy format). The agent MUST demultiplex it and route
  bytes to `stdout` / `stderr` accordingly. The client never sees the raw
  multiplexed format.

## 6. Buffer-safety rule (normative)

Any byte slice obtained from a reused read buffer MUST be copied before being
placed in a protobuf message (gRPC serializes asynchronously). Implementations
that send the live read buffer are non-conformant.

## 7. Versioning

- The proto `package` is `swarmexec`. Breaking changes require a new package or
  an explicit version field; do not silently repurpose field numbers.
- Both binaries SHOULD expose `--version` and log the protocol/proto version on
  startup.
