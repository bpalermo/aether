#!/usr/bin/env bash
#
# Fails if the digest of a Go image of the root workspace depends on the commit
# (#1378).
#
# Scope: the `image_index` targets of this workspace, which are the Go images
# (agent, mesh-dns, proxy-supervisor, uds-csi, cni-install, registrar,
# controller, prober and the two e2e fixtures). The `aether-proxy` image is out
# of scope: it is built in the nested `proxy/` workspace by its own release
# workflow, it carries the aether commit on purpose (its revision label and
# Envoy's own version linkstamp), and the chart pins it statically, so it
# changes only when the pin is bumped.
#
# The charts pin every Go image by index digest, so a digest that moves with
# the commit rolls every workload on every deploy, whether or not a line of its
# code changed. Two things did exactly that until #1378: a version linked into
# seven binaries from the workspace status (`x_defs`), and the commit as an
# image label and annotation (`stamp = "force"`). Either one alone is enough to
# give all ten images a new digest with every commit.
#
# What this does, in two lines of defence.
#
# First, the action graph: no action that an image index depends on may take a
# workspace-status file as an input.
#
#   0. `bazel aquery` over the dependencies of every index, for actions whose
#      inputs include `stable-status.txt` or `volatile-status.txt`, or the two
#      headers Bazel itself derives from them. Any hit is a failure, and the
#      action is named. This is the only check that sees a VOLATILE key: Bazel
#      does not re-run an action when only a volatile key changed, so two warm
#      builds agree on an image that two cold builds would not.
#      One target is expected and allowed: Bazel's own
#      `@bazel_tools//tools/build_defs/build_info:cc_build_info`, which turns
#      the status files into headers for C++ linkstamps and is in every
#      toolchain's dependencies; nothing else may read those headers either.
#      The `image_push` next to each index is not a dependency of the index. It
#      is added to the query on purpose, as the control: it MUST read the
#      status (that is where the per-commit tag comes from), so a query that
#      does not report every push did not see status inputs at all.
#
# Second, the build itself: every index is built twice under `--stamp`, each
# time with a workspace-status script of its own that reports a different
# made-up commit, and the digests rules_img computes are compared.
#
#   1. Every index digest is the same under both commits.
#   2. Every per-commit tag (`dev-<sha>`, the `image_push` next to each index)
#      is NOT the same: it names the first commit after the first build and the
#      second commit after the second. This is the control: it proves the
#      made-up status reached the build. Without it, a build that ignored the
#      status would pass check 1 having compared a thing with itself. It also
#      fails if the push loses its per-commit tag.
#   3. bazel/workspace_status.sh, run for real, still reports
#      STABLE_GIT_COMMIT = HEAD. Now that an image does not change with the
#      commit, the tag is the one output that must, and Bazel re-expands it
#      only when a STABLE_ key changes. Measured with a status whose only
#      commit-derived key was the volatile GIT_COMMIT: the second commit's
#      build kept the first commit's `dev-<sha>` tag, so a push would have
#      published one commit's image under another's name. The charts'
#      `X.Y.Z-<sha>` version is re-made the same way. Checks 1 and 2 cannot see
#      this (the made-up statuses change a STABLE_ key by construction), so the
#      real script is held to it here.
#
# What neither sees: a commit that reaches an image without going through the
# workspace status, such as a rule that reads the repository itself.
#
# Cost: the aquery is analysis only. The first build is the ten images (a cache
# hit after any build of them, stamped or not: nothing in an image reads the
# status); the second re-runs the ten tag expansions and nothing else. If it
# re-runs a compile or a link, that is the regression, and check 1 names the
# image.
#
# Usage: scripts/check-image-digest-stability.sh [extra bazel build flags...]
#        (or: make check-image-digests)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

bazel="${BAZEL:-bazel}"

# The two made-up commits. Forty hex characters, like a real one, and nothing
# else in the tree can contain either by accident.
COMMIT_A="a1378a1378a1378a1378a1378a1378a1378a1378"
COMMIT_B="b1378b1378b1378b1378b1378b1378b1378b1378"

# An image known to exist: a query that does not return it cannot be trusted.
KNOWN_INDEX="//agent/cmd/agent:image_index"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

failures=0
fail() {
	echo "FAIL: $*" >&2
	failures=$((failures + 1))
}

# --- 3. the real workspace status ---------------------------------------------
head_commit="$(git rev-parse HEAD)"
# Output and status are taken apart: in `x="$(script | sed)"` under `set -e` a
# failing script ends this one with its own status and no line saying why.
status_rc=0
status_out="$(bash bazel/workspace_status.sh)" || status_rc=$?
if [ "$status_rc" -ne 0 ]; then
	echo "FAIL: bazel/workspace_status.sh failed (exit ${status_rc}): what it printed cannot be trusted." >&2
	exit 1
