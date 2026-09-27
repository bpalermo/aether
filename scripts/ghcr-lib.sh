#!/usr/bin/env bash
# Shared, read-only GHCR helpers.
#
# This file is SOURCED, never executed: `. scripts/ghcr-lib.sh`. It is sourced by
#   - .github/workflows/publish.yaml   (the sign step, which resolves the digest
#     it is about to sign)
#   - scripts/verify-published-artifacts.sh  (#880, which asserts those artifacts
#     actually landed)
#   - scripts/verify-image-signatures.sh  (#925, which cosign-verifies each index
#     AND every child manifest it lists)
#
# One copy on purpose. The pagination below was written for the sign step after
# it failed on e3b58e6 by reading 100 of 667 tags (#875); a second, subtly
# different copy in the verifier would be free to regress the same way while the
# original stayed correct — and a verifier that cannot see a tag reports a
# publish that happened as missing. (The verifier no longer lists at all: it
# asks for each tag it expects by name, ghcr_tag_exists — #985.)
#
# Everything here is GET/HEAD only. Nothing in this file can push, tag, delete or
# otherwise mutate the registry: publishing is the release workflow's job alone.

# Bearer token for pulling from one GHCR repository.
#
# GHCR hands out an anonymous pull token for public packages, which is what lets
# the verifier run from a workstation with no credentials at all. Set GHCR_TOKEN
# (a GITHUB_TOKEN or a PAT with read:packages) to read private ones.
ghcr_registry_token() {
	local repo="$1"
	local url="https://ghcr.io/token?scope=repository:${repo}:pull&service=ghcr.io"
	if [ -n "${GHCR_TOKEN:-}" ]; then
		curl -fsS -u "x:${GHCR_TOKEN}" "$url" | ghcr__json_str token
	else
		curl -fsS "$url" | ghcr__json_str token
	fi
}

# List EVERY tag in a repo, following the registry's pagination.
#
# /v2/<repo>/tags/list returns ONE PAGE and signals the rest with an RFC5988
# `Link: <...>; rel="next"` header. Tags come back in ascending order, so a tag
# just pushed sorts LAST and is in the final page, never the first. ghcr honours
# `n` up to some cap and silently returns fewer; whether a given repo answers in
# one page or ten is not something a caller may assume, which is why this loop
# exists rather than a single request with a large `n`.
#
# That is exactly how the sign step failed on e3b58e6: the tag existed and the
# lookup could not see it, so a correct step reported the images unpublished. A
# lookup that silently examines 15% of its input is the #853 shape.
#
# `last=` is the cursor; ghcr's own next-URL carries `n=0`, so re-state a real
# page size rather than following that URL verbatim.
#
# Even a complete walk is not a consistent snapshot: tags written while the
# pages are read can shift a page boundary past an existing tag (#985). Use this
# only to DISCOVER tags whose names you do not know; to ask whether a known tag
# exists, use ghcr_tag_exists below.
#
# Usage: ghcr_all_tags <repo> <token>   -> one tag per line on stdout
ghcr_all_tags() {
	local repo="$1" tok="$2" last="" hdr body url
	hdr="$(mktemp)"
	while :; do
		url="https://ghcr.io/v2/${repo}/tags/list?n=1000"
		[ -n "$last" ] && url="${url}&last=${last}"
		body="$(curl -fsS -D "$hdr" -H "Authorization: Bearer $tok" "$url")"
		printf '%s' "$body" | ghcr__json_tags
		# No rel="next" means this was the final page.
		grep -qi '^link:.*rel="next"' "$hdr" || break
		last="$(printf '%s' "$body" | ghcr__json_tags | tail -1)"
		[ -n "$last" ] || break
	done
	rm -f "$hdr"
}

# The manifest media types a published coordinate can carry: a multi-arch index
# (OCI or Docker) or a single manifest (OCI or Docker) — which covers a Helm
# chart, a cosign 2 `.sig` and a cosign 3 fallback index alike. A HEAD whose
# Accept matches none of them may be refused for a manifest that exists.
GHCR_MANIFEST_ACCEPT='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'

# Does ONE tag exist? `HEAD /v2/<repo>/manifests/<tag>` (#985).
#
# Anything that knows the tag it expects looks it up this way — never by listing
# every tag and grepping. A listing is paged, and a publish writing tags while
# the pages are read can move the page boundary so an existing tag falls
# between two pages and is never seen: the 2026-09-27 sweep reported
# cni-install's d526bf2 signature MISSING while it was there, and a re-run
# minutes later passed (#985). A direct lookup has no pages to fall between, and
# costs O(expected tags) instead of O(every tag the repository ever had).
#
# Three answers, never two:
#   0  present  (200)
#   1  absent   (404: the registry's own answer — no such manifest)
#   2  unknown  (anything else: 401/403/429/5xx, a timeout, no connection; the
#               code goes to stderr). An unanswered lookup is NEVER "absent" —
#               that turns a registry hiccup into a false alarm — and never
#               "present", which turns an outage into a pass.
#
# Usage: ghcr_tag_exists <repo> <tag> <token>
ghcr_tag_exists() {
	local repo="$1" tag="$2" tok="$3" code
	code="$(curl -sS --retry 2 -o /dev/null -w '%{http_code}' -I \
		-H "Authorization: Bearer $tok" -H "Accept: ${GHCR_MANIFEST_ACCEPT}" \
		"https://ghcr.io/v2/${repo}/manifests/${tag}")" || true
	case "$code" in
	200) return 0 ;;
	404) return 1 ;;
	*)
		echo "ghcr_tag_exists: HEAD ghcr.io/v2/${repo}/manifests/${tag} answered '${code:-nothing}'" >&2
		return 2
		;;
	esac
}

