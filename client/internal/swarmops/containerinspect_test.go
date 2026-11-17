// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"strings"
	"testing"
)

const containerDoc = `{
  "Id": "abcdef0123456789",
  "Name": "/web.1.xyz",
  "Created": "2026-11-17T10:00:00Z",
  "RestartCount": 2,
  "State": {"Status": "exited", "Running": false, "ExitCode": 137, "OOMKilled": true,
    "StartedAt": "2026-11-17T10:00:01Z", "FinishedAt": "2026-11-17T10:05:00Z",
    "Health": {"Status": "unhealthy", "FailingStreak": 4, "Log": [
      {"End": "2026-11-17T10:01:00Z", "ExitCode": 0, "Output": "ok"},
      {"End": "2026-11-17T10:02:00Z", "ExitCode": 1, "Output": "curl: (7) Failed\nto connect"}]}},
  "Mounts": [{"Type": "volume", "Name": "data", "Source": "/var/lib/docker/volumes/data/_data", "Destination": "/data", "RW": true},
             {"Type": "bind", "Source": "/etc/ssl", "Destination": "/ssl", "RW": false}],
  "Config": {"Image": "nginx:1.27", "Cmd": ["nginx", "-g", "daemon off;"], "User": "101",
             "Env": ["DB_PASSWORD=hunter2"]},
  "NetworkSettings": {"Networks": {"shop_default": {"IPAddress": "10.0.1.7", "IPPrefixLen": 24,
    "MacAddress": "02:42:0a:00:01:07", "Aliases": ["web"]}}}
}`

func TestContainerInspectViews(t *testing.T) {
	lines, raw, err := ContainerInspectViews([]byte(containerDoc))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text + "\n")
	}
	text := b.String()
	for _, want := range []string{
		"CONTAINER  abcdef012345  web.1.xyz",
		"status         exited", "exit code      137", "oom killed     yes", "restarts       2",
		"status         unhealthy", "failing streak 4",
		"curl: (7) Failed to connect", // newest probe first, output on one line
		"shop_default  10.0.1.7/24  mac 02:42:0a:00:01:07  aliases web",
		"volume data -> /data (rw)", "bind /etc/ssl -> /ssl (ro)",
		"image          nginx:1.27", "cmd            nginx -g daemon off;",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary lacks %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "curl") > strings.Index(text, " ok") {
		t.Error("probes must be newest first")
	}
	if strings.Contains(text, "hunter2") {
		t.Error("the summary must not show the environment")
	}
	if !strings.Contains(raw, "hunter2") || !strings.Contains(raw, "\n  \"Id\"") {
		t.Error("the raw view is the whole document, indented")
	}
}

func TestContainerInspectViewsSparseAndBroken(t *testing.T) {
	lines, _, err := ContainerInspectViews([]byte(`{"Id":"x","State":{"Status":"running","Running":true,"Pid":42}}`))
	if err != nil || len(lines) == 0 {
		t.Fatalf("sparse document: %v", err)
	}
	var text string
	for _, l := range lines {
		text += l.Text + "\n"
	}
	if !strings.Contains(text, "pid            42") || strings.Contains(text, "exit code") || strings.Contains(text, "HEALTH") {
		t.Errorf("running container summary:\n%s", text)
	}
	if lines, _, _ := ContainerInspectViews([]byte(`{"Id":"x"}`)); len(lines) == 0 {
		t.Error("a document without state still renders")
	}
	if _, _, err := ContainerInspectViews([]byte("not json")); err == nil {
		t.Error("broken JSON must be an error")
	}
}