fi
real_commit="$(sed -n 's/^STABLE_GIT_COMMIT //p' <<<"$status_out")"
if [ "$real_commit" = "$head_commit" ] && [[ "$real_commit" =~ ^[0-9a-f]{40}$ ]]; then
	echo "ok: bazel/workspace_status.sh reports STABLE_GIT_COMMIT = HEAD"
else
	fail "bazel/workspace_status.sh reports STABLE_GIT_COMMIT '${real_commit}', want HEAD '${head_commit}'. A STABLE_ key that changes with the commit is what re-makes the per-commit image tag and the charts' X.Y.Z-<sha> version; without one they keep the previous commit's value on a warm Bazel server."
fi

# --- the images ----------------------------------------------------------------
# The query's status is read before its output is used. Inside
# `mapfile < <(query | sort)` it is lost twice (the pipe and the process
# substitution), and a query that printed some images and then failed would
# leave this script checking those and passing.
query_rc=0
"$bazel" query 'kind("image_index rule", //...)' --output=label >"$tmp/query.out" 2>"$tmp/query.err" || query_rc=$?
if [ "$query_rc" -ne 0 ]; then
	cat "$tmp/query.err" >&2
	echo "FAIL: the image_index query failed (exit ${query_rc}): a partial list of images cannot be trusted." >&2
	exit 1
fi
mapfile -t indexes < <(sort "$tmp/query.out")
if [ "${#indexes[@]}" -eq 0 ] || ! grep -qxF "$KNOWN_INDEX" "$tmp/query.out"; then
	cat "$tmp/query.err" >&2
	echo "FAIL: the image_index query returned ${#indexes[@]} target(s) and not ${KNOWN_INDEX}: it cannot be trusted." >&2
	exit 1
fi
pushes=()
for index in "${indexes[@]}"; do
	pushes+=("${index%:*}:image_push")
done

# --- 0. the action graph ---------------------------------------------------------
# Bazel's own target that reads the status files, in every toolchain's
# dependencies; its two outputs are the headers the pattern below also names.
BUILD_INFO_TARGET="@bazel_tools//tools/build_defs/build_info:cc_build_info"
STATUS_INPUTS='.*(/(stable|volatile)-status\.txt|/build_defs/build_info/(non_)?volatile_file\.h)'

# The status is read before the output is used, as for the query above.
aquery_rc=0
"$bazel" aquery --stamp "$@" --output=text \
	"inputs(\"${STATUS_INPUTS}\", deps(set(${indexes[*]})) + set(${pushes[*]}))" \
	>"$tmp/aquery.out" 2>"$tmp/aquery.err" || aquery_rc=$?
if [ "$aquery_rc" -ne 0 ]; then
	cat "$tmp/aquery.err" >&2
	echo "FAIL: the action query failed (exit ${aquery_rc}): a partial list of actions cannot be trusted." >&2
	exit 1
fi
# One "<mnemonic> <target>" line per action. `--output=text` prints a block per
# action that opens with `action '...'` and holds `  Mnemonic: X` and
# `  Target: //label` lines.
awk '
	/^action / { if (target != "") print mnemonic, target; mnemonic = ""; target = "" }
	/^  Mnemonic: / { mnemonic = $2 }
	/^  Target: / { target = $2 }
	END { if (target != "") print mnemonic, target }
' "$tmp/aquery.out" | sort -u >"$tmp/status-actions"
printf '%s\n' "${pushes[@]}" >"$tmp/pushes"

seen_pushes=0
for push in "${pushes[@]}"; do
	if awk -v t="$push" '$2 == t { found = 1 } END { exit !found }' "$tmp/status-actions"; then
		seen_pushes=$((seen_pushes + 1))
	else
		fail "the action query does not report ${push} reading the workspace status. Each push must (its per-commit tag comes from it), so the query did not see status inputs and its silence about the images cannot be trusted."
	fi
done
status_readers=0
while read -r mnemonic target; do
	[ -n "$target" ] || continue
	[ "$target" = "$BUILD_INFO_TARGET" ] && continue
	grep -qxF -- "$target" "$tmp/pushes" && continue
	fail "${target}: its ${mnemonic} action takes a workspace-status file as an input, and an image index depends on it. Whatever it writes follows the commit: a stamped label or annotation, or a value linked into a binary (x_defs). A volatile key is not re-read on a warm Bazel server, so the two builds below can agree all the same."
	status_readers=$((status_readers + 1))
