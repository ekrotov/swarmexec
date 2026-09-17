#!/bin/sh
# Build the LinkedIn post for a swarmexec release, and save it as a CI artifact.
#
# Run by the `linkedin-announce` CI job on a stable semver tag. It composes an
# English post from the release notes (the annotated git-tag message), writes it
# to $LINKEDIN_POST_FILE — kept as a job artifact and printed to the log — and
# then, if configured, also creates it on LinkedIn as a DRAFT via the REST Posts
# API. Pipeline composes, a human publishes → four-eyes.
#
# The ARTIFACT is the deliverable; the API call is a convenience. That is the
# lesson of v1.17.1: the API returned 201 and a share URN, and the draft could
# not be found anywhere in the LinkedIn Page admin UI, which does not list
# API-created drafts. Nothing that has to be published should depend on it.
# LINKEDIN_POST=0 builds the artifact and sends nothing.
#
# Auth — pick ONE:
#   Preferred (survives token expiry): a refresh-token flow. Set
#     LINKEDIN_CLIENT_ID, LINKEDIN_CLIENT_SECRET, LINKEDIN_REFRESH_TOKEN
#   and the job exchanges the refresh token for a fresh access token each run.
#   (LinkedIn refresh tokens last ~1 year and need the app enrolled for
#   programmatic refresh; renew by re-authorising once a year.)
#   Fallback: a static short-lived token, LINKEDIN_ACCESS_TOKEN (~60 days).
#
# Required to post (the artifact is built without any of it):
#   LINKEDIN_AUTHOR_URN    urn:li:organization:<id> (company page) or
#                          urn:li:person:<id>.
# Optional:
#   LINKEDIN_POST=0        build the artifact only, call no API.
#   LINKEDIN_POST_FILE     where to write the post (default
#                          linkedin-post-<tag>.txt, the artifact path in CI).
#   LINKEDIN_LIFECYCLE     DRAFT (default) or PUBLISHED (skip the human gate).
#   LINKEDIN_API_VERSION   LinkedIn-Version header, YYYYMM (default: this month).
#   SITE_URL               link in the post (default swarm-exec.cloud-surfers.net);
#                          utm_* tracking parameters are appended to it.
#   LINKEDIN_DRY_RUN=1     build the artifact and print the payload, send nothing.
#
# Scopes: w_organization_social (company page) or w_member_social (personal).
# The job is allow_failure, so neither a LinkedIn outage nor missing credentials
# can fail a release — and the artifact is written before either can happen.
set -eu

: "${CI_COMMIT_TAG:?must run on a tag}"

# Never announce prereleases (v1.9.0-rc1, …).
case "$CI_COMMIT_TAG" in
	*-*) echo "prerelease $CI_COMMIT_TAG — skipping LinkedIn announce." ; exit 0 ;;
esac

. "$(dirname "$0")/release-post-lib.sh"

SITE_URL="${SITE_URL:-https://swarm-exec.cloud-surfers.net}"
RELEASE_URL="$CI_PROJECT_URL/-/releases/$CI_COMMIT_TAG"
SITE_LINK=$(tracked_link "$SITE_URL" linkedin)
# A fixed path, not one carrying the tag: the tag is already in the artifact URL
# (/-/jobs/artifacts/<tag>/raw/release-posts/linkedin.txt), so the filename would
# only repeat it — and a constant path makes the link for any release
# predictable instead of something to look up.
#
# In its own directory because the repository root already holds hand-written
# marketing working documents with the obvious names; a job that writes
# ./reddit-post.md would overwrite one the first time someone runs it locally.
POST_FILE="${LINKEDIN_POST_FILE:-release-posts/linkedin.txt}"

# LinkedIn posts are PLAIN TEXT — the Posts API renders no markup at all, and
# tag messages use Markdown emphasis, which was going out literally: readers saw
# "**A security report over the whole cluster.**", asterisks and all.
notes=$(release_notes | strip_markdown)

commentary=$(printf '%s\n\n%s\n\nRelease notes: %s\nDocs & downloads: %s\n\n#DockerSwarm #Docker #DevOps #CLI #OpenSource' \
	"🚀 swarmexec $CI_COMMIT_TAG is out — cluster-wide docker exec, logs and volume management for Docker Swarm, from a single terminal." \
	"$notes" \
	"$RELEASE_URL" \
	"$SITE_LINK")

