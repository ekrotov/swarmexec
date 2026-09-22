# Design — Port Forwarding

**Status:** proposal, not implemented.
**Scope:** a new `PortForward` RPC, a `swarmexec port-forward` command, and TUI
integration modelled on `k9s`. Touches [`CONTRACT.md`](CONTRACT.md) §3 — the
wire protocol is versioned with both binaries, so agent and cli ship together.

## 1. Goal

From the operator's workstation, bind a local TCP port and forward it to a port
on a container anywhere in the swarm, without publishing that port on the
cluster and without SSHing to the node. The equivalent of `kubectl port-forward`,
reachable from the TUI with a single keystroke.

## 2. The constraint that shapes everything

The obvious design — the agent calls `net.Dial(containerIP, port)` and pipes the
bytes over gRPC — **does not work for the normal swarm case**. The agent cannot
route to a target container's overlay address.

The agent is deployed either with no networks at all
(`agentServiceSpec`, [`client/internal/cli/init.go:398`](client/internal/cli/init.go)) or attached
only to its own `swarmexec` overlay, declared `attachable: false`
([`agent/deploy/agent-stack-selfsigned.yml:47`](agent/deploy/agent-stack-selfsigned.yml)).
Target containers live on their own stacks' overlays. Overlay network namespaces
are isolated from each other and from the node's host namespace, so neither the
agent nor the host can reach those addresses.

