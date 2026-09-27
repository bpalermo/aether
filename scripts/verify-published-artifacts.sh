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
# WHAT IT CHECKS, per commit — 20 registry coordinates plus one per child
# manifest (36 today: every image is a two-platform index)
#
#   1. All FOUR charts under their commit-addressable tag:
#      charts/{aether,crds,prober,udsecho}:<X.Y.Z>-<full 40-char sha>. crds,
#      prober and udsecho spell that in their own Chart.yaml
#      (`version: "X.Y.Z-{GIT_COMMIT}"`); aether gets it from
#      //charts/aether:aether_commit (#692). The version is read from Chart.yaml
#      AS OF that commit, so a commit that bumped a chart is checked against the
#      version it actually published under.
#   2. The image tag `<release tag>-<full sha>` (`dev-<sha>` today) in each of
#      the eight published image repositories (GHCR_IMAGE_REPOS in
#      scripts/ghcr-lib.sh). The prefix is read from the `release_tag` flag's
#      default in bazel/img/go_multi_arch_image.bzl AS OF that commit, like the
#      chart versions.
#   3. A cosign signature for each of those images: the index digest resolved
#      from (2), present in the same repository as exactly one of
#      `sha256-<hex>.sig` (cosign 2) or `sha256-<hex>` (cosign 3 bundle).
#      "Published but unsigned" is its own silent failure (#875) and reads
#      identically to "never published" unless someone asks the registry.
#   4. The same, for EVERY child manifest the index lists (#925). The signer
#      uses `cosign sign --recursive`, and the per-architecture manifests are
#      what a node pulls; `cosign verify` has no `--recursive`, so nothing else
#      would look at them. An index whose children cannot be enumerated (or
#      which has none) is exit 2, never a pass.
#
#   5. The aether-proxy image the commit DEPLOYS (#984): the index digest
#      pinned in that commit's charts/aether/values.yaml — not a `*-<sha>` tag,
#      because proxy-release.yml versions the proxy by the commit that changed
#      proxy/, and every later commit ships the same pin. The digest must exist,
#      and the index and every child must carry a signature. Pins introduced
#      before proxy signing existed, whose digest has no signature tag, are
#      printed as `skip` and not counted — see scripts/proxy-pin-lib.sh for the
#      cut-over; a pin introduced after it with no signature is MISSING.
#
# (3), (4) and (5) ask whether a signature is THERE. Whether it VERIFIES is
# scripts/verify-image-signatures.sh's job; set SIGNED_REFS_OUT=<file> and this
# script appends each resolved `ghcr.io/<repo>@<index digest>` for it to read.
# The proxy is signed by a DIFFERENT workflow (proxy-release.yml, so a different
# certificate identity); its refs go to PROXY_SIGNED_REFS_OUT=<file> instead.
#
# That is every artefact the `Push charts + images` step publishes, bar the bare
# mutable `charts/*:<X.Y.Z>` tags, which carry no commit coordinate and which no
# query can attribute to a commit.
#
# HOW IT LOOKS (#985)
#
# Every coordinate above has a name this script can compute, so each one is
# asked for BY NAME — `HEAD /v2/<repo>/manifests/<tag>` (ghcr_tag_exists) —
# and no tag list is ever read. It used to page through every tag of every
# repository and grep; a publish writing tags mid-walk can shift a page
# boundary past an existing tag, and the 2026-09-27 sweep reported a present
# signature MISSING that way. 200 is present, 404 is MISSING, and any other
# answer is exit 2: an unanswered lookup is never reported as either. A 404 is
# only reported with a witness — a tag the same repository lists, looked up the
# same way, answering 200 (`witness <tag>: 200` on the MISSING line) — so a
# lookup that can no longer say "present" is exit 2, not a wall of MISSING.
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
# able from "no publish ever ran". This check never asks GitHub whether a
# workflow ran; `--recent` asks it only which commits were pushed, below.)
#
# `--recent` selects the commits itself: every PUSH HEAD on main from the last
# day that is old enough to have published, newest-first. Used by the scheduled
# sweep in .github/workflows/publish-verify.yaml.
#
# Push heads, not every commit (#975): publish runs once per push, for its head.
# An atomic stack merge puts several commits on main in one push, and only the
# last of them is ever built — the others have no artifacts by construction and
# nothing can pin them. Commits in the window that were not a push head are
# printed as `skip <sha> (not a push head ...)` so the narrowing is never silent.
# The push heads come from GitHub's activity log for refs/heads/main (see
# scripts/push-heads-lib.sh for why that and not the list of publish runs); that
# needs `gh` with a token (GH_TOKEN in Actions, `gh auth` locally), or
# PUSH_HEADS_FILE naming a file of full shas to use instead.
#
# An explicitly named commit is always checked, push head or not: the caller
# asked about that commit, and "no artifacts" is the true answer for a stack
# intermediate.
#
# Reads public packages anonymously. Set GHCR_TOKEN for private ones.
#
# EXIT CODES
#   0  every artifact for every commit is present
#   1  at least one artifact is MISSING
#   2  the check could not be performed (bad usage, unresolvable commit, a
#      lookup the registry did not answer with 200 or 404) — never conflated
#      with "present" or with "missing"

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/ghcr-lib.sh
. "${here}/ghcr-lib.sh"
# shellcheck source=scripts/push-heads-lib.sh
. "${here}/push-heads-lib.sh"
# shellcheck source=scripts/proxy-pin-lib.sh
. "${here}/proxy-pin-lib.sh"

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
	# The push heads publish was obliged to publish (#975). Read once, into a
	# file, so a failed or empty read is caught here rather than turning into
	# "every commit skipped".
	heads_file="$(mktemp)"
	trap 'rm -f "$heads_file"' EXIT
	if ! github_push_heads >"$heads_file"; then
		echo "::error::--recent: could not read the push heads of main (GitHub activity log)" >&2
		exit 2
	fi

	mapfile -t window < <(git log "$main_ref" \
		--since="$RECENT_WINDOW" --before="$RECENT_GRACE" --format=%H)
	recent=()
	skipped=0
	if [ "${#window[@]}" -gt 0 ]; then
		if ! selection="$(printf '%s\n' "${window[@]}" | select_push_heads "$heads_file")"; then
			exit 2
		fi
		while read -r verdict sha; do
			case "$verdict" in
			check) recent+=("$sha") ;;
			skip)
				skipped=$((skipped + 1))
				echo "skip ${sha} (not a push head: publish never ran for it — $(git log -1 --format=%s "$sha"))"
				;;
			*)
				echo "::error::internal: unexpected selection line '${verdict} ${sha}'" >&2
				exit 2
				;;
			esac
		done <<<"$selection"
	fi
	# An empty window — or one holding only stack intermediates — is a quiet day,
	# not a pass. Fall back to the newest PUSH HEAD old enough to have published
	# so a scheduled run ALWAYS checks something real and is always capable of
	# failing (#853).
	if [ "${#recent[@]}" -eq 0 ]; then
		if ! selection="$(git log "$main_ref" --before="$RECENT_GRACE" --format=%H -n 500 |
			select_push_heads "$heads_file")"; then
			exit 2
		fi
		fallback="$(printf '%s\n' "$selection" | sed -n 's/^check //p' | head -1)"
		[ -z "$fallback" ] || recent=("$fallback")
	fi
	if [ "${#recent[@]}" -eq 0 ]; then
		echo "::error::--recent: ${main_ref} has no push head older than '${RECENT_GRACE}' in the activity log" >&2
		exit 2
	fi
	echo "--recent: ${#recent[@]} push head(s) on ${main_ref} since '${RECENT_WINDOW}', older than '${RECENT_GRACE}' (${skipped} non-head commit(s) skipped)"
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
lookups_total=0
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

