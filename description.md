# swarmexec — Component Description

`swarmexec` is a cluster-wide `docker exec -it` (plus logs and volume management)
for Docker Swarm. From a single workstation an operator can open an interactive
shell, stream logs, or manage volumes in **any** container/node of the swarm,
without manually SSHing to the node that hosts the task.

It is one Go module (`module swarmexec`) producing two binaries — an **agent**
(server, one per node) and a **cli** (client, on the operator's machine) — that
speak a single gRPC wire protocol defined in [`CONTRACT.md`](CONTRACT.md).

```
operator terminal ──gRPC/mTLS──> agent(nodeN) ──docker.sock──> container / volumes
        │
        └── Docker manager API (TaskList / NodeList / NodeInspect) to resolve the
            node + container id (honors the operator's Docker CLI context)
```

The routing model is **direct**: the cli asks the Swarm manager API which node a
target runs on, then connects straight to that node's agent. There is no
agent-to-agent mesh.

---

## The agent (server)

A long-running gRPC server deployed as a **global** Swarm service (one task per
node). It bind-mounts the local Docker socket and exposes a narrow gRPC surface
that proxies operations into containers/volumes **on its own node**.

**Build / runtime.** Go 1.22+, official Docker SDK (`github.com/docker/docker`),
`google.golang.org/grpc`. Ships as a static binary in a minimal distroless image
(no shell, no Docker CLI). The Docker socket is bind-mounted read-write; the
agent never exposes the raw socket to the network.

**gRPC service (`Agent`):**

| RPC | Purpose |
|-----|---------|
| `ListContainers` | running containers on this node (optional name/service filter) |
| `Exec` (bidi stream) | interactive exec: first message is `StartExec`, then stdin/resize ⇄ stdout/stderr/exit_code |
| `Logs` (server stream) | stream a container's logs (follow/tail/timestamps/since) |
| `ListVolumes` | volumes on this node (swarm volumes are node-local) |
| `RemoveVolume` | delete a volume on this node (in-use → FailedPrecondition) |

**Exec / Logs framing.** For a TTY container the Docker stream is raw and is
forwarded as `stdout`; for a non-TTY container the stream is `stdcopy`-multiplexed
and the agent demultiplexes it into `stdout`/`stderr`. Byte slices read from a
reused buffer are always copied before entering a protobuf message
(buffer-safety rule, CONTRACT §6).

**Security & authentication.** Two modes:

- **mTLS (default):** the server requires and verifies a client certificate
  against a configured CA (`RequireAndVerifyClientCert`); the client cert's
  Common Name is the operator identity used for authorization and audit.
- **Self-signed + shared secret (Portainer-style):** the agent generates its own
  server certificate at startup with SANs taken from the Docker node info, and
  authenticates clients with a shared secret (`SWARMEXEC_AGENT_SECRET`) enforced
  by a constant-time gRPC interceptor. This removes per-node cert provisioning.

**Authorization.** Every exec/logs/volume operation passes through an
`Authorizer` interface (action + identity + target). The v1 default allows any
authenticated client; a richer policy (per-user/per-service allowlists, deny
`root`, command allowlists) can be dropped in without touching the RPC handlers.

**Audit & observability.** One structured JSON line per event — session/logs
start+end, volume removals, and every authorization decision — never containing
stdin/stdout payloads. Optional Prometheus metrics (active/total sessions, auth
denials, bytes). Graceful shutdown drains in-flight sessions on SIGTERM.

**Deployment.** A `global` Swarm service publishing `:9443` with `mode: host`, so
`node:9443` always reaches the agent **on that node** (the direct-routing model).
The CA/cert/key (or the shared secret) are injected as Docker **secrets**; the
image is pulled from a registry every node can reach.

---

## The cli (client)

Runs on the operator's machine. It resolves a target to a node via the Swarm
manager API, then connects directly to that node's agent.

**Manager-API access.** The cli uses the operator's **Docker CLI context** the
same way `docker` does — including `ssh://` endpoints — so if `docker ps` works
against the swarm, so does swarmexec. Resolution order: `--context` →
`$DOCKER_CONTEXT` → `$DOCKER_HOST` → the config's current context → default
socket. `ssh://` endpoints are tunneled via the Docker connection helper.

**Target resolution.** A target may be a service name, `service.slot`, task id,
or container id. The cli resolves it to `{dial host, container id}` via
`ServiceList`/`TaskList`/`NodeInspect`. The dial host is the node's hostname
(default) or advertised IP (`--addr-mode ip`); an unusable `0.0.0.0` advertise
address falls back to the hostname.

**Agent connection.** mTLS with a CA-verified server and a per-operator client
cert, **or** a shared secret with `insecure: true` (self-signed agents).
Configured via flags, `$SWARMEXEC_*` env vars, or
`~/.config/swarmexec/config.yaml`.

**Provisioning (`init`).** `swarmexec init` deploys the agent itself, entirely
through the manager API: it creates a shared-secret Docker secret, creates the
agent as a global Swarm service in self-signed mode (host port 9443 on every
node), forwards local registry credentials so nodes can pull a private image,
and writes the matching client config. Agent-needing commands that can't reach
any agent report **“no swarmexec agent found … run `swarmexec init`.”**

**Commands:**

| Command | Description |
|---------|-------------|
| `init` | provision the agent on every node via the manager API (self-signed + shared secret), pass registry creds, and write the client config |
| `down` | remove the agent service (and its shared secret) provisioned by `init` |
| `doctor` | diagnose the swarm: manager reachable, agent deployed (+ pinned image), and each node's agent reachability + version (flags too-old agents) |
| `config show` | print the effective client config (context, port, auth mode, secret masked) |
| `completion` | generate a shell completion script (bash/zsh/fish/powershell) |
| `ps [service]` | table of running tasks across the swarm (service, slot, container, node, ip, uptime) |
| `exec <target> [-- cmd]` | interactive exec (auto-TTY for a bare shell; `-t` to force); raw-terminal bridge |
| `logs <target> [-f] [--tail] [-t] [--since]` | stream a container's logs |
| `volume ls [filter]` | volumes aggregated across all nodes, with which nodes hold each |
| `volume rm <name> --all \| --node …` | remove a volume on all/selected nodes (parallel, confirmed) |
| `ui [service]` | interactive TUI (see below) |
| `--info` / `--version` / `--help` | build/license/contact info; version; help |

**Swarm-wide volumes.** Swarm volumes are node-local, so the cli queries every
node's agent (with bounded concurrency) and aggregates `volume → [nodes that
hold it]`, reporting unreachable nodes rather than failing. Removal can target
all nodes or a chosen subset.

**Scripting.** `ps`, `volume ls`, and `doctor` accept `--json` for machine-
readable output, and `config show` prints the resolved configuration.

**Interactive TUI (`ui`).** A two-tab terminal UI (tview):

- **Containers** tab — navigable table; Enter opens a modal menu **logs / bash /
  sh**. Logs render in a scrollable live viewer where **`f`** toggles follow
  (auto-scroll) on/off so you can pause to read; bash/sh run in an **embedded
  terminal modal** (a vt10x emulator bridged to the Exec stream, with resize and
  Ctrl-] to detach) so the TUI is never left.
- **Volumes** tab — aggregated volume table; Enter lists the nodes holding a
  volume with multi-select, deleting on selected or all nodes with confirmation.

Navigation: arrows / `j` `k` `h` `l`, `Tab` or `1`/`2` to switch tabs, `r` to
refresh, `q` to quit.

---

## Shared wire protocol

[`CONTRACT.md`](CONTRACT.md) is the single source of truth: a proto3 definition
(`package swarmexec`, `go_package = swarmexec/internal/pb`) that both binaries
generate from into the shared `internal/pb`. Transport is gRPC over TLS, default
port **9443**. Normative rules cover the exec/logs stdout/stderr framing, the
exec session lifecycle, and buffer-safety.

---

## Build & release

A single Go module: generated proto in `internal/pb` (shared), agent under
`agent/`, cli under `client/`. `.gitlab-ci.yml` runs tests, cross-compiles the
cli for linux/macOS/windows as artifacts, and builds the agent image with Kaniko
into the GitLab Container Registry. A **semantic-version tag on `main`** cuts a
release (image `:<version>` + `:latest`, cli binaries, and a GitLab Release),
gated so the tag must sit on `main`.

## Non-goals (v1)

No agent-to-agent mesh, no web UI, no PKI bootstrap (certs are provided
externally, or the agent self-signs), and no exec into containers on other nodes
(each agent serves only its own node).
