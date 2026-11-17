#!/bin/sh
# Per-file coverage floors (TST-02): fail when a file listed in
# ci/cover-floors.txt is covered below its floor, or is no longer in the
# profile at all.
#
# The overall gate (Makefile COVER_MIN) cannot see a core path sitting at 0 %
# while the rest of the tree is well covered; this one can. It reads the
# profile `go test -coverprofile` wrote (default coverage.out) and counts
# statements per file, the way `go tool cover` does: a block is covered when
# its count is above zero.
#
#   ci/cover-floors.sh [coverage.out [floors-file]]
#   ci/cover-floors.sh --report [coverage.out]   # print every file's value
#
# A floor is set a few points below what the file measures, to be raised as
# its tests grow and never lowered to make a change pass.
set -eu

report=0
if [ "${1:-}" = "--report" ]; then report=1; shift; fi
profile=${1:-coverage.out}
floors=${2:-ci/cover-floors.txt}

[ -f "$profile" ] || { echo "cover-floors: $profile not found (run make cover)" >&2; exit 2; }

# file<TAB>covered<TAB>total for every file in the profile.
per_file=$(awk -F'[: ,]' '
	NR == 1 && /^mode:/ { next }
	{
		# path:start,end numstmts count
		n = $(NF-1); c = $NF
		total[$1] += n
		if (c > 0) cov[$1] += n
	}
	END { for (f in total) printf "%s\t%d\t%d\n", f, cov[f], total[f] }
' "$profile")

if [ "$report" = 1 ]; then
	echo "$per_file" | awk -F"\t" '$3 > 0 { printf "%-60s %5.1f%%\n", $1, 100 * $2 / $3 }' | sort
	exit 0
fi

[ -f "$floors" ] || { echo "cover-floors: $floors not found" >&2; exit 2; }

status=0
while read -r file min _; do
	case "$file" in ''|'#'*) continue ;; esac
	line=$(echo "$per_file" | awk -F'\t' -v f="swarmexec/$file" '$1 == f { print $2 "\t" $3 }')
	if [ -z "$line" ]; then
		echo "cover-floors: $file is not in the profile (renamed, deleted or untested)" >&2
		status=1
		continue
	fi
	got=$(echo "$line" | awk -F'\t' '{ printf "%.1f", ($2 > 0) ? 100 * $1 / $2 : 100 }')
	if [ "$(echo "$got $min" | awk '{ print ($1 + 0 < $2 + 0) ? 1 : 0 }')" = 1 ]; then
		echo "cover-floors: $file is at $got %, below its floor of $min %" >&2
		status=1
	fi
done < "$floors"

if [ "$status" = 0 ]; then
	echo "per-file coverage floors hold ($(grep -cv '^#\|^$' "$floors") files)"
fi
exit $status
