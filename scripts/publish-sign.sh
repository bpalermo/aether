#!/usr/bin/env bash
# Sign what ONE COMMIT's publish pushed, keyless -- but only what is not signed
# already (#1378). The sign steps of .github/workflows/publish.yaml.
#
#   scripts/publish-sign.sh images <full sha> <out file>
#   scripts/publish-sign.sh charts <full sha> <out file>
#
# WHY "ONLY WHAT IS NOT SIGNED". While every image carried its commit, every
# publish produced nine digests nobody had seen and signed each once. Without
# the commit in the image, an image whose inputs did not change keeps its digest
# from one commit to the next, and signing it on every publish would attach one
# more signature referrer to the same manifest each time (about seventeen a
# day). A re-run of a publish has the same shape today. So each artifact is
# asked first, and signed only when the answer is not "yes, all of it".
#
# WHAT "ALREADY SIGNED" MEANS: the manifest, and for an image EVERY child
# manifest its index lists, verifies under this workflow's identity and issuer.
# One unsigned child, or a signature by another identity, and the artifact is
# signed again (`cosign sign --recursive` signs the whole tree; the manifests
# that had a signature gain a second one, which is harmless, where an unsigned
# child is not). Two questions per artifact, cheapest first:
#
#   1. The registry's referrers index of each manifest (registry_referrers, one
#      GET). An index with no signature referrer is the registry's own answer
#      "nothing is attached": not signed, and no cosign run is spent on it. This
#      is every artifact of a publish whose digests are all new. A registry
#      without the Referrers API (404) cannot answer; question 2 decides alone.
#   2. //bazel/cosign:verify_image_signatures on the reference -- the same
#      script, identity and issuer the verify steps use, children walked. Only
#      asked when every manifest has something attached. Measured against
#      quay.io with the pinned cosign v3.1.2: 3.4 to 4.5 s per `cosign verify`,
#      signed, unsigned or signed by someone else, against 0.5 s for a referrers
#      lookup; nine images are 27 manifests.
#
# AN UNANSWERED QUESTION IS NOT "SIGNED". A lookup that fails, an index whose
# children cannot be listed, a verification that could not be performed: the
# artifact is signed now and the line says why. The worst that does is one
# duplicate signature; reading an outage as "signed" would ship an unsigned
# image.
#
# NOTHING HERE DECIDES WHETHER THE RUN IS GOOD. Every reference, signed now or
# before, goes to <out file>, and the workflow's verify steps verify every line
# of it afterwards, exactly as they did when everything was signed on the spot.
#
# It also writes <out file>.state, `<ref> TAB <tag[,tag]> TAB
# <signed-now|already-signed>` per artifact: the tag the digest was resolved
# from and what this run did, for the run summary
# (scripts/publish-provenance.sh summary). Nothing verifies from it.
#
# One line per artifact:
#   already signed: <ref> (first signature attached <time>; N signature(s) ...)
#   signing now:    <ref> (<why>)
# "first signature attached" is the oldest `org.opencontainers.image.created`
# among the signature referrers the registry lists. The commit that built the
# digest first is not in that listing; the provenance check prints it.
#
# Environment:
#   BAZEL                 the bazel to run cosign through (default `bazel`)
#   CERT_IDENTITY_REGEXP, CERT_OIDC_ISSUER
#                         passed through to the verifier (its defaults are
#                         publish.yaml of $GITHUB_REPOSITORY and GitHub's OIDC)
#   REGISTRY_USERNAME, REGISTRY_PASSWORD   optional (scripts/registry-lib.sh)
#   COSIGN_YES, and the Actions OIDC variables keyless signing reads
#
# Exit: 0 every artifact is in <out file>; 1 a tag or digest did not resolve,
# or a `cosign sign` failed (what was signed before it stays signed, and a
# re-run skips it); 2 usage, or no registry setting.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/registry-lib.sh
. "${here}/registry-lib.sh"

bazel="${BAZEL:-bazel}"
kind="${1:-}"
commit="${2:-}"
out="${3:-}"

if { [ "$kind" != images ] && [ "$kind" != charts ]; } || ! [[ "$commit" =~ ^[0-9a-f]{40}$ ]] || [ -z "$out" ]; then
	echo "usage: $(basename "$0") <images|charts> <full 40-char commit sha> <out file>" >&2
	exit 2
fi
if [ -z "${REGISTRY_HOST:-}" ]; then
	echo "::error::no registry host (scripts/image-registry.sh could not read bazel/registry/registry.bzl)" >&2
	exit 2
