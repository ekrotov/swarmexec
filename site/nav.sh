#!/bin/sh
# The top bar (<header class="rail">) is the same on every page of the site.
# It lives once, in partials/nav.html; each page carries a verbatim copy, with
# one difference: the item for the page itself gets aria-current.
#
#   sh site/nav.sh check   fail if any page's top bar differs from the partial
#                          (CI: the site-check job)
#   sh site/nav.sh write   rewrite every page's top bar from the partial
#
# To change the nav: edit partials/nav.html (and nav.css), run `write`, commit.
# A new page: add it to the table below, give it a <header class="rail"> block
# (any content; `write` replaces it) and link nav.css.
#
# POSIX sh + busybox tools only, so it runs in the plain alpine CI image.
set -eu
cd "$(dirname "$0")"

PARTIAL=partials/nav.html
mode=${1:-check}
case "$mode" in check|write) ;; *) echo "usage: $0 check|write" >&2; exit 2 ;; esac

# page  href-of-the-current-item  aria-current-value   ("-" = no current item)
# A docs topic page is inside Docs, not Docs itself: aria-current="true".
pages() {
	cat <<'EOF'
index.html / page
docs.html /docs page
examples.html /examples page
compare.html /docker-swarm-exec-alternatives page
releases.html /releases page
datenschutz.html - -
datenschutz.en.html - -
EOF
	for f in docs/*.html; do echo "$f /docs true"; done
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
pages > "$tmp/pages"

fail=0

# Every served page must be in the table (proposals/ is not served).
for f in *.html docs/*.html; do
	if ! awk -v f="$f" '$1 == f { found = 1 } END { exit !found }' "$tmp/pages"; then
		echo "nav.sh: $f is not in the page table of site/nav.sh" >&2
		fail=1
	fi
done

while read -r page href cur; do
	if [ ! -f "$page" ]; then
		echo "nav.sh: $page is in the table but does not exist" >&2
		fail=1; continue
	fi
	# The expected top bar of this page.
	if [ "$href" = "-" ]; then
		cp "$PARTIAL" "$tmp/want"
	else
		sed "s|href=\"$href\"|& aria-current=\"$cur\"|" "$PARTIAL" > "$tmp/want"
		if ! grep -q "aria-current=\"$cur\"" "$tmp/want"; then
			echo "nav.sh: $page: no item with href=\"$href\" in $PARTIAL" >&2
			fail=1; continue
		fi
	fi
	n=$(grep -c '<header class="rail">' "$page" || true)
	if [ "$n" != 1 ]; then
		echo "nav.sh: $page: expected exactly one <header class=\"rail\">, found $n" >&2
		fail=1; continue
	fi
	if [ "$mode" = write ]; then
		awk -v want="$tmp/want" '
			/<header class="rail">/ { while ((getline l < want) > 0) print l; skip = 1; next }
			skip && /<\/header>/   { skip = 0; next }
			!skip                  { print }
		' "$page" > "$tmp/new"
		if ! cmp -s "$tmp/new" "$page"; then
			cat "$tmp/new" > "$page"
			echo "nav.sh: updated $page"
		fi
	else
		awk '/<header class="rail">/ { p = 1 } p { print } p && /<\/header>/ { exit }' "$page" > "$tmp/got"
		if ! diff -u "$tmp/want" "$tmp/got" > "$tmp/diff"; then
			echo "nav.sh: $page: top bar differs from $PARTIAL (run: sh site/nav.sh write)" >&2
			cat "$tmp/diff" >&2
			fail=1
		fi
		# Every page loads the one stylesheet the top bar is styled by.
		if ! grep -q 'href="/nav.css' "$page"; then
			echo "nav.sh: $page does not link /nav.css" >&2
			fail=1
		fi
	fi
done < "$tmp/pages"

[ "$fail" = 0 ] && [ "$mode" = check ] && echo "nav.sh: top bar identical on $(wc -l < "$tmp/pages") pages"
exit "$fail"
