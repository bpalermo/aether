#!/usr/bin/env bash
# Exercise the child-manifest walk (registry__json_children) over every shape it
# must accept or refuse (#925).
#
# The walk is what extends signature checking from a multi-arch index to the
# per-architecture manifests a node actually pulls. Its dangerous failure is not
# a crash but an EMPTY answer: a walk that yields zero children makes "verify
# every child" a loop over nothing, which passes -- the index-only gap one level
# down (#853). So every shape that is not "an index with well-formed children"
# must fail, not print nothing and succeed.
#
# The walk also has to happen on the RIGHT registry (proposal 040 phase 2): the
# sweep checks a pre-cut-over commit's children on ghcr.io and a post-cut-over
# commit's on quay.io, in one run, by setting REGISTRY_HOST per commit. The last
# cases walk the same index through a fake `curl` that serves it on ONE host
# only: asked on that host the walk yields the children, asked on the other it
# yields nothing and FAILS -- a child walk that silently went to the other
# registry must never read as "no children to check".
#
# No registry access: the documents are literals, shaped like what an OCI registry
# returns for our rules_img indexes (and a buildx one, whose attestation
# manifests must be walked too, not filtered out).
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
# shellcheck disable=SC1091
. scripts/registry-lib.sh

a="sha256:$(printf 'a%.0s' {1..64})"
b="sha256:$(printf 'b%.0s' {1..64})"
c="sha256:$(printf 'c%.0s' {1..64})"
fail=0
n=0

# want_children <name> <expected newline-joined digests> <json>
want_children() {
	local name="$1" want="$2" doc="$3" got
	n=$((n + 1))
	if got="$(printf '%s' "$doc" | registry__json_children)" && [ "$got" = "$want" ]; then
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
	if got="$(printf '%s' "$doc" | registry__json_children 2>/dev/null)"; then
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

# --- the walk goes to the registry REGISTRY_HOST names ---------------------
index_doc="{\"mediaType\":\"application/vnd.oci.image.index.v1+json\",\"manifests\":[{\"digest\":\"$a\"},{\"digest\":\"$b\"}]}"
asked="$(mktemp)"
trap 'rm -f "$asked"' EXIT
# A fake curl: records the URL, serves the index only on $SERVED_ON.
curl() {
	local url="" x
	for x in "$@"; do
		case "$x" in https://*) url="$x" ;; esac
	done
	printf '%s\n' "$url" >>"$asked"
	case "$url" in
	"https://${SERVED_ON}/v2/"*"/manifests/sha256:"*) printf '%s' "$index_doc" ;;
	*) return 22 ;;
	esac
}

# walk_on <name> <REGISTRY_HOST> <index served on> <want children | refused>
walk_on() {
	local name="$1" host="$2" want="$4" got rc=0
	: >"$asked"
	got="$(SERVED_ON="$3" REGISTRY_HOST="$host" registry_index_children some/repo "$c" tok 2>/dev/null)" || rc=$?
	n=$((n + 1))
	if [ "$want" = refused ]; then
		if [ "$rc" != 0 ] && [ -z "$got" ]; then
			printf '  ok    %s (refused)\n' "$name"
		else
			printf '  FAIL  %s: accepted [%s]\n' "$name" "$got"
			fail=1
		fi
	elif [ "$rc" = 0 ] && [ "$got" = "$want" ] && grep -qxF "https://${host}/v2/some/repo/manifests/${c}" "$asked"; then
		printf '  ok    %s\n' "$name"
	else
		printf '  FAIL  %s: rc %s got [%s], asked [%s]\n' "$name" "$rc" "$got" "$(cat "$asked")"
		fail=1
	fi
}
walk_on "pre-cut-over commit: the walk asks ghcr.io" ghcr.io ghcr.io "$a"$'\n'"$b"
walk_on "post-cut-over commit: the walk asks quay.io" quay.io quay.io "$a"$'\n'"$b"
walk_on "post-cut-over commit whose index is only on ghcr.io: no children, FAILS" quay.io ghcr.io refused
unset -f curl

if [ "$n" -ne 10 ]; then
	echo "::error::ran ${n} cases, expected 10 -- a gate that checks nothing passes" >&2
	exit 2
fi
if [ "$fail" -ne 0 ]; then
	echo "::error::child-manifest enumeration is wrong; see cases above" >&2
	exit 1
fi
echo "index children: ${n}/${n} cases correct"
