# swarmexec

![swarmexec vs docker exec: instead of docker node ls → find the node → ssh node-3 → docker exec, just run `swarmexec exec web -- sh`](site/demo.gif)

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
make check-generate  # fail if internal/pb does not match the .proto (CI runs this)
```

### Documentation translations

`site/docs.html` (English) is the source of the docs page; `docs.de/es/fr/pl.html`
are derived from it, section by section. `site/i18n/` records which English
each translated section came from, and a test fails — naming the sections — as
soon as the English changes without the translations following.

```sh
make docs-i18n-check   # which sections are stale (no network)
make docs-i18n         # re-translate exactly those, via the Claude API (ANTHROPIC_API_KEY)
make docs-i18n-accept  # or translate by hand, then record them as up to date
```

`docs-i18n` sends each stale section with the English it was translated from
and the current translation, so only what changed is rewritten; every answer is
checked (same tables, command blocks, flags and keys; the head's language,
canonical and hreflang tags untouched) before it is written. CI never calls
the API — it only checks.

### Releases (CI/CD)

[`.gitlab-ci.yml`](.gitlab-ci.yml) builds and versions automatically:

- **Branches / merge requests** — run tests and produce dev cli binaries
  (stamped with the short commit SHA) as job artifacts.
- **Default branch (`main`)** — additionally push a dev agent image
  (`$CI_REGISTRY_IMAGE/agent:main` and `:<short-sha>`).
- **Semantic-version tag on `main`** — cut a release. A release is built
  **exactly when** the tag matches `vMAJOR.MINOR.PATCH` (e.g. `v1.2.3`,
  optionally `-rc.1` / `+build`) **and** the tagged commit is on `main`
  (enforced by the `verify-tag-on-main` gate). It produces:
  - cli binaries for linux/macOS/windows stamped with the version,
  - the agent image at `$CI_REGISTRY_IMAGE/agent:<version>` and `:latest`,
  - a GitLab **Release** with the binaries attached.

  A semver tag that is **not** on `main` fails the gate and builds nothing.

Cut a release:

```sh
git checkout main && git pull
git tag -a v1.2.3 -m "swarmexec v1.2.3"
git push origin v1.2.3
```

---

## 3. Set up the agent (server side)

### Quick start — `swarmexec init` (recommended)

If your Docker CLI already targets the swarm (e.g. `docker context use prod`,
including an `ssh://` context), one command provisions everything via the
manager API — no certificate handling, no stack file:

```sh
swarmexec init                 # deploys the agent on every node + writes your client config
swarmexec init --image <reg>/agent:v1.0.7   # pin a specific image
swarmexec init --port 8443     # use a custom port everywhere (default 9443)
swarmexec init --force         # update an already-deployed agent
swarmexec init --force --allow-legacy-secret=false   # …and stop accepting the raw secret
```

`init` first shows **which Docker context** it will deploy into. If you have
several contexts it lets you pick one (the active one is the default); with a
single context it just tells you. `--context <name>` (or `$DOCKER_CONTEXT`)
selects non-interactively. It then creates a shared-secret Docker secret,
deploys the agent as a **global** service in self-signed mode (host port 9443 on
every node), passes your local registry credentials so nodes can pull a private
image, waits for the agents to come up (showing `agents: X/Y running`), and
writes `~/.config/swarmexec/config.yaml` (secret + `insecure: true` +
`addr_mode: ip`). After it finishes, `swarmexec ps` / `ui` / `exec` / `volume`
just work.