fi

refs=()
# tags[i]: the tag(s) refs[i] was resolved from, comma-separated.
tags=()
if [ "$kind" = images ]; then
	# A missing entry in REGISTRY_IMAGE_REPOS silently ships an UNSIGNED image,
	# so the verify step re-reads what this loop records, never the list.
	if [ "${#REGISTRY_IMAGE_REPOS[@]}" -eq 0 ]; then
		echo "::error::no image repositories (scripts/image-registry.sh could not read bazel/registry/registry.bzl)"
		exit 1
	fi
	for repo in "${REGISTRY_IMAGE_REPOS[@]}"; do
		if ! tok="$(registry_registry_token "$repo")" || [ -z "$tok" ]; then
			echo "::error::could not obtain a pull token for ${repo} on ${REGISTRY_HOST} (the registry-lib line above says what the token endpoint answered)"
			exit 1
		fi
		# The commit-suffixed tag (`<tag>-<full sha>`) is the only immutable one
		# -- `dev` is rewritten by every publish (#692). registry_commit_tag
		# re-lists for ~60 s (the listing can lag the push) and prints its own
		# ::error:: on a final miss (#1046).
		if ! tag="$(registry_commit_tag "$repo" "$tok" "$commit")"; then
			exit 1
		fi
		# The INDEX DIGEST, not the tag: signing a tag re-resolves it inside
		# cosign, a TOCTOU window against a mutable tag. `|| true`: the helper
		# is a `curl -f | ...` pipeline, and a failed HEAD must reach the error
		# line below rather than end the script without one (#1046).
		digest="$(registry_manifest_digest "$repo" "$tag" "$tok" || true)"
		if ! [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
			echo "::error::could not resolve a digest for ${REGISTRY_HOST}/${repo}:${tag}"
			exit 1
		fi
		echo "resolved ${REGISTRY_HOST}/${repo}:${tag} -> ${digest}"
		refs+=("${REGISTRY_HOST}/${repo}@${digest}")
		tags+=("$tag")
	done
else
	# Prints nothing on stdout unless EVERY chart tag resolved. One line per
	# tag; two tags of one digest are one reference to sign.
	listed="$("${here}/published-chart-refs.sh" --tags "$commit")" || exit 1
	while read -r line tag; do
		[ -n "$line" ] || continue
		seen=0
		for i in "${!refs[@]}"; do
			if [ "${refs[$i]}" = "$line" ]; then
				tags[i]="${tags[$i]},${tag}"
				seen=1
			fi
		done
		if [ "$seen" -eq 0 ]; then
			refs+=("$line")
			tags+=("$tag")
		fi
	done <<<"$listed"
fi
if [ "${#refs[@]}" -eq 0 ]; then
	echo "::error::no ${kind} references resolved -- nothing to sign"
	exit 1
fi

# The signature referrers in a referrers index on stdin: `<count> <oldest
# created annotation, or ->`. Exits 1 on a document that is not an index. The
# same test of "is a signature" as registry-lib.sh: cosign 2's artifactType, or
# a sigstore bundle whose predicate is cosign's sign predicate (a bundle with
# any other predicate is an attestation).
signature_referrers() {
	python3 -c '
import json, sys
bundle, sig, pred = sys.argv[1:4]
doc = json.load(sys.stdin)
kids = doc.get("manifests") if isinstance(doc, dict) else None
if not isinstance(kids, list):
    sys.exit(1)
created = []
n = 0
for m in kids:
    if not isinstance(m, dict):
        continue
    ann = m.get("annotations") or {}
    at = m.get("artifactType", "")
    if at == sig or (at == bundle and ann.get("dev.sigstore.bundle.predicateType") == pred):
        n += 1
        if isinstance(ann.get("org.opencontainers.image.created"), str):
            created.append(ann["org.opencontainers.image.created"])
print(n, min(created) if created else "-")
' "$REGISTRY_SIGSTORE_BUNDLE_TYPE" "$REGISTRY_COSIGN_SIG_TYPE" "$REGISTRY_COSIGN_SIGN_PREDICATE"
}

# Is <ref> signed already -- the manifest and, for an image, every child?
#   0  yes               (note: when, and how many signatures)
#   1  no                (note: which manifest, and what was found)
#   2  could not tell    (note: which question went unanswered)
# The answer's reason is left in $note.
note=""
already_signed() {
	local ref="$1" repo digest tok children="" m rc listing count created first="-" total=0
	local log
	repo="${ref#*/}"
	repo="${repo%@*}"
	digest="${ref##*@}"
	if ! tok="$(registry_registry_token "$repo")" || [ -z "$tok" ]; then
		note="no pull token for ${repo}"
		return 2
	fi
	if [ "$kind" = images ]; then
		if ! children="$(registry_index_children "$repo" "$digest" "$tok")" || [ -z "$children" ]; then
			note="its child manifests could not be listed"
			return 2
		fi
	fi
	while read -r m; do
		[ -n "$m" ] || continue
		rc=0
		listing="$(registry_referrers "$repo" "$m" "$tok")" || rc=$?
		case "$rc" in
		0)
			if ! read -r count created < <(printf '%s' "$listing" | signature_referrers) || ! [[ "${count:-}" =~ ^[0-9]+$ ]]; then
				note="the referrers of ${m} are not readable"
				return 2
			fi
			if [ "$count" -eq 0 ]; then
				if [ "$m" = "$digest" ]; then
					note="no signature attached"
				else
					note="child ${m} has no signature attached"
				fi
				return 1
			fi
			if [ "$m" = "$digest" ]; then
				first="$created"
				total="$count"
			fi
			;;
		1) ;; # No Referrers API on this registry: cosign decides, below.
		*)
			note="the referrers lookup of ${m} went unanswered"
			return 2
			;;
		esac
	done <<<"${digest}"$'\n'"${children}"

	# Something is attached to every manifest. Whether it is THIS workflow's
	# signature is cosign's answer, through the verifier the verify steps run.
	log="$(mktemp)"
	rc=0
	if [ "$kind" = charts ]; then
		"$bazel" run //bazel/cosign:verify_image_signatures -- --single "$ref" >"$log" 2>&1 || rc=$?
	else
		"$bazel" run //bazel/cosign:verify_image_signatures -- "$ref" >"$log" 2>&1 || rc=$?
	fi
	if [ "$rc" -ne 0 ]; then
		# The verifier's own lines, without its ::error:: prefix: this is a
		# question, and an annotation would mark a healthy run.
		grep -E 'FAILED|::error::' "$log" | sed -E 's/^(::error::)?[[:space:]]*/    | /' || true
	fi
	rm -f "$log"
	case "$rc" in
	0)
		if [ "$first" = "-" ]; then
			note="verified; the registry lists no time for its signature"
		else
			note="first signature attached ${first}; ${total} signature(s) on the manifest"
		fi
		return 0
		;;
	1)
		note="what is attached does not verify as this workflow's signature"
		return 1
		;;
	*)
		note="the verification could not be performed"
		return 2
		;;
	esac
}

