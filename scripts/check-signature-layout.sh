#!/usr/bin/env bash
# Exercise registry_signature_layout() over every state it can report.
#
# WHY THIS IS A SCRIPT AND NOT A COMMENT. The verifier's signature check accepts
# three layouts -- cosign 2's `sha256-<digest>.sig` tag, cosign 3's bare
# `sha256-<digest>` fallback-index tag (a registry without the Referrers API:
# ghcr.io), and cosign 3's OCI 1.1 referrer (a registry WITH the API: quay.io,
# proposal 040) -- because everything published before the v3 migration carries
# the first and must stay verifiable, and the Quay cut-over brings the third.
# "Accept any" is one `grep -q` away from "accept anything", and the state that
# matters most is the one nobody thinks to test: MORE THAN ONE layout present
# (`both`), which is what a double-write or a half-finished migration looks like
# and which presence-only checking reports as a healthy signature.
#
# It also pins two subtleties that are easy to lose in a refactor:
#   - `sha256-<d>.sig` must NOT satisfy the bare-tag check. `grep -qxF` matches
#     whole lines, so it does not today; a change to `grep -qF` would silently
#     make every legacy signature also look like a bundle signature, i.e.
#     report `both` for everything.
#   - a sigstore bundle referrer is ALSO what `cosign attest` attaches. Only a
#     bundle whose predicateType is cosign's sign predicate is a SIGNATURE; an
#     attestation (or buildx's own attestation manifests, which quay.io lists as
#     referrers of keycloak's children today) must read as `none`.
#
# No registry access: the tag lists and referrers indexes are literals, the
# referrer entries shaped exactly like quay.io's answer for argoproj/argocd
# (2026-09-27).
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
# shellcheck disable=SC1091
. scripts/registry-lib.sh

digest="sha256:abc123"
legacy="sha256-abc123.sig"
bundle="sha256-abc123"
fail=0
n=0

# ref_index <entry>... -> a referrers index document listing the entries.
ref_index() {
	local IFS=,
	printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[%s]}' "$*"
}
# One referrer entry: <artifactType> [<predicateType>].
ref_entry() {
	local ann=""
	[ -n "${2:-}" ] && ann=",\"annotations\":{\"dev.sigstore.bundle.content\":\"dsse-envelope\",\"dev.sigstore.bundle.predicateType\":\"$2\"}"
	printf '{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%064d","size":895,"artifactType":"%s"%s}' 7 "$1" "$ann"
}
sign_ref="$(ref_entry application/vnd.dev.sigstore.bundle.v0.3+json https://sigstore.dev/cosign/sign/v1)"
attest_ref="$(ref_entry application/vnd.dev.sigstore.bundle.v0.3+json https://slsa.dev/provenance/v1)"
cosign2_ref="$(ref_entry application/vnd.dev.cosign.artifact.sig.v1+json)"
buildx_ref="$(ref_entry application/vnd.docker.attestation.manifest.v1+json)"

REFS=""
check() {
	local want="$1" got
	shift
	got="$(registry_signature_layout "$digest" "$(printf '%s\n' "$@")" "$REFS")"
	n=$((n + 1))
	local label="[$*]"
	[ -n "$REFS" ] && label="${label} + referrers(${REFS_NAME})"
	if [ "$got" = "$want" ]; then
		printf '  ok    %-8s <- %s\n' "$got" "$label"
	else
		printf '  FAIL  want=%s got=%s <- %s\n' "$want" "$got" "$label"
		fail=1
	fi
}

# Tag layouts, no referrers (ghcr.io: the API 404s, so there is no index).
check legacy "$legacy" other-tag
check bundle "$bundle" other-tag
check both "$legacy" "$bundle"
check none other-tag dev-deadbeef
# The .sig subtlety above, asserted directly rather than implied.
check legacy "$legacy"

# Referrer layouts (quay.io).
REFS="$(ref_index "$sign_ref")" REFS_NAME="cosign 3 sign bundle"
check referrer other-tag
REFS="$(ref_index "$cosign2_ref")" REFS_NAME="cosign 2 oci-1-1 sig"
check referrer other-tag
REFS="$(ref_index "$buildx_ref" "$sign_ref" "$sign_ref")" REFS_NAME="buildx attestation + two sign bundles"
check referrer other-tag
REFS="$(ref_index)" REFS_NAME="empty index"
check none other-tag
REFS="$(ref_index "$attest_ref")" REFS_NAME="attestation bundle only"
check none other-tag
REFS="$(ref_index "$buildx_ref")" REFS_NAME="buildx attestation only"
check none other-tag
REFS="$(ref_index "$sign_ref")" REFS_NAME="cosign 3 sign bundle"
check both "$legacy"
check both "$bundle"
REFS="" REFS_NAME=""

if [ "$n" -ne 13 ]; then
	echo "::error::ran ${n} cases, expected 13 -- a gate that checks nothing passes" >&2
	exit 2
fi
if [ "$fail" -ne 0 ]; then
	echo "::error::signature layout resolution is wrong; see cases above" >&2
	exit 1
fi
echo "signature layout: ${n}/${n} cases correct"
