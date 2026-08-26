#!/bin/sh
# Announce a swarmexec release on LinkedIn.
#
# Run by the `linkedin-announce` CI job on a stable semver tag. It builds an
# English post from the release notes (the annotated git-tag message) and posts
# it to LinkedIn via the REST Posts API.
#
# Required CI/CD variables (masked; set them in the project's Settings > CI/CD):
#   LINKEDIN_ACCESS_TOKEN  OAuth2 access token with the right scope:
#                          - a personal profile needs `w_member_social`
#                          - a company page needs `w_organization_social`
#                            (app added to the "Community Management API" product,
#                             token issued for a page admin).
#   LINKEDIN_AUTHOR_URN    who posts, e.g. urn:li:person:<id> or
#                          urn:li:organization:<id>.
# Optional:
#   LINKEDIN_API_VERSION   LinkedIn-Version header, YYYYMM (default 202401).
#   SITE_URL               link shown in the post (default swarm-exec.cloud-surfers.net).
#   LINKEDIN_DRY_RUN       when set to 1, compose and print the post + payload but
#                          do NOT post (use a manual/web pipeline run to preview).
#
# It exits 0 (skips) when the token/author is missing or the tag is a prerelease,
# so it never blocks a release; the job is also allow_failure so a LinkedIn
# outage cannot fail the pipeline.
set -eu

: "${CI_COMMIT_TAG:?must run on a tag}"

# Never announce prereleases (v1.9.0-rc1, …).
case "$CI_COMMIT_TAG" in
	*-*) echo "prerelease $CI_COMMIT_TAG — skipping LinkedIn announce." ; exit 0 ;;
esac

if [ -z "${LINKEDIN_ACCESS_TOKEN:-}" ] || [ -z "${LINKEDIN_AUTHOR_URN:-}" ]; then
	echo "LINKEDIN_ACCESS_TOKEN / LINKEDIN_AUTHOR_URN not set — skipping LinkedIn announce."
	echo "Add them as masked CI/CD variables to enable it (see ci/linkedin-announce.sh header)."
	exit 0
fi

API_VERSION="${LINKEDIN_API_VERSION:-202401}"
SITE_URL="${SITE_URL:-https://swarm-exec.cloud-surfers.net}"
RELEASE_URL="$CI_PROJECT_URL/-/releases/$CI_COMMIT_TAG"

# Release notes = the annotated tag message from the GitLab API (the git-tag
# body is the real changelog; the GitLab Release description is boilerplate).
tag_json=$(curl -sS --fail -H "JOB-TOKEN: $CI_JOB_TOKEN" \
	"$CI_API_V4_URL/projects/$CI_PROJECT_ID/repository/tags/$CI_COMMIT_TAG") || {
	echo "could not read tag $CI_COMMIT_TAG from the API" >&2; exit 1; }
message=$(printf '%s' "$tag_json" | jq -r '.message // ""')

# Drop the leading "swarmexec vX.Y.Z" title line (the headline restates it) and
# any blank lines that follow it.
notes=$(printf '%s\n' "$message" | awk '
	NR==1 && /^swarmexec /       {next}
	!started && /^[[:space:]]*$/ {next}
	{started=1; print}
')

# Compose the post. LinkedIn commentary allows ~3000 chars; keep headroom and,
# if we have to trim, point at the full notes.
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

payload=$(jq -n --arg author "$LINKEDIN_AUTHOR_URN" --arg text "$commentary" '{
	author: $author,
	commentary: $text,
	visibility: "PUBLIC",
	distribution: { feedDistribution: "MAIN_FEED", targetEntities: [], thirdPartyDistributionChannels: [] },
	lifecycleState: "PUBLISHED",
	isReshareDisabledByAuthor: false
}')

if [ "${LINKEDIN_DRY_RUN:-}" = "1" ]; then
	echo "── LinkedIn dry run — the post that WOULD be published: ──"
	printf '%s\n' "$commentary"
	echo "── payload ──"
	printf '%s\n' "$payload"
	exit 0
fi

echo "posting swarmexec $CI_COMMIT_TAG to LinkedIn as $LINKEDIN_AUTHOR_URN…"
code=$(curl -sS -o /tmp/li_resp.json -w '%{http_code}' -X POST \
	"https://api.linkedin.com/rest/posts" \
	-H "Authorization: Bearer $LINKEDIN_ACCESS_TOKEN" \
	-H "Content-Type: application/json" \
	-H "LinkedIn-Version: $API_VERSION" \
	-H "X-Restli-Protocol-Version: 2.0.0" \
	--data "$payload")

echo "LinkedIn HTTP $code"
cat /tmp/li_resp.json 2>/dev/null || true
echo
case "$code" in
	2*) echo "✓ posted to LinkedIn." ;;
	*)  echo "✗ LinkedIn post failed (HTTP $code)." >&2 ; exit 1 ;;
esac
