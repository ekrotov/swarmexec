// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"swarmexec/agent/internal/statsampler"
)

// The memory limit on the wire is the container's own when it has one, the
// node's total otherwise — never the frame's value read as a budget when the
// node total is known. CPU appears only once a percentage could be computed.
func TestStatsProtoResolvesTheMemoryLimit(t *testing.T) {
	own := statsProto("c1", statsampler.Sample{MemBytes: 10, MemLimit: 512, MemIsOwn: true, CPULimit: 2, Health: "healthy"}, 8192)
	if own.GetMemoryLimitBytes() != 512 || !own.GetMemoryLimited() || own.GetCpuLimitCores() != 2 || own.GetHealth() != "healthy" {
		t.Errorf("own limit: %+v", own)
	}
	node := statsProto("c2", statsampler.Sample{MemBytes: 10, MemLimit: 4096}, 8192)
	if node.GetMemoryLimitBytes() != 8192 || node.GetMemoryLimited() {
		t.Errorf("no own limit, node total known: %+v", node)
	}
	frame := statsProto("c3", statsampler.Sample{MemLimit: 4096}, 0)
	if frame.GetMemoryLimitBytes() != 4096 {
		t.Errorf("no own limit, node total unknown: %+v", frame)
	}
	if node.GetCpuPercent() != 0 {
		t.Errorf("cpu without a usable reading = %v, want 0", node.GetCpuPercent())
	}
	cpu := statsProto("c4", statsampler.Sample{CPUUsable: true, CPUPercent: 150}, 0)
	if cpu.GetCpuPercent() != 150 {
		t.Errorf("cpu = %v, want 150", cpu.GetCpuPercent())
	}
}
