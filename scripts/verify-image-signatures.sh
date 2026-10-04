#!/usr/bin/env bash
# cosign-verify published images: each INDEX and EVERY child manifest it lists
# (#925).
#
# WHY THIS EXISTS
#
# publish.yaml signs with `cosign sign --recursive`, which signs a multi-arch
# index and each per-architecture manifest under it. `cosign verify` has no
# `--recursive` — not in v2.4.1, v3.0.6 or v3.1.2 — so the verify step used to
# check the eight index digests and nothing else. The per-arch manifests are
# what a node actually pulls, and they were signed and never checked: a green
# verify covered less than it read as. This walks the children the way the
# signer does and verifies each one with the same identity and issuer.
#
# USAGE
#
#   bazel run //bazel/cosign:verify_image_signatures -- <ref> [<ref>...]
#   bazel run //bazel/cosign:verify_image_signatures -- --file <refs, one per line>
#
# That is how CI runs it and how to run it by hand: the target exports COSIGN as
# the Bazel-pinned cosign (the rules_img_signer_cosign bazel_dep's release
# binary, sha256-pinned in its lock; `bazel run //bazel/cosign -- version`), so
# every verify uses the same cosign as the signer. Relative paths resolve
# against your working directory. The script still runs standalone:
#
#   COSIGN=/path/to/cosign scripts/verify-image-signatures.sh <ref>...
#
# Each ref is `<registry>/<repo>@sha256:<index digest>` — a DIGEST, never a tag,
# so nothing here can re-resolve to a different artefact than the caller named.
# The registry is taken from each ref (REGISTRY_HOST is set per ref for the
# child walk), so one run can verify refs on more than one registry.
#
# REGISTRY-NEUTRAL. `cosign verify` finds the signature itself in every layout
# cosign writes: the cosign 2 `.sig` tag, cosign 3's referrers fallback tag on a
# registry without the Referrers API, and — on quay.io, which serves
# the API — the bundle attached as an OCI 1.1 referrer, which cosign 3
# discovers through /v2/<repo>/referrers/<digest> with no tag at all.
#
# Every ref and every child is checked even after a failure, so one run names
# every unsigned manifest rather than the first.
#
# ENVIRONMENT
#
#   COSIGN               cosign binary (default: `cosign` on PATH). Set by
#                        //bazel/cosign:verify_image_signatures to the pinned
#                        v3.1.2; a standalone run uses whatever you point it at.
#   CERT_IDENTITY_REGEXP certificate identity; default is publish.yaml on
#                        ${GITHUB_REPOSITORY:-bpalermo/aether}, any ref.
#   CERT_OIDC_ISSUER     default https://token.actions.githubusercontent.com
#   REGISTRY_USERNAME, REGISTRY_PASSWORD
#                        optional; public packages read anonymously
#                        (scripts/registry-lib.sh).
#
# READ-ONLY. cosign verify and registry GETs only; nothing here can sign, push
# or delete.
#
# EXIT CODES
#   0  every index and every child verified
#   1  at least one index or child failed verification (unsigned, or signed by
#      another identity)
#   2  the check could not be performed: bad usage, no cosign, a ref that is not
#      a digest, or an index whose children could not be enumerated (including
#      zero children — a walk over nothing is not a pass)

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/registry-lib.sh
. "${here}/registry-lib.sh"

cosign_bin="${COSIGN:-cosign}"
identity="${CERT_IDENTITY_REGEXP:-^https://github\.com/${GITHUB_REPOSITORY:-bpalermo/aether}/\.github/workflows/publish\.yaml@}"
issuer="${CERT_OIDC_ISSUER:-https://token.actions.githubusercontent.com}"

refs=()
if [ "${1:-}" = "--file" ]; then
	if [ "$#" -ne 2 ] || [ ! -r "$2" ]; then
		echo "usage: $(basename "$0") --file <readable file of refs>" >&2
		exit 2
	fi
	while read -r line; do
		[ -n "$line" ] && refs+=("$line")
	done <"$2"
else
	refs=("$@")
fi

# An empty list means the caller recorded nothing — fail rather than "verify"
# zero images (#853).
if [ "${#refs[@]}" -eq 0 ]; then
	echo "::error::no image refs to verify" >&2
	exit 2
fi
if ! command -v "$cosign_bin" >/dev/null 2>&1; then
	echo "::error::cosign not found (${cosign_bin})" >&2
	exit 2
fi

verified=0
failed=0

verify_one() {
	local what="$1" ref="$2" err
	err="$(mktemp)"
	if "$cosign_bin" verify \
		--certificate-identity-regexp "$identity" \
		--certificate-oidc-issuer "$issuer" \
		"$ref" >/dev/null 2>"$err"; then
		echo "  verified ${what} ${ref}"
		verified=$((verified + 1))
	else
		echo "  FAILED   ${what} ${ref}: $(tail -1 "$err")"
		echo "::error::signature did not verify: ${what} ${ref}"
		failed=$((failed + 1))
	fi
	rm -f "$err"
}

for ref in "${refs[@]}"; do
	if ! [[ "$ref" =~ ^([a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?)/([^@]+)@(sha256:[0-9a-f]{64})$ ]]; then
		echo "::error::not a <registry>/<repo>@sha256:<digest> reference: ${ref}" >&2
		exit 2
	fi
	REGISTRY_HOST="${BASH_REMATCH[1]}"
	repo="${BASH_REMATCH[4]}"
	digest="${BASH_REMATCH[5]}"

	echo "${ref}"
	verify_one index "$ref"

	if ! tok="$(registry_registry_token "$repo")" || [ -z "$tok" ]; then
		echo "::error::could not obtain a pull token for ${repo}" >&2
		exit 2
	fi
	if ! children="$(registry_index_children "$repo" "$digest" "$tok")" || [ -z "$children" ]; then
		echo "::error::could not enumerate the child manifests of ${ref} (not an index, or no children)" >&2
		exit 2
	fi
	n_children=0
	while read -r child; do
		verify_one child "${REGISTRY_HOST}/${repo}@${child}"
		n_children=$((n_children + 1))
	done <<<"$children"
	echo "  ${n_children} child manifest(s) walked"
done

echo ""
if [ "$failed" -gt 0 ]; then
	echo "FAIL: ${failed} manifest(s) did not verify, ${verified} did, across ${#refs[@]} index(es)"
	exit 1
fi
echo "PASS: ${verified} manifest(s) verified (${#refs[@]} index(es) + their children)"
