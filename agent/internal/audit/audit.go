// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package audit emits structured, one-line-per-event audit records. Audit
// records never contain stdin/stdout payload contents (REQUIREMENTS §6).
package audit

import (
	"log/slog"
	"sync"
	"time"
)

// Logger writes audit events. It wraps an slog.Logger so output is structured
// (JSON or logfmt) and routable to a dedicated destination.
type Logger struct {
	log *slog.Logger

	// pollMu guards polled: for each (identity, action) of a polled read the
	// time of the last allow-record written and how many were held back since.
	pollMu sync.Mutex
	polled map[pollKey]*pollState
	now    func() time.Time
}

// pollInterval is how often an allowed, polled read is recorded per identity
// and action. A denial is never throttled.
const pollInterval = time.Minute

// maxPolled bounds the throttle table; identities are client-supplied in
// shared-secret mode.
const maxPolled = 4096

type pollKey struct{ identity, action string }

type pollState struct {
	last       time.Time
	suppressed int
}

// New returns an audit Logger writing through the given slog.Logger. Every
// record is tagged with component=audit so it can be filtered from operational
// logs even when they share a destination, and with identity_verified so a
// reader can tell what the identity field is worth.
//
// identityVerified must be false wherever the operator name is client-supplied
// — shared-secret mode without a client CA, where every operator holds the same
// credential and the `operator` header is simply believed. An audit trail that
// prints a colleague's name with the same authority as a certificate CN is
// worse than one that admits it cannot tell: it invites an accountability claim
// it cannot support. Per-operator attribution needs client certs (-ca-cert).
func New(l *slog.Logger, identityVerified bool) *Logger {
	return &Logger{log: l.With("component", "audit", "identity_verified", identityVerified), now: time.Now}
}

// SessionStart records the beginning of an exec session. Payload bytes are
// never logged — only metadata.
//
// user is the account the command runs as — the requested one, else the
// container's, else root — so a root shell is visible in the trail.
func (l *Logger) SessionStart(identity, containerID, service, user string, cmd []string, tty bool, clientAddr string) {
	l.log.Info("session_start",
		"event", "session_start",
		"identity", identity,
		"container_id", containerID,
		"service", service,
		"user", user,
		"cmd", cmd,
		"tty", tty,
		"client_addr", clientAddr,
	)
}

// SessionEnd records the end of an exec session, including the exit code,
// duration, and byte counts in each direction.
func (l *Logger) SessionEnd(identity, containerID string, exitCode int, dur time.Duration, bytesIn, bytesOut int64) {
	l.log.Info("session_end",
		"event", "session_end",
		"identity", identity,
		"container_id", containerID,
		"exit_code", exitCode,
		"duration_ms", dur.Milliseconds(),
		"bytes_in", bytesIn,
		"bytes_out", bytesOut,
	)
}

// LogsStart records the beginning of a logs stream.
func (l *Logger) LogsStart(identity, containerID, service string, follow bool, clientAddr string) {
	l.log.Info("logs_start",
		"event", "logs_start",
		"identity", identity,
		"container_id", containerID,
		"service", service,
		"follow", follow,
		"client_addr", clientAddr,
	)
}

// LogsEnd records the end of a logs stream, including duration and bytes sent.
func (l *Logger) LogsEnd(identity, containerID string, dur time.Duration, bytesOut int64) {
	l.log.Info("logs_end",
		"event", "logs_end",
		"identity", identity,
		"container_id", containerID,
		"duration_ms", dur.Milliseconds(),
		"bytes_out", bytesOut,
	)
}

// ForwardStart records the beginning of a port-forward connection. Each
// forwarded TCP connection is its own stream and so its own audit record;
// "sidecar" reports whether the agent had to join the target's network
// namespace to reach the port, or could dial it directly.
func (l *Logger) ForwardStart(identity, containerID, service string, port uint32, sidecar bool, clientAddr string) {
	l.log.Info("forward_start",
		"event", "forward_start",
		"identity", identity,
		"container_id", containerID,
		"service", service,
		"port", port,
		"sidecar", sidecar,
		"client_addr", clientAddr,
	)
}

