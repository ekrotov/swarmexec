# swarmexec — exec, logs, port-forward and a terminal UI for Docker Swarm

`swarmexec` gives an operator exec, logs, port-forward and a terminal UI for
**any** container in a Docker Swarm, from a single host — without SSH to the
nodes. An interactive shell or a one-off command is the `docker exec -it`
experience, but cluster-wide; `logs -f` follows a service across task
restarts, `port-forward` reaches a container port, and `swarmexec ui` browses
stacks, services, volumes, networks, secrets, nodes and configs. It is the CLI
client of the `Agent` gRPC service defined in [`../CONTRACT.md`](../CONTRACT.md).

Underneath, it does two things:

1. **Resolves** which node a target task runs on (and the container ID) via the
   Swarm **manager API**.
2. **Bridges** your terminal to that node's agent over an **mTLS gRPC** stream
   (direct connection — no agent-to-agent mesh).

```
operator terminal ──gRPC/mTLS──> agent(nodeN) ──docker.sock──> container
        │
        └── Docker manager API (TaskList / NodeInspect) to resolve node + container id
```

## Install

Pre-built static binaries are produced for `linux/amd64`, `linux/arm64`,
`darwin/amd64`, `darwin/arm64` and `windows/amd64`.

```sh
# from a release (bare binaries, no archive)
install -m 0755 swarmexec-linux-amd64 /usr/local/bin/swarmexec

# or build from source (the Go version go.mod pins, see DEVELOPMENT.md)
make build           # -> ./bin/swarmexec
```

Building generated proto code from scratch needs the codegen tools once:

```sh
make tools           # installs buf + protoc-gen-go[-grpc]
make generate        # regenerates internal/pb from proto/swarmexec.proto
make check-generate  # fail if internal/pb does not match the .proto (CI runs this)
```

## Configure TLS (mandatory)

mTLS is **required** — there is no insecure mode. You present a client
certificate and the agent is verified against the configured CA. The client
certificate's **Common Name (CN) is your operator identity**, used by the agent
for authorization and audit, so **each operator gets their own certificate** —
never share one.

Provide TLS material (and other settings) by flag, environment variable, or a
config file. Precedence, lowest to highest: **defaults → config file → env →
flags**.

| Setting   | Flag            | Env                    | Config key    |
|-----------|-----------------|------------------------|---------------|
| CA cert   | `--ca`          | `SWARMEXEC_CA`         | `ca`          |
| Client cert | `--cert`      | `SWARMEXEC_CERT`       | `cert`        |
| Client key  | `--key`       | `SWARMEXEC_KEY`        | `key`         |
| Agent port  | `--port`      | `SWARMEXEC_PORT`       | `port`        |
| Address mode| `--addr-mode` | `SWARMEXEC_ADDR_MODE`  | `addr_mode`   |
| TLS server name | `--server-name` | `SWARMEXEC_SERVER_NAME` | `server_name` |
| Backdrop dim | —             | `SWARMEXEC_UI_DIM`     | `ui.dim`      |
| Underlay CIDRs (per context) | — | —                  | `contexts.<name>.underlay` |
| Ask before a TUI switch (per context) | — | —         | `contexts.<name>.confirm_switch` |
| TUI switch also sets docker's context | — | —         | `ui.sync_docker_context` |

The flags in this table (plus `--agent-secret`, `--agent-secret-file`,
`--insecure` and `--operator`) are the **connection flags**. Every command
accepts them, but a command's `--help` only points at them; `swarmexec --help`
and `swarmexec help connection` print the full list.

Config file (default `~/.config/swarmexec/config.yaml`, override with `--config`
or `SWARMEXEC_CONFIG`):

```yaml
ca:   /etc/swarmexec/ca.pem
cert: /etc/swarmexec/alice.pem
key:  /etc/swarmexec/alice-key.pem
port: 9443
addr_mode: hostname

ui:
  dim: 0.6   # fade behind an open overlay: 0 = off, 1 = flat background
  sync_docker_context: false   # true: a TUI cluster switch also runs `docker context use`

contexts:            # optional, per Docker context name
  prod:
    underlay: [10.0.0.0/16]   # the hosts' own network
    confirm_switch: true      # the TUI asks before switching to prod
```

