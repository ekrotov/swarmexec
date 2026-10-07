#!/bin/sh
# Copyright 2026 Cloud Surfers GmbH
# SPDX-License-Identifier: Apache-2.0
#
# verify-release.sh TAG — check, from the outside, that a release reached the
# places people get swarmexec from.
#
# Every other release job checks its own step. This one checks the result, the
# way a user meets it: anonymously, through the public URLs, against the
# checksums the build produced. It runs last and fails loudly, because each of
# these went wrong once without anyone noticing for a while (downloads that
# were member-only, a GitHub release without binaries, a stale site).
#
# Checks (a target whose repository variable is unset is skipped, and says so):
#   gitlab    every binary downloads anonymously and matches SHA256SUMS
#   github    the release is published, carries all assets, the same SHA256SUMS
#   homebrew  the formula is at this version and its hashes are this build's
#   scoop     the manifest is at this version and its hash is this build's
#   dockerhub the agent image tag exists
#   install   install.sh from the live site installs exactly this version
#   site      the site's release notes list this version
#
# Environment: GITHUB_MIRROR_REPO, GITHUB_TAP_REPO, GITHUB_SCOOP_REPO,
# DOCKERHUB_IMAGE or DOCKERHUB_USER, SITE_URL, GITLAB_PROJECT_URL.
# The GitHub CDN and the site deploy lag behind the tag by minutes, so the
# checks that read them retry for up to VERIFY_WAIT seconds (default 900).

set -u

TAG="${1:?usage: verify-release.sh TAG}"
SEMVER="${TAG#v}"
SITE_URL="${SITE_URL:-https://swarm-exec.cloud-surfers.net}"
GITLAB_PROJECT_URL="${GITLAB_PROJECT_URL:-${CI_PROJECT_URL:-https://gitlab.logle.io/cs-public/swarm-remote-exec}}"
WAIT="${VERIFY_WAIT:-900}"
BINARIES="swarmexec-linux-amd64 swarmexec-linux-arm64 swarmexec-darwin-amd64 swarmexec-darwin-arm64 swarmexec-windows-amd64.exe"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failed=""
ok() { echo "  ✓ $*"; }
bad() { echo "  ✗ $*"; }
skip() { echo "  - $*"; }

# retry NAME FN: run FN until it succeeds or WAIT seconds have passed.
retry() {
	name="$1"
	shift
	start=$(date +%s)
	while ! "$@"; do
		if [ $(($(date +%s) - start)) -ge "$WAIT" ]; then
			failed="$failed $name"
			return 1
		fi
		sleep 30
	done
}

# fetch URL FILE: anonymous download; no credentials on purpose.
fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }

# hash_of NAME: this build's checksum for a binary.
hash_of() { awk -v n="$1" '$2 == n || $2 == "*"n {print $1}' "$work/SHA256SUMS"; }

echo "== gitlab: anonymous downloads"
base="$GITLAB_PROJECT_URL/-/releases/$TAG/downloads/bin"
if fetch "$base/SHA256SUMS" "$work/SHA256SUMS"; then
	for b in $BINARIES; do
		want="$(hash_of "$b")"
		if [ -z "$want" ]; then
			bad "$b is not in SHA256SUMS"
			failed="$failed gitlab"
		elif fetch "$base/$b" "$work/$b" && [ "$(sha256sum "$work/$b" | cut -d' ' -f1)" = "$want" ]; then
			ok "$b"
		else
			bad "$b: download failed or checksum mismatch"
			failed="$failed gitlab"
		fi
	done
else
	bad "SHA256SUMS cannot be downloaded anonymously from $base"
	echo "verify-release: nothing else can be checked without it"
	exit 1
fi

