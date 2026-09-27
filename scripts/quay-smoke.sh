#!/usr/bin/env bash
# Quay smoke: push + keyless sign + verify ONE throwaway image on quay.io before
# the real cut-over (proposal 040, phase 2 gate). Driven step by step by
# .github/workflows/quay-smoke.yaml (workflow_dispatch only, environment
# `release`); see docs/runbook.md, "Quay smoke".
#
# It answers the three things phase 2 cannot assume:
#   (a) can the robot account CREATE a repository in the org on first push?
#   (b) does a repository it creates come out PUBLIC (anonymous pulls, the
#       publish-verify sweep's witness check)?
#   (c) where does cosign v3.1.2 put a signature on a registry that serves the
#       OCI 1.1 Referrers API: `referrer`, a fallback tag (`bundle`/`legacy`),
#       or `both`? That decides phase 2's verify path.
#
# THE IMAGE. //e2e/l4echo:smoke_push: the l4echo test image's rules_img
# multi-arch index (amd64 + arm64), pushed with this repository's own rules_img
# push to a destination given by flags. Same builder, same index shape and the
# same push path as every released image, so a green smoke is evidence about
# the real thing; l4echo itself is never released.
#
# THE REGISTRY. quay.io explicitly (SMOKE_REGISTRY / SMOKE_ORG), NOT the
# single setting in bazel/img/registry.bzl: the smoke ran before the flip, while
# the setting still said ghcr.io, and it targets the throwaway `smoke`
# repository, not a published one. Host and org are separate variables on
# purpose — `<host>/<org>` IS the setting now, and
# scripts/check-registry-config.sh forbids spelling it out anywhere else.
#
# SUBCOMMANDS (each one a workflow step; state is carried between them in
# $QUAY_SMOKE_STATE, KEY=VALUE lines):
#   preflight  robot pull token, did the repository exist before, the Actions
#              OIDC subject (the token's `sub`, never the token)
#   push       bazel run //e2e/l4echo:smoke_push; digest; created?; public?
#   sign       cosign sign --recursive by digest; signature layout of the index
#              and every child, read from the registry (referrers + both tags)
#   verify     //tools/cosign:verify_image_signatures (index AND children) with
#              this workflow's identity; the signing certificate's SAN, read
#              from the sigstore bundle (`cosign download signature` + openssl)
#   cleanup    delete the run's tag and any cosign fallback tags it produced;
#              the repository itself stays
#   summary    Markdown table to $GITHUB_STEP_SUMMARY (or stdout); exit 1 unless
#              every gate held — a private repository is a RED, not a skip
#
# ENVIRONMENT
#   REGISTRY_USERNAME / REGISTRY_PASSWORD  the Quay robot (secrets QUAY_USERNAME
#                        / QUAY_TOKEN). Never printed; GitHub masks them, and
#                        every bearer token derived from them is masked too.
#   SMOKE_REGISTRY       default quay.io
#   SMOKE_ORG            default aethermesh
#   SMOKE_TAG            default ${GITHUB_RUN_ID} (the workflow adds -<attempt>)
#   BUILDBUDDY_API_KEY   optional; adds --config=remote to the image build
#   BAZEL                default bazel
#   QUAY_SMOKE_STATE     default ${RUNNER_TEMP:-/tmp}/quay-smoke.state
#
# Exit: 0 ok, 1 a gate failed (push denied, private repository, sign or verify
# failed), 2 could not run (usage, missing credentials, unanswered lookup).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${here}/.." && pwd)"

SMOKE_REGISTRY="${SMOKE_REGISTRY:-quay.io}"
SMOKE_ORG="${SMOKE_ORG:-aethermesh}"
SMOKE_TAG="${SMOKE_TAG:-${GITHUB_RUN_ID:-local}}"
smoke_repo="${SMOKE_ORG}/smoke"
state="${QUAY_SMOKE_STATE:-${RUNNER_TEMP:-/tmp}/quay-smoke.state}"
bazel="${BAZEL:-bazel}"

# The library reads REGISTRY_HOST at call time; pin it to the smoke target.
REGISTRY_HOST="$SMOKE_REGISTRY"
export REGISTRY_HOST
# shellcheck source=scripts/registry-lib.sh
. "${here}/registry-lib.sh"

# --- state -----------------------------------------------------------------

