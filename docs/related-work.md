# How swarmexec compares

<!-- Moved out of the top-level README, unchanged; the README now keeps the essentials and points to https://swarm-exec.cloud-surfers.net for the rest. -->


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