A cluster switch in the `ui` contexts sidebar is the TUI's own: docker's stored
current context — what `docker` in your other terminals talks to, and where the
next `swarmexec ui` starts — is left alone unless `ui.sync_docker_context: true`.
`swarmexec context use` is an explicit command and always sets it.

### Network subnet overlap

Overlay subnets must not overlap the hosts' own (underlay) network: a subnet
that contains a node's address makes that host unreachable from the network's
containers, and IPAM may hand the host's IP to a container. The `ui` Networks
tab lists the swarm-scope networks (`a` adds the manager node's own local
networks — `bridge`, `host`, compose networks — which exist on that node only,
and the status line says so while they are shown). It shows each network's
`SUBNET` and running `CONTAINERS`, and marks the subnet
with a red `⚠` when node addresses lie inside it, a yellow one when it overlaps
the underlay or another network. `contexts.<name>.underlay` (optional, CIDRs,
keyed by the Docker context name; validated at load) adds the underlay check —
without it only node addresses count. The members view (`Enter`) explains each
finding and the fix (an existing network cannot be renumbered: detach, remove,
recreate with e.g. `--subnet 10.100.1.0/24`, re-attach). Creating a network
runs the same check, and with no subnet given warns when the swarm's default
address pool (`10.0.0.0/8` unless set at `swarm init`) holds the hosts; both
ask "create anyway?" instead of creating directly. The MCP `networks_list`
tool reports the same subnets and findings.

### Network endpoints and stale addresses

Each node's daemon keeps its own copy of an overlay network's endpoints (IP +
MAC); a node that missed a departure keeps the leftover. On entering the
Networks tab (and on `r`) the `ui` asks every agent once, in the background,
for its view (`NetworkEndpoints`, policy action `network.inspect`) and sets it
against the manager's tasks: `CONTAINERS` reads e.g. `12 · 14 addr ⚠2 stale`
(running tasks · task addresses the nodes know, LB excluded · stale
candidates), yellow when something is stale or duplicated, `n/a` (or
`+2 n/a`) when node agents could not answer — the status line explains it. A stale candidate is a task endpoint whose address no live
task has (not an LB endpoint or VIP), or an endpoint without a container. The
members view lists per node running tasks vs local containers, LB, MACs and
networkdb entries, then each stale candidate and duplicate IP/MAC; an agent
predating the RPC shows "agent too old — update the agent image". Heuristic:
a task starting/stopping during the check can show for one refresh, and
leftovers already gone from the networkdb but still in the kernel are not
seen. Fix: `docker network disconnect -f <network> <endpoint>` on a node that
lists it, or restart Docker on the node that held it. MCP: `networks_list`
with `endpoints: true`, computed over the visible services only (a hidden
service's tasks and endpoints count nowhere and never show as stale or
duplicate; a removed service's leftover stays, unnamed), hostnames only,
never node IPs.

### TUI appearance

When an overlay is open (inspect, an editor, a confirm dialog), the `ui` view
fades everything behind it toward the background so the focused window stands
out. `ui.dim` (default **0.6**) controls how strong that fade is, on a `0`–`1`
scale — `0` disables it entirely, higher values push the backdrop further back.
Values outside `0`–`1` are clamped.

With a config file in place, the common case is just `swarmexec exec myservice`.

## Reachability / addressing model

The CLI resolves the target task's node via the manager, then dials that node's
agent **directly** on its gRPC/TLS port (default **9443**). How the node's dial
address is derived is configurable to match the agent's reachability model:

- `--addr-mode hostname` (default) — dial the node's reported **hostname**.
  The hostname must be resolvable and routable from the operator's machine, and
  must match the agent certificate (or use `--server-name`).
- `--addr-mode ip` — dial the node's advertised **IP** (`Status.Addr` as the
  manager sees it). Useful on flat networks; pair with `--server-name` if the
  agent certificate carries a hostname rather than an IP SAN.