> If a command later reports **“no swarmexec agent found in this swarm — run
> `swarmexec init`”**, the agent isn't deployed (or isn't reachable) — run
> `init` to provision it. To diagnose the whole swarm (manager reachable, agent
> deployed, each node's agent reachability + version), run **`swarmexec doctor`**
> — it flags nodes whose agent is unreachable or too old (run
> `swarmexec init --force` to update those).

The manual paths below give you full control (mTLS, custom stack) if you prefer.

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
image from a registry they can all reach.

**GitLab Container Registry (recommended).** The included
[`.gitlab-ci.yml`](.gitlab-ci.yml) builds and pushes the agent image on every
default-branch commit and tag to:

```
$CI_REGISTRY_IMAGE/agent:<version>      # e.g. registry.gitlab.example.com/your-group/swarmexec/agent:latest
```

Nothing to build by hand — just note that image path for step 3.4.

**Build/push manually** (any other registry):

```sh
make agent-image VERSION=v1.0.0
docker tag swarmexec-agent:v1.0.0 registry.example.com/swarmexec/agent:v1.0.0
docker push registry.example.com/swarmexec/agent:v1.0.0
```

**Single-node swarm** can skip the registry and just build locally
(`make agent-image VERSION=v1.0.0` → `swarmexec-agent:v1.0.0`).

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
Its `image:` defaults to `${SWARMEXEC_AGENT_IMAGE:-...}`, so point that variable
at the image from step 3.2 (or edit the file). From a manager:

```sh
# 1. tell the stack which image to use
export SWARMEXEC_AGENT_IMAGE=registry.gitlab.example.com/your-group/swarmexec/agent:latest

# 2. log in to the registry so the manager can authenticate
docker login registry.gitlab.example.com
#    (in CI/headless, use a deploy token:
#     docker login -u <token-name> -p <token> registry.gitlab.example.com)

# 3. deploy — --with-registry-auth forwards the login to every node so the
#    GLOBAL service can pull from the private registry on all of them
docker stack deploy --with-registry-auth -c agent/deploy/agent-stack.yml swarmexec
```

> **`--with-registry-auth` is required** for a private registry: without it the
> manager pulls fine but the other nodes have no credentials and their agent
> tasks fail with a pull/authentication error.

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

### Alternative: self-signed + shared secret (less cert management)

Provisioning per-node server certs (and getting the SANs right) is fiddly. The
agent can instead **generate its own server certificate at startup** — SANs are
taken from the Docker node info, so it is automatically valid for the node's
hostname and address — and authenticate clients with a **shared secret**
(Portainer-style). There is then only **one** secret to manage and no server
cert/key or per-node SANs at all.

```sh
# 1. one shared secret for the whole fleet
openssl rand -base64 32 | docker secret create swarmexec_agent_secret -

# 2. deploy the self-signed stack (no server cert/key secrets needed)
#    the default agent image is public on Docker Hub, so no registry login:
docker stack deploy -c agent/deploy/agent-stack-selfsigned.yml swarmexec
```

Client side — put the same secret in `~/.config/swarmexec/config.yaml`:

```yaml
agent_secret: <the value created above>
insecure: true        # self-signed agent — no CA to verify against
addr_mode: ip
operator: eugen       # reported in the agent audit log (default: OS username)
```

Trade-off vs. full mTLS: the shared secret is one fleet-wide credential, and
`insecure: true` skips server-cert verification. You lose per-operator
certificate identity — though the client still reports an `operator` name for
audit. To keep per-operator identity, additionally provide a client CA
(`swarmexec_ca` secret + `-ca-cert=/run/secrets/swarmexec_ca`): the agent then
self-signs its server cert **and** verifies operator client certs.

**What `insecure: true` does and does not cost you.** It does *not* put the
secret at risk: the client never sends it. It sends a proof computed from the
secret and the certificate of the connection it is travelling on, so a server
that answered the connection — including one that presented a certificate you
did not verify — receives nothing that works against a real agent. That closes
the attack that mattered: capture one credential, replay it to every node, get
Docker-socket access on each.

**Upgrading from before v1.17.3.** A v1.17.3 client sends only the bound proof,
so it cannot authenticate to an older agent, which does not know that form.
Update the agents first:

```sh
swarmexec init --force
```

If you need the client working against not-yet-updated agents in the meantime,
add `legacy_secret: true` to the client config — it then also sends the raw
secret, with the exposure described above. Remove it once the agents are
current, then close the door behind you:

```sh
swarmexec init --force --allow-legacy-secret=false
```

After that the agents refuse the raw secret outright, so no client can put the
credential on the wire by accident.

What remains is that the agent is **not authenticated to you**. Someone who can
place themselves on the path can terminate the connection and see what that
session sends them — a command line, whatever you type into a shell. Binding
defeats credential theft, not eavesdropping on a session you chose not to
verify. So: self-signed + secret is fine on a network you trust; on one you do
not, provision a CA and drop `insecure`.

The two modes use different agent flags:

| | mTLS (default) | self-signed + secret |
|---|---|---|
| server cert | provisioned (`-server-cert`/`-server-key`) | generated at startup (`-self-signed`) |
| client auth | client cert verified vs CA (`-ca-cert`) | shared secret (`-agent-secret-file`) |
| operator identity | client cert CN | `operator` header (or client cert CN if CA also set) |
| client config | `ca` + `cert` + `key` | `agent_secret` + `insecure` |

---

## 4. Connect with the client (operator side)

### Step 4.1 — Get the cli and your operator cert

Install the client — the script picks the build for your machine, verifies it
against the release's `SHA256SUMS`, refuses an unverified binary, and never uses
sudo ([read it first](site/install.sh)):

```sh
curl -fsSL https://swarm-exec.cloud-surfers.net/install.sh | sh
curl -fsSL https://swarm-exec.cloud-surfers.net/install.sh | SWARMEXEC_VERSION=v1.19.2 sh   # a specific release
```

Or with a package manager, so `brew upgrade` / `scoop update` keep the client in
step with the agents:

```sh
brew install ekrotov/swarmexec/swarmexec                                   # macOS, Linux
scoop bucket add swarmexec https://github.com/ekrotov/scoop-swarmexec && scoop install swarmexec   # Windows
```

Or put a build from `./bin/swarmexec` on your `PATH`. For mTLS, have your
`ca.crt`, `operator.crt`, `operator.key` available locally.

Shell completion (completes service names, slots, stacks and contexts — it asks
only the manager, gives up after two seconds, and never prompts over ssh):

```sh
swarmexec completion bash > ~/.local/share/bash-completion/completions/swarmexec
swarmexec completion zsh  > "${fpath[1]}/_swarmexec"
```

### Step 4.2 — Point the cli at a Swarm manager

The cli queries the **manager API** to resolve a target to a node. It uses your
**Docker CLI context** the same way `docker` does — including `ssh://` endpoints
— so if `docker ps` already works against your swarm, so does swarmexec.
Resolution order (highest first):

```sh
swarmexec --context pk ps          # explicit context (a)
export DOCKER_CONTEXT=pk           # or via env                       (b)
export DOCKER_HOST=tcp://mgr:2376  # or a raw host (TLS via DOCKER_TLS_VERIFY/_CERT_PATH)  (c)
swarmexec ps                       # else the active context from ~/.docker/config.json (d)
```

So with an `ssh://root@manager` context (e.g. `docker context use pk`),
`swarmexec ps` simply works from your workstation — no need to run on a manager
or expose the API over TLS.

Over an `ssh://` context the manager API and the node agents each get their own
ssh connection, and there is one per exec, log stream, port-forward and refresh.
They **share one transport** via OpenSSH's `ControlMaster`, so only the first
pays for the handshake and the authentication — measured on a three-node cluster
behind a jump host, `swarmexec doctor` went from 8 authentications and 3.6 s to
2 and 1.0 s. The sockets live in `$XDG_RUNTIME_DIR/swarmexec/ssh/` (mode 0700)
and the shared connection exits 60 s after the last command. Set
`SWARMEXEC_SSH_MULTIPLEX=0` to turn it off; it is unavailable on Windows, whose
OpenSSH does not implement connection sharing.

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
# ui:
#   dim: 0.6         # TUI: fade behind an open overlay (0 = off, 1 = flat bg)
```

Env equivalents: `SWARMEXEC_CA`, `SWARMEXEC_CERT`, `SWARMEXEC_KEY`,
`SWARMEXEC_PORT`, `SWARMEXEC_ADDR_MODE`, `SWARMEXEC_SERVER_NAME`,
`SWARMEXEC_UI_DIM`.

### Step 4.4 — List what you can exec into

```sh
swarmexec ps                 # all running tasks across the swarm
swarmexec ps <service>       # filter to one service
```

Wait until a deploy has actually converged — for CI (exit 0, or 1 with the reason):

```sh
swarmexec stack deploy shop.yml --yes && swarmexec wait --stack shop --timeout 3m
swarmexec wait api worker --healthy   # also require healthy containers
```

Stream logs — of one container, several, every replica of a service, or a
whole stack:

```sh
swarmexec logs web                 # all logs (every replica, prefixed, if web has several)
swarmexec logs web -f --tail 100   # follow, starting from the last 100 lines
swarmexec logs web -t --since 10m  # with timestamps, last 10 minutes
swarmexec logs -f api worker db    # several services at once, one prefixed stream
swarmexec logs -f --stack shop     # every service of a stack
```

### Volumes (swarm-wide)

Swarm volumes are node-local, so deleting one means visiting every node.
swarmexec aggregates them: it queries each node's agent and shows which nodes
hold each volume, and can delete on all nodes at once or on selected ones.

```sh
swarmexec volume ls                    # VOLUME  DRIVER  NODES (which nodes hold it)
swarmexec volume rm data --all         # remove on every node that has it
swarmexec volume rm data --node docker1 --node docker2   # only these nodes
```

### Interactive ui

Browse interactively — a **two-tab** view (Containers / Volumes), switch with
**Tab** or **1/2**:

```sh
swarmexec ui                 # ↑/↓ j/k h/l move, Tab switch, Enter, r refresh, q quit
swarmexec ui <service>       # filter containers to one service
```

- **Containers** tab: Enter on a row opens **logs / bash / sh** in a modal pane
  (the shells run in an embedded terminal; **Ctrl-]** detaches).
- **Volumes** tab: Enter on a volume lists the nodes that hold it; **space** to
  select nodes, **d** to delete the selected (or highlighted) ones, **a** to
  delete on all — with a confirmation step.

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
| `-port` | `SWARMEXEC_PORT` | `9443` | gRPC listen port |
| `-listen` | `SWARMEXEC_LISTEN` | *(from `-port`)* | full listen address; overrides `-port` when set, e.g. `:9443` |
| `-ca-cert` | `SWARMEXEC_CA_CERT` | *(required)* | CA to verify client certs |
| `-server-cert` | `SWARMEXEC_SERVER_CERT` | *(required)* | server certificate |
| `-server-key` | `SWARMEXEC_SERVER_KEY` | *(required)* | server private key |
| `-docker-host` | `SWARMEXEC_DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon endpoint |
| `-drain-timeout` | `SWARMEXEC_DRAIN_TIMEOUT` | `5s` | graceful-shutdown drain window |
| `-idle-timeout` | `SWARMEXEC_IDLE_TIMEOUT` | `30m` (`0` = off) | per-session idle timeout |
| `-max-session` | `SWARMEXEC_MAX_SESSION` | `12h` (`0` = off) | per-session max duration |
| `-max-streams` | `SWARMEXEC_MAX_STREAMS` | `256` | concurrent exec/logs/port-forward streams; negative = unlimited |
| `-max-forward-sidecars` | `SWARMEXEC_MAX_FORWARD_SIDECARS` | `64` | live port-forward sidecar containers; negative = unlimited |
| `-log-level` | `SWARMEXEC_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `-log-format` | `SWARMEXEC_LOG_FORMAT` | `json` | `json`/`text` |
| `-audit-dest` | `SWARMEXEC_AUDIT_DEST` | `stdout` | `stdout`/`stderr`/file path |
| `-metrics-addr` | `SWARMEXEC_METRICS_ADDR` | *(off)* | Prometheus addr, e.g. `:9100` |
| `-self-signed` | `SWARMEXEC_SELF_SIGNED` | `false` | generate a self-signed server cert at startup (no `-server-cert`/`-server-key` needed) |
| `-cert-sans` | `SWARMEXEC_CERT_SANS` | — | extra SANs for the self-signed cert, e.g. `DNS:swarmexec-agent,IP:10.0.0.5` |
| `-agent-secret` | `SWARMEXEC_AGENT_SECRET` | — | shared secret clients must present (value) |
| `-agent-secret-file` | `SWARMEXEC_AGENT_SECRET_FILE` | — | read the shared secret from a file (e.g. a Docker secret) |
| `--version` | — | — | print version and exit |

### Client: global flags + `exec` flags

Global: `--ca`, `--cert`, `--key`, `--port` (9443), `--addr-mode hostname|ip`,
`--server-name`, `--config`, `--context` (Docker context for the manager API,
also `$DOCKER_CONTEXT`), `--agent-secret`(`-file`), `--insecure`, `--operator`.

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
| `connect to Docker manager API` fails | No reachable manager: pick a context with `--context`/`$DOCKER_CONTEXT`, or check `docker ps` works for that context (ssh keys, host). See step 4.2. |
| agent task stuck `Pending`/`Rejected` (image) | Node can't pull the image. Push to a registry all nodes can reach (step 3.2) and deploy with `--with-registry-auth` (step 3.4). |
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

If you provisioned with `swarmexec init`, the inverse removes the global agent
service and its shared secret (with a confirmation):

```sh
swarmexec down                 # remove the agent service + secret
swarmexec down --keep-secret   # keep the secret (e.g. to re-init with it)
swarmexec down -y              # no confirmation prompt
```

For a manually-deployed stack:

```sh
docker stack rm swarmexec
docker secret rm swarmexec_ca swarmexec_cert swarmexec_key
# if you created a throwaway registry:
docker service rm registry
```

---

## 9. Related work

Swarm has no built-in cluster-wide `docker exec`: containers, volumes and images
are node-local, so the manager API only exposes services/tasks — not a shell into
the container on a worker. Several projects work around it, in three different
ways: a throwaway helper per command, an SSH tunnel, or — like swarmexec — a
**persistent, authenticated agent** on every node.

**Terminal UIs for Swarm** — this section used to describe every CLI alternative
as ephemeral. Since 2025/2026 that is no longer true: two of them are neither
ephemeral nor going away, both Go, both k9s-inspired.

- [jr-k/d4s](https://github.com/jr-k/d4s) — the closest in spirit: same stack
  (Go + tview), Swarm objects down to secrets and configs, shell, log streaming
  and port-forward. Deliberately **agentless** — it reaches remote daemons over
  an SSH tunnel, so there is nothing to deploy, and equally no operator identity
  of its own: access is whatever the SSH login and the socket grant, and
  node-local objects cannot be aggregated across the cluster without tunnelling
  to each node in turn. Apache-2.0.
- [Eldara-Tech/swarmcli](https://github.com/Eldara-Tech/swarmcli) — architecturally
  the nearest: its **Business Edition** deploys an mTLS-fronted RBAC proxy plus a
  per-node agent stack, and from that offers shell into a service task,
  port-forward, cross-node volumes and per-user RBAC by client certificate. Open
  core: the Community Edition is Apache-2.0 without those features, and the free
  tier covers them up to three nodes. swarmexec gives the same capabilities
  wholly under Apache-2.0, at any size.

**CLI tools** — all spin up an *ephemeral* helper (a throwaway service/container
with the Docker socket mounted) for each command, then tear it down. All four are
unmaintained; the most recent stopped in 2023:

- [opsani/skopos-plugin-swarm-exec](https://github.com/opsani/skopos-plugin-swarm-exec)
  — creates a temporary service constrained to the target node, execs, cleans up.
- [mavenugo/swarm-exec](https://github.com/mavenugo/swarm-exec)
  — a global service that bind-mounts the Docker CLI/socket to run a command on every node.
- [neuroforgede/docker-swarm-proxy](https://github.com/neuroforgede/docker-swarm-proxy)
  — a Python Docker CLI plugin that proxies commands to any node (via `DOCKER_HOST`/SSH).
  Its README asks the same question this project does: *"What if you wanted a
  docker exec, but for Docker swarm?"*
- [pantafive/swarmServiceExec](https://github.com/pantafive/swarmServiceExec)
  — an async wrapper around the "find the task's node, then exec" flow.

These need nothing deployed, but each exec pays a container-spawn cost, they are
typically exec-only (no aggregated logs/volume views), and access is whatever the
node's Docker socket grants — no per-operator auth or audit.

**GUI platforms** — [Portainer](https://docs.portainer.io/admin/environments/add/swarm/agent)
(with the Portainer Agent), [Swarmpit](https://swarmpit.io) and
[Komodo](https://komo.do/docs/swarm) solve the same node-local API limitation with
a per-node agent and can exec/inspect cluster-wide — but as web platforms, not
headless CLI/TUIs. Portainer's GitOps comparison is a commit-hash check rather
than a content diff, and Swarmpit has had an open request for a container console
since 2018.

**Where swarmexec sits:** exec itself is no longer the distinguishing trait — d4s
and swarmcli's Business Edition both have it. What has not turned up in any of
them is the **security scan of service specs**, a **content diff of a deployed
stack against a file**, and **service-wide logs that reconnect across container
replacement**. Add the persistent per-node agent with its own mTLS /
shared-secret identity and audit log, ssh-tunnel support for nodes behind a
bastion — and all of it under Apache-2.0 with no node limit.

*Checked against the projects' own repositories and READMEs on 2026-09-26.*

---

## More detail

- [`CONTRACT.md`](CONTRACT.md) — the wire protocol's semantics; the messages and RPCs themselves are in [`proto/swarmexec.proto`](proto/swarmexec.proto).
- [`agent/README.md`](agent/README.md) — agent internals, deployment, cert rotation.
- [`client/README.md`](client/README.md) — cli internals and behavior.

---

## License

swarmexec is open source under the [Apache License 2.0](LICENSE).

Copyright 2026 Cloud Surfers GmbH.

Contributions are welcome under the same license and require a DCO sign-off
(`git commit -s`) — see [`CONTRIBUTING.md`](CONTRIBUTING.md).

If you redistribute swarmexec — as source, as a binary, or inside a derivative
work — the license requires you to keep the [`LICENSE`](LICENSE) and
[`NOTICE`](NOTICE) files with it, so the attribution to this project travels
along. Modified files must be marked as changed.
