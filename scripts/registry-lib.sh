#!/usr/bin/env bash
# Shared, read-only OCI registry helpers (GHCR and Quay; proposal 040).
#
# This file is SOURCED, never executed: `. scripts/registry-lib.sh`. It is
# sourced by
#   - .github/workflows/publish.yaml   (the sign step, which resolves the digest
#     it is about to sign)
#   - scripts/verify-published-artifacts.sh  (#880, which asserts those artifacts
#     actually landed)
#   - scripts/verify-image-signatures.sh  (#925, which cosign-verifies each index
#     AND every child manifest it lists)
#
# WHICH REGISTRY. REGISTRY_HOST, read at CALL time (so a caller can point one
# lookup at another registry by setting it), defaulting to the host in
# bazel/img/registry.bzl via scripts/image-registry.sh — the single setting.
# The repository lists below come from the same place. Everything here speaks
# the plain OCI distribution API; the one registry-specific thing is where the
# anonymous pull token is handed out (registry_token_url).
#
# One copy on purpose. The pagination below was written for the sign step after
# it failed on e3b58e6 by reading 100 of 667 tags (#875); a second, subtly
# different copy in the verifier would be free to regress the same way while the
# original stayed correct — and a verifier that cannot see a tag reports a
# publish that happened as missing. (The verifier no longer lists at all: it
# asks for each tag it expects by name, registry_tag_exists — #985.)
#
# Everything here is GET/HEAD only. Nothing in this file can push, tag, delete or
# otherwise mutate the registry: publishing is the release workflow's job alone.

registry__here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The setting, parsed once at source time. A setting that cannot be read leaves
# REGISTRY_HOST empty, and every function below refuses to run without one (rc 2)
# rather than guessing a registry.
if [ -z "${REGISTRY_HOST:-}" ]; then
	REGISTRY_HOST="$("${registry__here}/image-registry.sh" host)" || REGISTRY_HOST=""
fi

# The pull-token endpoint for one repository on $REGISTRY_HOST.
#
#   ghcr.io  https://ghcr.io/token?service=ghcr.io&scope=repository:<repo>:pull
#   quay.io  https://quay.io/v2/auth?service=quay.io&scope=repository:<repo>:pull
#
# The Starlark twin is registry_token_url() in bazel/img/registry.bzl (used by
# //bazel/proxy_pin). Both registries answer `{"token": "..."}`.
registry_token_url() {
	local repo="$1" path=token
	[ "$REGISTRY_HOST" = quay.io ] && path=v2/auth
	printf 'https://%s/%s?service=%s&scope=repository:%s:pull\n' \
		"$REGISTRY_HOST" "$path" "$REGISTRY_HOST" "$repo"
}

# Bearer token for pulling from one repository.
#
# Both registries hand out an anonymous pull token for public repositories,
# which is what lets the verifier run from a workstation with no credentials at
# all. For private ones: REGISTRY_USERNAME + REGISTRY_PASSWORD (a Quay robot
# account, say). (The ghcr.io-only GHCR_TOKEN was removed with the sweep's
# pre-cut-over branch, proposal 040 phase 4.)
#
# THE CREDENTIALS ARE BOUND TO ONE HOST. REGISTRY_USERNAME/REGISTRY_PASSWORD
# were issued by one registry -- REGISTRY_CREDENTIAL_HOST, default the
# IMAGE_REGISTRY_HOST the workflows export from scripts/image-registry.sh (or,
# unset, the setting's host) -- and are sent to that host's token endpoint and
# NO other. A token request to any other host goes anonymous, with one stderr
# line saying so: nothing that ever points REGISTRY_HOST at another registry
# may hand it that password.
registry__credential_host() {
	if [ -n "${REGISTRY_CREDENTIAL_HOST:-}" ]; then
		printf '%s\n' "$REGISTRY_CREDENTIAL_HOST"
	elif [ -n "${IMAGE_REGISTRY_HOST:-}" ]; then
		printf '%s\n' "$IMAGE_REGISTRY_HOST"
	else
		"${registry__here}/image-registry.sh" host 2>/dev/null
	fi
}

