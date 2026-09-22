// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package audit emits structured, one-line-per-event audit records. Audit
// records never contain stdin/stdout payload contents (REQUIREMENTS §6).
package audit

import (
	"log/slog"
	"time"
)

// Logger writes audit events. It wraps an slog.Logger so output is structured
// (JSON or logfmt) and routable to a dedicated destination.
type Logger struct {
	log *slog.Logger
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
	return &Logger{log: l.With("component", "audit", "identity_verified", identityVerified)}
}

// SessionStart records the beginning of an exec session. Payload bytes are
// never logged — only metadata.
func (l *Logger) SessionStart(identity, containerID, service string, cmd []string, tty bool, clientAddr string) {
	l.log.Info("session_start",
		"event", "session_start",
		"identity", identity,
		"container_id", containerID,
		"service", service,
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
	l.log.Info("image.prune",
		"identity", identity,
		"all", all,
		"reclaimed_bytes", reclaimed,
		"deleted", deleted,
		"ok", ok,
		"error", errMsg,
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

// AuthDecision records an authorization allow/deny along with the reason.
func (l *Logger) AuthDecision(identity, containerID, service string, allow bool, reason string) {
	l.log.Info("auth_decision",
		"event", "auth_decision",
		"identity", identity,
		"container_id", containerID,
		"service", service,
		"allow", allow,
		"reason", reason,
	)
}
