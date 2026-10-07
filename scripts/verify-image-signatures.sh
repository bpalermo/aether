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
# TRANSIENT ANSWERS ARE RETRIED (#1316). One 502 from the registry's token
# endpoint ended run 37522284996 with a Python traceback and filed "UNVERIFIED"
# for artifacts that were fine. Two layers, both bounded:
#   - the registry lookups made here (the pull token, the child walk) retry
#     inside scripts/registry-lib.sh (registry__fetch_json: no answer, 408, 429,
#     5xx, a 200 that is not JSON), and say what was asked and what came back;
#   - a `cosign verify` that fails is run again, up to VERIFY_ATTEMPTS times in
#     all, unless cosign said the signature it FETCHED does not match the
#     identity: that answer cannot change. "no signatures found" IS retried: it
#     is also what a failed lookup under cosign can read as. A run spends at
#     most VERIFY_RETRY_BUDGET re-runs in all, so a commit that really is
#     unsigned costs seconds, not the step timeout.
# A manifest that still fails is FAILED with cosign's last error line and the
# number of attempts.
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
#   VERIFY_ATTEMPTS      `cosign verify` runs per manifest, at most (default 3)
#   VERIFY_RETRY_INTERVAL
#                        seconds before the first re-run, doubled for each
#                        further one (default 2: 2 s, then 4 s)
#   VERIFY_RETRY_BUDGET  re-runs one invocation may spend in all (default 12)
#   REGISTRY_FETCH_ATTEMPTS, REGISTRY_FETCH_INTERVAL
#                        the same for the registry lookups (registry-lib.sh)
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

attempts="${VERIFY_ATTEMPTS:-3}"
interval="${VERIFY_RETRY_INTERVAL:-2}"
budget="${VERIFY_RETRY_BUDGET:-12}"
for knob in "$attempts" "$interval" "$budget"; do
	if ! [[ "$knob" =~ ^[0-9]+$ ]]; then
		echo "::error::VERIFY_ATTEMPTS, VERIFY_RETRY_INTERVAL and VERIFY_RETRY_BUDGET must be whole numbers (got '${knob}')" >&2
		exit 2
	fi
done
[ "$attempts" -ge 1 ] || attempts=1

verified=0
failed=0

# Did cosign fetch a signature and reject it? The one failure another attempt
# cannot change: the certificate's identity or issuer is not the one asked for
# (cosign v3.1.2: "no matching attestations: failed to verify certificate
# identity: no matching CertificateIdentity found, ..."; cosign 2 said "none of
# the expected identities matched"). Everything else is retried.
is_identity_mismatch() {
	grep -E 'no matching CertificateIdentity found|none of the expected identities matched' "$1" >/dev/null
}

verify_one() {
	local what="$1" ref="$2" err i=1 wait="$interval" last
	err="$(mktemp)"
	while :; do
		if "$cosign_bin" verify \
			--certificate-identity-regexp "$identity" \
			--certificate-oidc-issuer "$issuer" \
			"$ref" >/dev/null 2>"$err"; then
			if [ "$i" -gt 1 ]; then
				echo "  verified ${what} ${ref} (attempt ${i} of ${attempts})"
			else
				echo "  verified ${what} ${ref}"
			fi
			verified=$((verified + 1))
			break
		fi
		last="$(tail -1 "$err" | tr -c '[:print:]' ' ' | sed 's/ *$//')"
		if is_identity_mismatch "$err" || [ "$i" -ge "$attempts" ] || [ "$budget" -le 0 ]; then
			echo "  FAILED   ${what} ${ref}: ${last:-cosign printed nothing} (${i} attempt(s))"
			echo "::error::signature did not verify: ${what} ${ref}"
			failed=$((failed + 1))
			break
		fi
		budget=$((budget - 1))
		echo "  retrying ${what} ${ref} in ${wait}s (attempt ${i} of ${attempts} failed: ${last:-cosign printed nothing})"
		sleep "$wait"
		wait=$((wait * 2))
		i=$((i + 1))
	done
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
		echo "::error::could not obtain a pull token for ${repo} on ${REGISTRY_HOST} (the registry-lib line above says what the token endpoint answered)" >&2
		exit 2
	fi
	if ! children="$(registry_index_children "$repo" "$digest" "$tok")" || [ -z "$children" ]; then
		echo "::error::could not enumerate the child manifests of ${ref} (not an index, no children, or the lookup failed: see any registry-lib line above)" >&2
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