registry_registry_token() {
	local repo="$1" url cred_host=""
	registry__host_ok || return 2
	url="$(registry_token_url "$repo")"
	if [ -n "${REGISTRY_PASSWORD:-}" ]; then
		cred_host="$(registry__credential_host)" || cred_host=""
		if [ -z "$cred_host" ] || [ "$REGISTRY_HOST" != "$cred_host" ]; then
			echo "registry-lib: credentials are for ${cred_host:-an unknown host}; reading ${REGISTRY_HOST} anonymously" >&2
		fi
	fi
	if [ -n "${REGISTRY_PASSWORD:-}" ] && [ -n "$cred_host" ] && [ "$REGISTRY_HOST" = "$cred_host" ]; then
		curl -fsS -u "${REGISTRY_USERNAME:-x}:${REGISTRY_PASSWORD}" "$url" | registry__json_str token
	else
		curl -fsS "$url" | registry__json_str token
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
# exists, use registry_tag_exists below.
#
# Usage: registry_all_tags <repo> <token>   -> one tag per line on stdout
registry_all_tags() {
	local repo="$1" tok="$2" last="" hdr body url
	registry__host_ok || return 2
	hdr="$(mktemp)"
	while :; do
		url="https://${REGISTRY_HOST}/v2/${repo}/tags/list?n=1000"
		[ -n "$last" ] && url="${url}&last=${last}"
		body="$(curl -fsS -D "$hdr" -H "Authorization: Bearer $tok" "$url")"
		printf '%s' "$body" | registry__json_tags
		# No rel="next" means this was the final page.
		grep -qi '^link:.*rel="next"' "$hdr" || break
		last="$(printf '%s' "$body" | registry__json_tags | tail -1)"
		[ -n "$last" ] || break
	done
	rm -f "$hdr"
}

# Resolve the commit-suffixed tag (`<tag>-<full sha>`) a publish JUST pushed,
# for the sign step in .github/workflows/publish.yaml. That tag is the only
# immutable one (`dev` is rewritten by every publish, #692); the step signs the
# digest it points at. Its prefix is not known here, so it is found by listing.
#
# Two things this function exists for (#1046):
#
#   - A MISS MUST BE LOUD. The step used to run
#       tag="$(printf '%s\n' "$tags" | grep -E -- "-${COMMIT}$" | head -1)"
#     under `set -euo pipefail`: when nothing matched, grep exited 1, pipefail
#     failed the substitution, and `set -e` ended the step BEFORE the
#     `if [ -z "$tag" ]` written to explain the miss. Publish run 36417203322
#     died that way with zero output. The grep below cannot fail its pipeline,
#     and nothing here relies on `set -e` (bash clears it inside a command
#     substitution anyway): every outcome is an explicit return.
#   - THE LISTING LAGS THE PUSH. The same job pushed the tag seconds earlier,
#     and a registry's `tags/list` need not show it yet, so re-list up to
#     REGISTRY_COMMIT_TAG_ATTEMPTS times (default 13), REGISTRY_COMMIT_TAG_INTERVAL
#     seconds apart (default 5): about 60 s. Every miss is printed with the
#     number of tags it scanned -- "not found in 667" is a real absence, "not
#     found in 100" is the pagination regressing (#875), and the two must never
#     look the same.
#
# Progress and the final ::error:: go to stderr; only the tag goes to stdout.
#
# Usage: registry_commit_tag <repo> <token> <commit>  -> the tag on stdout
#   0  found   1  still absent after every listing
registry_commit_tag() {
	local repo="$1" tok="$2" commit="$3"
	local attempts="${REGISTRY_COMMIT_TAG_ATTEMPTS:-13}"
	local interval="${REGISTRY_COMMIT_TAG_INTERVAL:-5}"
	local i=1 tags n_tags tag
	while :; do
		tags="$(registry_all_tags "$repo" "$tok")"
		n_tags="$(printf '%s\n' "$tags" | grep -c . || true)"
		tag="$(printf '%s\n' "$tags" | { grep -E -- "-${commit}$" || true; } | head -1)"
		if [ -n "$tag" ]; then
			echo "resolved ${repo}: ${tag} (scanned ${n_tags} tags, listing ${i}/${attempts})" >&2
			printf '%s\n' "$tag"
			return 0
		fi
		if [ "$i" -ge "$attempts" ]; then
			echo "::error::no tag ending in -${commit} in ${repo} (scanned ${n_tags} tags); still absent after ${attempts} listing(s) ${interval}s apart -- refusing to sign a tag that is not this commit" >&2
			return 1
		fi
		echo "no tag ending in -${commit} in ${repo} yet (scanned ${n_tags} tags, listing ${i}/${attempts}); the listing can lag the push, re-listing in ${interval}s" >&2
		sleep "$interval"
		i=$((i + 1))
	done
}

# The manifest media types a published coordinate can carry: a multi-arch index
# (OCI or Docker) or a single manifest (OCI or Docker) — which covers a Helm
# chart, a cosign 2 `.sig` and a cosign 3 fallback index alike. A HEAD whose
# Accept matches none of them may be refused for a manifest that exists.
REGISTRY_MANIFEST_ACCEPT='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'

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
# Usage: registry_tag_exists <repo> <tag> <token>
registry_tag_exists() {
	local repo="$1" tag="$2" tok="$3" code
	registry__host_ok || return 2
	code="$(curl -sS --retry 2 -o /dev/null -w '%{http_code}' -I \
		-H "Authorization: Bearer $tok" -H "Accept: ${REGISTRY_MANIFEST_ACCEPT}" \
		"https://${REGISTRY_HOST}/v2/${repo}/manifests/${tag}")" || true
	case "$code" in
	200) return 0 ;;
	404) return 1 ;;
	*)
		echo "registry_tag_exists: HEAD ${REGISTRY_HOST}/v2/${repo}/manifests/${tag} answered '${code:-nothing}'" >&2
		return 2
		;;
	esac
}

