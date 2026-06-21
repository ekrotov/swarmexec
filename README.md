# swarmexec

Cluster-wide `docker exec -it` for Docker Swarm. Get an interactive shell (or run
a one-off command) inside **any** container in a Swarm, from a single host —
without SSHing to the node that happens to run the task.

```
operator terminal ──gRPC/mTLS──> agent(nodeN) ──docker.sock──> container
        │
        └── Docker manager API (TaskList / NodeInspect) to resolve node + container id
```

Two components, one Go module (`module swarmexec`):

- **agent** (`agent/`) — runs as a **global** Swarm service (one task per node),
  has the local Docker socket mounted, and serves a mTLS gRPC endpoint that
  proxies exec sessions into containers **on its own node**.
- **cli** (`client/`) — runs on the operator's machine. Resolves which node hosts
  the target via the Swarm manager API, then connects **directly** to that node's
  agent over mTLS and bridges your terminal.

The wire protocol is defined in [`CONTRACT.md`](CONTRACT.md); both components
generate from `proto/swarmexec.proto` into the shared `internal/pb`.

---

## 1. Prerequisites

- **Go 1.22+** to build (the module pins `go 1.25` in `go.mod`; any toolchain
  that recent works).
- **Docker Swarm** already initialised, and access to a **manager** node.
- TLS certificates (see step 2.1). The project does **no** PKI bootstrap — certs
  come from your own CA. A dev-cert script is included for testing.

---

## 2. Build

All `make` targets run from the **repository root**.

```sh
make build-all       # both binaries -> ./bin/swarmexec (cli) and ./bin/agent
make build           # cli only      -> ./bin/swarmexec
make agent           # agent only    -> ./bin/agent
make agent-image     # agent container image (swarmexec-agent:<version>)
make test            # unit tests for the whole module
```

Regenerating the gRPC code (only needed after editing the contract):

```sh
make tools           # installs buf + protoc-gen-go(-grpc) into $GOPATH/bin
make generate        # buf generate -> internal/pb/
```

---

## 3. Set up the agent (server side)

### Step 3.1 — Get certificates

mTLS is mandatory. You need:

| File | Purpose | Key requirement |
|------|---------|-----------------|
| `ca.crt` | CA that signs both server and client certs | shared by agent + operators |
| `agent.crt` / `agent.key` | the agent's server certificate | **SAN must match how the cli dials the node** (see note) |
| `operator.crt` / `operator.key` | one per operator | **CN = the operator identity** used for authz + audit |

> **SAN note.** The cli dials a node by its **hostname** (default `--addr-mode
> hostname`) or its **IP** (`--addr-mode ip`). The agent's server certificate
> must carry a matching SAN. For a swarm whose nodes are `node1/node2/node3`,
> issue the server cert with `DNS:node1,DNS:node2,DNS:node3` (and/or the node
> IPs). One shared server cert with all node SANs is simplest.

For **testing only**, generate a throwaway CA + server + operator certs:

```sh
# args: <output-dir> <server-SAN-list>
./agent/deploy/gen-dev-certs.sh certs "DNS:node1,DNS:node2,DNS:node3,IP:10.0.0.1"
# -> certs/{ca.crt, agent.crt, agent.key, operator.crt, operator.key}
```

### Step 3.2 — Make the agent image available to every node

A **global** service runs on all nodes, so every node must be able to pull the
image.

**Single-node swarm:** just build it locally:

```sh
make agent-image VERSION=v1.0.0           # -> swarmexec-agent:v1.0.0
```

**Multi-node swarm:** push to a registry every node can reach. With your own
registry:

```sh
make agent-image VERSION=v1.0.0
docker tag swarmexec-agent:v1.0.0 registry.example.com/swarmexec-agent:v1.0.0
docker push registry.example.com/swarmexec-agent:v1.0.0
```

Then set that image name in the stack file (step 3.4). If you have no registry,
a quick swarm-local one works:

```sh
docker service create --name registry --publish published=5000,target=5000 registry:2
docker tag swarmexec-agent:v1.0.0 127.0.0.1:5000/swarmexec-agent:v1.0.0
docker push 127.0.0.1:5000/swarmexec-agent:v1.0.0    # 127.0.0.0/8 is insecure-allowed
# use image: 127.0.0.1:5000/swarmexec-agent:v1.0.0 in the stack file
```