This was verified on a live 3-node swarm, not inferred. See
[Appendix A](#appendix-a--verification) for the transcript. In short:

| Observation | Result |
|---|---|
| Agent container's networks | `bridge=172.17.0.2` |
| A target container's networks | `gateway=10.0.1.8`, `nextcloud-internal=10.0.3.7` |
| TCP connect from the agent's network position to `10.0.3.7:3002` | **fails** |
| TCP connect from inside the target's netns to `127.0.0.1:3002` | **succeeds** |
| Full HTTP request/response through a netns-joined container's stdio | **succeeds** |

## 3. Options considered

**A — agent attaches itself to the target's network on demand**
(`NetworkConnect` on its own container ID). Rejected: stack overlays are
`attachable: false` by default, which blocks this for the common case. It also
mutates the agent's own network configuration while it is serving other
sessions, which is racy under concurrent forwards.

**B — sidecar container in the target's network namespace.** Start a container
with `NetworkMode: "container:<target-id>"`; it shares the target's netns, so
the target is reachable as `127.0.0.1`. Bytes travel between agent and sidecar
over the sidecar's stdin/stdout via `docker attach`. Works regardless of
network attachability, requires no cluster network configuration, and needs no
tooling inside the target container.

**C — direct dial when agent and target already share a network.** A cheap fast
path, detectable by comparing `NetworkSettings.Networks` from `ContainerInspect`
against the agent's own. Covers plain-bridge containers and the case where an
operator deliberately attached the agent to an application network.

**Decision: B as the general mechanism, C as an optimisation.**

B fits this codebase unusually well. `docker attach` returns a
`types.HijackedResponse` — exactly the type
[`agent/internal/server/exec.go:81`](agent/internal/server/exec.go) (`runSession`)
already bridges onto a gRPC stream — and `msgWriter` (exec.go:318) is already an
`io.Writer` over a server stream. The sidecar image is the **agent's own image**
with a `-forward` mode: it is guaranteed present on every node, because the
agent runs from it, so a forward never waits on a registry pull.

### Cost of B

One container start per forwarded TCP connection: **~270–330 ms**, measured on
the node via the local socket. (An earlier estimate of ~200 ms was wrong in the
other direction too — measuring through an `ssh://` docker context showed ~1.1 s,
of which ~0.25 s is context overhead. The agent uses the local socket, so
~300 ms is the number that matters.)

That is tolerable for a debugging tool and invisible for a long-lived connection,
but a browser opening six parallel connections pays it six times, and a client
with a connection pool pays it per pooled connection.

**Started with one sidecar per connection.** It reused the existing streaming
machinery almost entirely and was the smallest correct thing.

**Now one sidecar per forward** (`agent/internal/server/forwardmux.go`,
`internal/forwardmux`). The container start is paid once per (container, port)
rather than once per TCP connection, and the sidecar lingers for a couple of
minutes after its last connection so a browser reload or a reconnecting pool
does not pay it again. As predicted, the gRPC protocol between cli and agent is
untouched — still one stream per TCP connection; only what the agent hangs that
stream on changed.

Three consequences worth knowing:

- **The sidecar's stdout carries frames, not payload.** There are now two
  framing layers on this path: docker's stdcopy separating stdout from stderr,
  and ours separating the connections inside stdout. Both sides call the same
  codec so they cannot drift.
- **Half-close needs its own frame.** Closing the stdio would end every
  connection on the sidecar, not one.
- **Connections on one forward share a writer.** A target that stops reading
  applies backpressure to the others on the same sidecar — head-of-line
  blocking that the per-connection design did not have. Accepted deliberately:
  per-connection flow control is a much larger protocol, and a forward is
  normally a handful of connections to one service. The writer is a goroutine
  with a bounded queue rather than a mutex, so a wedged sidecar costs its own
  connections and never the agent.

## 4. Wire protocol

A new bidirectional RPC mirroring the `StartExec` contract. One gRPC stream per
accepted TCP connection; HTTP/2 already multiplexes streams over the single
`*grpc.ClientConn`, so no connection-id field is needed in the messages.

```proto
rpc PortForward(stream ForwardClientMessage) returns (stream ForwardServerMessage);

message StartForward {
  string container_id = 1;  // full container ID
  uint32 port = 2;          // TCP port inside the container
}

message ForwardClientMessage {
  oneof payload {
    StartForward start = 1;  // MUST be the first message
    bytes data = 2;          // raw bytes toward the container
  }
}

message ForwardReady {}

message ForwardServerMessage {
  oneof payload {
    ForwardReady ready = 1;  // sent once, after the target port accepted
    bytes data = 2;          // raw bytes from the container
    string error = 3;        // terminal error; stream ends after this
  }
}
```

`ForwardReady` is load-bearing: it separates "the target port accepted a
connection" from "connected, but the peer has not spoken yet". Without it the
UI would report a forward as healthy when it actually points at a closed port,
and the operator would only find out when their client times out.

Lifecycle follows [`CONTRACT.md`](CONTRACT.md) §4: `StartForward` first or
`INVALID_ARGUMENT`; authorize then connect; half-close propagates as a
close-write on the target connection while the read direction stays open. The
buffer-safety rule (§6) applies unchanged — bytes from a read buffer must be
copied before going into a protobuf message.

An agent that predates this RPC returns `Unimplemented`, which the existing
"agent too old — run `init --force`" path already turns into a useful message.

## 5. Agent implementation

Shape it after `runSession`:

1. Authorize (see §7) and audit.
2. `ContainerInspect` the target. If it shares a network with the agent, dial
   directly (option C) and skip to step 5.
3. Otherwise start a sidecar from the agent's own image, `NetworkMode:
   "container:<target-id>"`, `AutoRemove: true`, no mounts, no privileges, in
   `-forward 127.0.0.1:<port>` mode.
4. Attach to it; the resulting `HijackedResponse` is the byte channel.
5. Send `ForwardReady`, then bridge in both directions with `msgWriter` and the
   `pumpInput` pattern (exec.go:167/233).
6. Reuse the cancel-closes-the-conn unblock trick (exec.go:153), the
   `idleMonitor` (exec.go:256), and the drain/`wg` accounting (server.go:495).

The sidecar must be torn down on every exit path, including agent crash —
`AutoRemove` plus a label (`swarmexec.role=forward`) so a restarted agent can
sweep orphans. `DockerClient` (docker.go:568) gains `ContainerCreate`,
`ContainerStart`, `ContainerAttach`, and `ContainerRemove`.

## 6. Client implementation

### `swarmexec port-forward <target> [local:]remote`

A `newPortForwardCmd(g)` following the existing cobra pattern
([`client/internal/cli/root.go:90`](client/internal/cli/root.go)). Resolve the
target with `resolve.Resolve`, dial with `dial.Dial`, `net.Listen`, and open one
`PortForward` stream per accepted connection. Build this first: it makes the
feature testable and scriptable without the TUI, and the TUI then calls the same
code.

**Bind to `127.0.0.1` by default**, never `0.0.0.0`. A forward pulls an internal
cluster port onto the workstation; binding it to every interface re-exposes it
to the local network. Offer `--address` for operators who genuinely want that.

The ssh-bastion case works for free: `cfg.ProxyDialer`
([`client/internal/cli/sshproxy.go:27`](client/internal/cli/sshproxy.go)) and the
keepalive parameters at [`client/internal/dial/dial.go:57`](client/internal/dial/dial.go)
apply to the new connection unchanged.

For a **service** target, forward to exactly one task, as `kubectl port-forward`
targets one pod. Display which task was chosen. Do not round-robin across tasks
— a forward that silently load-balances produces confusing debugging sessions.

### TUI

The TUI currently has **no persistent background work**: every stream is bound
1:1 to a visible page and dies when the overlay closes
([`client/internal/cli/ui.go`](client/internal/cli/ui.go)). A port-forward is the
first resource that must outlive its overlay, so this introduces the first
forwards registry.

- A registry as a `runUI` local alongside `vols`/`lastCands`: target, local
  port, remote port, state, last error, `cancel func()`.
- Teardown must use the `sync.Once` pattern from `openTerminal` (ui.go:204),
  not the looser `closeLogs` (ui.go:250). A resource that outlives its page
  makes the once-guard mandatory rather than stylistic.

Bindings — `p` is free on both tabs and claimed by no overlay:

| Key | Context | Action |
|---|---|---|
| `p` | container or service node | open the port-selection overlay |
| `3` | tab bar | Forwards tab |
| `d` | Forwards tab | stop the selected forward |
| `o` | Forwards tab | open `http://localhost:<port>` in a browser |

- The port overlay lists exposed ports from `ContainerInspect` plus a "custom…"
  entry, since plenty of listeners are never declared with `EXPOSE`.
- Local port defaults to the remote port, falling back to the next free one.
- Annotate forwarded containers in the tree with `→ :8080`; `renderContainers`
  (ui.go:151) is already re-render safe and preserves the cursor.
- Show an `N fwd` counter in the footer flex next to `cluster` (ui.go:762) —
  the one slot designed for asynchronously updated state. `flash()` (ui.go:842)
  is right for the one-shot confirmation but self-clears after 1.5 s, so it
  cannot carry persistent state.
- The container menu is sized `centered(list, 48, 6)` (ui.go:374), hardcoded to
  its current four items; adding an entry means bumping it.
- Any new overlay list must fall back to `vimListKeys` (ui.go:1039), and new
  overlay runes belong in the pass-through assertion at `ui_test.go:30`.

## 7. Security and audit

Add `"portforward"` as an `auth.Request` action alongside `"exec"` and `"logs"`,
and `ForwardStart`/`ForwardEnd` audit events following the existing shape in
[`agent/internal/audit/audit.go`](agent/internal/audit/audit.go) — identity,
container, service, target port, local port, duration, bytes each way.

Port forwarding deserves its own authorization action even though `exec` is
strictly more powerful. It is a different *exposure* class: it moves an internal
service port onto an operator workstation, where it is reachable by every other
process on that machine, and it should be independently deniable by policy.

The sidecar widens the agent's Docker API usage from exec/attach/inspect to
container creation. That is worth stating plainly: an agent that can create
containers on a node is not more privileged than one that can exec into any
container on it — both are host-root-equivalent through the socket — but the
audit trail should make forward-sidecars unmistakable, hence the label.

## 8. Open questions

- **Sidecar per connection vs. per session.** Start per-connection; revisit if
  the ~300 ms shows up in real use. Contained change, no wire impact.
- **UDP.** Out of scope. `kubectl port-forward` gained UDP late and rarely; TCP
  covers the debugging cases.
- **Reconnect on task reschedule.** If the target task moves to another node the
  forward breaks. Simplest honest behaviour: mark it failed in the registry with
  the reason, and let the operator restart it. Auto-reconnect to a *different*
  container silently changes what the local port means.

## Appendix A — verification

Run against a live 3-node swarm (`docker context pk`) on 2026-07-20. Target
container `nextcloud_whiteboard-server.1` listening on 3002.

```console
$ docker inspect <agent>  --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}={{$v.IPAddress}} {{end}}'
bridge=172.17.0.2

$ docker inspect <target> --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}={{$v.IPAddress}} {{end}}'
gateway=10.0.1.8 nextcloud_nextcloud-internal=10.0.3.7

# A — from the agent's network position to the target's overlay IP
$ docker run --rm alpine sh -c 'nc -z -w3 10.0.3.7 3002; echo exit=$?'
exit=1                                    # unreachable, as predicted

# B — from inside the target's network namespace
$ docker run --rm --network container:<target> alpine sh -c 'nc -z -w3 127.0.0.1 3002; echo exit=$?'
exit=0

# C — real bytes both ways through the sidecar's stdio
$ printf 'GET / HTTP/1.0\r\nHost: x\r\n\r\n' \
    | docker run --rm -i --network container:<target> alpine nc 127.0.0.1 3002
HTTP/1.1 200 OK
X-Powered-By: Express
Content-Type: text/html; charset=utf-8
Content-Length: 41

# D — sidecar start latency, measured on the node over the local socket
332 ms / 308 ms / 269 ms
```

Test C is the important one: it demonstrates the complete mechanism end to end —
a request written to a netns-joined container's stdin reaching the target's
listener, and the response coming back out of stdout — which is precisely the
channel the agent would bridge onto the gRPC stream.