// ForwardEnd records the end of a port-forward connection.
func (l *Logger) ForwardEnd(identity, containerID string, port uint32, dur time.Duration, bytesIn, bytesOut int64) {
	l.log.Info("forward_end",
		"event", "forward_end",
		"identity", identity,
		"container_id", containerID,
		"port", port,
		"duration_ms", dur.Milliseconds(),
		"bytes_in", bytesIn,
		"bytes_out", bytesOut,
	)
}

// CopyStart records the beginning of a file copy (copy.go). direction is
// "from" or "to"; path is the path inside the container. The path belongs in
// the record: "who took what out of which container" is the question asked
// afterwards, and the container id alone cannot answer it.
func (l *Logger) CopyStart(identity, containerID, service, direction, path, clientAddr string) {
	l.log.Info("copy_start",
		"event", "copy_start",
		"identity", identity,
		"container_id", containerID,
		"service", service,
		"direction", direction,
		"path", path,
		"client_addr", clientAddr,
	)
}

// CopyEnd records the end of a file copy, its size and its outcome. A copy
// that was refused mid-stream (over the size cap, a failed extraction) ends
// here with ok=false, so the log never shows a start without an end.
func (l *Logger) CopyEnd(identity, containerID, direction string, dur time.Duration, bytes int64, ok bool, errMsg string) {
	l.log.Info("copy_end",
		"event", "copy_end",
		"identity", identity,
		"container_id", containerID,
		"direction", direction,
		"duration_ms", dur.Milliseconds(),
		"bytes", bytes,
		"ok", ok,
		"error", errMsg,
	)
}

// VolumeRemove records a volume deletion attempt and its outcome.
func (l *Logger) VolumeRemove(identity, name string, ok bool, errMsg string) {
	l.log.Info("volume_remove",
		"event", "volume_remove",
		"identity", identity,
		"volume", name,
		"ok", ok,
		"error", errMsg,
	)
}

// VolumeCreate records a volume creation attempt and its outcome.
func (l *Logger) VolumeCreate(identity, name string, ok bool, errMsg string) {
	l.log.Info("volume_create",
		"event", "volume_create",
		"identity", identity,
		"volume", name,
		"ok", ok,
		"error", errMsg,
	)
}

// ImagePrune records a reclaim on this node. The `all` flag is recorded
// separately because the two modes are different acts: one removes untagged
// leftovers, the other removes images a stopped service still needs.
func (l *Logger) ImagePrune(identity string, all bool, reclaimed int64, deleted int, ok bool, errMsg string) {
	l.log.Info("image_prune",
		"event", "image_prune",
		"identity", identity,
		"all", all,
		"reclaimed_bytes", reclaimed,
		"deleted", deleted,
		"ok", ok,
		"error", errMsg,
	)
}

// PruneRecord is one category of a Prune as the trail records it.
type PruneRecord struct {
	Identity string
	// Trigger is "manual" (a client asked) or "schedule" (the agent's own
	// prune schedule, identity "scheduler").
	Trigger  string
	Category string
	// Freed is the bytes reclaimed, Count the objects removed.
	Freed, Count int64
	// Skipped marks a category not attempted (build cache under a label
	// filter: it has no labels to match).
	Skipped bool
	OK      bool
	Error   string
}

// Prune records one category of a Prune, manual or scheduled. One record per
// category, because each is its own act with its own authorization: a policy
// that allowed the containers and refused the volumes leaves exactly that in
// the trail.
func (l *Logger) Prune(r PruneRecord) {
	l.log.Info("prune",
		"event", "prune",
		"identity", r.Identity,
		"trigger", r.Trigger,
		"category", r.Category,
		"freed_bytes", r.Freed,
		"count", r.Count,
		"skipped", r.Skipped,
		"ok", r.OK,
		"error", r.Error,
	)
}

// EventWatchStart records the beginning of a container event watch. The watch
// itself reveals nothing an operator could not see with logs on the same
// container, but it is a long-lived subscription to a container's lifecycle and
// belongs in the trail like every other stream.
func (l *Logger) EventWatchStart(identity, containerID, service, clientAddr string) {
	l.log.Info("event_watch_start",
		"event", "event_watch_start",
		"identity", identity,
		"container_id", containerID,
		"service", service,
		"client_addr", clientAddr,
	)
}