### Step 3.3 — Create the Docker secrets

The CA/cert/key are injected as Docker **secrets** (the key is never a bind
mount). Run on a manager:

```sh
./agent/deploy/secrets.sh certs/ca.crt certs/agent.crt certs/agent.key
# creates secrets: swarmexec_ca, swarmexec_cert, swarmexec_key
```

(`docker secret create` reads the files locally and sends their contents to the
manager, so this works even over an `ssh://` Docker context.)

### Step 3.4 — Deploy the stack

The stack file is [`agent/deploy/agent-stack.yml`](agent/deploy/agent-stack.yml).
Edit the `image:` line to match what you built/pushed in step 3.2, then deploy
from a manager:

```sh
docker stack deploy -c agent/deploy/agent-stack.yml swarmexec
```

This creates a `global` service `swarmexec_agent` (one task per node), mounts
`/var/run/docker.sock` read-write, attaches the secrets, and publishes `9443`
with **`mode: host`** so `node:9443` always reaches the agent **on that node**
(the direct-routing model the cli relies on — not the routing mesh).

### Step 3.5 — Verify

```sh
docker service ls                         # swarmexec_agent should be N/N
docker service ps swarmexec_agent         # one Running task per node
docker service logs swarmexec_agent       # JSON startup + audit lines
```

---

## 4. Connect with the client (operator side)

### Step 4.1 — Get the cli and your operator cert

Put `./bin/swarmexec` on your `PATH`, and have your `ca.crt`,
`operator.crt`, `operator.key` available locally.

### Step 4.2 — Point the cli at a Swarm manager

The cli queries the **manager API** to resolve a target to a node. It uses the
standard Docker SDK, which honours `DOCKER_HOST` (and `DOCKER_TLS_VERIFY` /
`DOCKER_CERT_PATH`). Pick one:

```sh
# a) run the cli ON a manager node (uses the local socket automatically)
# b) point at a manager's TLS-protected API
export DOCKER_HOST=tcp://manager.example.com:2376
export DOCKER_TLS_VERIFY=1
export DOCKER_CERT_PATH=~/.docker/manager-certs
```

> The cli reads `DOCKER_HOST`, **not** the `docker` CLI's named contexts, and the
> bare SDK does not wire up `ssh://` transports. If you normally use an
> `ssh://`-based context, run the cli on the manager itself or expose the
> manager API over TLS.

### Step 4.3 — Provide the mTLS material

Via flags, env vars, or a config file (precedence: defaults → file → env →
flags). Config file lives at `~/.config/swarmexec/config.yaml` (override with
`$SWARMEXEC_CONFIG`):

```yaml
# ~/.config/swarmexec/config.yaml
ca:   /home/me/certs/ca.crt
cert: /home/me/certs/operator.crt
key:  /home/me/certs/operator.key
port: 9443           # default
addr_mode: hostname  # or "ip"
# server_name: node1 # optional: override TLS SNI (e.g. when dialing by IP)
```

Env equivalents: `SWARMEXEC_CA`, `SWARMEXEC_CERT`, `SWARMEXEC_KEY`,
`SWARMEXEC_PORT`, `SWARMEXEC_ADDR_MODE`, `SWARMEXEC_SERVER_NAME`.

### Step 4.4 — List what you can exec into

```sh
swarmexec ps                 # all running tasks across the swarm
swarmexec ps <service>       # filter to one service
```

### Step 4.5 — Exec

```sh
# interactive shell into the (single) task of a service
swarmexec exec web

# a specific replica
swarmexec exec web.3

# by task id or container id (container id needs a node hint)
swarmexec exec <task-id>
swarmexec exec <container-id> --node node2

# one-off command (non-interactive: stdout/stderr/exit code are preserved)
swarmexec exec web -- ls -la /app
swarmexec exec api -- sh -c 'echo $HOSTNAME'
```

If TLS material is on the config file/env, the full command is just
`swarmexec exec web`. With explicit flags:

```sh
swarmexec exec \
  --ca ca.crt --cert operator.crt --key operator.key \
  --addr-mode hostname \
  web -- echo hello
```

A multi-replica service with no slot specified prompts you to pick one (or, when
non-interactive, lists the candidates and exits).

---

## 5. Configuration reference

