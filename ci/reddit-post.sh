#!/bin/sh
# Build the Reddit post for a swarmexec release and save it as a CI artifact.
#
# Run by the `reddit-post` CI job on a stable semver tag. It composes a post from
# the release notes (the annotated git-tag message), writes it to
# $REDDIT_POST_FILE and prints it to the job log.
#
# NOTHING IS POSTED, and that is deliberate rather than unfinished. Reddit is a
# community, not a distribution channel: a release announcement dropped in by a
# robot is what "self-promotion" rules exist to stop, and every subreddit words
# those rules differently. The value of automating it is the text; the judgement
# — which subreddit, which flair, which title, whether this release is even
# worth a post — stays with a person, who then has to be there to answer
# comments. So the job hands over a draft and stops.
#
# Reddit RENDERS Markdown, so unlike the LinkedIn post the emphasis in the tag
# message is kept as written.
#
# Optional:
#   REDDIT_POST_FILE  where to write it (default reddit-post-<tag>.md).
#   SITE_URL          site link (default swarm-exec.cloud-surfers.net).
set -eu

: "${CI_COMMIT_TAG:?must run on a tag}"

# Never announce prereleases (v1.9.0-rc1, …).
case "$CI_COMMIT_TAG" in
	*-*) echo "prerelease $CI_COMMIT_TAG — skipping Reddit post." ; exit 0 ;;
esac

. "$(dirname "$0")/release-post-lib.sh"

SITE_URL="${SITE_URL:-https://swarm-exec.cloud-surfers.net}"
RELEASE_URL="$CI_PROJECT_URL/-/releases/$CI_COMMIT_TAG"
# Fixed path — the tag is already in the artifact URL, and a constant path makes
# the link for any release predictable. Its own directory keeps it clear of
# ./reddit-post.md, the hand-written working document in the repository root.
POST_FILE="${REDDIT_POST_FILE:-release-posts/reddit.md}"

notes=$(release_notes)
title=$(release_title)
[ -n "$title" ] || title="what changed in this release"

# Each link gets its own utm_content suffix. Without them three links collapse
# into one number, and "people clicked" cannot be told apart from "people wanted
# the docs" — which is the only part of the reaction worth acting on.
site_link=$(tracked_link "$SITE_URL" reddit)
docs_link=$(tracked_link "$SITE_URL/docs" reddit docs)
notes_link=$(tracked_link "$SITE_URL/releases" reddit notes)

post=$(cat <<EOF
swarmexec $CI_COMMIT_TAG — $title

$notes

**What it is.** One Go binary for Docker Swarm: cluster-wide \`docker exec\`,
logs, port-forward, volume and image management, and a terminal UI over the
whole cluster. Cluster state comes from the manager API; anything node-local
goes through a small agent running as a global service on each node, over mTLS.

**What it isn't.** No metrics backend, no web dashboard, no scheduler, no
Kubernetes. It needs manager API access **plus** the agent on every node — and
that agent mounts the Docker socket, so treat it as the privileged component it
is. The security report is a static spec audit, not runtime detection.

Apache-2.0, self-hosted, no telemetry in the binary. Prebuilt for Linux, macOS
and Windows.

* Site & docs (EN/DE/ES/FR/PL): $site_link
* Docs directly: $docs_link
* Release notes: $notes_link
* Source: https://gitlab.logle.io/cs-public/swarm-remote-exec
* Agent image: https://hub.docker.com/r/logleio/swarmexec-agent

Happy to answer anything — especially from people running Swarm at a size where
these views start to strain.
EOF
)

write_post "$POST_FILE" "$post"

cat <<EOF
Nothing was posted. Before this goes anywhere:

  * The FIRST line of $POST_FILE is the title; everything after the blank line
    is the body. Reddit has no title/body separator in a paste, so split it by
    hand in the composer.
  * Check the subreddit's self-promotion rule and set the flair it asks for.
    r/docker, r/selfhosted and r/devops each word theirs differently.
  * Cross-posting? Give each subreddit its own utm_content suffix, or the three
    arrive as one undifferentiated number.
  * Release notes are written for people who already use the tool. For a
    first-touch audience, lead with a concrete measurement from the notes
    instead of the feature list.
  * Post only if you can be around to answer replies. A maintainer in the
    comments does more than the post does.

Full notes: $RELEASE_URL
EOF
