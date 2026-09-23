#!/usr/bin/env bash
# Assert that the artifacts for a commit on `main` actually exist in GHCR (#880).
#
# WHY THIS EXISTS
#
# `.github/workflows/publish.yaml` serialises publishes through
# `concurrency: { group: publish-main, cancel-in-progress: false }` (#692). That
# protects the RUNNING publish, but GitHub keeps at most ONE pending run per
# concurrency group: a newly queued run cancels the older pending one. So a
# commit merged into a busy publish window can be superseded before it starts a
# single job, and its run ends `cancelled` — not `failure`. Nothing is red,
# nothing is pushed, and the commit-suffixed chart tag that #692 introduced as
# THE deploy coordinate silently does not exist. That happened to f332061 on
# 2026-09-20.
#
# So this check does not ask "did the workflow exit 0". It asks the registry
# whether the artifacts for this commit are there. A publish that reported
# success but pushed nothing, a run that was cancelled at queue time, and a run
# that never existed are all the same answer here: MISSING.
#
# WHAT IT CHECKS, per commit — 20 registry coordinates
#
#   1. All FOUR charts under their commit-addressable tag:
#      charts/{aether,crds,prober,udsecho}:<X.Y.Z>-<full 40-char sha>. crds,
#      prober and udsecho spell that in their own Chart.yaml
#      (`version: "X.Y.Z-{GIT_COMMIT}"`); aether gets it from
#      //charts/aether:aether_commit (#692). The version is read from Chart.yaml
#      AS OF that commit, so a commit that bumped a chart is checked against the
#      version it actually published under.
#   2. A tag ending in `-<full sha>` in each of the eight published image
#      repositories (GHCR_IMAGE_REPOS in scripts/ghcr-lib.sh).
#   3. A cosign signature for each of those images: the index digest resolved
#      from (2), present as the tag `sha256-<hex>.sig` in the same repository.
#      "Published but unsigned" is its own silent failure (#875) and reads
#      identically to "never published" unless someone asks the registry.
#
# That is every artefact the `Push charts + images` step publishes, bar the bare
# mutable `charts/*:<X.Y.Z>` tags, which carry no commit coordinate and which no
# query can attribute to a commit.
#
# READ-ONLY. Every request below is a GET or a HEAD. This script cannot push,
# retag or delete anything: the release workflow is the only publisher.
#
# USAGE
#
#   scripts/verify-published-artifacts.sh <commit-ish> [<commit-ish>...]
#   scripts/verify-published-artifacts.sh --recent
#
# Any commit-ish git can resolve (a branch, a tag, an abbreviated sha) is
# expanded to its full 40-char sha HERE, by git. Nothing downstream accepts a
# hand-typed sha: an abbreviation silently matches nothing in a registry tag
# list, and a fabricated one has already cost this project a failed deploy.
# (The same trap bites `gh run list --commit=<sha>`, which matches only the full
# 40 characters and answers an abbreviation with an empty list — indistinguish-
# able from "no publish ever ran". This check never asks GitHub anything.)
#
# `--recent` selects the commits itself: everything on main from the last day
# that is old enough to have published, newest-first. Used by the scheduled
# sweep in .github/workflows/publish-verify.yaml.
#
# Reads public packages anonymously. Set GHCR_TOKEN for private ones.
#
# EXIT CODES
#   0  every artifact for every commit is present
#   1  at least one artifact is MISSING
#   2  the check could not be performed (bad usage, unresolvable commit,
#      unreadable repository) — never conflated with "present"

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/ghcr-lib.sh
. "${here}/ghcr-lib.sh"

if [ "$#" -eq 0 ]; then
	echo "usage: $(basename "$0") {--recent | <commit-ish> [<commit-ish>...]}" >&2
	exit 2
fi

# How far back the `--recent` sweep looks, and how long a just-merged commit is
# given before its absence counts as a gap.
#
# The grace period is generous on purpose. The sweep is a BACKSTOP; the primary
# detector is the per-run check that fires the moment a publish run reaches a
# conclusion, which has no race at all (a run that ended `cancelled` will never
# push anything). A publish normally takes ~4 minutes, but it can sit queued
# behind another one for much longer, and a backstop that cries wolf about a
# publish still in flight is a backstop people switch off.
RECENT_WINDOW="${RECENT_WINDOW:-24 hours ago}"
RECENT_GRACE="${RECENT_GRACE:-2 hours ago}"