# The tag an image publishes under FOR ONE COMMIT: `<release tag>-<full sha>`,
# the second entry of go_multi_arch_image()'s image_push tag_list, where the
# release tag is the `release_tag` string_flag's default (publish.yaml does not
# override it). Read as of that commit, so a change of prefix is checked against
# what that commit actually published — and an unreadable one is exit 2, never a
# guess.
image_commit_tag() {
	local sha="$1" prefix
	prefix="$(git show "${sha}:bazel/img/go_multi_arch_image.bzl" |
		awk '/name = "release_tag"/ { f = 1 } f && /build_setting_default/ { print; exit }' |
		sed -nE 's/.*build_setting_default[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p')"
	if [ -z "$prefix" ]; then
		echo "::error::could not read the release_tag default from bazel/img/go_multi_arch_image.bzl at ${sha}" >&2
		exit 2
	fi
	printf '%s-%s\n' "$prefix" "$sha"
}

# Look ONE expected tag up by name (#985). Returns 0 present, 1 absent; exits 2
# on any answer that is neither, naming the coordinate.
lookup() {
	local repo="$1" tag="$2" tok="$3" rc=0
	lookups_total=$((lookups_total + 1))
	ghcr_tag_exists "$repo" "$tag" "$tok" || rc=$?
	case "$rc" in
	0 | 1) return "$rc" ;;
	*)
		echo "::error::inconclusive: ghcr.io/${repo}:${tag} could not be looked up (neither 200 nor 404)" >&2
		exit 2
		;;
	esac
}

