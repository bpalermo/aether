#!/usr/bin/env bash
# Exercise the child-manifest walk (ghcr__json_children) over every shape it
# must accept or refuse (#925).
#
# The walk is what extends signature checking from a multi-arch index to the
# per-architecture manifests a node actually pulls. Its dangerous failure is not
# a crash but an EMPTY answer: a walk that yields zero children makes "verify
# every child" a loop over nothing, which passes -- the index-only gap one level
# down (#853). So every shape that is not "an index with well-formed children"
# must fail, not print nothing and succeed.
#
# No registry access: the documents are literals, shaped like what ghcr.io
# returns for our rules_img indexes (and a buildx one, whose attestation
# manifests must be walked too, not filtered out).
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
# shellcheck disable=SC1091
. scripts/ghcr-lib.sh

a="sha256:$(printf 'a%.0s' {1..64})"
b="sha256:$(printf 'b%.0s' {1..64})"
c="sha256:$(printf 'c%.0s' {1..64})"
fail=0
n=0

# want_children <name> <expected newline-joined digests> <json>
want_children() {
	local name="$1" want="$2" doc="$3" got
	n=$((n + 1))
	if got="$(printf '%s' "$doc" | ghcr__json_children)" && [ "$got" = "$want" ]; then
		printf '  ok    %s\n' "$name"
	else
		printf '  FAIL  %s: want [%s] got [%s]\n' "$name" "$want" "$got"
		fail=1
	fi
}

# want_refused <name> <json>
want_refused() {
	local name="$1" doc="$2" got
	n=$((n + 1))
	if got="$(printf '%s' "$doc" | ghcr__json_children 2>/dev/null)"; then
		printf '  FAIL  %s: accepted, printed [%s]\n' "$name" "$got"
		fail=1
	else
		printf '  ok    %s (refused)\n' "$name"
	fi
}

want_children "rules_img two-platform index" "$a"$'\n'"$b" \
	"{\"mediaType\":\"application/vnd.oci.image.index.v1+json\",\"manifests\":[{\"digest\":\"$a\",\"platform\":{\"os\":\"linux\",\"architecture\":\"amd64\"}},{\"digest\":\"$b\",\"platform\":{\"os\":\"linux\",\"architecture\":\"arm64\"}}]}"
want_children "buildx index with an unknown/unknown attestation" "$a"$'\n'"$b"$'\n'"$c" \
	"{\"manifests\":[{\"digest\":\"$a\",\"platform\":{\"os\":\"linux\",\"architecture\":\"amd64\"}},{\"digest\":\"$b\",\"platform\":{\"os\":\"linux\",\"architecture\":\"arm64\"}},{\"digest\":\"$c\",\"platform\":{\"os\":\"unknown\",\"architecture\":\"unknown\"}}]}"
want_refused "single-arch image manifest (no manifests key)" \
	"{\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"layers\":[]}"
want_refused "index with zero children" '{"manifests":[]}'
want_refused "child without a digest" "{\"manifests\":[{\"digest\":\"$a\"},{\"platform\":{}}]}"
want_refused "child with a malformed digest" '{"manifests":[{"digest":"sha256:abc"}]}'
want_refused "not JSON" 'not json'

if [ "$n" -ne 7 ]; then
	echo "::error::ran ${n} cases, expected 7 -- a gate that checks nothing passes" >&2
	exit 2
fi
if [ "$fail" -ne 0 ]; then
	echo "::error::child-manifest enumeration is wrong; see cases above" >&2
	exit 1
fi
echo "index children: ${n}/${n} cases correct"