if [ "$1" = "--recent" ]; then
	if [ "$#" -ne 1 ]; then
		echo "usage: $(basename "$0") --recent   (takes no other arguments)" >&2
		exit 2
	fi
	main_ref=""
	for candidate in origin/main main HEAD; do
		if git rev-parse --verify --quiet "${candidate}^{commit}" >/dev/null; then
			main_ref="$candidate"
			break
		fi
	done
	if [ -z "$main_ref" ]; then
		echo "::error::--recent: no origin/main, main or HEAD to read commits from" >&2
		exit 2
	fi
	mapfile -t recent < <(git log "$main_ref" \
		--since="$RECENT_WINDOW" --before="$RECENT_GRACE" --format=%H)
	# An empty window is a quiet day, not a pass. Fall back to the newest commit
	# old enough to have published so a scheduled run ALWAYS checks something real
	# and is always capable of failing (#853).
	if [ "${#recent[@]}" -eq 0 ]; then
		mapfile -t recent < <(git log "$main_ref" -1 --before="$RECENT_GRACE" --format=%H)
	fi
	if [ "${#recent[@]}" -eq 0 ]; then
		echo "::error::--recent: ${main_ref} has no commit older than '${RECENT_GRACE}'" >&2
		exit 2
	fi
	echo "--recent: ${#recent[@]} commit(s) on ${main_ref} since '${RECENT_WINDOW}', older than '${RECENT_GRACE}'"
	set -- "${recent[@]}"
fi

# A gate with nothing to check is not a passing gate (#853). The repo list is
# shared with the signer, so an empty one would mean the signer signs nothing
# too — refuse rather than report eight-for-eight on zero repositories.
if [ "${#GHCR_IMAGE_REPOS[@]}" -eq 0 ] || [ "${#GHCR_CHARTS[@]}" -eq 0 ]; then
	echo "::error::GHCR_IMAGE_REPOS or GHCR_CHARTS is empty — there is nothing to verify" >&2
	exit 2
fi

missing_total=0
checks_total=0
report=""

say() { printf '%s\n' "$*"; }

record() { report="${report}$1"$'\n'; }

present() {
	checks_total=$((checks_total + 1))
	say "  ok      $1"
}

absent() {
	checks_total=$((checks_total + 1))
	missing_total=$((missing_total + 1))
	say "  MISSING $1"
	record "MISSING $1"
	echo "::error::not published: $1"
}

# The tag a chart publishes under FOR ONE COMMIT, derived from Chart.yaml as of
# that commit — never assembled from a version typed here.
#
# Two spellings, because the build has two: crds/prober/udsecho carry
# `version: "X.Y.Z-{GIT_COMMIT}"` and rules_helm substitutes the sha at package
# time; aether carries a bare `X.Y.Z` and //charts/aether:chart_commit_yaml
# appends `-{GIT_COMMIT}` in a genrule. Both end at `X.Y.Z-<full sha>`.
chart_commit_tag() {
	local sha="$1" chart="$2" version
	version="$(git show "${sha}:charts/${chart}/Chart.yaml" |
		sed -nE 's/^version:[[:space:]]*"?([^"[:space:]]+)"?.*/\1/p' | head -1)"
	if [ -z "$version" ]; then
		echo "::error::could not read charts/${chart}/Chart.yaml version at ${sha}" >&2
		exit 2
	fi
	case "$version" in
	*"{GIT_COMMIT}"*) printf '%s\n' "${version//\{GIT_COMMIT\}/$sha}" ;;
	*) printf '%s\n' "${version}-${sha}" ;;
	esac
}

