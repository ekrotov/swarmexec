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

message ListVolumesRequest {}

message ListVolumesResponse {
  repeated VolumeInfo volumes = 1;
}

message VolumeInfo {
  string name = 1;
  string driver = 2;
  string mountpoint = 3;
  string created_at = 4;  // RFC3339, if known
  string scope = 5;       // "local" or "global"
  int64 size_bytes = 6;   // on-disk size; only meaningful when size_known is true
  bool size_known = 7;    // true when the agent actually computed the size
  map<string, string> labels = 8;  // volume metadata labels, as set at creation
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