# LinkedIn refuses a commentary over 4000 characters outright, so a long tag
# message has to be cut down to a lead-in plus a link.
#
# The cut goes through jq, and that is not decoration. This used to be
# `cut -c1-$max`, which truncates EVERY LINE to that width rather than the text
# as a whole: on a multi-line message no line is anywhere near the limit, so it
# changed nothing and then appended the "read the rest" tail — making an
# over-long post LONGER. It was a silent no-op for every release until one
# finally exceeded 4000 and LinkedIn rejected it (v1.17.0, at 4193). jq's string
# slice is over the whole value and counts codepoints, which is also what
# LinkedIn counts — `cut`/`head -c` count bytes and would split a multibyte
# character, and this text is full of them (🚀 — ▶ · ✗).
# It also drops the final, half-finished paragraph rather than stopping
# mid-word: this is a public post, and "…as far as API 1.24 (Dock" reads as a
# mistake rather than as an excerpt. The trimmed version is only used if it
# still fills 60% of the budget, so a message written as one long paragraph
# keeps its allowance instead of being cut back to the headline.
#
# Done with split/join and NOT with rindex, deliberately: jq's string index
# functions report BYTE offsets while `.[a:b]` slices CODEPOINTS, so feeding one
# to the other overshoots by however many multibyte characters came before —
# verified at 14 characters on this very release's notes, which are full of
# them (🚀 — ▶ · ✗). It would have looked almost right, which is the worst way
# for it to be wrong.
max=2900
if [ "$(printf '%s' "$commentary" | jq -Rs 'length')" -gt "$max" ]; then
	commentary=$(printf '%s' "$commentary" | jq -Rrs --argjson n "$max" '
		.[0:$n] as $head
		| ($head | split("\n\n")) as $paras
		| (if ($paras | length) > 1 then ($paras[0:-1] | join("\n\n")) else $head end) as $whole
		| if ($whole | length) > ($n * 0.6) then $whole else $head end')
	commentary="$commentary
…
Full notes: $RELEASE_URL"
fi

# Save the post BEFORE anything is sent, and before the credential check — the
# text is the deliverable, the API call is only one way to use it.
#
# This order exists because the other one failed. The job used to compose the
# post in memory and hand it straight to LinkedIn as a DRAFT, on the assumption
# that a page admin would find it under Page > Drafts. For v1.17.1 the API
# returned 201 with a share URN and the draft was nowhere in the LinkedIn UI —
# the admin draft view does not list posts created through the API. A post that
# exists but cannot be found is not a four-eyes gate; it is a dead end.
#
# So the artifact is now the primary output: it is produced even when LinkedIn
# is unconfigured, unreachable or refuses the post, and a human publishes from
# it by hand. Whatever the API does afterwards is a bonus, not the product.
write_post "$POST_FILE" "$commentary"

have_refresh=
if [ -n "${LINKEDIN_CLIENT_ID:-}" ] && [ -n "${LINKEDIN_CLIENT_SECRET:-}" ] && [ -n "${LINKEDIN_REFRESH_TOKEN:-}" ]; then
	have_refresh=1
fi
if [ -z "${LINKEDIN_AUTHOR_URN:-}" ] || { [ -z "$have_refresh" ] && [ -z "${LINKEDIN_ACCESS_TOKEN:-}" ]; }; then
	echo "LinkedIn not configured (need LINKEDIN_AUTHOR_URN plus either the refresh-token"
	echo "trio LINKEDIN_CLIENT_ID/SECRET/REFRESH_TOKEN or LINKEDIN_ACCESS_TOKEN)."
	echo "The post is in $POST_FILE — publish it by hand. See ci/linkedin-announce.sh."
	exit 0
fi

# Posting to the API is opt-OUT now (LINKEDIN_POST=0 to build the artifact only),
# since the draft it creates has proven hard to reach from the UI.
if [ "${LINKEDIN_POST:-1}" = "0" ]; then
	echo "LINKEDIN_POST=0 — artifact only, nothing sent."
	exit 0
fi

# LinkedIn-Version is a YYYYMM stamp and only a narrow window of them stays
# active — a pinned default rots (the old 202401 did, and even a three-month-old
# 202506 was rejected with 426 NONEXISTENT_VERSION). Track the current month, so
# the job keeps working without anyone maintaining a constant. Right after a
# month rolls over the new stamp may not be live yet; the post retries once with
# the previous month in that case. Override with LINKEDIN_API_VERSION if needed.
API_VERSION="${LINKEDIN_API_VERSION:-$(date -u +%Y%m)}"
# Previous month, computed arithmetically: this job runs on alpine, whose busybox
# date supports neither GNU's -d "… -1 month" nor BSD's -v-1m.
_y=$(date -u +%Y)
_m=$(date -u +%m)
_m=${_m#0} # 09 -> 9, so it isn't read as octal
if [ "$_m" = "1" ]; then
	API_VERSION_PREV=$(printf '%04d12' $((_y - 1)))
else
	API_VERSION_PREV=$(printf '%04d%02d' "$_y" $((_m - 1)))
fi
LIFECYCLE="${LINKEDIN_LIFECYCLE:-DRAFT}"

# Mint a fresh access token from the refresh token when available; otherwise use
# the static one. (Not needed for a dry run.)
access_token="${LINKEDIN_ACCESS_TOKEN:-}"
if [ -n "$have_refresh" ] && [ "${LINKEDIN_DRY_RUN:-}" != "1" ]; then
	echo "exchanging refresh token for a fresh access token…"
	tok_resp=$(curl -sS --fail -X POST "https://www.linkedin.com/oauth/v2/accessToken" \
		--data-urlencode "grant_type=refresh_token" \
		--data-urlencode "refresh_token=$LINKEDIN_REFRESH_TOKEN" \
		--data-urlencode "client_id=$LINKEDIN_CLIENT_ID" \
		--data-urlencode "client_secret=$LINKEDIN_CLIENT_SECRET") || {
		echo "refresh-token exchange failed" >&2; exit 1; }
	access_token=$(printf '%s' "$tok_resp" | jq -r '.access_token // empty')
	[ -n "$access_token" ] || { echo "no access_token in refresh response" >&2; exit 1; }
fi

payload=$(jq -n --arg author "$LINKEDIN_AUTHOR_URN" --arg text "$commentary" --arg life "$LIFECYCLE" '{
	author: $author,
	commentary: $text,
	visibility: "PUBLIC",
	distribution: { feedDistribution: "MAIN_FEED", targetEntities: [], thirdPartyDistributionChannels: [] },
	lifecycleState: $life,
	isReshareDisabledByAuthor: false
}')

if [ "${LINKEDIN_DRY_RUN:-}" = "1" ]; then
	echo "── LinkedIn dry run ($LIFECYCLE) — payload that WOULD be sent ──"
	printf '%s\n' "$payload" # the post itself was printed and saved above
	exit 0
fi

post_with_version() {
	curl -sS -D /tmp/li_hdr -o /tmp/li_resp.json -w '%{http_code}' -X POST \
		"https://api.linkedin.com/rest/posts" \
		-H "Authorization: Bearer $access_token" \
		-H "Content-Type: application/json" \
		-H "LinkedIn-Version: $1" \
		-H "X-Restli-Protocol-Version: 2.0.0" \
		--data "$payload"
}

echo "creating swarmexec $CI_COMMIT_TAG on LinkedIn ($LIFECYCLE) as $LINKEDIN_AUTHOR_URN (version $API_VERSION)…"
code=$(post_with_version "$API_VERSION")

# 426 NONEXISTENT_VERSION: this month's stamp isn't live yet — fall back once.
if [ "$code" = "426" ] && [ -n "$API_VERSION_PREV" ] && [ "$API_VERSION_PREV" != "$API_VERSION" ] \
	&& grep -q NONEXISTENT_VERSION /tmp/li_resp.json 2>/dev/null; then
	echo "version $API_VERSION not active — retrying with $API_VERSION_PREV"
	code=$(post_with_version "$API_VERSION_PREV")
fi

echo "LinkedIn HTTP $code"
post_id=$(awk 'tolower($1)=="x-restli-id:"{print $2}' /tmp/li_hdr 2>/dev/null | tr -d '\r')
cat /tmp/li_resp.json 2>/dev/null || true
echo
case "$code" in
	2*)
		echo "✓ LinkedIn $LIFECYCLE created${post_id:+ ($post_id)}."
		if [ "$LIFECYCLE" = "DRAFT" ]; then
			# Deliberately not "find it under Page > Drafts": for v1.17.1 it was
			# not there. The API confirms the draft exists; the admin UI does not
			# list API-created ones. Point at the copy that can actually be
			# opened instead of at a view that may be empty.
			echo "  → a DRAFT now exists on LinkedIn, but the Page admin UI does not"
			echo "    reliably list drafts created through the API. Publish from the"
			echo "    artifact $POST_FILE instead (four-eyes)."
		fi
		;;
	*)
		echo "✗ LinkedIn post failed (HTTP $code) — the post itself is fine." >&2
		echo "  Publish $POST_FILE by hand; the artifact is kept regardless." >&2
		exit 1
		;;
esac
