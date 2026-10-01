#!/bin/sh
# swarmexec installer — downloads the client binary for this machine, verifies
# it against the release's SHA256SUMS, and puts it on your PATH.
#
#   curl -fsSL https://swarm-exec.cloud-surfers.net/install.sh | sh
#
# Read it first if you like; that is the point of keeping it short. Everything
# below is a function, and nothing runs until the last line, so a download cut
# off half way executes nothing.
#
# Environment:
#   SWARMEXEC_VERSION      a release tag, e.g. v1.19.2 (default: the latest)
#   SWARMEXEC_INSTALL_DIR  where to put the binary (default: /usr/local/bin if
#                          writable, else ~/.local/bin)
#   SWARMEXEC_BASE_URL     download from this directory instead — a mirror that
#                          holds the release's binaries and its SHA256SUMS
#
# It never uses sudo, never edits shell profiles, and refuses to install a
# binary it could not verify. Windows: download swarmexec-windows-amd64.exe
# from the releases page instead.

set -eu

PROJECT="https://gitlab.logle.io/cs-public/swarm-remote-exec"

say() { printf '%s\n' "$*" >&2; }
die() { say "swarmexec install: $*"; exit 1; }

platform() {
	os=$(uname -s)
	arch=$(uname -m)
	case "$os" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "unsupported OS '$os' (Linux and macOS only; Windows: use the .exe from $PROJECT/-/releases)" ;;
	esac
	case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture '$arch' (amd64 and arm64 only)" ;;
	esac
	printf 'swarmexec-%s-%s' "$os" "$arch"
}

base_url() {
	if [ -n "${SWARMEXEC_BASE_URL:-}" ]; then
		printf '%s' "${SWARMEXEC_BASE_URL%/}"
	elif [ -n "${SWARMEXEC_VERSION:-}" ]; then
		printf '%s/-/releases/%s/downloads/bin' "$PROJECT" "$SWARMEXEC_VERSION"
	else
		printf '%s/-/releases/permalink/latest/downloads/bin' "$PROJECT"
	fi
}

fetch() { # url dest
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "needs curl or wget"
	fi
}

sha256() { # file
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "needs sha256sum or shasum to verify the download — refusing to install an unverified binary"
	fi
}

install_dir() {
	if [ -n "${SWARMEXEC_INSTALL_DIR:-}" ]; then
		printf '%s' "$SWARMEXEC_INSTALL_DIR"
	elif [ -w /usr/local/bin ]; then
		printf '/usr/local/bin'
	else
		printf '%s/.local/bin' "$HOME"
	fi
}

main() {
	name=$(platform)
	url=$(base_url)
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	say "downloading $name ${SWARMEXEC_VERSION:-(latest)}"
	fetch "$url/$name" "$tmp/$name" || die "download failed: $url/$name"
	fetch "$url/SHA256SUMS" "$tmp/SHA256SUMS" || die "download failed: $url/SHA256SUMS"

	want=$(awk -v n="$name" '$2 == n || $2 == "*"n { print $1 }' "$tmp/SHA256SUMS")
	[ -n "$want" ] || die "$name is not listed in SHA256SUMS"
	got=$(sha256 "$tmp/$name")
	[ "$got" = "$want" ] || die "checksum mismatch for $name (got $got, want $want) — not installing"
	say "checksum ok"

	dir=$(install_dir)
	mkdir -p "$dir" || die "cannot create $dir (set SWARMEXEC_INSTALL_DIR)"
	chmod 0755 "$tmp/$name"
	mv "$tmp/$name" "$dir/swarmexec" || die "cannot write $dir/swarmexec (set SWARMEXEC_INSTALL_DIR, or run with sudo yourself)"
	say "installed $("$dir/swarmexec" --version 2>/dev/null || echo swarmexec) to $dir/swarmexec"

	case ":$PATH:" in
	*":$dir:"*) ;;
	*) say "note: $dir is not on your PATH — add it, e.g. export PATH=\"$dir:\$PATH\"" ;;
	esac
}

main "$@"