# The evidence behind a 404, printed as `witness <tag>: 200` (#985). A 404 only
# means MISSING if the same lookup, in the same repository, can answer 200: take
# one tag the repository itself lists and look it up. A repository that lists
# nothing (unreadable, misnamed) or a lookup that 404s a tag the registry just
# listed (a broken Accept, say) makes every absence in it meaningless — exit 2,
# never MISSING. This is the direct-lookup form of the old "scanned N tags",
# which could not tell a real absence from an unread listing either.
#
# Called in $(...), so an `exit` here only ends the subshell: callers must check.
absence_witness() {
	local repo="$1" tok="$2" anchor rc=0
	anchor="$(ghcr_any_tag "$repo" "$tok")" || true
	if [ -z "$anchor" ]; then
		echo "::error::inconclusive: ghcr.io/${repo} lists no tags — an unreadable or misnamed repository, not a missing artifact" >&2
		return 2
	fi
	ghcr_tag_exists "$repo" "$anchor" "$tok" || rc=$?
	case "$rc" in
	0) printf 'witness %s: 200\n' "$anchor" ;;
	1)
		echo "::error::inconclusive: ghcr.io/${repo} lists ${anchor} but the lookup answers 404 for it — the lookup is broken, so its 404s prove nothing" >&2
		return 2
		;;
	*)
		echo "::error::inconclusive: ghcr.io/${repo}:${anchor} (the witness) could not be looked up" >&2
		return 2
		;;
	esac
}

# absent_direct <repo> <tag> <tok>: record <repo>:<tag> MISSING with its
# witness, or exit 2 when there is none.
absent_direct() {
	local repo="$1" tag="$2" tok="$3" w
	if ! w="$(absence_witness "$repo" "$tok")"; then
		exit 2
	fi
	absent "ghcr.io/${repo}:${tag} (looked up directly: 404; ${w})"
}

# One signature, for one digest (an index or one of its children), in exactly
# one layout.
#
# A signature counts as published in EITHER layout — cosign 2's
# `sha256-<digest>.sig` or cosign 3's `sha256-<digest>` fallback index — because
# everything published before the v3 migration carries the former and must stay
# verifiable.
#
# But exactly ONE must be present. Accepting "either" without rejecting "both"
# would read a double-write or a half-finished migration as healthy, and that is
# the state a format migration actually fails into.
#
# Both tag shapes are looked up by name, every time (two HEADs), so `both` is
# seen; either lookup going unanswered is exit 2.
check_signature() {
	local repo="$1" digest="$2" tok="$3" what="$4" layout
	lookups_total=$((lookups_total + 2))
	if ! layout="$(ghcr_signature_layout_direct "$repo" "$digest" "$tok")"; then
		echo "::error::inconclusive: could not look up the signature tags of ghcr.io/${repo}@${digest}" >&2
		exit 2
	fi
	case "$layout" in
	legacy)
		present "ghcr.io/${repo}:$(ghcr_signature_tag_legacy "$digest") (signature of ${what} ${digest}, cosign 2 layout)"
		;;
	bundle)
		present "ghcr.io/${repo}:$(ghcr_signature_tag_bundle "$digest") (signature of ${what} ${digest}, cosign 3 layout)"
		;;
	both)
		absent "ghcr.io/${repo} signature for ${what} ${digest} — BOTH layouts present; a double-write or half-finished migration, not a healthy signature"
		;;
	none)
		absent "ghcr.io/${repo} signature for ${what} ${digest} (neither ${digest//:/-}.sig nor ${digest//:/-}; both looked up directly, both 404; the image in this repository answered 200)"
		;;
	*)
		echo "::error::internal: unexpected signature layout '${layout}' for ghcr.io/${repo}@${digest}" >&2
		exit 2
		;;
	esac
}

