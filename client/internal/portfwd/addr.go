// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package portfwd

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// DefaultAddress is where a forward listens unless the operator says
// otherwise: loopback, so an internal port is not re-exposed to the network the
// operator's machine sits on. The CLI's --address defaults to the same.
const DefaultAddress = "127.0.0.1"

// IsLoopback reports whether a local bind address keeps the forward on this
// machine: 127.0.0.0/8, ::1 or the name localhost. Empty counts as loopback,
// because empty means the default.
func IsLoopback(addr string) bool {
	addr = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(addr), "["), "]")
	if addr == "" || strings.EqualFold(addr, "localhost") {
		return true
	}
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}

// BrowseHost is the host to put in a URL for a forward bound to addr. An
// unspecified bind address listens everywhere but is not something to connect
// to, so the URL goes through loopback, which it also listens on.
func BrowseHost(addr string) string {
	if addr == "" {
		return DefaultAddress
	}
	if ip := net.ParseIP(addr); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			return "127.0.0.1"
		}
		return "::1"
	}
	return addr
}

// ParsePortSpec accepts "8080" (same port both sides) or "9090:8080"
// (local:remote), matching the kubectl port-forward spelling.
func ParsePortSpec(spec string) (local, remote uint32, err error) {
	parsePort := func(s, what string) (uint32, error) {
		n, perr := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
		if perr != nil || n == 0 || n > 65535 {
			return 0, fmt.Errorf("invalid %s port %q: expected 1-65535", what, s)
		}
		return uint32(n), nil
	}

	l, r, found := strings.Cut(spec, ":")
	if !found {
		p, perr := parsePort(spec, "")
		return p, p, perr
	}
	if local, err = parsePort(l, "local"); err != nil {
		return 0, 0, err
	}
	if remote, err = parsePort(r, "remote"); err != nil {
		return 0, 0, err
	}
	return local, remote, nil
}