# ONE tag that exists in <repo>: the first entry of a single listing page. Never
# used to decide whether an expected tag is there — that is ghcr_tag_exists'
# job — but as the WITNESS behind a 404 (#985): if a tag this listing just named
# also answers 200 to ghcr_tag_exists, the repository is readable and the lookup
# can say "present", so the 404 beside it is a real absence. Without it, a lookup
# that 404s everything (ghcr does exactly that to a manifest HEAD whose Accept it
# does not like) would report a complete publish as MISSING. Prints nothing when
# the repository cannot be listed or lists no tags.
#
# Usage: ghcr_any_tag <repo> <token>  -> one tag on stdout
ghcr_any_tag() {
	local repo="$1" tok="$2"
	curl -fsS -H "Authorization: Bearer $tok" \
		"https://ghcr.io/v2/${repo}/tags/list?n=1" | ghcr__json_tags | head -1
}

# Resolve a tag to the digest the registry itself reports for it.
#
# HEAD + Docker-Content-Digest rather than sha256sum of a GET body: the digest
# must be the registry's own answer for the media type we asked for, and for a
# multi-arch image that is the INDEX digest — the thing cosign signs.
#
# Usage: ghcr_manifest_digest <repo> <tag> <token>  -> `sha256:...` on stdout
ghcr_manifest_digest() {
	local repo="$1" tag="$2" tok="$3"
	curl -fsSI -H "Authorization: Bearer $tok" \
		-H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json' \
		"https://ghcr.io/v2/${repo}/manifests/${tag}" |
		tr -d '\r' | sed -nE 's/^[Dd]ocker-[Cc]ontent-[Dd]igest:[[:space:]]*//p' | head -1
}

# Every child manifest digest of a multi-arch INDEX (#925).
#
# publish.yaml signs with `cosign sign --recursive`, which signs the index AND
# each child it lists. `cosign verify` has no `--recursive` (not in v2.4.1,
# v3.0.6 or v3.1.2), so a verifier that checks only the index digest never looks at the
# per-architecture manifests — which are exactly what a node pulls. Anything that
# checks signatures walks the children with this and checks each one.
#
# Prints EVERY entry of `.manifests[]`, not only the ones with a real platform.
# rules_img indexes carry just linux/amd64 + linux/arm64 today; a buildx index
# would also list `unknown/unknown` attestation manifests, and `--recursive`
# signs those too, so they are held to the same bar rather than filtered out.
#
# Fails (non-zero, nothing printed) when the digest is not an index or lists no
# children. A walk that silently yields zero children is the index-only gap one
# level down (#853), so "no children" must never be readable as "all children
# verified".
#
# Usage: ghcr_index_children <repo> <index digest> <token>  -> one digest per line
ghcr_index_children() {
	local repo="$1" digest="$2" tok="$3" body
	body="$(curl -fsS -H "Authorization: Bearer $tok" \
		-H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json' \
		"https://ghcr.io/v2/${repo}/manifests/${digest}")" || return 1
	printf '%s' "$body" | ghcr__json_children
}

# cosign's signature tag for a digest. There are TWO shapes, and which one you
# get depends on the cosign major that signed:
#
#   cosign 2.x            `sha256-abc….sig`   a plain signature manifest
#   cosign 3.x (default)  `sha256-abc…`       an OCI 1.1 referrers FALLBACK index
#
# ghcr.io does NOT implement the OCI 1.1 Referrers API — GET
# /v2/<repo>/referrers/<digest> 404s there while tags/list and manifests/ succeed
# with the same token. That is why cosign 3 writes the fallback TAG rather than a
# real referrer, and it is why the tag scheme is not a preference here: it is the
# only scheme that works on this registry, in both majors.
#
# Everything published before 2026-09-24 carries the legacy shape and must stay
# verifiable, so the verifier accepts either — but EXACTLY one. Both present for
# the same digest means a double-write or a half-finished migration, which
# presence-only checking reads as healthy. See ghcr_signature_layout().
ghcr_signature_tag_legacy() {
	printf '%s.sig\n' "${1/:/-}"
}

ghcr_signature_tag_bundle() {
	printf '%s\n' "${1/:/-}"
}