# Values are words, digests and short phrases; %q keeps the file safe to source.
put() { printf '%s=%q\n' "$1" "$2" >>"$state"; }
load() {
	# shellcheck disable=SC1090
	[ -r "$state" ] && . "$state"
	return 0
}

err() { echo "::error::$*" >&2; }
note() { echo "::notice::$*"; }

need_creds() {
	if [ -z "${REGISTRY_USERNAME:-}" ] || [ -z "${REGISTRY_PASSWORD:-}" ]; then
		err "REGISTRY_USERNAME / REGISTRY_PASSWORD are not set (secrets QUAY_USERNAME / QUAY_TOKEN, environment 'release' — only readable on a run from main)"
		exit 2
	fi
}

mask() {
	if [ -n "$1" ]; then echo "::add-mask::$1"; fi
}

# A robot bearer token for <scope> (pull, or pull,push for cleanup).
robot_token() {
	local scope="$1" url tok
	url="https://${SMOKE_REGISTRY}/v2/auth?service=${SMOKE_REGISTRY}&scope=repository:${smoke_repo}:${scope}"
	tok="$(curl -fsS -u "${REGISTRY_USERNAME}:${REGISTRY_PASSWORD}" "$url" | registry__json_str token)" || return 2
	# NOT masked here: this function's stdout is what the caller captures, so a
	# ::add-mask:: line printed from inside it becomes part of the token value
	# (the 2026-09-27 first dispatch: curl 43 on every request). Callers mask.
	printf '%s\n' "$tok"
}

anon_token() {
	local tok
	tok="$(curl -fsS "$(registry_token_url "$smoke_repo")" | registry__json_str token)" || return 2
	printf '%s\n' "$tok"
}

# HTTP code of GET /v2/<repo>/tags/list with <token> (000 on no answer).
tags_code() {
	curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" \
		"https://${SMOKE_REGISTRY}/v2/${smoke_repo}/tags/list?n=1" || true
}

# --- subcommands -------------------------------------------------------------

cmd_preflight() {
	need_creds
	: >"$state"
	put SMOKE_REF_REPO "${SMOKE_REGISTRY}/${smoke_repo}"
	put SMOKE_TAG "$SMOKE_TAG"

	local tok code existed
	if ! tok="$(robot_token pull)" || [ -z "$tok" ]; then
		err "the robot account could not get a pull token from ${SMOKE_REGISTRY}/v2/auth — wrong QUAY_USERNAME (it is '<org>+<robot>') or QUAY_TOKEN"
		exit 2
	fi
	mask "$tok"
	code="$(tags_code "$tok")"
	case "$code" in
	200) existed=yes ;;
	401 | 403 | 404) existed=no ;;
	*)
		err "GET ${SMOKE_REGISTRY}/v2/${smoke_repo}/tags/list answered '${code}' — cannot tell whether the repository exists"
		exit 2
		;;
	esac
	echo "repository ${SMOKE_REGISTRY}/${smoke_repo} existed before this run: ${existed} (robot tags/list: ${code})"
	put SMOKE_EXISTED "$existed"

	# The Actions OIDC token cosign will exchange with Fulcio. Print its `sub`
	# (with `environment: release` it is repo:<owner>/<repo>:environment:release)
	# and its workflow ref — NEVER the token. The certificate SAN is NOT the sub;
	# `verify` prints the SAN cosign actually reports.
	if [ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ] && [ -n "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ]; then
		local jwt sub
		jwt="$(curl -fsS -H "Authorization: bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
			"${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=sigstore" | registry__json_str value)" || jwt=""
		if [ -n "$jwt" ]; then
			mask "$jwt"
			sub="$(printf '%s' "$jwt" | python3 -c '
import base64, json, sys
p = sys.stdin.read().split(".")[1]
c = json.loads(base64.urlsafe_b64decode(p + "=" * (-len(p) % 4)))
print("sub=%s job_workflow_ref=%s" % (c.get("sub", "?"), c.get("job_workflow_ref", "?")))
')" || sub="(undecodable)"
			echo "OIDC token: ${sub}"
			put SMOKE_OIDC "$sub"
		fi
	else
		echo "no Actions OIDC token in this environment (needs permissions: id-token: write)"
	fi
}

