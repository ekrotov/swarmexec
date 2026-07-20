# swarmexec agent

**Cluster-wide `docker exec -it` for Docker Swarm** — plus logs and swarm-wide
volume management, from a single terminal, without SSHing to the node that
happens to run the task.

📦 **Source & docs:** <https://gitlab.logle.io/internal-tools/swarm-remote-exec>

This image is the **agent**: it runs as a **global** Swarm service (one task per
node), has the local Docker socket mounted, and serves an mTLS gRPC endpoint that
the `swarmexec` CLI connects to. The CLI resolves which node hosts your target
via the Swarm manager API, then talks **directly** to that node's agent.

```
operator terminal ──gRPC/mTLS──> agent(nodeN) ──docker.sock──> container
        │
        └── Docker manager API (TaskList / NodeInspect) to resolve node + container id
```

## Why

Swarm has no built-in cluster-wide `docker exec`: containers, volumes and images
are node-local, so the manager API only exposes services/tasks — not a shell into
the container on a worker. swarmexec closes that gap with a small persistent
agent and a scriptable CLI/TUI, with its own auth and audit log.

## Tags

- `:latest` — the latest **stable** release.
- `:vX.Y.Z` — a specific release (recommended to pin in production).

## Quick start

The easy path provisions the agent on every node from your machine (needs a
Docker context pointing at a Swarm manager, including an `ssh://` context):

```sh
swarmexec init                       # deploy the agent fleet + write client config
swarmexec ps                         # list what you can exec into
swarmexec exec <service> -- sh       # interactive shell into a task
swarmexec logs <service>             # stream logs
swarmexec volume ls                  # swarm-wide volume view
swarmexec ui                         # interactive TUI
```

Pin this image explicitly if you like:

```sh
swarmexec init --image <namespace>/swarmexec-agent:latest
```

### Manual deploy (global service)

```sh
docker service create \
  --name swarmexec_agent \
  --mode global \
  --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
  --publish mode=host,target=9443,published=9443 \
  <namespace>/swarmexec-agent:latest
```

The agent listens on host port **9443** on every node. See the project docs for
the mTLS / shared-secret setup and certificate material.

## Security

- **mTLS** (CA-verified) or **self-signed + shared secret** authentication.
- Every action is recorded in an **audit log**.
- The agent needs the Docker socket, so treat access to it as
  root-equivalent on that node and lock down the port/credentials accordingly.

## Configuration (agent)

| Flag | Env | Default | Purpose |
|------|-----|---------|---------|
| `-listen` | `SWARMEXEC_LISTEN` | `:9443` | listen address |
| `-log-level` | `SWARMEXEC_LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `-log-format` | `SWARMEXEC_LOG_FORMAT` | `json` | `json\|text` |
| `-audit-dest` | `SWARMEXEC_AUDIT_DEST` | `stdout` | `stdout\|stderr\|<file>` |

Operational and audit logs go to the container's stdout/stderr, so they land in
your Docker log driver (`docker service logs swarmexec_agent`).

## Links

- **Source & full documentation:**
  <https://gitlab.logle.io/internal-tools/swarm-remote-exec> — build,
  certificates, configuration reference, troubleshooting, security notes.
- **CLI binaries** (Linux / macOS / Windows) are attached to each
  [release](https://gitlab.logle.io/internal-tools/swarm-remote-exec/-/releases).

## License

Apache-2.0 — © 2026 Cloud Surfers GmbH. The `LICENSE` and `NOTICE` files ship
inside the image at `/licenses/`.

---

*The `swarmexec` CLI and this agent are versioned together — keep them on
matching versions (`swarmexec doctor` flags version skew).*
