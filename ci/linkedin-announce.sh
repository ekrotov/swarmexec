#!/bin/sh
# Announce a swarmexec release on LinkedIn as a DRAFT (four-eyes).
#
# Run by the `linkedin-announce` CI job on a stable semver tag. It builds an
# English post from the release notes (the annotated git-tag message) and
# creates it on LinkedIn as a DRAFT via the REST Posts API. The draft is NOT
# public: a page admin reviews it in LinkedIn (Page > Drafts) and clicks
# Publish. Pipeline authors, a human publishes → four-eyes.
#
# Auth — pick ONE:
#   Preferred (survives token expiry): a refresh-token flow. Set
#     LINKEDIN_CLIENT_ID, LINKEDIN_CLIENT_SECRET, LINKEDIN_REFRESH_TOKEN
#   and the job exchanges the refresh token for a fresh access token each run.
#   (LinkedIn refresh tokens last ~1 year and need the app enrolled for
#   programmatic refresh; renew by re-authorising once a year.)
#   Fallback: a static short-lived token, LINKEDIN_ACCESS_TOKEN (~60 days).
#
# Also required:
#   LINKEDIN_AUTHOR_URN    urn:li:organization:<id> (company page — recommended,
#                          drafts are first-class there) or urn:li:person:<id>.
# Optional:
#   LINKEDIN_LIFECYCLE     DRAFT (default) or PUBLISHED (skip the human gate).
#   LINKEDIN_API_VERSION   LinkedIn-Version header, YYYYMM (default 202401).
#   SITE_URL               link in the post (default swarm-exec.cloud-surfers.net).
#   LINKEDIN_DRY_RUN=1     compose and print the post + payload, do NOT call the API.
#
# Scopes: w_organization_social (company page) or w_member_social (personal).
# The job is opt-in (skips when unconfigured) and allow_failure, so it never
# blocks a release.
set -eu

: "${CI_COMMIT_TAG:?must run on a tag}"

# Never announce prereleases (v1.9.0-rc1, …).
case "$CI_COMMIT_TAG" in
	*-*) echo "prerelease $CI_COMMIT_TAG — skipping LinkedIn announce." ; exit 0 ;;
esac

have_refresh=
if [ -n "${LINKEDIN_CLIENT_ID:-}" ] && [ -n "${LINKEDIN_CLIENT_SECRET:-}" ] && [ -n "${LINKEDIN_REFRESH_TOKEN:-}" ]; then
	have_refresh=1
fi
if [ -z "${LINKEDIN_AUTHOR_URN:-}" ] || { [ -z "$have_refresh" ] && [ -z "${LINKEDIN_ACCESS_TOKEN:-}" ]; }; then
	echo "LinkedIn not configured (need LINKEDIN_AUTHOR_URN plus either the refresh-token"
	echo "trio LINKEDIN_CLIENT_ID/SECRET/REFRESH_TOKEN or LINKEDIN_ACCESS_TOKEN) — skipping."
	echo "See ci/linkedin-announce.sh for setup."
	exit 0
fi

API_VERSION="${LINKEDIN_API_VERSION:-202401}"
LIFECYCLE="${LINKEDIN_LIFECYCLE:-DRAFT}"
SITE_URL="${SITE_URL:-https://swarm-exec.cloud-surfers.net}"
RELEASE_URL="$CI_PROJECT_URL/-/releases/$CI_COMMIT_TAG"

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

# Release notes = the annotated tag message from the GitLab API (the git-tag body
# is the real changelog; the GitLab Release description is boilerplate).
tag_json=$(curl -sS --fail -H "JOB-TOKEN: $CI_JOB_TOKEN" \
	"$CI_API_V4_URL/projects/$CI_PROJECT_ID/repository/tags/$CI_COMMIT_TAG") || {
	echo "could not read tag $CI_COMMIT_TAG from the API" >&2; exit 1; }
message=$(printf '%s' "$tag_json" | jq -r '.message // ""')

# Drop the leading "swarmexec vX.Y.Z" title line (the headline restates it) and
# the blank lines that follow it.
notes=$(printf '%s\n' "$message" | awk '
	NR==1 && /^swarmexec /       {next}
	!started && /^[[:space:]]*$/ {next}
	{started=1; print}
')

commentary=$(printf '%s\n\n%s\n\nRelease notes: %s\nDocs & downloads: %s\n\n#DockerSwarm #Docker #DevOps #CLI #OpenSource' \
	"🚀 swarmexec $CI_COMMIT_TAG is out — cluster-wide docker exec, logs and volume management for Docker Swarm, from a single terminal." \
	"$notes" \
	"$RELEASE_URL" \
	"$SITE_URL")

max=2900
if [ "$(printf '%s' "$commentary" | wc -m)" -gt "$max" ]; then
	commentary=$(printf '%s' "$commentary" | cut -c1-"$max")
	commentary="$commentary
…
Full notes: $RELEASE_URL"
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
	echo "── LinkedIn dry run ($LIFECYCLE) — the post that WOULD be created: ──"
	printf '%s\n' "$commentary"
	echo "── payload ──"
	printf '%s\n' "$payload"
	exit 0
fi

echo "creating swarmexec $CI_COMMIT_TAG on LinkedIn ($LIFECYCLE) as $LINKEDIN_AUTHOR_URN…"
code=$(curl -sS -D /tmp/li_hdr -o /tmp/li_resp.json -w '%{http_code}' -X POST \
	"https://api.linkedin.com/rest/posts" \
	-H "Authorization: Bearer $access_token" \
	-H "Content-Type: application/json" \
	-H "LinkedIn-Version: $API_VERSION" \
	-H "X-Restli-Protocol-Version: 2.0.0" \
	--data "$payload")

echo "LinkedIn HTTP $code"
post_id=$(awk 'tolower($1)=="x-restli-id:"{print $2}' /tmp/li_hdr 2>/dev/null | tr -d '\r')
cat /tmp/li_resp.json 2>/dev/null || true
echo
case "$code" in
	2*)
		echo "✓ LinkedIn $LIFECYCLE created${post_id:+ ($post_id)}."
		if [ "$LIFECYCLE" = "DRAFT" ]; then
			echo "  → review & publish it from the LinkedIn Page > Drafts (four-eyes)."
		fi
		;;
	*)  echo "✗ LinkedIn post failed (HTTP $code)." >&2 ; exit 1 ;;
esac