echo "== github: release"
github_ok() {
	api="https://api.github.com/repos/$GITHUB_MIRROR_REPO/releases/tags/$TAG"
	curl -fsSL "$api" >"$work/gh.json" 2>/dev/null || { echo "    (not visible yet)"; return 1; }
	draft="$(jq -r '.draft' "$work/gh.json")"
	n="$(jq '.assets | length' "$work/gh.json")"
	[ "$draft" = "false" ] || { echo "    (still a draft)"; return 1; }
	[ "$n" -ge 6 ] || { echo "    (only $n assets)"; return 1; }
	url="$(jq -r '.assets[] | select(.name == "SHA256SUMS") | .browser_download_url' "$work/gh.json")"
	fetch "$url" "$work/gh-SHA256SUMS" && cmp -s "$work/gh-SHA256SUMS" "$work/SHA256SUMS"
}
if [ -n "${GITHUB_MIRROR_REPO:-}" ]; then
	retry github github_ok && ok "published, 6 assets, same SHA256SUMS" || bad "GitHub release incomplete or different"
else
	skip "GITHUB_MIRROR_REPO not set"
fi

echo "== homebrew: formula"
brew_ok() {
	fetch "https://raw.githubusercontent.com/$GITHUB_TAP_REPO/HEAD/Formula/swarmexec.rb" "$work/swarmexec.rb" || return 1
	grep -q "version \"$SEMVER\"" "$work/swarmexec.rb" || { echo "    (formula not at $SEMVER yet)"; return 1; }
	for b in swarmexec-linux-amd64 swarmexec-linux-arm64 swarmexec-darwin-amd64 swarmexec-darwin-arm64; do
		grep -q "$(hash_of "$b")" "$work/swarmexec.rb" || { echo "    ($b hash missing)"; return 1; }
	done
}
if [ -n "${GITHUB_TAP_REPO:-}" ]; then
	retry homebrew brew_ok && ok "version $SEMVER, hashes match" || bad "Homebrew formula wrong"
else
	skip "GITHUB_TAP_REPO not set"
fi

echo "== scoop: manifest"
scoop_ok() {
	fetch "https://raw.githubusercontent.com/$GITHUB_SCOOP_REPO/HEAD/bucket/swarmexec.json" "$work/swarmexec.json" || return 1
	[ "$(jq -r '.version' "$work/swarmexec.json")" = "$SEMVER" ] || { echo "    (manifest not at $SEMVER yet)"; return 1; }
	grep -q "$(hash_of swarmexec-windows-amd64.exe)" "$work/swarmexec.json" || { echo "    (hash differs)"; return 1; }
}
if [ -n "${GITHUB_SCOOP_REPO:-}" ]; then
	retry scoop scoop_ok && ok "version $SEMVER, hash matches" || bad "Scoop manifest wrong"
else
	skip "GITHUB_SCOOP_REPO not set"
fi

echo "== dockerhub: agent image"
hub_repo=""
if [ -n "${DOCKERHUB_IMAGE:-}" ]; then
	hub_repo="${DOCKERHUB_IMAGE#docker.io/}"
elif [ -n "${DOCKERHUB_USER:-}" ]; then
	hub_repo="$DOCKERHUB_USER/swarmexec-agent"
fi
hub_ok() { curl -fsS -o /dev/null "https://hub.docker.com/v2/repositories/$hub_repo/tags/$TAG"; }
if [ -n "$hub_repo" ]; then
	retry dockerhub hub_ok && ok "$hub_repo:$TAG" || bad "$hub_repo:$TAG not found"
else
	skip "no Docker Hub image configured"
fi

echo "== install.sh from the live site"
install_ok() {
	rm -rf "$work/bin"
	curl -fsSL "$SITE_URL/install.sh" | SWARMEXEC_VERSION="$TAG" SWARMEXEC_INSTALL_DIR="$work/bin" sh >"$work/install.log" 2>&1 || {
		sed 's/^/    /' "$work/install.log"
		return 1
	}
	"$work/bin/swarmexec" --version 2>/dev/null | grep -q "$TAG"
}
retry install install_ok && ok "installs $TAG" || bad "install.sh did not install $TAG"

echo "== site: release notes"
site_ok() {
	fetch "$SITE_URL/releases.json" "$work/releases.json" || return 1
	jq -e --arg t "$TAG" 'map(.version) | index($t) != null' "$work/releases.json" >/dev/null || { echo "    (not deployed yet)"; return 1; }
}
retry site site_ok && ok "lists $TAG" || bad "the site does not list $TAG"

if [ -n "$failed" ]; then
	echo
	echo "verify-release: FAILED:$failed"
	exit 1
fi
echo
echo "verify-release: $TAG is out everywhere"