# Which layout is published for $1 (a digest), given $2 (the repo's tag list).
#
# Echoes `legacy`, `bundle`, `none`, or `both`. The caller decides what to do; the
# point of naming `both` separately is that it is a DIFFERENT defect from `none`
# and must not be reported as a healthy signature.
ghcr_signature_layout() {
	local digest="$1" tags="$2" has_legacy=0 has_bundle=0
	printf '%s\n' "$tags" | grep -qxF -- "$(ghcr_signature_tag_legacy "$digest")" && has_legacy=1
	printf '%s\n' "$tags" | grep -qxF -- "$(ghcr_signature_tag_bundle "$digest")" && has_bundle=1
	ghcr__layout "$has_legacy" "$has_bundle"
}

# The same answer from two direct lookups instead of a tag list (#985). BOTH
# tags are asked for by name — not "stop at the first hit" — so `both` is still
# seen.
#
# Returns 2 with nothing on stdout when either lookup goes unanswered: a layout
# decided on one answer out of two could call `both` `legacy`.
#
# Usage: ghcr_signature_layout_direct <repo> <digest> <token>
ghcr_signature_layout_direct() {
	local repo="$1" digest="$2" tok="$3" has_legacy has_bundle rc
	rc=0
	ghcr_tag_exists "$repo" "$(ghcr_signature_tag_legacy "$digest")" "$tok" || rc=$?
	case "$rc" in 0) has_legacy=1 ;; 1) has_legacy=0 ;; *) return 2 ;; esac
	rc=0
	ghcr_tag_exists "$repo" "$(ghcr_signature_tag_bundle "$digest")" "$tok" || rc=$?
	case "$rc" in 0) has_bundle=1 ;; 1) has_bundle=0 ;; *) return 2 ;; esac
	ghcr__layout "$has_legacy" "$has_bundle"
}

# Every image repository the publish workflow pushes AND signs.
#
# ONE list, shared by the signer and the verifier. It used to be two: the sign
# step's own comment warned that "a second copy of the repo list can drift from
# this one", and a drifted verifier is worse than none — it would pass while an
# image it forgot about was never published at all.
#
# Keep in sync with the go_multi_arch_image() repositories that //charts/*:*.push
# publishes. //e2e/l4echo builds an image too and is deliberately absent: it is
# never pushed to a registry. A name that is wrong rather than missing fails
# loudly — the verifier hard-fails on a repo it cannot find a readable tag in
# (the witness behind every absence, ghcr_any_tag).
# shellcheck disable=SC2034  # consumed by whoever sources this file.
GHCR_IMAGE_REPOS=(
	bpalermo/aether/agent
	bpalermo/aether/mesh-dns
	bpalermo/aether/proxy-supervisor
	bpalermo/aether/cni-install
	bpalermo/aether/registrar
	bpalermo/aether/controller
	bpalermo/aether/prober
	bpalermo/aether/udsecho
)

# Every chart the publish workflow pushes. ALL FOUR are commit-addressable:
# crds, prober and udsecho carry `version: "X.Y.Z-{GIT_COMMIT}"` in their own
# Chart.yaml, and //charts/aether:aether_commit derives the same shape from the
# bare version (#692). So each one has exactly one tag that belongs to exactly
# one commit, which is what makes them checkable at all.
#
# The chart directory name is also the repository basename, and the Chart.yaml
# path — charts/<name>/Chart.yaml -> ghcr.io/bpalermo/aether/charts/<name>.
# shellcheck disable=SC2034  # consumed by whoever sources this file.
GHCR_CHARTS=(
	aether
	crds
	prober
	udsecho
)

# shellcheck disable=SC2034  # consumed by whoever sources this file.
GHCR_CHART_REPO_PREFIX=bpalermo/aether/charts

# --- internals -------------------------------------------------------------

# legacy | bundle | both | none, from two 0/1 presence flags.
ghcr__layout() {
	if [ "$1" = 1 ] && [ "$2" = 1 ]; then
		printf 'both\n'
	elif [ "$1" = 1 ]; then
		printf 'legacy\n'
	elif [ "$2" = 1 ]; then
		printf 'bundle\n'
	else
		printf 'none\n'
	fi
}

ghcr__json_str() {
	python3 -c 'import sys,json;print(json.load(sys.stdin)[sys.argv[1]])' "$1"
}

ghcr__json_tags() {
	python3 -c 'import sys,json;[print(t) for t in (json.load(sys.stdin).get("tags") or [])]'
}

# Child digests of an index document on stdin. Exits 1 on anything that is not
# an index with at least one well-formed `sha256:` child: a manifest (no
# `manifests` key), an empty list, or an entry without a digest.
ghcr__json_children() {
	python3 -c '
import json, re, sys
doc = json.load(sys.stdin)
kids = doc.get("manifests")
if not isinstance(kids, list) or not kids:
    sys.exit(1)
digests = [k.get("digest", "") if isinstance(k, dict) else "" for k in kids]
if not all(re.fullmatch(r"sha256:[0-9a-f]{64}", d) for d in digests):
    sys.exit(1)
print("\n".join(digests))
'
}
