// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

// KOR-07: strconv.ParseFloat accepts "NaN", "inf" and "1e30"; the int64
// conversion of those is -9223372036854775808, which went to the manager as a
// limit.
func TestParseCPUCoresRejectsWhatIsNotACount(t *testing.T) {
	good := []struct {
		in   string
		want int64
	}{
		{"", 0}, {"0", 0}, {"0.5", 500_000_000}, {"2", 2_000_000_000}, {" 1.25 ", 1_250_000_000},
		{"9e9", 9_000_000_000_000_000_000}, // the largest order of magnitude that still fits
	}
	for _, c := range good {
		if got, err := ParseCPUCores(c.in); err != nil || got != c.want {
			t.Errorf("ParseCPUCores(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"NaN", "nan", "inf", "+Inf", "-inf", "Infinity", "1e30", "1e19", "9.3e9", "-1", "-0.5", "abc", "1,5"} {
		if got, err := ParseCPUCores(bad); err == nil {
			t.Errorf("ParseCPUCores(%q) = %d, nil; want an error", bad, got)
		}
	}
}

// KOR-07: a port is 16 bits. 70000 used to pass and be sent to the manager.
func TestParseServicePortRange(t *testing.T) {
	good := []string{"1:1", "65535:65535", "8080:80/tcp", "80:65535/udp", "65535:1 host"}
	for _, s := range good {
		if _, err := ParseServicePort(s); err != nil {
			t.Errorf("ParseServicePort(%q): %v", s, err)
		}
	}
	bad := []string{"65536:80", "80:65536", "70000:80", "80:70000/tcp", "4294967296:80", "99999999999:80", "-1:80", "80:-1"}
	for _, s := range bad {
		if p, err := ParseServicePort(s); err == nil {
			t.Errorf("ParseServicePort(%q) = %+v; want an error", s, p)
		}
	}
}

// KOR-07: the scheduler treats x86_64 as amd64 and aarch64 as arm64 when it
// matches a service's platforms against a node's; comparing the raw strings
// reported "platform not supported" for a node that would have been used.
func TestNodeMatchesPlatformNormalisesArchitecture(t *testing.T) {
	node := func(os, arch string) swarm.Node {
		var n swarm.Node
		n.Description.Platform.OS, n.Description.Platform.Architecture = os, arch
		return n
	}
	plat := func(os, arch string) []swarm.Platform { return []swarm.Platform{{OS: os, Architecture: arch}} }
	tests := []struct {
		name string
		node swarm.Node
		want []swarm.Platform
		ok   bool
	}{
		{"same", node("linux", "amd64"), plat("linux", "amd64"), true},
		{"node x86_64, service amd64", node("linux", "x86_64"), plat("linux", "amd64"), true},
		{"node amd64, service x86_64", node("linux", "amd64"), plat("linux", "x86_64"), true},
		{"node aarch64, service arm64", node("linux", "aarch64"), plat("linux", "arm64"), true},
		{"node arm64, service aarch64", node("linux", "arm64"), plat("linux", "aarch64"), true},
		{"case", node("linux", "AMD64"), plat("linux", "amd64"), true},
		{"empty arch matches anything", node("linux", "arm64"), plat("linux", ""), true},
		{"empty os matches anything", node("linux", "arm64"), plat("", "arm64"), true},
		{"really different arch", node("linux", "x86_64"), plat("linux", "arm64"), false},
		{"really different os", node("windows", "x86_64"), plat("linux", "amd64"), false},
		{"one of several", node("linux", "aarch64"), []swarm.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}, true},
	}
	for _, tc := range tests {
		if got := nodeMatchesPlatform(tc.node, tc.want); got != tc.ok {
			t.Errorf("%s: nodeMatchesPlatform = %v, want %v", tc.name, got, tc.ok)
		}
	}
}