# ONE tag that exists in <repo>: the first entry of a single listing page. Never
# used to decide whether an expected tag is there — that is registry_tag_exists'
# job — but as the WITNESS behind a 404 (#985): if a tag this listing just named
# also answers 200 to registry_tag_exists, the repository is readable and the
# lookup can say "present", so the 404 beside it is a real absence. Without it,
# a lookup that 404s everything (ghcr does exactly that to a manifest HEAD whose
# Accept it does not like) would report a complete publish as MISSING. Prints
# nothing when the repository cannot be listed or lists no tags.
#
# Usage: registry_any_tag <repo> <token>  -> one tag on stdout
registry_any_tag() {
	local repo="$1" tok="$2"
	registry__host_ok || return 2
	curl -fsS -H "Authorization: Bearer $tok" \
		"https://${REGISTRY_HOST}/v2/${repo}/tags/list?n=1" | registry__json_tags | head -1
}

# Resolve a tag to the digest the registry itself reports for it.
#
# HEAD + Docker-Content-Digest rather than sha256sum of a GET body: the digest
# must be the registry's own answer for the media type we asked for, and for a
# multi-arch image that is the INDEX digest — the thing cosign signs.
#
# Usage: registry_manifest_digest <repo> <tag> <token>  -> `sha256:...` on stdout
registry_manifest_digest() {
	local repo="$1" tag="$2" tok="$3"
	registry__host_ok || return 2
	curl -fsSI -H "Authorization: Bearer $tok" \
		-H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json' \
		"https://${REGISTRY_HOST}/v2/${repo}/manifests/${tag}" |
		tr -d '\r' | sed -nE 's/^[Dd]ocker-[Cc]ontent-[Dd]igest:[[:space:]]*//p' | head -1
}

# Every child manifest digest of a multi-arch INDEX (#925).
#
# publish.yaml signs with `cosign sign --recursive`, which signs the index AND
# each child it lists. `cosign verify` has no `--recursive` (not in v2.4.1, not
# in v3.0.6), so a verifier that checks only the index digest never looks at the
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
# Usage: registry_index_children <repo> <index digest> <token>  -> one digest per line
registry_index_children() {
	local repo="$1" digest="$2" tok="$3" body
	registry__host_ok || return 2
	body="$(curl -fsS -H "Authorization: Bearer $tok" \
		-H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json' \
		"https://${REGISTRY_HOST}/v2/${repo}/manifests/${digest}")" || return 1
	printf '%s' "$body" | registry__json_children
}

