// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"reflect"
	"testing"

	"github.com/docker/cli/cli/connhelper/ssh"
)

func TestSSHForwardArgs(t *testing.T) {
	tests := []struct {
		name   string
		spec   *ssh.Spec
		target string
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
			name:   "user and host",
			spec:   &ssh.Spec{User: "deploy", Host: "bastion"},
			target: "10.0.0.5:9443",
			want:   []string{"-l", "deploy", "-W", "10.0.0.5:9443", "--", "bastion"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sshForwardArgs(tt.spec, tt.target)
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
	dialer, err := sshProxyDialer("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dialer != nil {
		t.Fatalf("expected nil dialer for tcp:// host, got non-nil")
	}
}
