# Shared Contract — swarmexec

This file defines the wire protocol between the CLI client and the Swarm agent
together with [`proto/swarmexec.proto`](proto/swarmexec.proto), and each owns
one half: the `.proto` defines the messages, fields and RPCs; this file defines
what they mean — transport, authentication, framing, lifecycles, limits — and
that half is normative. Neither repeats the other. Both components are built
against both files; if either changes, both components must be updated.

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
| `x-swarmexec-operator` | Optional operator identity for audit when no client certificate is presented. Unauthenticated: it is believed as sent, and the audit log marks it `identity_verified=false`. Per-operator attribution needs client certificates. |

Clients send the binding, never the raw secret. The binding is per-connection:
the agent recomputes it over its own certificate, so a proof collected from any
other connection — including one an on-path attacker terminated — does not
authenticate. The secret itself never travels.

This authenticates the *client* to the agent. It does not authenticate the agent
to the client: on a connection the client chose not to verify (`insecure`), the
peer still sees what that session sends it.

## 3. Protocol Definition

The messages, fields and RPCs are defined in
[`proto/swarmexec.proto`](proto/swarmexec.proto). That file is their single
source: `internal/pb` is generated from it (`make generate`), and CI fails when
the checked-in generated code does not match it. This document used to carry a
copy of the `.proto`; two copies of a contract are two contracts, and the copy's
comments had already drifted, so it was removed.

What a `.proto` cannot say lives here instead — the semantics below, which are
normative.

### 3.1 Logs framing (normative)

Like Exec, the stdout/stderr split depends on the container's TTY setting: for a
TTY container the Docker log stream is raw and the agent forwards everything as
`stdout`; for a non-TTY container the stream is `stdcopy`-multiplexed and the
agent demultiplexes it into `stdout`/`stderr`. The same buffer-safety rule (§6)
applies. The agent authorizes a Logs request before streaming, exactly as for
Exec.

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

### 3.5 Container events (normative)

`WatchContainerEvents` exists because the manager cannot answer the question.
A task reads `running` while its container fails every health probe, is
OOM-killed, or exits and is restarted; only the daemon on the node knows, and
it knows immediately.

- **One container per stream.** The request names a container id and the agent
  subscribes with that filter at the DAEMON. An agent that received the node's
  whole event stream and discarded most of it would be doing the narrowing in
  the one place where a mistake leaks another container's lifecycle — including
  containers that are not part of any swarm service.
- **A typed subset crosses the wire, not the event.** Docker attaches the
  container's full label set to every event, and labels are operator-supplied
  strings. `ContainerEvent` therefore carries only `action`, `time_unix_nano`,
  `exit_code`, `health` and `error`; `Actor.Attributes` is never forwarded.
- `exit_code` is meaningful only when `action` is `die`, and is 0 otherwise.
  `health` is set only for `health_status:` actions and carries the verdict
  alone (`healthy` / `unhealthy` / `starting`).
- The RPC is authorized as `container.events` and audited at start and end. The
  audit record counts events; it never contains them.
- It counts against the agent's stream cap like Exec, Logs and PortForward. It
  is held open for as long as a view is open, so exempting it would be a quiet
  way around the limit.
- A client must treat this as an ENRICHMENT. An agent that predates the RPC
  answers `Unimplemented`, and the view it decorates has to keep working.

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
- The protocol version is the constant `pb.ProtocolVersion` (currently
  `swarmexec/v1`), defined next to the generated code. Both binaries compile it
  from that one package, so they cannot report different values, and it is not
  a build flag — only the binary's release version is injected at build time.
  `doctor` compares the agents' value to the client's by exact equality.
- Both binaries SHOULD expose `--version` and log the protocol/proto version on
  startup.

## 8. Deployment (normative)

§2 and §3 govern the bytes between a client and a running agent. This section
governs how the agent comes to exist, because that is a contract too: the client
writes the agent's command line, its secret mount, its socket mount and its
label, and the agent has to be the binary that accepts all four.

Every constant below lives in `internal/deploy`, which both halves import. Go's
`internal` rule makes a client↔agent import unconstructible, so a shared package
is the only place this vocabulary can be stated once — and stating it twice is
exactly how it used to drift: renaming an agent flag passed build, test and
review, then broke `swarmexec init` against every cluster.

| Fact | Value | Constant |
| --- | --- | --- |
| Service name | `swarmexec_agent` | `deploy.ServiceName` |
| Docker secret | `swarmexec_agent_secret` | `deploy.SecretName` |
| Secret path in the container | `/run/secrets/swarmexec_agent_secret` | `deploy.SecretPath` |
| Docker socket mount | `/var/run/docker.sock` | `deploy.SocketPath` |
| Docker endpoint flag value | `unix:///var/run/docker.sock` | `deploy.SocketURL` |
| Object label | `swarmexec.role` = `agent` / `port-forward` | `deploy.RoleLabel`, `deploy.RoleAgent`, `deploy.RoleForward` |
| Default port | 9443 | `deploy.DefaultPort` |
| Policy file in the container | `/run/configs/swarmexec_policy.yaml` (a Docker config `swarmexec_agent_policy_<hash>`) | `deploy.PolicyPath` |

- The deployed command line is `deploy.AgentArgs`, and the agent registers those
  flags from the same `deploy.Flag*` constants. The flags in that set —
  `-port`, `-self-signed`, `-agent-secret-file`, `-docker-host`,
  `-drain-timeout`, `-log-format`, `-audit-dest`, `-allow-legacy-secret`,
  `-metrics-addr`, `-policy-file` — are
  part of this contract and MUST NOT be renamed or repurposed on one side alone.
  Every other agent flag is the agent's own interface and carries no such
  promise.
- `-allow-legacy-secret` is written out only when turning the path OFF, so an
  agent deployed by an older client and one deployed by a current client carry
  identical command lines unless the operator asked for the change.
- `-metrics-addr` is written only when `swarmexec init --metrics-port` asks for
  it, as `:<port>`, and that port is then published in host mode next to the
  agent port. Without it the command line is unchanged.
- A round-trip test in `agent/internal/config` parses what `deploy.AgentArgs`
  produces and asserts the resulting `Config`. A rename is therefore a compile
  error on one side or a red test on the other — never a broken deployment.
- `agent/deploy/agent-stack-selfsigned.yml` provisions the same agent, not the
  same spec: it binds with `-listen` and attaches an overlay network where
  `swarmexec init` publishes `-port` in host mode. Both are valid deployments;
  only the vocabulary above is normative.