# The OCI 1.1 Referrers API: `GET /v2/<repo>/referrers/<digest>` (proposal 040).
#
# Quay serves it; ghcr.io does NOT (404 with a valid pull token, verified
# 2026-09-27). The distribution spec forbids a registry that implements the API
# from answering 404 to it — an unknown subject is a 200 with an empty index, a
# malformed digest a 400 — so a 404 means exactly "this registry has no
# referrers API" and NEVER "no signatures": the caller falls back to cosign's
# tag layouts. Anything else is unknown.
#
# Follows `Link: <...>; rel="next"` pagination and prints ONE merged index. The
# optional artifactType is sent as the spec's filter; registries may ignore it
# (they say so with OCI-Filters-Applied), so callers still filter client-side.
#
# Three answers:
#   0  200: the (merged) referrers index on stdout
#   1  404: no referrers API on this registry — nothing on stdout
#   2  anything else (401/403/429/5xx, no connection, a body that is not an
#      index): unknown, never "no referrers"
#
# Usage: registry_referrers <repo> <digest> <token> [artifactType]
registry_referrers() {
	local repo="$1" digest="$2" tok="$3" atype="${4:-}" url hdr body code pages="" next rc=0
	registry__host_ok || return 2
	url="https://${REGISTRY_HOST}/v2/${repo}/referrers/${digest}"
	[ -n "$atype" ] && url="${url}?artifactType=${atype//+/%2B}"
	hdr="$(mktemp)"
	body="$(mktemp)"
	while [ -n "$url" ]; do
		code="$(curl -sS --retry 2 -D "$hdr" -o "$body" -w '%{http_code}' \
			-H "Authorization: Bearer $tok" \
			-H 'Accept: application/vnd.oci.image.index.v1+json' "$url")" || true
		case "$code" in
		200) ;;
		404)
			if [ -z "$pages" ]; then rc=1; else rc=2; fi
			break
			;;
		*)
			echo "registry_referrers: GET ${url} answered '${code:-nothing}'" >&2
			rc=2
			break
			;;
		esac
		pages="${pages}$(cat "$body")"$'\n'
		next="$(tr -d '\r' <"$hdr" | sed -nE 's/^[Ll]ink:[[:space:]]*<([^>]+)>;.*rel="?next"?.*/\1/p' | head -1)"
		case "$next" in
		"") url="" ;;
		https://*) url="$next" ;;
		/*) url="https://${REGISTRY_HOST}${next}" ;;
		*)
			echo "registry_referrers: unusable Link next '${next}'" >&2
			rc=2
			break
			;;
		esac
	done
	rm -f "$hdr" "$body"
	[ "$rc" = 0 ] || return "$rc"
	printf '%s' "$pages" | registry__json_merge_index || {
		echo "registry_referrers: ${REGISTRY_HOST}/v2/${repo}/referrers/${digest} is not an OCI index" >&2
		return 2
	}
}

# The artifactTypes a cosign signature is attached under as an OCI 1.1 referrer:
#
#   application/vnd.dev.sigstore.bundle.v0.3+json   cosign 3 (new bundle format)
#   application/vnd.dev.cosign.artifact.sig.v1+json cosign 2's experimental
#                                                   `--registry-referrers-mode=oci-1-1`
#
# A sigstore bundle is ALSO what `cosign attest` attaches, so a bundle referrer
# counts as a SIGNATURE only when its `dev.sigstore.bundle.predicateType`
# annotation is cosign's sign predicate. Observed on quay.io 2026-09-27:
# argoproj/argocd and cilium/cilium carry exactly that
# (artifactType ...bundle.v0.3+json, predicateType
# https://sigstore.dev/cosign/sign/v1, content dsse-envelope) and no fallback
# tag; buildx attestation referrers (application/vnd.docker.attestation...) must
# never read as a signature.
# shellcheck disable=SC2034  # consumed by whoever sources this file.
REGISTRY_SIGSTORE_BUNDLE_TYPE='application/vnd.dev.sigstore.bundle.v0.3+json'
# shellcheck disable=SC2034  # consumed by whoever sources this file.
REGISTRY_COSIGN_SIG_TYPE='application/vnd.dev.cosign.artifact.sig.v1+json'
# shellcheck disable=SC2034  # consumed by whoever sources this file.
REGISTRY_COSIGN_SIGN_PREDICATE='https://sigstore.dev/cosign/sign/v1'

# cosign's signature tag for a digest. There are TWO shapes, and which one you
# get depends on the cosign major that signed:
#
#   cosign 2.x            `sha256-abc….sig`   a plain signature manifest
#   cosign 3.x (default)  `sha256-abc…`       an OCI 1.1 referrers FALLBACK index
#
# ghcr.io does NOT implement the OCI 1.1 Referrers API — GET
# /v2/<repo>/referrers/<digest> 404s there while tags/list and manifests/ succeed
# with the same token. That is why cosign 3 writes the fallback TAG there rather
# than a real referrer. On a registry that DOES serve the API (quay.io), cosign 3
# attaches the bundle as a referrer and writes no tag at all — the third layout,
# `referrer`, below.
#
# Everything published before 2026-09-24 carries the legacy shape and must stay
# verifiable, so the verifier accepts any ONE layout — but exactly one. Two
# present for the same digest means a double-write or a half-finished migration,
# which presence-only checking reads as healthy. See registry_signature_layout().
registry_signature_tag_legacy() {
	printf '%s.sig\n' "${1/:/-}"
}

registry_signature_tag_bundle() {
	printf '%s\n' "${1/:/-}"
}

# Which layout is published for $1 (a digest), given $2 (the repo's tag list)
# and, optionally, $3 (its referrers index, as registry_referrers prints it; ""
# for none, e.g. a registry without the API).
#
# Echoes `legacy`, `bundle`, `referrer`, `none`, or `both`. `both` means MORE
# THAN ONE layout is present (it predates `referrer`; the name is kept because
# every caller already treats it as the double-write defect). The caller decides
# what to do; the point of naming `both` separately is that it is a DIFFERENT
# defect from `none` and must not be reported as a healthy signature.
# (`grep -c`, not `-q`, on the piped tag list: see proxy_pin_introduced_by in
# scripts/proxy-pin-lib.sh -- an early-exiting grep under pipefail reads a
# present tag as absent.)
registry_signature_layout() {
	local digest="$1" tags="$2" referrers="${3:-}" has_legacy=0 has_bundle=0 has_ref=0
	printf '%s\n' "$tags" | grep -cxF -- "$(registry_signature_tag_legacy "$digest")" >/dev/null && has_legacy=1
	printf '%s\n' "$tags" | grep -cxF -- "$(registry_signature_tag_bundle "$digest")" >/dev/null && has_bundle=1
	if [ -n "$referrers" ]; then
		has_ref="$(printf '%s' "$referrers" | registry__json_has_signature_referrer)" || has_ref=0
	fi
	registry__layout "$has_legacy" "$has_bundle" "$has_ref"
}

# The same answer from direct lookups instead of a tag list (#985): the
# referrers API, then BOTH tags by name — not "stop at the first hit" — so a
# double-write is still seen.
#
# A referrers 404 (no API: ghcr.io) is "no referrer" and the tag layouts decide.
# Returns 2 with nothing on stdout when any lookup goes unanswered: a layout
# decided on some answers out of three could call `both` `legacy`.
#
# Usage: registry_signature_layout_direct <repo> <digest> <token>
registry_signature_layout_direct() {
	local repo="$1" digest="$2" tok="$3" has_legacy has_bundle has_ref=0 refs rc
	rc=0
	refs="$(registry_referrers "$repo" "$digest" "$tok")" || rc=$?
	case "$rc" in
	0) has_ref="$(printf '%s' "$refs" | registry__json_has_signature_referrer)" || return 2 ;;
	1) has_ref=0 ;;
	*) return 2 ;;
	esac
	rc=0
	registry_tag_exists "$repo" "$(registry_signature_tag_legacy "$digest")" "$tok" || rc=$?
	case "$rc" in 0) has_legacy=1 ;; 1) has_legacy=0 ;; *) return 2 ;; esac
	rc=0
	registry_tag_exists "$repo" "$(registry_signature_tag_bundle "$digest")" "$tok" || rc=$?
	case "$rc" in 0) has_bundle=1 ;; 1) has_bundle=0 ;; *) return 2 ;; esac
	registry__layout "$has_legacy" "$has_bundle" "$has_ref"
}

# The layout a registry setting PROMISES, per commit (proposal 040 phase 2):
# SIGNATURE_LAYOUT in the given bazel/img/registry.bzl -- `referrer` (quay.io)
# or `tag`. A file without that line predates the Quay cut-over: its commit
# published to the pre-cut-over registry, which proposal 040 phase 4
# decommissioned, so there is no layout to promise -- rc 3, with one stderr
# line saying so, never a default. A present but unparseable line is rc 2.
#
# Usage: registry_setting_signature_layout <registry.bzl>   -> referrer | tag
registry_setting_signature_layout() {
	local bzl="$1"
	[ -r "$bzl" ] || return 2
	if ! grep -qE '^SIGNATURE_LAYOUT[[:space:]]*=' "$bzl"; then
		echo "registry-lib: ${bzl} has no SIGNATURE_LAYOUT: a pre-cut-over setting, whose registry is decommissioned (proposal 040 phase 4)" >&2
		return 3
	fi
	IMAGE_REGISTRY_BZL="$bzl" "${registry__here}/image-registry.sh" signature-layout || return 2
}

# Does a signature found in <layout> (registry_signature_layout*) satisfy the
# <expected> layout a commit's setting promises?
#
#   0  yes: `referrer` for referrer; `legacy` or `bundle` for tag
#   1  no:  present in the OTHER shape (a fallback tag where a referrer is
#           promised, a referrer where a tag is), or `both` / `none`
#   2  <expected> is neither `referrer` nor `tag`
#
# Usage: registry_layout_satisfies <layout> <expected>
registry_layout_satisfies() {
	case "$2" in
	referrer) [ "$1" = referrer ] ;;
	tag) [ "$1" = legacy ] || [ "$1" = bundle ] ;;
	*) return 2 ;;
	esac
}

# The image components the publish workflow pushes AND signs.
#
# ONE list, shared by the signer and the verifier. It used to be two: the sign
# step's own comment warned that "a second copy of the repo list can drift from
# this one", and a drifted verifier is worse than none — it would pass while an
# image it forgot about was never published at all.
#
# Keep in sync with the go_multi_arch_image() calls whose image_push targets
# //charts/*:*.push publishes. //e2e/l4echo builds an image too and is
# deliberately absent: it is never pushed to a registry. A name that is wrong
# rather than missing fails loudly — the verifier hard-fails on a repo it cannot
# find a readable tag in (the witness behind every absence, registry_any_tag).
#
# Component names, not repositories: the repository is image_repository() of
# bazel/img/registry.bzl, asked of scripts/image-registry.sh.
REGISTRY_IMAGE_COMPONENTS=(
	agent
	mesh-dns
	proxy-supervisor
	uds-csi
	cni-install
	registrar
	controller
	prober
	udsecho
)

# Every chart the publish workflow pushes. ALL FOUR are commit-addressable:
# crds, prober and udsecho carry `version: "X.Y.Z-{GIT_COMMIT}"` in their own
# Chart.yaml, and //charts/aether:aether_commit derives the same shape from the
# bare version (#692). So each one has exactly one tag that belongs to exactly
# one commit, which is what makes them checkable at all.
#
# The chart directory name is also the chart name; its repository is
# chart_repository(<name>) of bazel/img/registry.bzl (registry_chart_repo).
# shellcheck disable=SC2034  # consumed by whoever sources this file.
REGISTRY_CHARTS=(
	aether
	crds
	prober
	udsecho
)

# The image components ONE COMMIT published (read with git, not the registry): REGISTRY_IMAGE_COMPONENTS as
# scripts/registry-lib.sh spelled it AT that commit, one per line. publish.yaml
# signs exactly that list at that commit, so a component added later (uds-csi,
# proposal 039) is not reported MISSING on the heads that predate it -- which the
# sweep would otherwise do for a whole day after the component lands. Falls back
# to this checkout's list when the file is absent at <sha> or yields nothing
# (a pre-040 head, or the fake histories of scripts/check-registry-lookup.sh).
# Used by the verifier per commit and by the expected-red control per base.
registry_image_components_at() {
	local sha="$1" list=""
	list="$(git show "${sha}:scripts/registry-lib.sh" 2>/dev/null |
		sed -n '/^REGISTRY_IMAGE_COMPONENTS=(/,/^)/{
			/^REGISTRY_IMAGE_COMPONENTS=(/d
			/^)/d
			s/[[:space:]]*#.*//
			s/^[[:space:]]*//
			s/[[:space:]]*$//
			/^$/d
			p
		}')" || list=""
	if [ -z "$list" ]; then
		printf '%s\n' "${REGISTRY_IMAGE_COMPONENTS[@]}"
	else
		printf '%s\n' "$list"
	fi
}

