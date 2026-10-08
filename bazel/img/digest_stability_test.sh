#!/usr/bin/env bash
# An image's digest must not depend on the commit, and the names that DO carry
# the commit must still follow it (#1378). The cheap, always-on half of that:
#
#   1. bazel/workspace_status.sh still emits STABLE_GIT_COMMIT, equal to HEAD
#      and different at the next commit. It is the only thing that re-runs the
#      actions writing the per-commit image tag (`dev-<sha>`) and the charts'
#      `X.Y.Z-<sha>` version; without it a warm Bazel server publishes the
#      previous commit's tag.
#   2. The image macro's per-commit tag reads that STABLE_ key, and the macro
#      holds no commit-derived label or annotation and no forced stamp.
#   3. Every built image index carries no commit: no
#      `org.opencontainers.image.revision`, no unexpanded template and no value
#      that looks like a commit, on the index or on its per-platform
#      descriptors (rules_img copies a manifest's annotations onto them).
#
# This test cannot see a commit-derived config LABEL in a built image (the
# config is built per platform, behind a transition); check 2 covers the source.
#
# Usage: digest_stability_test.sh <workspace_status.sh> <go_multi_arch_image.bzl> <index.json>...
# Run:   bazel test //bazel/img:digest_stability_test
set -uo pipefail

status_script="${1:?usage: digest_stability_test.sh <workspace_status.sh> <go_multi_arch_image.bzl> <index.json>...}"
macro="${2:?missing go_multi_arch_image.bzl}"
shift 2
[ "$#" -gt 0 ] || {
	echo "FAIL: no image index given: the test would pass having checked nothing"
	exit 1
}

if [ -n "${JQ_RLOCATIONPATH:-}" ]; then
	JQ="${TEST_SRCDIR:-${RUNFILES_DIR:-$PWD/..}}/${JQ_RLOCATIONPATH}"
fi
JQ="${JQ:-$(command -v jq)}"
[ -x "$JQ" ] || {
	echo "FAIL: no jq (JQ=${JQ})"
	exit 1
}

failures=0
fail() {
	echo "FAIL: $*"
	failures=$((failures + 1))
}
pass() { echo "ok: $*"; }

status_script="$(cd -- "$(dirname -- "$status_script")" && pwd)/$(basename -- "$status_script")"
macro="$(cd -- "$(dirname -- "$macro")" && pwd)/$(basename -- "$macro")"
indexes=()
for f in "$@"; do
	indexes+=("$(cd -- "$(dirname -- "$f")" && pwd)/$(basename -- "$f")")
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# --- 1. the workspace status ---------------------------------------------------
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid

repo="$TMP/repo"
mkdir -p "$repo"
git -C "$repo" init -q
echo one >"$repo/file"
git -C "$repo" add file
git -C "$repo" commit -q -m one

# stable_commit: the STABLE_GIT_COMMIT value the script reports in $repo.
stable_commit() {
	(cd "$repo" && bash "$status_script") | sed -n 's/^STABLE_GIT_COMMIT //p'
}

head_a="$(git -C "$repo" rev-parse HEAD)"
got_a="$(stable_commit)"
if [ "$got_a" = "$head_a" ] && [[ "$got_a" =~ ^[0-9a-f]{40}$ ]]; then
	pass "workspace_status.sh emits STABLE_GIT_COMMIT = HEAD ($got_a)"
else
	fail "workspace_status.sh: STABLE_GIT_COMMIT is '$got_a', want HEAD '$head_a'. This key is what keeps the per-commit image tag and the charts' X.Y.Z-<sha> version fresh; do not remove it (#1378)"
fi

echo two >"$repo/file"
git -C "$repo" commit -q -am two
head_b="$(git -C "$repo" rev-parse HEAD)"
got_b="$(stable_commit)"
if [ "$got_b" = "$head_b" ] && [ "$got_b" != "$got_a" ]; then
	pass "STABLE_GIT_COMMIT follows the next commit ($got_b)"
else
	fail "workspace_status.sh: STABLE_GIT_COMMIT is '$got_b' after a second commit, want '$head_b' (was '$got_a')"
fi

# Control: the scan below reads lines; it must be reading the real script.
grep -q '^echo "STABLE_GIT_VERSION ' "$status_script" ||
	fail "workspace_status.sh does not look like the status script (no STABLE_GIT_VERSION line): the checks above cannot be trusted"

# --- 2. the image macro --------------------------------------------------------
# Comments are prose about what used to be here; only code is checked.
code="$(sed -E 's/^[[:space:]]*#.*$//' "$macro")"

grep -q 'tag_list = \[' <<<"$code" ||
	fail "go_multi_arch_image.bzl has no tag_list: the scan cannot be trusted"

