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
```

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