# Repository (no host) of one image component / chart, from the single setting.
registry_image_repo() { "${registry__here}/image-registry.sh" repo "$1"; }
registry_chart_repo() { "${registry__here}/image-registry.sh" chart-repo "$1"; }

# Resolved once at source time. An unreadable setting leaves the arrays EMPTY,
# which every consumer already refuses (#853: nothing to verify is not a pass).
REGISTRY_IMAGE_REPOS=()
registry__c=""
for registry__c in "${REGISTRY_IMAGE_COMPONENTS[@]}"; do
	registry__r="$(registry_image_repo "$registry__c")" || {
		REGISTRY_IMAGE_REPOS=()
		break
	}
	REGISTRY_IMAGE_REPOS+=("$registry__r")
done
unset registry__c registry__r

# --- internals -------------------------------------------------------------

registry__host_ok() {
	[ -n "${REGISTRY_HOST:-}" ] && return 0
	echo "registry-lib: no REGISTRY_HOST (scripts/image-registry.sh could not read bazel/img/registry.bzl)" >&2
	return 2
}

# legacy | bundle | referrer | both | none, from three 0/1 presence flags.
registry__layout() {
	local n=$(($1 + $2 + ${3:-0}))
	if [ "$n" -gt 1 ]; then
		printf 'both\n'
	elif [ "$1" = 1 ]; then
		printf 'legacy\n'
	elif [ "$2" = 1 ]; then
		printf 'bundle\n'
	elif [ "${3:-0}" = 1 ]; then
		printf 'referrer\n'
	else
		printf 'none\n'
	fi
}

