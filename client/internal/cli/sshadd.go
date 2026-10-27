// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/cli/cli/connhelper/ssh"
)

// Where ssh cannot ask through swarmexec (OpenSSH before 8.4, Windows, the
// operator switched it off), the way past a key with a passphrase is the one
// operators already know: load the key into their ssh-agent. These helpers
// recognise that situation and build the ssh-add for it, so the TUI can run it
// in place and the CLI can name it exactly.

// isSSHAuthError reports whether err is ssh failing to authenticate — as
// opposed to a host that is down or a daemon that refused. commandconn puts
// ssh's stderr into the error, which is where these come from.
func isSSHAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, sig := range []string{
		"Permission denied (",
		"read_passphrase: can't open /dev/tty",
		"Too many authentication failures",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// sshAddCommand is the ssh-add that loads the keys this endpoint signs in
// with: the identity files ssh itself would use for the host and each jump
// host (`ssh -G`), those that exist. None found means plain `ssh-add`, which
// loads the default keys.
func sshAddCommand(host, proxyJump string) []string {
	var files []string
	seen := map[string]bool{}
	add := func(args []string) {
		for _, f := range sshConfigIdentityFiles(args) {
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
	}
	if sp, err := ssh.ParseURL(host); err == nil {
		add(sshTargetArgs(sp.User, sp.Port, sp.Host))
	}
	for _, hop := range strings.Split(proxyJump, ",") {
		if hop = strings.TrimSpace(hop); hop != "" {
			add(sshTargetArgs(splitJumpHop(hop)))
		}
	}
	return append([]string{"ssh-add"}, files...)
}

func sshTargetArgs(user, port, host string) []string {
	var args []string
	if user != "" {
		args = append(args, "-l", user)
	}
	if port != "" {
		args = append(args, "-p", port)
	}
	return append(args, "--", host)
}

// splitJumpHop reads one ProxyJump entry, "[user@]host[:port]" (or an
// ssh:// URL).
func splitJumpHop(hop string) (user, port, host string) {
	if strings.HasPrefix(hop, "ssh://") {
		if sp, err := ssh.ParseURL(hop); err == nil {
			return sp.User, sp.Port, sp.Host
		}
	}
	if i := strings.LastIndex(hop, "@"); i >= 0 {
		user, hop = hop[:i], hop[i+1:]
	}
	if i := strings.LastIndex(hop, ":"); i >= 0 && !strings.Contains(hop[:i], ":") {
		hop, port = hop[:i], hop[i+1:]
	}
	return user, port, hop
}

// sshConfigIdentityFiles asks ssh which identity files it would offer for
// target (`ssh -G`), and keeps the ones that exist.
func sshConfigIdentityFiles(target []string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", append([]string{"-G"}, target...)...).Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, f := range parseIdentityFiles(string(out)) {
		if _, err := os.Stat(f); err == nil {
			files = append(files, f)
		}
	}
	return files
}

// parseIdentityFiles extracts the identityfile lines of `ssh -G` output, with
// "~" expanded.
func parseIdentityFiles(out string) []string {
	home, _ := os.UserHomeDir()
	var files []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok || !strings.EqualFold(k, "identityfile") {
			continue
		}
		v = strings.TrimSpace(v)
		if home != "" && (v == "~" || strings.HasPrefix(v, "~/")) {
			v = filepath.Join(home, strings.TrimPrefix(v, "~"))
		}
		files = append(files, v)
	}
	return files
}

// sshAgentRunning reports whether there is an agent for ssh-add to load into.
func sshAgentRunning() bool {
	return strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK")) != ""
}

// sshAuthHint is what the CLI adds to an ssh sign-in failure: the ssh-add that
// gets past it, or why asking did not happen.
func sshAuthHint(err error, host, proxyJump string) string {
	if !isSSHAuthError(err) || !strings.HasPrefix(host, "ssh://") {
		return ""
	}
	if !sshAgentRunning() {
		return "\n\nssh could not sign in, and no ssh-agent is running. If your key has a passphrase, start an agent " +
			"(`eval \"$(ssh-agent)\"`), load the key with `ssh-add`, and run swarmexec again from that shell."
	}
	if askpassActive() {
		return "\n\nssh could not sign in. If it asked for a passphrase, it was declined or wrong; " +
			"or load the key once with `" + strings.Join(sshAddCommand(host, proxyJump), " ") + "`."
	}
	why := askpassUnavailable()
	if why == "" {
		why = "swarmexec could not set up asking"
	}
	return "\n\nssh could not sign in — a key with a passphrase cannot be asked for here (" + why + "). " +
		"Load it into your ssh-agent and run again:\n  " + strings.Join(sshAddCommand(host, proxyJump), " ")
}
