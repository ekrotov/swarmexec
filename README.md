# swarmexec

**`docker exec`, logs and port-forward for any container in a Docker Swarm — from one terminal, without SSH to the node that runs it.**

🌐 **Website and full documentation: [swarm-exec.cloud-surfers.net](https://swarm-exec.cloud-surfers.net)** — [docs](https://swarm-exec.cloud-surfers.net/docs) in English, Deutsch, Español, Français and Polski · [release notes](https://swarm-exec.cloud-surfers.net/releases)

![swarmexec vs docker exec: instead of docker node ls → find the node → ssh node-3 → docker exec, just run `swarmexec exec web -- sh`](site/demo.gif)

Docker Swarm schedules tasks from the manager, but a container's shell, its logs,
a port nobody published and the volume on its disk live on one node — and the
manager API reaches none of them. swarmexec (also written *swarm-exec*) puts a
small agent on every node and asks it directly.

## What it does

- **Exec anywhere** — `swarmexec exec web -- sh` opens a shell in the task, on whatever node it runs. [Targets →](https://swarm-exec.cloud-surfers.net/docs#targets)
- **Logs of a service or a whole stack** — every replica in one stream, following containers across replacement. [logs →](https://swarm-exec.cloud-surfers.net/docs#commands)
- **Port-forward** to ports nobody published, without touching the stack. [port-forward →](https://swarm-exec.cloud-surfers.net/docs#commands)
- **A terminal UI** for services, volumes, networks, secrets, configs and nodes — across several clusters in one session, with live CPU/RAM and health. [The TUI →](https://swarm-exec.cloud-surfers.net/docs#tui)
- **Stack diff and a pre-deploy check for CI** — a content diff of a file against the deployed stack, and a security gate before `stack deploy`. [stack diff →](https://swarm-exec.cloud-surfers.net/docs#stackfile)
- **A security report** of the whole cluster, as Markdown you can file and compare. [security report →](https://swarm-exec.cloud-surfers.net/docs#securityreport)
- **Its own identity and audit** — mTLS or a connection-bound shared secret, an audit log, an optional policy file. [Authentication →](https://swarm-exec.cloud-surfers.net/docs#auth)

Apache-2.0, self-hosted, no telemetry, no node limit. Docker Engine 19.03 or newer.

## Install

```sh
curl -fsSL https://swarm-exec.cloud-surfers.net/install.sh | sh   # Linux, macOS — verifies the checksum
brew install ekrotov/swarmexec/swarmexec                         # macOS, Linux
scoop bucket add swarmexec https://github.com/ekrotov/scoop-swarmexec
scoop install swarmexec                                          # Windows
```

Or take a binary from the [latest release](https://github.com/ekrotov/swarmexec/releases/latest). [Install →](https://swarm-exec.cloud-surfers.net/docs#install)

## Quick start

```sh
swarmexec --context prod init      # deploy the agent to every node, write the client config
swarmexec ps                       # what you can exec into, and on which node
swarmexec exec web -- sh           # a shell in a task of the service "web"
swarmexec ui                       # the terminal UI
```

[Quick start →](https://swarm-exec.cloud-surfers.net/docs#quickstart)

## How it works

```
your terminal ──gRPC (mTLS or bound secret)──> agent on node N ──docker.sock──> container
      │
      └── Docker manager API: which node runs the task, and its container id
```

The **agent** runs as a global Swarm service, one task per node, with the node's
Docker socket mounted. The **client** asks the manager where the target runs and
connects straight to that node's agent — there is no mesh between agents.
[How it works →](https://swarm-exec.cloud-surfers.net/docs#model)

## Documentation

The full documentation lives at **[swarm-exec.cloud-surfers.net/docs](https://swarm-exec.cloud-surfers.net/docs)**:
[configuration](https://swarm-exec.cloud-surfers.net/docs#config) ·
[Docker contexts and SSH](https://swarm-exec.cloud-surfers.net/docs#context) ·
[all commands](https://swarm-exec.cloud-surfers.net/docs#commands) ·
[the TUI](https://swarm-exec.cloud-surfers.net/docs#tui) ·
[monitoring](https://swarm-exec.cloud-surfers.net/docs#monitoring) ·
[policy file](https://swarm-exec.cloud-surfers.net/docs#policy) ·
[exit codes](https://swarm-exec.cloud-surfers.net/docs#exit)

In this repository:

- [`docs/manual-setup.md`](docs/manual-setup.md) — deploying the agent by hand with your own CA, the agent and client flags, troubleshooting, production notes.
- [`docs/related-work.md`](docs/related-work.md) — how swarmexec compares with d4s, swarmcli, Portainer, Swarmpit, Komodo and the older swarm-exec scripts.
- [`DEVELOPMENT.md`](DEVELOPMENT.md) — building, tests, the documentation translations, the release pipeline.
- [`CONTRACT.md`](CONTRACT.md) and [`proto/swarmexec.proto`](proto/swarmexec.proto) — the wire protocol; [`agent/README.md`](agent/README.md), [`client/README.md`](client/README.md) — the two components' internals.

The agent image is public on Docker Hub as [`logleio/swarmexec-agent`](https://hub.docker.com/r/logleio/swarmexec-agent).
Source: [gitlab.logle.io/cs-public/swarm-remote-exec](https://gitlab.logle.io/cs-public/swarm-remote-exec), mirrored to [github.com/ekrotov/swarmexec](https://github.com/ekrotov/swarmexec).

## License

swarmexec is open source under the [Apache License 2.0](LICENSE).

Copyright 2026 Cloud Surfers GmbH.

Contributions are welcome under the same license and require a DCO sign-off
(`git commit -s`) — see [`CONTRIBUTING.md`](CONTRIBUTING.md).

If you redistribute swarmexec — as source, as a binary, or inside a derivative
work — the license requires you to keep the [`LICENSE`](LICENSE) and
[`NOTICE`](NOTICE) files with it, so the attribution to this project travels
along. Modified files must be marked as changed.
