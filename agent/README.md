# swarmexec — Agent

The **agent** component of `swarmexec`. It is a long-running gRPC server that
runs as a **global Docker Swarm service** (one task per node), has the local
Docker socket mounted, and proxies interactive `exec` sessions into containers
running **on its own node**.

It is the server side of the `Agent` gRPC service defined in
[`../CONTRACT.md`](../CONTRACT.md) — the authoritative wire protocol. This README
covers build, deploy, configuration, the reachability model, and cert rotation.

```
operator terminal ──gRPC/mTLS──> agent(nodeN) ──docker.sock──> container
```

## Features

- `ListContainers` — list running containers on the local node, with an
  optional service/container-name substring filter.
- `Exec` — bidirectional interactive exec stream: TTY and non-TTY, terminal
  resize, correct stdout/stderr demuxing (`stdcopy`), real exit codes.
- **Mandatory mTLS**: client certs are required and verified against a CA; the
  client cert CN is the operator identity used for authorization and audit.
- Pluggable **`Authorizer`** (default: allow any CA-signed client).
- Structured **audit log** (session start/end, auth decisions) — never contains
  stdin/stdout payloads.
- Graceful shutdown with a configurable drain window; no goroutine/conn leaks on
  client disconnect.
- Optional **Prometheus** metrics.

## Layout

The agent and the cli share **one Go module** (`module swarmexec`, rooted at the
repository root) and the generated proto package in `internal/pb`. The agent's
own code lives under `agent/`:

```
proto/swarmexec.proto         contract proto (shared codegen source, repo root)
internal/pb/                  generated gRPC code (shared by cli + agent)
agent/internal/server/        Agent service: ListContainers + Exec bridge
agent/internal/auth/          Authorizer interface + AllowAll default policy
agent/internal/audit/         structured audit logger
agent/internal/tlsconf/       mTLS config loader
agent/internal/metrics/       optional Prometheus sink
agent/internal/config/        flag/env configuration
agent/internal/version/       build + protocol version
agent/cmd/agent/              main
agent/Dockerfile              runtime image (build context = repo root)
agent/deploy/                 stack file, secret + dev-cert scripts
```

## Build

Requires Go 1.22+ (the shared module currently pins `go 1.25` in `go.mod`; any
toolchain that recent works). Run all `make` targets **from the repository
root**:

```sh
make agent          # static binary -> ./bin/agent
make build          # the cli      -> ./bin/swarmexec
make build-all      # both
make test           # unit tests for the whole module (cli + agent)
make agent-image    # docker image swarmexec-agent:<version>
```

### Regenerating proto code

Generated code (`internal/pb`) is checked in and shared by both components. To
regenerate after a contract change, from the repo root:

```sh
make tools          # installs buf + protoc-gen-go(-grpc) into GOPATH/bin
make generate       # buf generate -> internal/pb/
```

## Configuration

Flags (each has an env-var equivalent). Run `agent --version` to print the
build and protocol version.

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `-listen` | `SWARMEXEC_LISTEN` | `:9443` | gRPC listen address |
| `-ca-cert` | `SWARMEXEC_CA_CERT` | — (required) | CA cert to verify client certs |
| `-server-cert` | `SWARMEXEC_SERVER_CERT` | — (required) | server certificate |
| `-server-key` | `SWARMEXEC_SERVER_KEY` | — (required) | server private key |
| `-docker-host` | `SWARMEXEC_DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon endpoint |
| `-drain-timeout` | `SWARMEXEC_DRAIN_TIMEOUT` | `5s` | graceful-shutdown drain window |
| `-idle-timeout` | `SWARMEXEC_IDLE_TIMEOUT` | `0` (disabled) | per-session idle timeout |
| `-max-session` | `SWARMEXEC_MAX_SESSION` | `0` (disabled) | per-session max duration |
| `-log-level` | `SWARMEXEC_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `-log-format` | `SWARMEXEC_LOG_FORMAT` | `json` | `json`/`text` |
| `-audit-dest` | `SWARMEXEC_AUDIT_DEST` | `stdout` | `stdout`/`stderr`/file path |
| `-metrics-addr` | `SWARMEXEC_METRICS_ADDR` | — (disabled) | Prometheus listen addr, e.g. `:9100` |