done <"$tmp/status-actions"
if [ "$status_readers" -eq 0 ] && [ "$seen_pushes" -eq "${#pushes[@]}" ]; then
	echo "ok: no action under the ${#indexes[@]} image indexes takes a workspace-status file (the ${seen_pushes} pushes do, as they must)"
fi

# status_script <commit> <timestamp>: a workspace-status script reporting that
# commit, with the keys bazel/workspace_status.sh reports.
status_script() {
	local out="$tmp/status-$1.sh"
	cat >"$out" <<-EOF
		#!/usr/bin/env bash
		echo "STABLE_GIT_VERSION v0.0.0-1-g$1"
		echo "STABLE_GIT_COMMIT $1"
		echo "BUILD_TIMESTAMP $2"
		echo "GIT_COMMIT $1"
		echo "GIT_BRANCH main"
		echo "GIT_DIRTY clean"
	EOF
	chmod +x "$out"
	printf '%s\n' "$out"
}

# build_under <commit> <timestamp> <extra bazel flags...>: build every index
# digest and every push's deploy manifest under that commit, and copy them to
# $tmp/<commit>/<package>.{digest,push}.
build_under() {
	local commit="$1" status files file pkg
	status="$(status_script "$1" "$2")"
	shift 2
	local flags=(--stamp "--workspace_status_command=$status" "--output_groups=digest,deploy_manifest" "$@")

	echo "building ${#indexes[@]} image indexes as commit ${commit}"
	"$bazel" build "${flags[@]}" "${indexes[@]}" "${pushes[@]}"
	files="$("$bazel" cquery "${flags[@]}" --output=files "set(${indexes[*]} ${pushes[*]})" 2>"$tmp/cquery.err")" || {
		cat "$tmp/cquery.err" >&2
		exit 1
	}

	mkdir -p "$tmp/$commit"
	while IFS= read -r file; do
		[ -n "$file" ] || continue
		# bazel-out/<config>/bin/<package>/<name>: the package is what is left.
		pkg="$(dirname "${file#bazel-out/*/bin/}")"
		case "$file" in
		*/image_index_digest) cp "$file" "$tmp/$commit/${pkg//\//_}.digest" ;;
		*/image_push*) cat "$file" >>"$tmp/$commit/${pkg//\//_}.push" ;;
		esac
	done <<<"$files"
}

build_under "$COMMIT_A" 1000000000 "$@"
build_under "$COMMIT_B" 2000000000 "$@"

# --- 1 and 2 ---------------------------------------------------------------------
stable=0
for index in "${indexes[@]}"; do
	pkg="${index%:*}"
	key="${pkg#//}"
	key="${key//\//_}"

	a="$(cat "$tmp/$COMMIT_A/$key.digest" 2>/dev/null || true)"
	b="$(cat "$tmp/$COMMIT_B/$key.digest" 2>/dev/null || true)"
	if ! [[ "$a" =~ ^sha256:[0-9a-f]{64}$ && "$b" =~ ^sha256:[0-9a-f]{64}$ ]]; then
		fail "${index}: no digest read (first build '${a}', second '${b}')"
	elif [ "$a" != "$b" ]; then
		fail "${index}: the digest depends on the commit (${a} as one commit, ${b} as the next). Something in the image reads the workspace status: a stamped label or annotation, or a value linked into a binary (x_defs)."
	else
		echo "ok: ${index} ${a}"
		stable=$((stable + 1))
	fi

	push_a="$tmp/$COMMIT_A/$key.push"
	push_b="$tmp/$COMMIT_B/$key.push"
	if ! grep -qF -- "-${COMMIT_A}" "$push_a" 2>/dev/null; then
		fail "${pkg}:image_push: no tag ending in -${COMMIT_A} after the first build. Either the workspace status did not reach the build, in which case the digests above were compared with themselves, or the push no longer writes a per-commit tag."
	elif ! grep -qF -- "-${COMMIT_B}" "$push_b" 2>/dev/null || grep -qF -- "$COMMIT_A" "$push_b"; then
		fail "${pkg}:image_push: the per-commit tag did not follow the commit (after the second build it does not name ${COMMIT_B}, or still names ${COMMIT_A}). A push would publish one commit's image under another commit's tag."
	fi
done

if [ "$failures" -ne 0 ]; then
	echo "${failures} check(s) failed" >&2
	exit 1
fi
echo "OK: no action under an image reads the workspace status, ${stable} of ${#indexes[@]} image digests are the same under two commits, and every per-commit tag follows the commit."