cmd_push() {
	need_creds
	load
	local flags=() rc=0
	if [ -n "${BUILDBUDDY_API_KEY:-}" ]; then
		flags+=(--config=remote "--remote_header=x-buildbuddy-api-key=${BUILDBUDDY_API_KEY}")
	fi
	echo "pushing //e2e/l4echo:smoke_push -> ${SMOKE_REGISTRY}/${smoke_repo}:${SMOKE_TAG}"
	(cd "$repo_root" && "$bazel" run "${flags[@]}" //e2e/l4echo:smoke_push \
		"--//e2e/l4echo:smoke_registry=${SMOKE_REGISTRY}" \
		"--//e2e/l4echo:smoke_repository=${smoke_repo}" \
		"--//e2e/l4echo:smoke_tag=${SMOKE_TAG}") || rc=$?
	if [ "$rc" -ne 0 ]; then
		put SMOKE_PUSH denied
		err "push to ${SMOKE_REGISTRY}/${smoke_repo} FAILED (bazel exit ${rc}). If the log says denied/unauthorized: pre-create the repository '${smoke_repo}' as PUBLIC and give the robot write on it, or give the robot the org 'creator' role (and set the org's default repository visibility to public) so a first push can create it."
		exit 1
	fi
	put SMOKE_PUSH ok

	local tok digest built=""
	tok="$(robot_token pull)" || {
		err "no robot pull token after the push"
		exit 2
	}
	mask "$tok"
	digest="$(registry_manifest_digest "$smoke_repo" "$SMOKE_TAG" "$tok")" || digest=""
	if ! [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
		err "could not resolve ${SMOKE_REGISTRY}/${smoke_repo}:${SMOKE_TAG} to a digest after the push (got '${digest}')"
		exit 2
	fi
	# What Bazel says it pushed must be what the registry holds under the tag.
	if [ -r "${repo_root}/bazel-bin/e2e/l4echo/smoke_push.json" ]; then
		built="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["operations"][0]["root"]["digest"])' \
			"${repo_root}/bazel-bin/e2e/l4echo/smoke_push.json" 2>/dev/null)" || built=""
		if [ -n "$built" ] && [ "$built" != "$digest" ]; then
			err "the registry resolves ${SMOKE_TAG} to ${digest} but Bazel pushed ${built}"
			exit 1
		fi
	fi
	echo "pushed ${SMOKE_REGISTRY}/${smoke_repo}@${digest}"
	put SMOKE_DIGEST "$digest"
	if [ "${SMOKE_EXISTED:-}" = no ]; then
		put SMOKE_CREATED yes
		note "the push CREATED ${SMOKE_REGISTRY}/${smoke_repo}: the robot can create repositories in ${SMOKE_ORG}"
	else
		put SMOKE_CREATED "no (already existed)"
	fi

	# Public? The Quay API answers anonymously for a public repository only; a
	# private one (or one this token cannot see) is a 401/403/404. Confirm with
	# what actually matters downstream: an ANONYMOUS pull of the tag.
	local api code is_public atok anon
	api="$(mktemp)"
	code="$(curl -sS -o "$api" -w '%{http_code}' "https://${SMOKE_REGISTRY}/api/v1/repository/${smoke_repo}")" || code=000
	if [ "$code" = 200 ]; then
		is_public="$(python3 -c 'import json,sys;print(str(json.load(open(sys.argv[1])).get("is_public")).lower())' "$api")" || is_public=unknown
	else
		is_public="false (anonymous API answered ${code})"
	fi
	rm -f "$api"
	anon=no
	if atok="$(anon_token)" && [ -n "$atok" ] && registry_tag_exists "$smoke_repo" "$SMOKE_TAG" "$atok"; then
		anon=yes
	fi
	echo "is_public: ${is_public}; anonymous pull of the tag: ${anon}"
	if [ "$is_public" = true ] && [ "$anon" = yes ]; then
		put SMOKE_PUBLIC yes
	else
		put SMOKE_PUBLIC "no (is_public=${is_public}, anonymous pull=${anon})"
		err "${SMOKE_REGISTRY}/${smoke_repo} is NOT publicly pullable (is_public=${is_public}, anonymous pull=${anon}). Every aether repository must be public: pre-create each one as public (Quay: repository settings -> Make Public), or set the org's default visibility to public before the robot creates them. The job continues so sign/verify still answer, then FAILS in the summary."
	fi
}

# Signature layout of one digest, from the registry: `referrer`, `bundle`,
# `legacy`, `both` or `none`, plus the referrers' artifactTypes for the record.
layout_of() {
	local digest="$1" tok="$2" lay refs types rc=0
	lay="$(registry_signature_layout_direct "$smoke_repo" "$digest" "$tok")" || lay="unknown"
	refs="$(registry_referrers "$smoke_repo" "$digest" "$tok")" || rc=$?
	case "$rc" in
	0) types="$(printf '%s' "$refs" | python3 -c 'import json,sys;print(",".join(sorted({m.get("artifactType","?") for m in json.load(sys.stdin)["manifests"]})) or "-")')" ;;
	1) types="(no referrers API)" ;;
	*) types="(unanswered)" ;;
	esac
	printf '%s %s\n' "$lay" "$types"
}

