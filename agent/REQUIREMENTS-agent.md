# REQUIREMENTS — swarmexec Agent

> Implements the agent component defined in `CONTRACT.md`. Read `CONTRACT.md`
> first; it is the authoritative wire protocol. This document specifies the
> agent's behavior, deployment, and security. Do not redefine the proto here —
> follow `CONTRACT.md`.

## 1. Purpose

A long-running gRPC server that runs as a **global Docker Swarm service** (one
replica per node). It has the local Docker socket mounted and proxies
interactive `exec` sessions into containers that run **on its own node**. It is
the server side of the `Agent` gRPC service.

## 2. Language, libraries, build

- Language: **Go** (1.22+).
- Docker access via the official SDK: `github.com/moby/moby/client`
  (types from `github.com/moby/moby/api`).
  - Exec: `ExecCreate`, `ExecAttach`, `ExecResize`, `ExecInspect`.
  - stdcopy demux: `github.com/moby/moby/api/pkg/stdcopy`.
- gRPC: `google.golang.org/grpc`, generated code from the contract proto.
- Build a **static binary**; ship in a minimal image (`scratch` or `distroless`)
  with the Docker socket bind-mounted, not the Docker CLI.

## 3. Functional requirements

### 3.1 gRPC service
Implement both RPCs from `CONTRACT.md`:

- `ListContainers(ListRequest) -> ListResponse`
  - Lists running containers on the local node only.
  - If `service_filter` is non-empty, filter by service name substring (use the
    `com.docker.swarm.service.name` label) or container name substring.
  - Populate `service` from the `com.docker.swarm.service.name` label when
    present.

- `Exec(stream ClientMessage) -> stream ServerMessage)`
  - First message MUST be `StartExec`; otherwise return `INVALID_ARGUMENT`.
  - Authorize before creating the exec (see §5).
  - Create exec with `AttachStdin/Stdout/Stderr = true`, `Tty` and `Cmd` from
    `StartExec`. Apply optional `env`, `working_dir`, `user` if provided.
  - If `tty=true` and width/height > 0, call `ContainerExecResize` with the
    initial size before/after attach.
  - Bridge both directions concurrently:
    - Client→container: write `stdin` payloads to the hijacked conn; apply
      `resize` events via `ContainerExecResize`.
    - Container→client: forward output. With TTY, forward raw bytes as
      `stdout`. Without TTY, demultiplex via `stdcopy` into `stdout`/`stderr`.
  - On client half-close (`CloseSend`/Recv EOF): close-write the hijacked conn
    (EOF to the process) but keep reading output until it ends.
  - When output ends, call `ContainerExecInspect`, send one `exit_code`
    message, then return (ending the stream).
  - On error: send an `error` message or return a gRPC status; never leak a
    half-open stream.

### 3.2 Buffer safety
Copy every byte slice out of a reused read buffer before putting it in a
protobuf message (see `CONTRACT.md` §6).

### 3.3 TTY/non-TTY framing
Follow `CONTRACT.md` §5 exactly.

## 4. Non-functional requirements

- **Concurrency**: support multiple simultaneous exec sessions per agent; each
  session fully isolated (own goroutines, own exec ID). No shared mutable state
  across sessions except metrics/logging.
- **Resource hygiene**: every session must release the hijacked conn
  (`attach.Close()`), goroutines, and any timers on exit, including error and
  client-disconnect paths. No goroutine leaks on abrupt client disconnect —
  watch `stream.Context().Done()`.
- **Backpressure**: respect gRPC flow control; do not unboundedly buffer output
  if the client is slow.
- **Timeouts**: configurable idle timeout per session (default: disabled, since
  interactive shells idle legitimately). Configurable max session duration
  (default: disabled).
- **Graceful shutdown**: on SIGTERM, stop accepting new sessions, allow existing
  ones a short drain window (configurable, default 5s), then `GracefulStop`.

## 5. Security & authorization

