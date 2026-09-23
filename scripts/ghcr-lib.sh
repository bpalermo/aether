#!/usr/bin/env bash
# Shared, read-only GHCR helpers.
#
# This file is SOURCED, never executed: `. scripts/ghcr-lib.sh`. It is sourced by
#   - .github/workflows/publish.yaml   (the sign step, which resolves the digest
#     it is about to sign)
#   - scripts/verify-published-artifacts.sh  (#880, which asserts those artifacts
#     actually landed)
#
# One copy on purpose. The pagination below was written for the sign step after
# it failed on e3b58e6 by reading 100 of 667 tags (#875); a second, subtly
# different copy in the verifier would be free to regress the same way while the
# original stayed correct — and a verifier that cannot see a tag reports a
# publish that happened as missing.
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

# cosign's signature tag for a digest: `sha256:abc…` -> `sha256-abc….sig`.
#
# ghcr.io does NOT implement the OCI 1.1 Referrers API — GET
# /v2/<repo>/referrers/<digest> 404s there while tags/list and manifests/ succeed
# with the same token — so the tag scheme is not a preference, it is the only
# scheme that works on this registry. Both the signer and the verifier therefore
# have to agree on this exact string.
ghcr_signature_tag() {
	printf '%s.sig\n' "${1/:/-}"
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
# loudly — the verifier hard-fails on a repo whose tag list it cannot read.
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

ghcr__json_str() {
	python3 -c 'import sys,json;print(json.load(sys.stdin)[sys.argv[1]])' "$1"
}

ghcr__json_tags() {
	python3 -c 'import sys,json;[print(t) for t in (json.load(sys.stdin).get("tags") or [])]'
}
