#!/usr/bin/env bash
# Every chart reference ONE COMMIT's publish pushed, by digest, one per line:
#
#   <registry>/<chart repository>@sha256:<manifest digest>
#
# publish.yaml signs and attests exactly these lines, so the list is derived
# here, from the same two sources the publish itself uses, and never typed into
# the workflow:
#   - which charts:  REGISTRY_CHARTS (scripts/registry-lib.sh);
#   - which tags:    each chart's Chart.yaml AS OF <sha>. Every chart has its
#                    commit tag `X.Y.Z-<full sha>` (#692). A chart whose
#                    Chart.yaml carries a bare `X.Y.Z` (aether) is ALSO pushed
#                    under that bare tag, with a different Chart.yaml and so a
#                    different digest: that one is listed too, because it is the
#                    tag `helm pull --version X.Y.Z` resolves.
#
# The digest is the registry's answer for the tag, read right after the push
# (the publish job is serialised, so nothing else writes these tags meanwhile).
# A tag that does not resolve within the retries fails the script: a list that
# is one chart short would sign and attest less than it reads as (#853).
#
# Usage: scripts/published-chart-refs.sh [--tags] <full 40-char sha>
#
# --tags prints `<reference> <tag>` instead, one line per TAG (so a digest two
# tags resolve to is listed twice): what scripts/publish-sign.sh reads, so the
# publish summary can say which tag each digest was pushed under (#1378).
#
# Environment: REGISTRY_USERNAME / REGISTRY_PASSWORD (optional; registry-lib.sh),
#   CHART_REF_ATTEMPTS (default 6), CHART_REF_INTERVAL seconds (default 5).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/registry-lib.sh
. "${here}/registry-lib.sh"

with_tags=0
if [ "${1:-}" = "--tags" ]; then
	with_tags=1
	shift
fi
sha="${1:-}"
if ! [[ "$sha" =~ ^[0-9a-f]{40}$ ]]; then
	echo "usage: $(basename "$0") [--tags] <full 40-char commit sha>" >&2
	exit 2
fi
if [ "${#REGISTRY_CHARTS[@]}" -eq 0 ] || [ -z "${REGISTRY_HOST:-}" ]; then
	echo "::error::no charts or no registry host (scripts/image-registry.sh could not read bazel/registry/registry.bzl)" >&2
	exit 2
fi
attempts="${CHART_REF_ATTEMPTS:-6}"
interval="${CHART_REF_INTERVAL:-5}"

# Chart.yaml's version at <sha>, verbatim (it may hold the {GIT_COMMIT} stamp).
chart_version() {
	git show "${sha}:charts/$1/Chart.yaml" |
		sed -nE 's/^version:[[:space:]]*"?([^"[:space:]]+)"?.*/\1/p' | head -1
}

resolve() {
	local repo="$1" tag="$2" tok digest i=1
	while :; do
		digest=""
		if tok="$(registry_registry_token "$repo")" && [ -n "$tok" ]; then
			digest="$(registry_manifest_digest "$repo" "$tag" "$tok" || true)"
		fi
		if [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
			printf '%s/%s@%s\n' "$REGISTRY_HOST" "$repo" "$digest"
			return 0
		fi
		if [ "$i" -ge "$attempts" ]; then
			echo "::error::could not resolve a digest for ${REGISTRY_HOST}/${repo}:${tag} (${i} attempt(s))" >&2
			return 1
		fi
		echo "  ${REGISTRY_HOST}/${repo}:${tag} not readable yet (attempt ${i} of ${attempts}); retrying in ${interval}s" >&2
		sleep "$interval"
		i=$((i + 1))
	done
}

refs=()
tagged=()
for chart in "${REGISTRY_CHARTS[@]}"; do
	repo="$(registry_chart_repo "$chart")"
	version="$(chart_version "$chart")"
	if [ -z "$version" ]; then
		echo "::error::could not read charts/${chart}/Chart.yaml version at ${sha}" >&2
		exit 2
	fi
	case "$version" in
	*"{GIT_COMMIT}"*) tags=("${version//\{GIT_COMMIT\}/$sha}") ;;
	*) tags=("${version}-${sha}" "$version") ;;
	esac
	for tag in "${tags[@]}"; do
		ref="$(resolve "$repo" "$tag")" || exit 1
		echo "  ${REGISTRY_HOST}/${repo}:${tag} -> ${ref##*@}" >&2
		refs+=("$ref")
		tagged+=("${ref} ${tag}")
	done
done

if [ "$with_tags" -eq 1 ]; then
	printf '%s\n' "${tagged[@]}"
else
	printf '%s\n' "${refs[@]}" | awk '!seen[$0]++'
fi