registry__json_str() {
	python3 -c 'import sys,json;print(json.load(sys.stdin)[sys.argv[1]])' "$1"
}

registry__json_tags() {
	python3 -c 'import sys,json;[print(t) for t in (json.load(sys.stdin).get("tags") or [])]'
}

# Child digests of an index document on stdin. Exits 1 on anything that is not
# an index with at least one well-formed `sha256:` child: a manifest (no
# `manifests` key), an empty list, or an entry without a digest.
registry__json_children() {
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

# One or more referrers index pages (JSON documents, concatenated) on stdin ->
# one index with every page's manifests. Exits 1 on a page that is not an index
# (no `manifests` list): an unparseable answer is unknown, never "no referrers".
registry__json_merge_index() {
	python3 -c '
import json, sys
dec = json.JSONDecoder()
text = sys.stdin.read()
i, out, pages = 0, [], 0
while True:
    while i < len(text) and text[i].isspace():
        i += 1
    if i >= len(text):
        break
    doc, i = dec.raw_decode(text, i)
    if not isinstance(doc, dict) or not isinstance(doc.get("manifests"), list):
        sys.exit(1)
    out.extend(doc["manifests"])
    pages += 1
if pages == 0:
    sys.exit(1)
print(json.dumps({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": out}))
'
}

# A referrers index on stdin -> `1` when it lists a cosign SIGNATURE referrer,
# `0` when it does not. Exits 1 on a document that is not an index.
registry__json_has_signature_referrer() {
	python3 -c '
import json, sys
bundle, sig, pred = sys.argv[1:4]
doc = json.load(sys.stdin)
kids = doc.get("manifests") if isinstance(doc, dict) else None
if not isinstance(kids, list):
    sys.exit(1)
def is_sig(m):
    if not isinstance(m, dict):
        return False
    at = m.get("artifactType", "")
    if at == sig:
        return True
    ann = m.get("annotations") or {}
    return at == bundle and ann.get("dev.sigstore.bundle.predicateType") == pred
print(1 if any(is_sig(m) for m in kids) else 0)
' "$REGISTRY_SIGSTORE_BUNDLE_TYPE" "$REGISTRY_COSIGN_SIG_TYPE" "$REGISTRY_COSIGN_SIGN_PREDICATE"
}
