#!/bin/sh
# Push the generated Homebrew formula and Scoop manifest to their public
# repositories, so `brew upgrade` and `scoop update` see a new release.
#
#   ci/publish-packages.sh <tag> <packaging-dir>
#
# Environment:
#   GITHUB_TAP_REPO        owner/repo of the Homebrew tap, e.g. ekrotov/homebrew-swarmexec
#   GITHUB_SCOOP_REPO      owner/repo of the Scoop bucket, e.g. ekrotov/scoop-swarmexec
#   GITHUB_PACKAGES_TOKEN  token that may push to both (falls back to GITHUB_MIRROR_TOKEN)
#   GIT_REMOTE_BASE        test hook: where the repos live (default https://github.com)
#
# A repository that is not configured is skipped with a note, so either one can
# be set up first. A prerelease is never published: the formula and the
# manifest name ONE version, and a release candidate must not become what
# `brew upgrade` installs.
set -eu

tag=$1
dir=$2
token=${GITHUB_PACKAGES_TOKEN:-${GITHUB_MIRROR_TOKEN:-}}
base=${GIT_REMOTE_BASE:-https://github.com}

case "$tag" in
*-*)
	echo "prerelease $tag — not published to Homebrew or Scoop"
	exit 0
	;;
esac

# git_logged <what> <action> <git args...>: run git and, if it fails, say why.
# git's own message is the useful part — "remote: Internal Server Error" is a
# GitHub outage, "Permission denied" a token, "rejected (fetch first)" a race —
# but it may repeat the remote URL, and the URL carries the token. So the
# message is shown with the token cut out, never raw and never not at all.
git_logged() {
	what=$1 action=$2
	shift 2
	if ! out=$(git "$@" 2>&1); then
		echo "$what: $action failed:"
		printf '%s\n' "$out" | redact | sed 's/^/    /'
		return 1
	fi
}

redact() {
	if [ -n "$token" ]; then
		# The token as a literal, whatever characters it holds.
		pat=$(printf '%s' "$token" | sed 's/[][\\.*^$|/&]/\\&/g')
		sed "s|$pat|***|g"
	else
		cat
	fi
}

# publish <repo> <source file> <path in repo> <what>
publish() {
	repo=$1 src=$2 dest=$3 what=$4
	if [ -z "$repo" ]; then
		echo "$what: no repository configured — skipped"
		return 0
	fi
	[ -n "$token" ] || { echo "$what: no token (GITHUB_PACKAGES_TOKEN) — cannot push to $repo"; return 1; }
	[ -f "$src" ] || { echo "$what: $src was not generated"; return 1; }

	work=$(mktemp -d)
	url="$base/$repo.git"
	case "$base" in
	https://*) url="https://x-access-token:${token}@${base#https://}/$repo.git" ;;
	esac
	# The token is in the URL only for git's own use; the log shows the repo.
	git_logged "$what" "clone of $repo (does it exist, may the token read it?)" clone --quiet --depth 1 "$url" "$work" || { rm -rf "$work"; return 1; }
	cd "$work"
	# An empty repository has no branch yet; give it main.
	git rev-parse --verify --quiet HEAD >/dev/null || git checkout --quiet -b main
	mkdir -p "$(dirname "$dest")"
	cp "$src" "$dest"
	git add "$dest"
	if git diff --cached --quiet; then
		echo "$what: $repo already has $tag"
	else
		git -c user.name="swarmexec release" -c user.email="noreply@cloud-surfers.de" \
			commit --quiet -m "swarmexec $tag"
		git_logged "$what" "push to $repo" push --quiet origin HEAD || { cd - >/dev/null; rm -rf "$work"; return 1; }
		echo "$what: published $tag to $repo"
	fi
	cd - >/dev/null
	rm -rf "$work"
}

rc=0
publish "${GITHUB_TAP_REPO:-}" "$PWD/$dir/swarmexec.rb" Formula/swarmexec.rb "Homebrew" || rc=1
publish "${GITHUB_SCOOP_REPO:-}" "$PWD/$dir/swarmexec.json" bucket/swarmexec.json "Scoop" || rc=1
exit $rc