verify_commit() {
	local ref="$1"
	local sha chart chart_repo chart_tag repo tok tags tag digest sig want
	local before="$checks_total"

	if ! sha="$(git rev-parse --verify --quiet "${ref}^{commit}")"; then
		echo "::error::not a commit in this repository: ${ref}" >&2
		exit 2
	fi

	say "commit ${sha} ($(git log -1 --format='%cI %s' "$sha"))"

	# 1. every chart, under the tag that belongs to this commit alone (#692).
	for chart in "${GHCR_CHARTS[@]}"; do
		chart_repo="${GHCR_CHART_REPO_PREFIX}/${chart}"
		chart_tag="$(chart_commit_tag "$sha" "$chart")"
		if ! tok="$(ghcr_registry_token "$chart_repo")" || [ -z "$tok" ]; then
			echo "::error::could not obtain a pull token for ${chart_repo}" >&2
			exit 2
		fi
		if ! tags="$(ghcr_all_tags "$chart_repo" "$tok")"; then
			echo "::error::could not list tags for ${chart_repo}" >&2
			exit 2
		fi
		if printf '%s\n' "$tags" | grep -qxF -- "$chart_tag"; then
			present "ghcr.io/${chart_repo}:${chart_tag}"
		else
			absent "ghcr.io/${chart_repo}:${chart_tag} (scanned $(printf '%s\n' "$tags" | grep -c . || true) tags)"
		fi
	done

	# 2 + 3. every published image, and its signature.
	for repo in "${GHCR_IMAGE_REPOS[@]}"; do
		# A repository we cannot read is an inconclusive check, not a passing one.
		if ! tok="$(ghcr_registry_token "$repo")" || [ -z "$tok" ]; then
			echo "::error::could not obtain a pull token for ${repo}" >&2
			exit 2
		fi
		if ! tags="$(ghcr_all_tags "$repo" "$tok")"; then
			echo "::error::could not list tags for ${repo}" >&2
			exit 2
		fi

		# The image tag is `<stamped tag>-<full sha>` (`dev-<sha>` today). Match on
		# the sha suffix so a change to the stamped prefix does not turn this into a
		# check that can never pass.
		tag="$(printf '%s\n' "$tags" | grep -E -- "-${sha}\$" | head -1 || true)"
		if [ -z "$tag" ]; then
			# Report the scan size: "not found in 732" is a real absence, "not found
			# in 100" is a pagination regression, and the two must not look alike.
			absent "ghcr.io/${repo}:*-${sha} (scanned $(printf '%s\n' "$tags" | grep -c . || true) tags)"
			# No image means no digest to look a signature up by. Count the signature
			# as missing too rather than skipping it — a skipped check is a check that
			# cannot fail.
			absent "ghcr.io/${repo} signature for *-${sha} (no image to sign)"
			continue
		fi
		present "ghcr.io/${repo}:${tag}"

		digest="$(ghcr_manifest_digest "$repo" "$tag" "$tok")"
		if [ -z "$digest" ]; then
			echo "::error::could not resolve a digest for ghcr.io/${repo}:${tag}" >&2
			exit 2
		fi
		sig="$(ghcr_signature_tag "$digest")"
		want="ghcr.io/${repo}:${sig} (signature of ${digest})"
		if printf '%s\n' "$tags" | grep -qxF -- "$sig"; then
			present "$want"
		else
			absent "$want"
		fi
	done

	# 4 charts + 8 images + 8 signatures. If the loops ever stop iterating, this
	# says so instead of reporting a clean run over nothing.
	local expected=$((${#GHCR_CHARTS[@]} + 2 * ${#GHCR_IMAGE_REPOS[@]}))
	local did=$((checks_total - before))
	if [ "$did" -ne "$expected" ]; then
		echo "::error::internal: ran ${did} checks for ${sha}, expected ${expected}" >&2
		exit 2
	fi
}

for ref in "$@"; do
	verify_commit "$ref"
done

say ""
if [ "$missing_total" -gt 0 ]; then
	say "FAIL: ${missing_total} of ${checks_total} artifact(s) missing across $# commit(s)"
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		cat >>"$GITHUB_STEP_SUMMARY" <<EOF
### publish verification FAILED

${missing_total} of ${checks_total} artifacts are missing from ghcr.io.

\`\`\`
${report}\`\`\`
EOF
	fi
	exit 1
fi

say "PASS: ${checks_total} artifact(s) present across $# commit(s)"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	printf '### publish verification passed\n\n%s artifacts present across %s commit(s).\n' \
		"$checks_total" "$#" >>"$GITHUB_STEP_SUMMARY"
fi