verify_commit() {
	local ref="$1"
	local sha chart chart_repo chart_tag repo tok tag digest children child
	local before="$checks_total" lookups_before="$lookups_total" expected_children=0

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
		if lookup "$chart_repo" "$chart_tag" "$tok"; then
			present "ghcr.io/${chart_repo}:${chart_tag}"
		else
			absent_direct "$chart_repo" "$chart_tag" "$tok"
		fi
	done

	# 2 + 3. every published image, and its signature.
	tag="$(image_commit_tag "$sha")"
	for repo in "${GHCR_IMAGE_REPOS[@]}"; do
		# A repository we cannot read is an inconclusive check, not a passing one.
		if ! tok="$(ghcr_registry_token "$repo")" || [ -z "$tok" ]; then
			echo "::error::could not obtain a pull token for ${repo}" >&2
			exit 2
		fi

		if ! lookup "$repo" "$tag" "$tok"; then
			absent_direct "$repo" "$tag" "$tok"
			# No image means no digest to look a signature up by. Count the signature
			# as missing too rather than skipping it — a skipped check is a check that
			# cannot fail.
			absent "ghcr.io/${repo} signature for ${tag} (no image to sign)"
			continue
		fi
		present "ghcr.io/${repo}:${tag}"

		digest="$(ghcr_manifest_digest "$repo" "$tag" "$tok")"
		if [ -z "$digest" ]; then
			echo "::error::could not resolve a digest for ghcr.io/${repo}:${tag}" >&2
			exit 2
		fi
		check_signature "$repo" "$digest" "$tok" "index"

		# 4. every CHILD manifest's signature (#925). `cosign sign --recursive`
		# signs the per-architecture manifests too, and those are what a node
		# actually pulls — checking the index alone would leave them unchecked.
		# The walk must yield at least one child: an index with none, or one we
		# cannot read, is an inconclusive check, never a vacuous pass.
		if ! children="$(ghcr_index_children "$repo" "$digest" "$tok")" || [ -z "$children" ]; then
			echo "::error::could not enumerate the child manifests of ghcr.io/${repo}@${digest}" >&2
			exit 2
		fi
		while read -r child; do
			expected_children=$((expected_children + 1))
			check_signature "$repo" "$child" "$tok" "child"
		done <<<"$children"

		# Hand the exact index reference to the cosign pass, when asked for
		# (publish-verify.yaml). Presence of a signature TAG is what this script
		# can see without cosign; whether that signature VERIFIES is the job of
		# scripts/verify-image-signatures.sh, over these same digests.
		if [ -n "${SIGNED_REFS_OUT:-}" ]; then
			printf 'ghcr.io/%s@%s\n' "$repo" "$digest" >>"$SIGNED_REFS_OUT"
		fi
	done

	# 5. the aether-proxy image this commit's chart pins (#984).
	#
	# PROXY_PIN_CHECK=0 skips this step. Only the expected-red control sets it
	# (scripts/publish-verify-control.sh): the control proves that the
	# PER-COMMIT artifact gate goes red for a never-published commit, and a
	# constructed commit still pins whatever proxy digest main's chart pins —
	# which, once that digest is signed, is legitimately PRESENT and would add
	# `ok` lines (or, in the offline harness's fake registry, spurious MISSING
	# lines) to a red that must otherwise be exactly the 20 per-commit
	# coordinates. The proxy pin has its own gate and its own seen-red
	# (scripts/check-proxy-pin.sh, the cut-over cases). The real sweep and the
	# workflow_run path never set this.
	local proxy_expected=0 values pin verdict got
	if [ "${PROXY_PIN_CHECK:-1}" = 0 ]; then
		say "  skip    ghcr.io/${PROXY_REPO} pin check (PROXY_PIN_CHECK=0: the expected-red control covers the per-commit coordinates only)"
	else
	if ! values="$(git show "${sha}:${PROXY_VALUES_PATH}")" ||
		! pin="$(printf '%s\n' "$values" | proxy_pinned_digest)"; then
		echo "::error::could not read the aether-proxy digest pinned in ${PROXY_VALUES_PATH} at ${sha}" >&2
		exit 2
	fi
	if ! tok="$(ghcr_registry_token "$PROXY_REPO")" || [ -z "$tok" ]; then
		echo "::error::could not obtain a pull token for ${PROXY_REPO}" >&2
		exit 2
	fi
	if ! tags="$(ghcr_all_tags "$PROXY_REPO" "$tok")"; then
		echo "::error::could not list tags for ${PROXY_REPO}" >&2
		exit 2
	fi
	if ! verdict="$(proxy_pin_verdict "$sha" "$pin" "$tags")"; then
		exit 2
	fi
	case "$verdict" in
	"skip "*)
		# Printed, never silent, and never counted: a skipped check is not a pass.
		say "  skip    ghcr.io/${PROXY_REPO}@${pin} (pinned by ${verdict#skip }, before proxy signing existed — cut-over ${PROXY_SIGNING_CUTOVER:0:12}; unsigned by history, #984)"
		;;
	check)
		got="$(ghcr_manifest_digest "$PROXY_REPO" "$pin" "$tok")" || got=""
		if [ "$got" != "$pin" ]; then
			proxy_expected=2
			absent "ghcr.io/${PROXY_REPO}@${pin} (pinned in ${PROXY_VALUES_PATH}; the registry does not serve it)"
			absent "ghcr.io/${PROXY_REPO} signature for pinned ${pin} (no image to sign)"
		else
			present "ghcr.io/${PROXY_REPO}@${pin} (pinned in ${PROXY_VALUES_PATH})"
			check_signature "$PROXY_REPO" "$pin" "$tags" "proxy index"
			if ! children="$(ghcr_index_children "$PROXY_REPO" "$pin" "$tok")" || [ -z "$children" ]; then
				echo "::error::could not enumerate the child manifests of ghcr.io/${PROXY_REPO}@${pin}" >&2
				exit 2
			fi
			proxy_expected=2
			while read -r child; do
				proxy_expected=$((proxy_expected + 1))
				check_signature "$PROXY_REPO" "$child" "$tags" "proxy child"
			done <<<"$children"
			if [ -n "${PROXY_SIGNED_REFS_OUT:-}" ]; then
				printf 'ghcr.io/%s@%s\n' "$PROXY_REPO" "$pin" >>"$PROXY_SIGNED_REFS_OUT"
			fi
		fi
		;;
	*)
		echo "::error::internal: unexpected proxy pin verdict '${verdict}'" >&2
		exit 2
		;;
	esac
	fi

	# 4 charts + 8 images + 8 index signatures + one signature per child, plus
	# the proxy pin's checks. If the loops ever stop iterating, this says so
	# instead of reporting a clean run over nothing.
	local expected=$((${#GHCR_CHARTS[@]} + 2 * ${#GHCR_IMAGE_REPOS[@]} + expected_children + proxy_expected))
	local did=$((checks_total - before))
	if [ "$did" -ne "$expected" ]; then
		echo "::error::internal: ran ${did} checks for ${sha}, expected ${expected}" >&2
		exit 2
	fi
	# What was actually asked of the registry: one HEAD per chart and image tag,
	# two per signature (both layouts). No tag list was read, so there is no
	# "scanned N tags" to report — a lookup count of 0 would be the vacuous run.
	say "  checked $((lookups_total - lookups_before)) expected tags directly (HEAD /v2/<repo>/manifests/<tag>; no tag listing)"
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