- **mTLS mandatory.** The server requires and verifies client certificates
  against a configured CA. Reject connections without a valid client cert
  (`tls.RequireAndVerifyClientCert`).
- Extract the client identity (client cert CN) from the gRPC peer
  (`peer.FromContext` → TLS `VerifiedChains`).
- **Authorization hook** invoked before each exec, given: client identity,
  target container ID, resolved service name, requested cmd/user.
  - Default policy (v1): allow any client whose cert is signed by the trusted
    CA. Implement it behind an `Authorizer` interface so a future policy
    (per-user/per-service allowlist, deny `root`, command allowlists) can drop
    in without touching the bridge code.
- **Never expose the raw Docker socket** to the network. Only the narrow gRPC
  surface (List + Exec) is reachable. The socket stays local to the agent.
- The gRPC listener SHOULD be reachable only on the Swarm overlay network, not
  published to the host. mTLS is the primary control; network isolation is
  defense-in-depth.

## 6. Audit & observability

- **Audit log** (structured, one line per event): session start (identity,
  container ID, service, cmd, tty, client addr, timestamp), session end
  (exit code, duration, bytes in/out), and authorization decisions
  (allow/deny + reason). Audit log must not contain stdin/stdout payload
  contents.
- Standard operational logs at info/warn/error.
- Optional (nice-to-have): Prometheus metrics — active sessions, total
  sessions, auth denials, bytes transferred.
- Optional (nice-to-have): pluggable session recording behind an interface
  (off by default).

## 7. Configuration

Via flags and/or env vars:

- listen address (default `:9443`)
- TLS: CA cert path, server cert path, server key path
- Docker host (default: `unix:///var/run/docker.sock`)
- drain timeout, idle timeout, max session duration
- log level, audit log destination
- `--version`

## 8. Deployment

- Deploy as a **global** Swarm service (`mode: global`).
- Mount `/var/run/docker.sock` read-write (exec needs write).
- Mount or secret-inject the CA cert, server cert, and server key (use Docker
  **secrets**, not bind mounts, for the key).
- Attach to a dedicated overlay network shared with… nothing else by default;
  the CLI connects in via the node address + port. (If the port is not
  published, document how the CLI reaches it — e.g. published on an internal
  interface, or via the node's routing mesh. Make the reachability model
  explicit in the README.)
- Provide an example `docker-compose.yml` (stack file) and the `docker stack
  deploy` command.

## 9. Deliverables

1. `agent/` Go source implementing the above.
2. Generated proto code (or a `make proto` target / `buf` config) from the
   contract proto.
3. `Dockerfile` (multi-stage, static binary, minimal runtime image).
4. Example stack file (`deploy/agent-stack.yml`) and secret-creation commands.
5. `Authorizer` interface + default allow-all implementation.
6. README: build, deploy, configure, reachability model, how to rotate certs.
7. Unit tests for: first-message-must-be-StartExec, stdcopy demux routing,
   buffer-copy safety, authorization allow/deny, graceful shutdown.

## 10. Explicit non-goals (v1)

- No agent-to-agent mesh / gossip routing (CLI does direct routing).
- No web UI.
- No cert issuance/PKI bootstrap (certs are provided externally).
- No exec into containers on *other* nodes (each agent serves only its node).

## 11. Acceptance criteria

- A global agent deployed on a multi-node swarm accepts an mTLS gRPC `Exec`
  stream and provides a working interactive `/bin/sh` (correct echo, line
  editing, working `clear`, correct `$COLUMNS`/`$LINES`).
- Resizing the operator's terminal during a session reflows the remote TTY.
- Exiting the remote shell returns the real exit code to the client.
- Non-TTY exec (e.g. `cmd=["ls","-la"]`, `tty=false`) returns stdout and stderr
  on the correct channels.
- Connections without a valid client cert are rejected.
- Killing the client mid-session leaves no leaked goroutines or hijacked conns
  on the agent (verify via pprof/goroutine count).
- An audit line is emitted for every session start, end, and auth decision.