: >"$out"
: >"${out}.state"
before=0
now=0
for i in "${!refs[@]}"; do
	ref="${refs[$i]}"
	did=already-signed
	state=0
	already_signed "$ref" || state=$?
	if [ "$state" -eq 0 ]; then
		echo "already signed: ${ref} (${note})"
		before=$((before + 1))
	else
		if [ "$state" -eq 1 ]; then
			echo "signing now:    ${ref} (${note})"
		else
			echo "signing now:    ${ref} (could not tell whether it is signed: ${note}; a second signature is harmless, an unsigned artifact is not)"
		fi
		# --recursive signs the index AND every child manifest: the images are
		# multi-arch, and a node pulls a child. A chart is one manifest.
		if [ "$kind" = images ]; then
			"$bazel" run //bazel/cosign -- sign --yes --recursive "$ref"
		else
			"$bazel" run //bazel/cosign -- sign --yes "$ref"
		fi
		now=$((now + 1))
		did=signed-now
	fi
	# The EXACT reference, for the verify step: it must not re-resolve a tag,
	# or it could pass against something other than what was signed.
	echo "$ref" >>"$out"
	printf '%s\t%s\t%s\n' "$ref" "${tags[$i]}" "$did" >>"${out}.state"
done

# A loop that records nothing, or less than it was given, must not report
# success (#853).
recorded="$(wc -l <"$out")"
if [ "$recorded" -ne "${#refs[@]}" ] || [ $((before + now)) -ne "${#refs[@]}" ]; then
	echo "::error::recorded ${recorded} of ${#refs[@]} ${kind} reference(s)"
	exit 1
fi
echo "${#refs[@]} ${kind} reference(s): ${now} signed now, ${before} already signed"
