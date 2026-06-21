# REQUIREMENTS — swarmexec CLI Client

> Implements the client component defined in `CONTRACT.md`. Read `CONTRACT.md`
> first; it is the authoritative wire protocol. This document specifies the
> CLI's behavior, terminal handling, and node resolution. Do not redefine the
> proto here — follow `CONTRACT.md`.

## 1. Purpose

A command-line tool the operator runs on their own machine to get an
interactive shell (or run a one-off command) inside any container in a Docker
Swarm — the `docker exec -it` experience, but cluster-wide and from one host.
It is the client side of the `Agent` gRPC service.

It does two jobs:
1. **Resolve** which node a target task runs on, and the container ID, via the
   Swarm **manager API**.
2. **Bridge** the operator's terminal to the agent's `Exec` stream over mTLS
   gRPC (direct connection to that node's agent).

## 2. Language, libraries, build

- Language: **Go** (1.22+).
- Swarm resolution via the official Docker SDK: `github.com/docker/docker/client`
  (`TaskList`, `ServiceList`, `NodeInspectWithRaw`), honoring `DOCKER_HOST` /
  Docker contexts so it can target a manager.
- Terminal: `golang.org/x/term` (`MakeRaw`, `Restore`, `GetSize`,
  `IsTerminal`).
- gRPC client: `google.golang.org/grpc`, generated code from the contract proto.
- Static binary; cross-compiled for the operator platforms you support
  (at least linux/amd64, linux/arm64, darwin/arm64).

## 3. CLI surface

Primary command (shape, not literal):

```
swarmexec exec [flags] <service|service.slot|task-id|container-id> [-- <cmd> [args...]]
```

- Target resolution accepts:
  - a **service name** (default: pick a running task; if multiple, see §4),
  - a **service.slot** (e.g. `web.3`) to pin a replica,
  - a **task ID**,
  - a **container ID** (with an explicit node hint flag if the manager can't
    resolve it).
- If `-- <cmd>` is omitted, default to an interactive `/bin/sh` with a TTY.
- If `-- <cmd>` is given and stdin is not a TTY (e.g. piped), run non-TTY.

Flags (at least):
- `-i/--stdin` (keep stdin open; default true for interactive)
- `-t/--tty` (allocate TTY; default: auto — true iff stdin is a terminal and no
  explicit command, false otherwise)
- `-u/--user`, `-w/--workdir`, `-e/--env KEY=VALUE` (repeatable)
- `--node` (hint/override for container-id targets)
- TLS material: `--ca`, `--cert`, `--key` (or via config/env)
- `--port` (agent port, default 9443)
- `--version`

Secondary command (nice-to-have):
```
swarmexec ps [service]    # list candidate tasks/containers + the node each is on
```

## 4. Node & container resolution (normative behavior)

- Query the manager for running tasks of the target:
  - filter by service + `desired-state=running`.
- For a bare **service name** with multiple running tasks:
  - if exactly one, use it;
  - if multiple, **do not silently pick one** — either require `service.slot`,
    or present the list (slot, node, container ID, uptime) and let the user
    choose (interactive prompt if attached to a TTY, error listing options if
    not).
- From the chosen task, extract:
  - `NodeID` → `NodeInspectWithRaw` → node hostname/address to dial.
  - `Status.ContainerStatus.ContainerID` → the container ID for `StartExec`.
- The address used to dial the agent must be documented and configurable
  (hostname vs. internal IP vs. routing-mesh endpoint) to match the agent's
  reachability model in `REQUIREMENTS-agent.md` §8.
- Fail clearly when: no running task, task has no container yet, node
  unreachable, or manager API unavailable.

## 5. Terminal handling (normative — this is where naive clients fail)

- Detect TTY with `term.IsTerminal(os.Stdin.Fd())`. Only do raw-mode bridging
  when interactive.
- **Raw mode**: `term.MakeRaw` on entry; **always** `term.Restore` on exit,
  including:
  - normal exit,
  - error paths,
  - panics (recover + restore),
  - the `exit_code` branch — note that `os.Exit` does NOT run deferred funcs,
    so restore the terminal explicitly **before** calling `os.Exit`.
- Send the initial terminal size in `StartExec`.
- Handle `SIGWINCH`: on each, read the new size and send a `Resize`. (On
  Windows, where SIGWINCH doesn't exist, poll size periodically or document the
  limitation.)
- Bridge:
  - goroutine: `os.Stdin` → `stdin` messages; on stdin EOF, `CloseSend()`.
  - main loop: `Recv()` → write `stdout`/`stderr` to the matching local stream.
- Copy stdin read buffers before sending (`CONTRACT.md` §6).

## 6. Exit semantics

- On `exit_code` from the agent: restore terminal, then exit the process with
  that exact code.
- On `error` message or gRPC status error: restore terminal, print a clear
  message to stderr, exit non-zero (distinguish "agent/protocol error" from
  "remote command exit code" so scripts can tell them apart — e.g. reserve a
  specific code like 125 for client/transport failures, mirroring docker).
- On local Ctrl-C while interactive: in raw mode the agent receives the bytes
  and the remote process handles it (don't trap SIGINT in the client during an
  interactive TTY session). Document this.

## 7. Security

- **mTLS mandatory.** Present the client cert/key; verify the agent against the
  configured CA (verify server identity, do not skip verification).
- The client cert CN is the operator identity the agent uses for authz/audit;
  document that operators get individual certs (not a shared one) so audit is
  meaningful.
- Load TLS material from explicit paths/config; never hardcode. Support a config
  file and/or env vars in addition to flags.
- Do not log stdin/stdout contents.

## 8. UX requirements

- Helpful, specific errors:
  - "no running task for service X",
  - "service X has 4 running tasks, specify a slot (web.1..web.4) or pass --pick",
  - "cannot reach agent on node N at host:port (mTLS/connection error: …)",
  - "agent denied exec: …".
- `--version` prints binary version + proto/protocol version.
- Sensible defaults so the common case is just
  `swarmexec exec myservice`.
- Quiet on success; no spurious output mixed into the remote stream.

## 9. Deliverables

1. `cli/` Go source implementing the above.
2. Generated proto code shared with the agent (same contract proto).
3. Build/release setup producing static binaries for the target platforms.
4. README: install, configure TLS, examples (interactive shell, one-off
   command, piped stdin, pinning a slot), and the reachability/addressing model.
5. Tests: target-resolution logic (single/multiple/none), TTY-vs-non-TTY mode
   selection, exit-code propagation, terminal restore on all exit paths
   (including simulated panic).

## 10. Explicit non-goals (v1)

- No mesh routing; the client does direct node resolution via the manager API.
- No PKI/cert issuance; certs are provided externally.
- No persistent connection multiplexing or session manager; one invocation =
  one session.
- No Windows-native TTY niceties beyond best-effort (document limitations).

## 11. Acceptance criteria

- `swarmexec exec <service>` on a single-replica service opens a working
  interactive shell: correct echo, line editing, `clear` works, `$COLUMNS`/
  `$LINES` match the local terminal.
- Resizing the local terminal reflows the remote shell live.
- `swarmexec exec <service> -- ls -la /` prints stdout (and any stderr on
  stderr) and exits with the command's real exit code.
- Piping (`echo hi | swarmexec exec <service> -- cat`) works in non-TTY mode.
- A multi-replica service without a slot does not silently pick a replica —
  it prompts or errors with the candidate list.
- After any exit path (clean, error, Ctrl-C, killed connection), the operator's
  local terminal is left in a sane (cooked) state.
- Connecting with a missing/invalid client cert fails with a clear mTLS error,
  not a hang.
