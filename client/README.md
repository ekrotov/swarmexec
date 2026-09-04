# swarmexec — cluster-wide `docker exec -it` for Docker Swarm

`swarmexec` gives an operator an interactive shell (or runs a one-off command)
inside **any** container in a Docker Swarm, from a single host — the
`docker exec -it` experience, but cluster-wide. It is the CLI client of the
`Agent` gRPC service defined in [`../CONTRACT.md`](../CONTRACT.md).

It does two things:

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

Pre-built static binaries are produced for `linux/amd64`, `linux/arm64`, and
`darwin/arm64`.

```sh
# from a release archive
tar xzf swarmexec_<version>_linux_amd64.tar.gz
install -m 0755 swarmexec /usr/local/bin/

# or build from source (Go 1.22+)
make build           # -> ./bin/swarmexec
```

Building generated proto code from scratch needs the codegen tools once:

```sh
make tools           # installs buf + protoc-gen-go[-grpc]
make generate        # regenerates internal/pb from proto/swarmexec.proto
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
```

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
swarmexec --version
```

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
  from a remote non-zero exit. Usage/flag errors exit with **2**.

## Non-goals (v1)

No mesh routing, no PKI/cert issuance (certs are provided externally), no session
multiplexing (one invocation = one session), and only best-effort Windows TTY
handling. See [`REQUIREMENTS-client.md`](REQUIREMENTS-client.md) §10.
