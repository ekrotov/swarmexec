// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"reflect"
	"testing"

	"github.com/docker/cli/cli/connhelper/ssh"
)

// The route args are asserted exactly, so sharing is switched off here — which
// makes this test do double duty: it is also the proof that the opt-out leaves
// precisely the argv that existed before sharing was added.
func TestSSHForwardArgs(t *testing.T) {
	t.Setenv(sshMuxEnv, "0")
	tests := []struct {
		name   string
		spec   *ssh.Spec
		target string
		jump   string
		want   []string
	}{
		{
			name:   "user host port",
			spec:   &ssh.Spec{User: "deploy", Host: "bastion", Port: "2222"},
			target: "node-1:9443",
			want:   []string{"-l", "deploy", "-p", "2222", "-W", "node-1:9443", "--", "bastion"},
		},
		{
			name:   "host only",
			spec:   &ssh.Spec{Host: "bastion"},
			target: "node-1:9443",
			want:   []string{"-W", "node-1:9443", "--", "bastion"},
		},
		{
			name:   "with jump hosts",
			spec:   &ssh.Spec{User: "deploy", Host: "manager"},
			target: "10.0.0.5:9443",
			jump:   "edge,bastion",
			want:   []string{"-l", "deploy", "-J", "edge,bastion", "-W", "10.0.0.5:9443", "--", "manager"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sshForwardArgs(tt.spec, tt.target, tt.jump)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("sshForwardArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSSHProxyDialerNonSSH(t *testing.T) {
	// A tcp:// host must not get a proxy dialer (direct dial).
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	t.Setenv("DOCKER_CONTEXT", "")
	dialer, err := resolveEndpoint("").agentDialer()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dialer != nil {
		t.Fatalf("expected nil dialer for tcp:// host, got non-nil")
	}
}