### Agent flags (env var in parentheses)

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `-listen` | `SWARMEXEC_LISTEN` | `:9443` | gRPC listen address |
| `-ca-cert` | `SWARMEXEC_CA_CERT` | *(required)* | CA to verify client certs |
| `-server-cert` | `SWARMEXEC_SERVER_CERT` | *(required)* | server certificate |
| `-server-key` | `SWARMEXEC_SERVER_KEY` | *(required)* | server private key |
| `-docker-host` | `SWARMEXEC_DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon endpoint |
| `-drain-timeout` | `SWARMEXEC_DRAIN_TIMEOUT` | `5s` | graceful-shutdown drain window |
| `-idle-timeout` | `SWARMEXEC_IDLE_TIMEOUT` | `0` (off) | per-session idle timeout |
| `-max-session` | `SWARMEXEC_MAX_SESSION` | `0` (off) | per-session max duration |
| `-log-level` | `SWARMEXEC_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `-log-format` | `SWARMEXEC_LOG_FORMAT` | `json` | `json`/`text` |
| `-audit-dest` | `SWARMEXEC_AUDIT_DEST` | `stdout` | `stdout`/`stderr`/file path |
| `-metrics-addr` | `SWARMEXEC_METRICS_ADDR` | *(off)* | Prometheus addr, e.g. `:9100` |
| `--version` | — | — | print version and exit |

### Client: global flags + `exec` flags

Global: `--ca`, `--cert`, `--key`, `--port` (9443), `--addr-mode hostname|ip`,
`--server-name`, `--config`.

`exec`: `-i/--stdin` (default on), `-t/--tty` (auto: on iff stdin is a terminal
and no command), `-u/--user`, `-w/--workdir`, `-e/--env KEY=VALUE` (repeatable),
`--node` (hint for container-id targets), `--connect-timeout` (10s).

---

## 6. Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| `tls: unknown certificate authority` | Client cert not signed by the agent's CA, **or** `--ca` doesn't match the agent's server CA. Use certs from the same CA. |
| `x509: certificate is valid for X, not Y` | Server cert SAN doesn't match the dialed host. Add the node hostname/IP to the server cert SAN, or set `--server-name` / `--addr-mode`. |
| `cannot reach agent on node:9443` | Port not reachable from the operator (firewall), or the agent task isn't running on that node. Check `docker service ps swarmexec_agent`. |
| `connect to Docker manager API` fails | `DOCKER_HOST` not set / not a manager / `ssh://` unsupported by the cli. See step 4.2. |
| agent task stuck `Pending`/`Rejected` (image) | Node can't pull the image. Push to a registry all nodes can reach (step 3.2). |
| `first message must be StartExec` / `PERMISSION_DENIED` | Protocol/authorization errors surfaced by the agent — check `docker service logs swarmexec_agent`. |

---

## 7. Security & production notes

- **mTLS is the only authentication.** Anyone with a CA-signed client cert can
  exec; protect operator keys and scope the CA. The default authorizer
  (`agent/internal/auth`, `AllowAll`) permits any CA-signed client — implement
  the `Authorizer` interface for per-user/per-service allowlists, `root` denial,
  or command allowlists without touching the exec bridge.
- **The Docker socket is never exposed to the network** — only the narrow gRPC
  surface (List + Exec) is reachable. The agent mounts the socket read-write
  (exec needs write) and runs as root, which is inherent to socket access.
- **Defense in depth:** restrict `9443` at the host firewall to operator
  networks even though mTLS is the primary control.
- **Audit:** every session start/end and authorization decision is logged as one
  structured JSON line (no stdin/stdout payloads). Ship `audit-dest` somewhere
  durable.
- **Certs are external.** Rotate by issuing new certs and redeploying with
  versioned secrets (`update_config: start-first` keeps a task serving). See the
  rotation section in [`agent/README.md`](agent/README.md).

---

## 8. Tear down

```sh
docker stack rm swarmexec
docker secret rm swarmexec_ca swarmexec_cert swarmexec_key
# if you created a throwaway registry:
docker service rm registry
```

---

## More detail

- [`CONTRACT.md`](CONTRACT.md) — the authoritative wire protocol.
- [`agent/README.md`](agent/README.md) — agent internals, deployment, cert rotation.
- [`client/README.md`](client/README.md) — cli internals and behavior.
