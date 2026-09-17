# Shared helpers for building release announcements. POSIX sh, meant to be
# SOURCED (`. "$(dirname "$0")/release-post-lib.sh"`), not executed.
#
# The release notes and the tracked-link scheme are identical for every channel,
# and there is now more than one channel. Two copies of this would drift — and
# the way they would drift is silent: a post that quietly stops matching the
# release notes, or a link that stops being counted. One source, two callers.
#
# Needs from the CI environment: CI_COMMIT_TAG, CI_API_V4_URL, CI_PROJECT_ID,
# CI_JOB_TOKEN.

# release_notes prints the annotated tag message without its title line.
#
# The tag message is the real changelog — the GitLab Release description is
# boilerplate generated around it — so it is read from the API rather than from
# the checkout, which on a CI clone may not carry the annotation.
#
# The title line goes because every channel states the version in its own
# headline; repeating it reads as a formatting accident. Both conventions are
# recognised: the old "swarmexec vX.Y.Z …" and the current "vX.Y.Z — summary".
release_notes() {
	_tag_json=$(curl -sS --fail -H "JOB-TOKEN: $CI_JOB_TOKEN" \
		"$CI_API_V4_URL/projects/$CI_PROJECT_ID/repository/tags/$CI_COMMIT_TAG") || {
		echo "could not read tag $CI_COMMIT_TAG from the API" >&2; return 1; }

	printf '%s' "$_tag_json" | jq -r '.message // ""' | awk '
		NR==1 && /^swarmexec /               {next}
		NR==1 && /^v?[0-9]+\.[0-9]+\.[0-9]+/ {next}
		!started && /^[[:space:]]*$/         {next}
		{started=1; print}
	'
}

# release_title prints the tag message's first line without the leading version,
# i.e. the summary a human already wrote for this release. Channels that want a
# headline should use it rather than invent one: the tag message is where the
# release is described, and a second description drifts from the first.
# Empty when the tag message has no title line in either convention.
release_title() {
	curl -sS --fail -H "JOB-TOKEN: $CI_JOB_TOKEN" \
		"$CI_API_V4_URL/projects/$CI_PROJECT_ID/repository/tags/$CI_COMMIT_TAG" 2>/dev/null \
		| jq -r '.message // ""' | awk 'NR==1{
			sub(/^swarmexec[[:space:]]+/, "")
			if ($0 ~ /^v?[0-9]+\.[0-9]+\.[0-9]+/) {
				sub(/^v?[0-9]+\.[0-9]+\.[0-9]+[[:space:]]*(—|-|:)?[[:space:]]*/, "")
				print
			}
		}'
}

# strip_markdown removes the two emphasis markers used in tag messages, for
# channels that render no markup. Tag messages are ALSO the release notes on the
# site, where the markers do render, so they are stripped per channel rather
# than banned at the source.
strip_markdown() {
	sed -e 's/\*\*\([^*]*\)\*\*/\1/g' -e 's/`\([^`]*\)`/\1/g'
}

# tracked_link <url> <source> [content-suffix] appends Umami's utm_* parameters.
#
# Referrer alone undercounts social traffic badly — LinkedIn's in-app browser
# strips it, Reddit's app does too and old.reddit sends out.reddit.com — but
# utm_* travels in the URL itself, so the visit is still attributed.
#
# utm_content carries the version with dots as dashes (tidier in reports), plus
# an optional suffix naming WHICH link in the post was clicked. That suffix is
# the only way to tell "people want the docs" from "people want the pitch".
#
# Only our own site is tagged. GitLab and Docker Hub links stay bare: Umami does
# not measure them, so parameters there would be decoration that makes a link
# look tracked when it is not.
tracked_link() {
	_url=$1
	_src=$2
	_content=$(printf '%s' "$CI_COMMIT_TAG" | tr '.' '-')
	[ -n "${3:-}" ] && _content="${_content}-$3"
	_utm="utm_source=${_src}&utm_medium=social&utm_campaign=release&utm_content=${_content}"
	case "$_url" in
		*\?*) printf '%s&%s' "$_url" "$_utm" ;; # an overridden URL may carry a query
		*)    printf '%s?%s' "$_url" "$_utm" ;;
	esac
}

# write_post <file> <text> saves one composed post and prints it to the job log.
#
# Both, deliberately. The artifact is the durable copy a human opens to publish
# from; the log copy is what makes the post visible in the pipeline without
# downloading anything, and it survives the artifact's expiry.
write_post() {
	mkdir -p "$(dirname "$1")"
	printf '%s\n' "$2" > "$1"
	echo
	echo "── $1 ($(printf '%s' "$2" | wc -m | tr -d ' ') characters) ──"
	printf '%s\n' "$2"
	echo "── end ──"
	echo
}