cmd_sign() {
	need_creds
	load
	[ -n "${SMOKE_DIGEST:-}" ] || {
		err "no digest recorded — the push step did not complete"
		exit 2
	}
	local ref="${SMOKE_REGISTRY}/${smoke_repo}@${SMOKE_DIGEST}" tok before children child line lay types all=""
	tok="$(robot_token pull)" || exit 2
	mask "$tok"

	# The index digest is deterministic per commit, so a re-run at the same
	# commit signs a digest that may already carry an earlier run's signature.
	# Say so, rather than let a stale layout pass as this run's answer.
	before="$(layout_of "$SMOKE_DIGEST" "$tok")"
	echo "layout BEFORE signing: ${before}"
	case "$before" in
	none\ *) put SMOKE_PRESIGNED no ;;
	*)
		put SMOKE_PRESIGNED "yes (${before%% *})"
		echo "::warning::${ref} already carried a signature before this run (an earlier smoke at the same commit); the layout below includes it"
		;;
	esac

	echo "signing ${ref} (--recursive: the index and every child)"
	if ! (cd "$repo_root" && COSIGN_YES=true "$bazel" run //tools/cosign -- sign --yes --recursive "$ref"); then
		put SMOKE_SIGN failed
		err "cosign sign failed for ${ref}"
		exit 1
	fi
	put SMOKE_SIGN ok

	# (c): where did the signature go? Index and every child, each read as
	# referrers API + BOTH tag shapes (the phase-1 library).
	line="$(layout_of "$SMOKE_DIGEST" "$tok")"
	lay="${line%% *}"
	types="${line#* }"
	echo "index ${SMOKE_DIGEST}: layout=${lay} referrer artifactTypes=${types}"
	put SMOKE_LAYOUT "$lay"
	put SMOKE_REFERRER_TYPES "$types"
	all="$lay"
	children="$(registry_index_children "$smoke_repo" "$SMOKE_DIGEST" "$tok")" || {
		err "could not enumerate the children of ${ref}"
		exit 2
	}
	while read -r child; do
		line="$(layout_of "$child" "$tok")"
		echo "child ${child}: layout=${line%% *} referrer artifactTypes=${line#* }"
		[ "${line%% *}" = "$lay" ] || all="mixed"
	done <<<"$children"
	put SMOKE_LAYOUT_CHILDREN "$all"
	case "$lay" in
	referrer | bundle | legacy) echo "signature layout on ${SMOKE_REGISTRY}: ${lay}" ;;
	both) echo "::warning::signature layout on ${SMOKE_REGISTRY}: both (a referrer AND a fallback tag) — the sweep reads that as the double-write defect; phase 2 must decide which one it asserts" ;;
	*)
		err "no signature found for ${ref} after a successful sign (layout ${lay})"
		exit 1
		;;
	esac
}