# shellcheck disable=SC2016 # a literal Go template, not a shell expansion
if grep -qF '{{.tag}}-{{.STABLE_GIT_COMMIT}}' <<<"$code"; then
	pass "the per-commit tag reads STABLE_GIT_COMMIT"
else
	fail "go_multi_arch_image.bzl: no '{{.tag}}-{{.STABLE_GIT_COMMIT}}' tag. A tag built from a volatile key goes stale on a warm Bazel server once the image itself no longer changes per commit (#1378)"
fi
if grep -qE '\{\{[^}]*\.GIT_COMMIT' <<<"$code"; then
	fail "go_multi_arch_image.bzl reads the volatile GIT_COMMIT in a template: use STABLE_GIT_COMMIT (#1378)"
fi
if grep -q 'image\.revision' <<<"$code"; then
	fail "go_multi_arch_image.bzl sets org.opencontainers.image.revision: a label or annotation is part of the digest, so every commit gives every image a new one (#1378)"
else
	pass "no org.opencontainers.image.revision in the macro"
fi
if grep -qE 'stamp[[:space:]]*=' <<<"$code"; then
	fail "go_multi_arch_image.bzl sets a stamp attribute: image_manifest and image_index must not read the workspace status (#1378)"
else
	pass "no stamp attribute on the image rules"
fi

# The provenance set may hold literals only: a template there reaches the
# config, the manifest and the index.
provenance="$(sed -n '/^_PROVENANCE = {/,/^}/p' <<<"$code")"
[ -n "$provenance" ] || fail "go_multi_arch_image.bzl has no _PROVENANCE block: the scan cannot be trusted"
if grep -q '{{' <<<"$provenance"; then
	fail "_PROVENANCE holds a template: $provenance"
else
	pass "_PROVENANCE holds literals only"
fi

# --- 3. the built indexes ------------------------------------------------------
# Every annotation on the index and on each per-platform descriptor, as
# "<where>\t<key>\t<value>" lines.
annotations() {
	"$JQ" -r '
		((.annotations // {}) | to_entries[] | ["index", .key, (.value | tostring)]),
		(.manifests[] | (.platform.architecture) as $a
			| (.annotations // {}) | to_entries[] | ["descriptor/" + $a, .key, (.value | tostring)])
		| @tsv' "$1"
}

# commit_like <value>: a bare 40-hex string, or one inside a longer value
# (`v1.2.3-4-g<sha>`). An image digest is `sha256:` plus 64 hex and is allowed:
# the base image's digest is content, not a commit.
commit_like() {
	local v
	v="$(sed -E 's/sha256:[0-9a-f]{64}//g' <<<"$1")"
	[[ "$v" =~ (^|[^0-9a-f])[0-9a-f]{40}($|[^0-9a-f]) ]]
}

# Self-test of the two detectors, so a broken regex cannot pass everything.
commit_like "0123456789abcdef0123456789abcdef01234567" || fail "self-test: a bare sha is not detected"
commit_like "v1.2.3-4-g0123456789abcdef0123456789abcdef01234567-dirty" || fail "self-test: a sha inside git-describe output is not detected"
commit_like "sha256:2293b36c7c9082bf4115aab724b4d2cddec82c8eba39bf27ac0517e159acf150" && fail "self-test: an image digest is taken for a commit"
commit_like "https://github.com/bpalermo/aether" && fail "self-test: a URL is taken for a commit"

checked=0
for idx in "${indexes[@]}"; do
	name="${idx#"$PWD"/}"
	n="$("$JQ" -r '.manifests | length' "$idx" 2>/dev/null)"
	if [ "${n:-0}" -lt 2 ]; then
		fail "$name: not a multi-platform image index (manifests: ${n:-unreadable})"
		continue
	fi
	bad=0
	seen=0
	while IFS=$'\t' read -r where key value; do
		[ -n "$where" ] || continue
		seen=$((seen + 1))
		if [[ "$key" == *revision* ]]; then
			fail "$name: $where carries '$key=$value'"
			bad=1
		elif [[ "$value" == *"{{"* ]]; then
			fail "$name: $where '$key' is an unexpanded template: $value"
			bad=1
		elif commit_like "$value"; then
			fail "$name: $where '$key=$value' looks like a commit"
			bad=1
		fi
	done < <(annotations "$idx")
	# Control: every index carries the source annotation; none seen means the
	# jq program stopped matching the document.
	if [ "$seen" -eq 0 ] || ! "$JQ" -e '.annotations["org.opencontainers.image.source"]' "$idx" >/dev/null; then
		fail "$name: no org.opencontainers.image.source annotation read: the scan cannot be trusted"
		bad=1
	fi
	[ "$bad" -ne 0 ] || checked=$((checked + 1))
done
[ "$checked" -eq "${#indexes[@]}" ] && pass "$checked image indexes carry no commit"

if [ "$failures" -ne 0 ]; then
	echo "$failures check(s) failed"
	exit 1
fi
echo "PASS"