For a **container-id** target the manager may not be able to map the ID to a
node; pass `--node <hostname|ip|nodeID>` as a hint. If `--node` names a known
Swarm node it is resolved via the manager; otherwise it is dialed directly.

The CLI honors `DOCKER_HOST` and Docker contexts, so point it at a manager the
usual way (e.g. `DOCKER_HOST=ssh://manager` or `docker context use …`).

## Usage

```
swarmexec exec [flags] <service|service.slot|task-id|container-id> [-- <cmd> [args...]]
swarmexec ps [service]
swarmexec status [--json]                      # nodes, services, replicas, health
swarmexec stats [service...] [--json]          # live CPU/memory per container
swarmexec node ls [--json]
swarmexec node prune <node> [--categories …] [--yes]
swarmexec node update <node> [--availability active|pause|drain] [--label-add k=v] [--label-rm k]
swarmexec node prune-schedule show|set
swarmexec stack ls | export | diff | deploy | check | rm
swarmexec security report | check
swarmexec mcp [--default none|list] [--allow report]
swarmexec --version
```

`swarmexec mcp` is an MCP server on stdin/stdout for AI assistants
(`claude mcp add swarmexec -- swarmexec mcp`); services opt in with the
service label `swarmexec.mcp.enable=true`. See the
[mcp docs](https://swarm-exec.cloud-surfers.net/docs/mcp).

Key `exec` flags: `-i/--stdin` (keep stdin open, default true), `-t/--tty`
(allocate a TTY; default **auto** — true iff stdin is a terminal and no command
was given), `-u/--user`, `-w/--workdir`, `-e/--env KEY=VALUE` (repeatable),
`--node`, `--connect-timeout`.

### Examples

Interactive shell on a single-replica service (defaults to `/bin/sh` + TTY):

```sh
swarmexec exec web
```

One-off command — stdout/stderr are separated and the **real exit code** is
propagated:

```sh
swarmexec exec web -- ls -la /
echo $?        # the remote command's exit code
```

Pipe stdin (runs non-TTY automatically):

```sh
echo hi | swarmexec exec web -- cat
```

Pin a specific replica by slot:

```sh
swarmexec exec web.3 -- cat /etc/hostname
```

A multi-replica service **without** a slot never silently picks one. Attached to
a terminal it prompts; non-interactively it errors with the candidate list:

```sh
$ swarmexec exec web -- hostname
swarmexec: service "web" has 3 running tasks, specify a slot (web.1, web.2, web.3) or pick interactively
```

List candidates and the node each runs on:

```sh
swarmexec ps web
# SERVICE  SLOT  NODE    CONTAINER     DIAL    UPTIME
# web      1     host-a  3f9a1c2b8e10  host-a  2h13m
# web      2     host-b  a1b2c3d4e5f6  host-b  2h13m
```

Run as a different user / workdir / with env:

```sh
swarmexec exec -u 1000:1000 -w /srv -e DEBUG=1 web -- env
```

## Terminal & exit behavior

- The local terminal is put into raw mode only for interactive TTY sessions and
  is **always** restored to a sane (cooked) state on exit — clean exit, error,
  killed connection, or panic.
- Resizing your terminal reflows the remote shell live (via `SIGWINCH`). On
  Windows live resize is best-effort (no `SIGWINCH`); re-launch after resizing.
- During an interactive TTY session **Ctrl-C is not trapped** by the client; the
  bytes go to the remote process, which handles the signal — just like
  `docker exec -it`.
- Exit codes: a remote command's exit code is propagated **exactly**.
  Client/transport/agent failures (unreachable node, bad mTLS, denied exec) exit
  with **125** (mirroring docker), so scripts can distinguish a transport failure
  from a remote non-zero exit. Usage/flag errors exit with **2**; a wrong number
  of arguments also prints the command's usage line.

## Non-goals (v1)

No mesh routing, no PKI/cert issuance (certs are provided externally), no session
multiplexing (one invocation = one session), and only best-effort Windows TTY
handling. See [`REQUIREMENTS-client.md`](REQUIREMENTS-client.md) §10.
