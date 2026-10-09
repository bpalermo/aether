#!/usr/bin/env bash
# What //bazel/buildid:release_build_ids cannot say about itself (#1427).
#
# That target checks the GNU build-ID of every binary it finds under the images
# the release pushes. It finds them by walking named attributes of rules_helm
# and rules_img, so "it found them all" is a claim, and a rule upgrade that
# renames an attribute would drop binaries from the check without failing it.
# This test holds the claim to two things that do not come from that walk:
#
#   1. `bazel query` (the genquery next to it) for the `content_build_id`
#      targets under the same roots. For each platform an image is published
#      for, the report must name exactly those targets;
#   2. publish.yaml: the roots are the `<chart>.push_images` targets it runs.
#
# Usage: release_build_ids_test.sh <report> <query output> <roots> <publish.yaml>
set -uo pipefail

REPORT="$1"
QUERY="$2"
ROOTS="$3"
PUBLISH="$4"
for f in "$REPORT" "$QUERY" "$ROOTS" "$PUBLISH"; do
	[ -s "$f" ] || {
		echo "FAIL: $f is missing or empty"
		exit 1
	}
done

# The platforms every image index is built for (bazel/img/go_multi_arch_image.bzl).
PLATFORMS="linux/amd64 linux/arm64"

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}
same() { # what, got, want
	if [ "$2" = "$3" ]; then
		pass "$1"
	else
		fail "$1"
		diff <(printf '%s\n' "$3") <(printf '%s\n' "$2") | sed -n 's/^< /    not there, expected: /p; s/^> /    there, not expected: /p'
	fi
}

# A line of the report: `<os>/<arch> <content_build_id target> (<binary>): <id>`.
malformed="$(grep -cvE '^[a-z0-9]+/[a-z0-9]+ //[^ ]+ \(//[^ ]+\): [0-9a-f]{40}$' "$REPORT")"
same "every line of the report is '<platform> <target> (<binary>): <40 hex digits>'" "$malformed" 0

same "the report covers the platforms the images are published for, and no other" \
	"$(cut -d' ' -f1 "$REPORT" | sort -u | tr '\n' ' ' | sed 's/ $//')" "$PLATFORMS"

expected="$(sort -u "$QUERY")"
[ "$(wc -l <<<"$expected")" -ge 2 ] || fail "the query found fewer than two content_build_id targets under the release"
for platform in $PLATFORMS; do
	same "$platform: the report names every content_build_id target under the release, once" \
		"$(awk -v p="$platform" '$1 == p { print $2 }' "$REPORT" | sort)" "$expected"
done

same "no build ID appears twice in the report" "$(awk '{ print $NF }' "$REPORT" | sort | uniq -d | wc -l)" 0

# shellcheck disable=SC2016 # the pattern is sed's, not the shell's
pushed="$(sed -n 's/^[[:space:]]*bazel_run \(\/\/[^[:space:]]*\.push_images\)[[:space:]]*$/\1/p' "$PUBLISH" | sort)"
[ -n "$pushed" ] || fail "publish.yaml runs no <chart>.push_images target (did its layout change?)"
same "the roots of the check are the push_images targets publish.yaml runs" "$(grep -v '^$' "$ROOTS" | sort)" "$pushed"

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