cmd_verify() {
	load
	[ -n "${SMOKE_DIGEST:-}" ] || {
		err "no digest recorded — the push step did not complete"
		exit 2
	}
	local ref="${SMOKE_REGISTRY}/${smoke_repo}@${SMOKE_DIGEST}" repo="${GITHUB_REPOSITORY:-bpalermo/aether}"
	local identity out san rc=0
	identity="^https://github\\.com/${repo//./\\.}/\\.github/workflows/quay-smoke\\.yaml@refs/heads/main$"
	echo "verifying ${ref} (index and every child) as ${identity}"
	(cd "$repo_root" && CERT_IDENTITY_REGEXP="$identity" \
		CERT_OIDC_ISSUER=https://token.actions.githubusercontent.com \
		"$bazel" run //tools/cosign:verify_image_signatures -- "$ref") || rc=$?
	if [ "$rc" -ne 0 ]; then
		put SMOKE_VERIFY "failed (exit ${rc})"
		err "verify_image_signatures failed for ${ref} (exit ${rc})"
	else
		put SMOKE_VERIFY ok
	fi

	# The SAN of the signing certificate (Fulcio puts the workflow ref there,
	# not the OIDC `sub`). cosign v3's `verify` JSON is `[{"critical": ...}]`
	# only -- no `optional` block, so no Subject to read there (it printed `?`
	# for this row until the fix). The certificate is in the sigstore bundle
	# the signature IS: `cosign download signature` prints one bundle per
	# signature, and its leaf certificate's SAN URI is what `verify` matched.
	local dl
	dl="$(mktemp)"
	san=""
	if (cd "$repo_root" && "$bazel" run //tools/cosign -- download signature "$ref") >"$dl" 2>/dev/null; then
		san="$(bundle_sans "$dl")" || san=""
	fi
	rm -f "$dl"
	if [ -z "$san" ]; then
		put SMOKE_SAN "(unreadable: no certificate SAN in the downloaded signature bundle)"
		put SMOKE_SAN_OK no
		err "could not read the signing certificate's SAN for ${ref}"
		rc=1
	elif printf '%s\n' "${san//, /$'\n'}" | grep -qE -- "$identity"; then
		echo "certificate SAN: ${san}"
		put SMOKE_SAN "$san"
		put SMOKE_SAN_OK yes
	else
		put SMOKE_SAN "${san} (does NOT match ${identity})"
		put SMOKE_SAN_OK no
		err "the signing certificate's SAN (${san}) does not match ${identity}"
		rc=1
	fi
	[ "$rc" -eq 0 ] || exit 1
}

# The SAN URI(s) of the signing certificate(s) in `cosign download signature`
# output (<file>: one sigstore bundle per line), ", "-joined. Reads the leaf
# from `verificationMaterial.certificate.rawBytes` (bundle v0.3) or the first of
# `verificationMaterial.x509CertificateChain.certificates` (v0.1/v0.2), DER,
# base64. Fails (nothing printed) when no certificate or no URI SAN is found:
# the summary must never show a vacuous `?`.
bundle_sans() {
	local dl="$1" dir f uri sans=""
	dir="$(mktemp -d)"
	if ! python3 - "$dl" "$dir" <<'PY'
import base64, json, sys
src, out = sys.argv[1], sys.argv[2]
n = 0
for line in open(src):
    line = line.strip()
    if not line:
        continue
    try:
        doc = json.loads(line)
    except ValueError:
        continue
    vm = doc.get("verificationMaterial") or {} if isinstance(doc, dict) else {}
    raw = (vm.get("certificate") or {}).get("rawBytes")
    if not raw:
        chain = (vm.get("x509CertificateChain") or {}).get("certificates") or []
        raw = chain[0].get("rawBytes") if chain else None
    if raw:
        n += 1
        with open("%s/%d.der" % (out, n), "wb") as fh:
            fh.write(base64.b64decode(raw))
sys.exit(0 if n else 1)
PY
	then
		rm -rf "$dir"
		return 1
	fi
	for f in "$dir"/*.der; do
		uri="$(openssl x509 -inform DER -noout -ext subjectAltName -in "$f" 2>/dev/null |
			sed -nE 's/^[[:space:]]*URI:([^,]+).*$/\1/p' | head -1)"
		[ -n "$uri" ] || continue
		case ", ${sans}, " in *", ${uri}, "*) ;; *) sans="${sans:+${sans}, }${uri}" ;; esac
	done
	rm -rf "$dir"
	[ -n "$sans" ] || return 1
	printf '%s\n' "$sans"
}

cmd_cleanup() {
	load
	if [ -z "${REGISTRY_USERNAME:-}" ] || [ -z "${REGISTRY_PASSWORD:-}" ] || [ "${SMOKE_PUSH:-}" != ok ]; then
		echo "nothing to clean up"
		put SMOKE_CLEANUP "skipped (nothing pushed)"
		return 0
	fi
	local tok code tags=("$SMOKE_TAG") failed=0 digests=() child t
	tok="$(robot_token pull,push)" || {
		echo "::warning::no robot push token; tag ${SMOKE_TAG} left behind"
		put SMOKE_CLEANUP "failed (no push token)"
		return 0
	}
	mask "$tok"
	# cosign fallback tags this run may have written (index and children).
	if [ -n "${SMOKE_DIGEST:-}" ]; then
		digests=("$SMOKE_DIGEST")
		while read -r child; do
			[ -n "$child" ] && digests+=("$child")
		done < <(registry_index_children "$smoke_repo" "$SMOKE_DIGEST" "$tok" 2>/dev/null || true)
		for child in "${digests[@]}"; do
			for t in "$(registry_signature_tag_bundle "$child")" "$(registry_signature_tag_legacy "$child")"; do
				registry_tag_exists "$smoke_repo" "$t" "$tok" 2>/dev/null && tags+=("$t")
			done
		done
	fi
	for t in "${tags[@]}"; do
		code="$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer ${tok}" \
			"https://${SMOKE_REGISTRY}/v2/${smoke_repo}/manifests/${t}")" || code=000
		case "$code" in
		200 | 202 | 204) echo "deleted tag ${t}" ;;
		*)
			echo "::warning::DELETE ${SMOKE_REGISTRY}/v2/${smoke_repo}/manifests/${t} answered ${code}; tag left behind"
			failed=$((failed + 1))
			;;
		esac
	done
	if [ "$failed" -eq 0 ]; then
		put SMOKE_CLEANUP "deleted ${#tags[@]} tag(s)"
	else
		put SMOKE_CLEANUP "failed for ${failed} of ${#tags[@]} tag(s)"
	fi
}

cmd_summary() {
	load
	local out="${GITHUB_STEP_SUMMARY:-/dev/stdout}" ok=1
	{
		echo "## Quay smoke — ${SMOKE_REF_REPO:-${SMOKE_REGISTRY}/${smoke_repo}}:${SMOKE_TAG}"
		echo ""
		echo "| check | result |"
		echo "|---|---|"
		echo "| repository existed before | ${SMOKE_EXISTED:-?} |"
		echo "| push | ${SMOKE_PUSH:-not run} |"
		echo "| repository created by this push | ${SMOKE_CREATED:-?} |"
		echo "| public (anonymous pull) | ${SMOKE_PUBLIC:-?} |"
		echo "| digest | \`${SMOKE_DIGEST:-?}\` |"
		echo "| signed before this run | ${SMOKE_PRESIGNED:-?} |"
		echo "| sign | ${SMOKE_SIGN:-not run} |"
		echo "| signature layout (index) | ${SMOKE_LAYOUT:-?} |"
		echo "| signature layout (index + children) | ${SMOKE_LAYOUT_CHILDREN:-?} |"
		echo "| referrer artifactTypes | ${SMOKE_REFERRER_TYPES:-?} |"
		echo "| verify (index + every child) | ${SMOKE_VERIFY:-not run} |"
		echo "| certificate SAN | ${SMOKE_SAN:-(not read — verify did not run)} |"
		echo "| OIDC token | ${SMOKE_OIDC:-?} |"
		echo "| cleanup | ${SMOKE_CLEANUP:-not run} |"
	} >>"$out"
	[ "${SMOKE_PUSH:-}" = ok ] || ok=0
	[ "${SMOKE_PUBLIC:-}" = yes ] || ok=0
	[ "${SMOKE_SIGN:-}" = ok ] || ok=0
	[ "${SMOKE_VERIFY:-}" = ok ] || ok=0
	# The SAN row must be READ, not assumed: an unreadable or mismatched
	# certificate SAN fails the smoke (it used to print `?` and pass).
	[ "${SMOKE_SAN_OK:-}" = yes ] || ok=0
	case "${SMOKE_LAYOUT:-}" in referrer | bundle | legacy | both) ;; *) ok=0 ;; esac
	if [ "$ok" -ne 1 ]; then
		err "Quay smoke FAILED: push=${SMOKE_PUSH:-?} public=${SMOKE_PUBLIC:-?} sign=${SMOKE_SIGN:-?} layout=${SMOKE_LAYOUT:-?} verify=${SMOKE_VERIFY:-?} san=${SMOKE_SAN_OK:-unread}"
		exit 1
	fi
	echo "Quay smoke PASSED: layout=${SMOKE_LAYOUT}"
}

case "${1:-}" in
preflight | push | sign | verify | cleanup | summary) "cmd_$1" ;;
*)
	echo "usage: $(basename "$0") preflight|push|sign|verify|cleanup|summary" >&2
	exit 2
	;;
esac