// EventWatchEnd records the end of a container event watch. Only the COUNT of
// events is recorded, never their content: the audit log is not a second copy
// of the node's container lifecycle.
func (l *Logger) EventWatchEnd(identity, containerID string, dur time.Duration, events int64) {
	l.log.Info("event_watch_end",
		"event", "event_watch_end",
		"identity", identity,
		"container_id", containerID,
		"duration_ms", dur.Milliseconds(),
		"events", events,
	)
}

// ContainerList records an enumeration of the node's containers.
//
// Discovery is the one thing an attacker needs before anything else: container
// ids and service names are the input to exec, logs and port-forward. It used
// to be the single RPC that produced no audit record at all, so mapping every
// container on every node left no trace. Only the shape of the answer is
// recorded — the filter and how many containers matched — never the listing
// itself, which would put the node's whole topology in the audit log on every
// refresh.
func (l *Logger) ContainerList(identity, filter string, matched int) {
	l.log.Info("container_list",
		"event", "container_list",
		"identity", identity,
		"filter", filter,
		"matched", matched,
	)
}

// AuthRecord is one authorization decision: who asked for what, on which
// object, from where, and what the policy answered. Only the fields the action
// carries are set. The command line and environment are deliberately absent.
type AuthRecord struct {
	Identity    string
	Action      string
	ContainerID string
	Service     string
	Port        uint32
	User        string // effective user for an exec
	Volume      string
	ClientAddr  string
	Allow       bool
	Reason      string
	// Polled marks a read a client repeats on a timer (the TUI asks for stats
	// at every tick). Its allow-records are written once per identity and
	// action per minute, carrying how many were held back; a denial is always
	// written.
	Polled bool
}

// AuthDecision records an authorization allow/deny along with the reason, the
// requested action and the object it targeted.
func (l *Logger) AuthDecision(r AuthRecord) {
	attrs := []any{
		"event", "auth_decision",
		"identity", r.Identity,
		"action", r.Action,
		"container_id", r.ContainerID,
		"service", r.Service,
		"allow", r.Allow,
		"reason", r.Reason,
	}
	if r.Allow && r.Polled {
		held, ok := l.throttle(r.Identity, r.Action)
		if !ok {
			return
		}
		if held > 0 {
			attrs = append(attrs, "suppressed", held)
		}
	}
	for _, f := range []struct{ k, v string }{
		{"user", r.User}, {"volume", r.Volume}, {"client_addr", r.ClientAddr},
	} {
		if f.v != "" {
			attrs = append(attrs, f.k, f.v)
		}
	}
	if r.Port != 0 {
		attrs = append(attrs, "port", r.Port)
	}
	l.log.Info("auth_decision", attrs...)
}

// throttle reports whether a polled allow-record is due, and how many were
// held back since the last one written. The first one is always due.
func (l *Logger) throttle(identity, action string) (held int, write bool) {
	l.pollMu.Lock()
	defer l.pollMu.Unlock()
	now := l.now()
	k := pollKey{identity, action}
	st := l.polled[k]
	if st != nil && now.Sub(st.last) < pollInterval {
		st.suppressed++
		return 0, false
	}
	if st != nil {
		held = st.suppressed
	}
	if l.polled == nil || len(l.polled) >= maxPolled {
		l.polled = map[pollKey]*pollState{}
	}
	l.polled[k] = &pollState{last: now}
	return held, true
}

// ContainerKill records a kill of a swarm task container — the one RPC that
// stops a workload — whatever its outcome, including a refusal because the
// container was not a swarm task.
func (l *Logger) ContainerKill(identity, containerID, service string, ok bool, errMsg string) {
	l.log.Info("container_kill",
		"event", "container_kill",
		"identity", identity,
		"container_id", containerID,
		"service", service,
		"ok", ok,
		"error", errMsg,
	)
}

// ContainerInspect records a node-local inspect of one container. The document
// itself is never recorded: it carries the container's environment, which
// routinely holds credentials.
func (l *Logger) ContainerInspect(identity, containerID, service string, ok bool, errMsg string) {
	l.log.Info("container_inspect",
		"event", "container_inspect",
		"identity", identity,
		"container_id", containerID,
		"service", service,
		"ok", ok,
		"error", errMsg,
	)
}
