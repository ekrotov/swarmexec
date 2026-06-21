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
// logs even when they share a destination.
func New(l *slog.Logger) *Logger {
	return &Logger{log: l.With("component", "audit")}
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