Idle/max-session timeouts default to **disabled** because interactive shells
idle legitimately.

## Deploy

Deployed as a **global** Swarm service. The TLS key is injected via a Docker
**secret** (not a bind mount); the Docker socket is bind-mounted read-write
(exec needs write).

1. **Provide certs.** Production certs come from your own PKI (the agent does no
   PKI bootstrap). For local testing only:

   ```sh
   ./deploy/gen-dev-certs.sh certs "DNS:node1,IP:10.0.0.5"
   ```

2. **Create secrets** on a Swarm manager:

   ```sh
   ./deploy/secrets.sh certs/ca.crt certs/agent.crt certs/agent.key
   ```

3. **Publish the image** to a registry every node can reach. The included
   `.gitlab-ci.yml` pushes it to the GitLab Container Registry at
   `$CI_REGISTRY_IMAGE/agent` on default-branch commits and tags. To build/push
   manually instead (context is the repo root — the whole shared module):

   ```sh
   make agent-image VERSION=v1.0.0
   docker tag swarmexec-agent:v1.0.0 registry.gitlab.example.com/your-group/swarmexec/agent:v1.0.0
   docker push registry.gitlab.example.com/your-group/swarmexec/agent:v1.0.0
   ```

4. **Deploy the stack.** Point `SWARMEXEC_AGENT_IMAGE` at that image, log in to
   the registry, and deploy with `--with-registry-auth` so every node receives
   the credentials a global service needs to pull from a private registry:

   ```sh
   export SWARMEXEC_AGENT_IMAGE=registry.gitlab.example.com/your-group/swarmexec/agent:latest
   docker login registry.gitlab.example.com
   docker stack deploy --with-registry-auth -c deploy/agent-stack.yml swarmexec
   ```

## Reachability model

The agent listens on `:9443` published with **`mode: host`**. Because the
service is `global`, every node runs one agent task and publishes `9443` on that
node's **own** interface — so `CLI → nodeN:9443` always reaches the agent task
**on nodeN**. This is exactly the direct-routing model the CLI uses (resolve the
node via the manager API, then connect straight to that node).

This deliberately does **not** use the Swarm routing mesh (which would load
balance `9443` across all agents and break per-node targeting).

Security posture:

- **mTLS is the primary control.** Without a valid client cert signed by the
  configured CA, the connection is rejected (`RequireAndVerifyClientCert`).
- The raw Docker socket is **never** exposed to the network — only the narrow
  gRPC surface (List + Exec) is reachable.
- As defense-in-depth, restrict `9443` at the host firewall to operator
  networks. If you do not want it on the public interface, bind the published
  port to an internal address or front it with a management network.

## Authorization

`internal/auth` defines the `Authorizer` interface. The v1 default,
`auth.AllowAll`, permits any client whose cert verified against the trusted CA.
To enforce a richer policy (per-user/per-service allowlists, deny `root`,
command allowlists), implement `Authorizer` and pass it to `server.New` — the
exec bridge is untouched. Every decision (allow/deny + reason) is audited.

## Audit & observability

- **Audit log** (`-audit-dest`): one structured JSON line per event —
  `session_start`, `session_end` (exit code, duration, bytes in/out), and
  `auth_decision`. Payload bytes are never logged.
- **Operational logs** go to stderr at the configured level/format.
- **Metrics** (optional, `-metrics-addr`): `/metrics` exposes active/total
  sessions, auth denials, and bytes transferred.

## Rotating certs

Certs are external; rotation is a redeploy:

1. Issue new server cert/key from your CA.
2. Create new **versioned** secrets (Swarm secrets are immutable):

   ```sh
   docker secret create swarmexec_cert_v2 agent.crt
   docker secret create swarmexec_key_v2  agent.key
   ```

3. Update `deploy/agent-stack.yml` to reference the new secret names and
   redeploy (`docker stack deploy …`). `update_config.order: start-first`
   keeps a task serving during the rollout.
4. Remove the old secrets once the rollout completes.

To rotate the **CA**, distribute a bundle CA (old + new) first, roll all
server/client certs onto the new CA, then drop the old CA from the bundle.

## Non-goals (v1)

No agent-to-agent mesh, no web UI, no PKI bootstrap, no cross-node exec. See
[`REQUIREMENTS-agent.md`](REQUIREMENTS-agent.md) §10.
