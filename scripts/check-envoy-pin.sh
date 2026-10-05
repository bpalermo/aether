#!/usr/bin/env bash
# Assert the aether-proxy Envoy pin is self-consistent (scripts/envoy-pin-lib.sh
# says what the pin is and why it is one fact in two files).
#
#   scripts/check-envoy-pin.sh             offline: proxy/MODULE.bazel + proxy/.bazelrc
#   scripts/check-envoy-pin.sh --online    + the pinned registry commit really serves it
#   scripts/check-envoy-pin.sh --self-test the offline harness: every red case must be red
#
# Offline:
#   (a) envoy and envoy_api are the same version, and the envoy
#       single_version_override (the carried patches) names that version too;
#   (b) proxy/.bazelrc has exactly ONE envoyproxy/bazel-registry line, pinned by
#       a full 40-hex commit (a branch name breaks on the next upstream publish);
#   (d) every snapshot version spelled out in either file, comments included, is
#       the pinned one. The workspace records the Envoy source commit only as the
#       short sha inside that version; nothing in proxy/ holds the full sha.
# Online (CI, `envoy-api-parity` job):
#   (c) the registry commit serves modules/<m>/<v>/MODULE.bazel (HTTP 200) for
#       envoy, envoy_api and every `.envoy`-suffixed bazel_dep in MODULE.bazel;
#   (d) modules/envoy/<v>/source.json at that commit archives a full Envoy commit
#       that starts with the short sha in <v>.
#
# The --self-test cases run on copies of the real files (mutated with sed) and a
# fake ep_http_status / ep_fetch, so they need no network; they also cover the
# bump tool's rewrite, registry-commit selection and patch report
# (scripts/bump-envoy-pin.sh).
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck disable=SC1091
. scripts/envoy-pin-lib.sh

MOD=proxy/MODULE.bazel
RC=proxy/.bazelrc

real() {
	local online="$1" rc=0
	echo "Envoy pin, offline (${MOD}, ${RC}):"
	ep_check_offline "$MOD" "$RC" || rc=1
	if [ "$online" = 1 ]; then
		echo "Envoy pin, online (${EP_REGISTRY_RAW}):"
		ep_check_online "$MOD" "$RC" || rc=1
	fi
	if [ "$rc" -ne 0 ]; then
		echo "::error::the Envoy pin is not self-consistent; see the FAIL lines above (proxy/README.md, \"Envoy version bumps\"; scripts/bump-envoy-pin.sh moves it as one)" >&2
		exit 1
	fi
	echo "OK: the Envoy pin is self-consistent"
}

self_test() {
	local fail=0 n=0
	# Global, not local: the EXIT trap runs after this function has returned.
	work="$(mktemp -d)"
	trap 'rm -rf "$work"' EXIT

	ok() { printf '  ok    %s\n' "$1"; }
	bad() {
		printf '  FAIL  %s\n' "$1"
		fail=1
	}

	local ver reg short
	ver="$(ep_dep_version "$MOD" envoy)"
	reg="$(ep_registry_commit "$RC")"
	short="$(ep_short_sha "$ver")"
	if [ -z "$ver" ] || [ -z "$reg" ] || [ -z "$short" ]; then
		echo "::error::the real pin does not parse (envoy=[${ver}] registry=[${reg}] short=[${short}]); the cases below would test nothing" >&2
		exit 2
	fi
	local full
	full="${short}$(printf '0%.0s' $(seq 1 $((40 - ${#short}))))"

	# fixture <name> [<sed expr for MODULE.bazel> [<sed expr for .bazelrc>]]
	fixture() {
		mkdir -p "$work/$1"
		sed -e "${2:-}" "$MOD" >"$work/$1/MODULE.bazel"
		sed -e "${3:-}" "$RC" >"$work/$1/bazelrc"
	}
	# offline_case <name> <want: pass|fail> <fixture dir>
	offline_case() {
		local out rc=0
		n=$((n + 1))
		out="$(ep_check_offline "$work/$3/MODULE.bazel" "$work/$3/bazelrc")" || rc=$?
		if { [ "$2" = pass ] && [ "$rc" = 0 ]; } || { [ "$2" = fail ] && [ "$rc" != 0 ]; }; then
			ok "$1 ($2)"
		else
			bad "$1: want $2, got rc=${rc}"
			printf '%s\n' "$out" | sed 's/^/        | /'
		fi
	}

	echo "offline (a) (b) (d):"
	fixture real
	offline_case "the real proxy/MODULE.bazel + proxy/.bazelrc" pass real
	fixture api_skew "s/^bazel_dep(name = \"envoy_api\", version = \"[^\"]*\")/bazel_dep(name = \"envoy_api\", version = \"1.40.0-dev.20260904.13144fb.envoy\")/"
	offline_case "envoy_api on a different snapshot than envoy" fail api_skew
	fixture ovr_skew "/^single_version_override(/,/^)/ s/^    version = \"[^\"]*\"/    version = \"1.40.0.envoy\"/"
	offline_case "single_version_override(envoy) on another version" fail ovr_skew
	fixture branch "" "s#envoyproxy/bazel-registry/[0-9a-f]\{40\}#envoyproxy/bazel-registry/main#"
	offline_case "registry pinned by a branch name" fail branch
	fixture abbrev "" "s#envoyproxy/bazel-registry/\([0-9a-f]\{12\}\)[0-9a-f]*#envoyproxy/bazel-registry/\1#"
	offline_case "registry pinned by an abbreviated commit" fail abbrev
	fixture two "" "/^common --registry=https:\/\/raw.githubusercontent.com\/envoyproxy/{p;s#/[0-9a-f]\{40\}#/$(printf 'a%.0s' $(seq 1 40))#;}"
	offline_case "two envoy registry lines" fail two
	fixture none "" "/^common --registry=https:\/\/raw.githubusercontent.com\/envoyproxy/d"
	offline_case "no envoy registry line" fail none
	fixture stale "1s/^/# was envoy@1.40.0-dev.20260904.13144fb.envoy\n/"
	offline_case "a stale snapshot version left in a comment" fail stale

	# online_case <name> <want> <missing url substring|''> <source.json archive sha>
	online_case() {
		local out rc=0
		n=$((n + 1))
		FAKE_MISSING="$3" FAKE_SHA="$4"
		out="$(ep_check_online "$work/real/MODULE.bazel" "$work/real/bazelrc")" || rc=$?
		if { [ "$2" = pass ] && [ "$rc" = 0 ]; } || { [ "$2" = fail ] && [ "$rc" != 0 ]; }; then
			ok "$1 ($2)"
		else
			bad "$1: want $2, got rc=${rc}"
			printf '%s\n' "$out" | sed 's/^/        | /'
		fi
	}
	# The fake registry: every module dir exists at the pinned commit except the
	# ones whose URL contains $FAKE_MISSING (404), or `5xx` for all of them.
	# FAKE_* are globals: ep_check_online's own locals (reg, ver) shadow the
	# harness's under bash's dynamic scoping.
	FAKE_MISSING="" FAKE_SHA="" FAKE_REG="$reg" FAKE_VER="$ver"
	ep_http_status() {
		case "$1" in
		"${EP_REGISTRY_RAW}/${FAKE_REG}/modules/"*) ;;
		*)
			echo 404
			return
			;;
		esac
		if [ "$FAKE_MISSING" = 5xx ]; then
			echo 503
		elif [ -n "$FAKE_MISSING" ] && [ "${1#*"$FAKE_MISSING"}" != "$1" ]; then
			echo 404
		else
			echo 200
		fi
	}
	ep_fetch() {
		case "$1" in
		"${EP_REGISTRY_RAW}/${FAKE_REG}/modules/envoy/${FAKE_VER}/source.json")
			printf '{\n    "url": "https://github.com/envoyproxy/envoy/archive/%s.tar.gz",\n    "strip_prefix": "envoy-%s"\n}\n' "$FAKE_SHA" "$FAKE_SHA"
			;;
		*) return 22 ;;
		esac
	}
	local sibling
	sibling="$(ep_deps "$MOD" | awk '$1 != "envoy" && $1 != "envoy_api" && $2 ~ /\.envoy$/ { print $1 "/" $2; exit }')"
	echo "online (c) (d), fake registry:"
	online_case "every module served, source.json matches the short sha" pass "" "$full"
	online_case "the envoy module dir is gone (registry moved on)" fail "/modules/envoy/${ver}/" "$full"
	online_case "the envoy_api module dir is gone" fail "/modules/envoy_api/${ver}/" "$full"
	online_case "a .envoy sibling (${sibling}) is gone" fail "/modules/${sibling}/" "$full"
	online_case "registry unreachable (503): fail closed" fail 5xx "$full"
	online_case "source.json archives a different Envoy commit" fail "" "$(printf 'f%.0s' $(seq 1 40))"

	echo "bump: ep_rewrite:"
	local newver="1.40.0-dev.20261001.abcdef0.envoy" newreg
	newreg="$(printf 'b%.0s' $(seq 1 40))"
	fixture rw
	local proto_old
	proto_old="$(ep_dep_version "$MOD" protobuf)"
	n=$((n + 1))
	if ep_rewrite "$work/rw/MODULE.bazel" "$work/rw/bazelrc" "$ver" "$newver" "$newreg" protobuf=99.0.envoy >/dev/null &&
		[ "$(ep_dep_version "$work/rw/MODULE.bazel" envoy)" = "$newver" ] &&
		[ "$(ep_dep_version "$work/rw/MODULE.bazel" envoy_api)" = "$newver" ] &&
		[ "$(ep_override_version "$work/rw/MODULE.bazel" envoy)" = "$newver" ] &&
		[ "$(ep_registry_commit "$work/rw/bazelrc")" = "$newreg" ] &&
		[ "$(ep_dep_version "$work/rw/MODULE.bazel" protobuf)" = 99.0.envoy ] &&
		! grep -qF "${ver%.envoy}" "$work/rw/MODULE.bazel" "$work/rw/bazelrc" &&
		ep_check_offline "$work/rw/MODULE.bazel" "$work/rw/bazelrc" >/dev/null &&
		[ "$(diff "$MOD" "$work/rw/MODULE.bazel" | grep -c '^>' || true)" -ge 4 ]; then
		ok "moves registry + envoy + envoy_api + override + sibling (protobuf ${proto_old} -> 99.0.envoy) together; result passes the offline check"
	else
		bad "the rewrite did not move the whole pin"
		diff "$MOD" "$work/rw/MODULE.bazel" | sed 's/^/        | /' || true
	fi
	# refuse_rewrite <name> <old> <new> <registry> [siblings...]
	refuse_rewrite() {
		local name="$1" before
		shift
		fixture rr
		before="$(cat "$work/rr/MODULE.bazel" "$work/rr/bazelrc")"
		n=$((n + 1))
		if ep_rewrite "$work/rr/MODULE.bazel" "$work/rr/bazelrc" "$@" 2>/dev/null; then
			bad "$name: accepted"
		elif [ "$(cat "$work/rr/MODULE.bazel" "$work/rr/bazelrc")" != "$before" ]; then
			bad "$name: refused but CHANGED a file"
		else
			ok "$name (refused, files unchanged)"
		fi
	}
	refuse_rewrite "old version is not what MODULE.bazel pins" "1.39.0.envoy" "$newver" "$newreg"
	refuse_rewrite "new registry is a branch name" "$ver" "$newver" main
	refuse_rewrite "new version is not a registry version" "$ver" "1.41.0-dev" "$newreg"
	refuse_rewrite "a sibling MODULE.bazel does not declare" "$ver" "$newver" "$newreg" no-such-module=1.0.envoy

	echo "bump: registry commit selection:"
	# pick_case <name> <envoy commit> <want "<registry> <version>"|FAIL> — the
	# history is newest first: c4 dropped 0926, c3 dropped 0904.
	local hist
	hist="$(printf '%s\n' \
		"c5 c5p 1.40.0-dev.20261001.9999999.envoy" \
		"c4 c4p 1.40.0-dev.20260927.5f95744.envoy" \
		"c3 c3p 1.40.0-dev.20260926.726d7ac.envoy 1.40.0-dev.20260927.5f95744.envoy" \
		"c2 c2p 1.40.0-dev.20260904.13144fb.envoy 1.40.0-dev.20260926.726d7ac.envoy" \
		"c1 c1p 1.40.0-dev.20260904.13144fb.envoy")"
	pick_case() {
		local got
		n=$((n + 1))
		got="$(printf '%s\n' "$hist" | ep_pick_registry_commit "$2" HEAD)" || got=FAIL
		if [ "$got" = "$3" ]; then ok "$1 -> $got"; else bad "$1: want [$3] got [$got]"; fi
	}
	pick_case "newest snapshot: registry HEAD" 9999999 "HEAD 1.40.0-dev.20261001.9999999.envoy"
	pick_case "full Envoy sha of a dropped snapshot: parent of the commit that dropped it" 726d7acb73934085cbbccc83ea47fdd78b583d7e "c4p 1.40.0-dev.20260926.726d7ac.envoy"
	pick_case "oldest snapshot: parent of c3, which dropped it" 13144fb "c3p 1.40.0-dev.20260904.13144fb.envoy"
	pick_case "an Envoy commit no snapshot was cut from" 0123456 FAIL
	n=$((n + 1))
	if [ "$(printf '%s\n' 1.40.0-dev.20260927.5f95744.envoy 1.40.0-dev.20260926.726d7ac.envoy | ep_newest_snapshot)" = 1.40.0-dev.20260927.5f95744.envoy ]; then
		ok "latest: the newest snapshot in metadata.json wins"
	else
		bad "latest: wrong snapshot picked"
	fi

	echo "bump: carried-patch report:"
	local t="$work/tree"
	mkdir -p "$t"
	git -C "$t" init -q
	printf 'a\nb\nc\n' >"$t/f.txt"
	printf 'x\ny\nz\n' >"$t/g.txt"
	mk_patch() { printf -- '--- a/%s\n+++ b/%s\n@@ -1,3 +1,3 @@\n %s\n-%s\n+%s\n %s\n' "$1" "$1" "$2" "$3" "$4" "$5" >"$work/$6"; }
	mk_patch f.txt a b B c 1-applies.patch
	mk_patch f.txt a B BB c 2-stacked.patch # needs 1 applied first
	mk_patch g.txt w y Y z 3-conflict.patch # context never matches
	mk_patch g.txt x q y z 4-upstream.patch # g.txt already has its result
	local report want
	# The first patch by a path relative to the caller's directory: ep_patch_report runs
	# git -C <tree>, so it must resolve it, not report it as a CONFLICT.
	report="$(cd "$work" && ep_patch_report "$t" 1-applies.patch "$work/2-stacked.patch" "$work/3-conflict.patch" "$work/4-upstream.patch" | awk '{ print $1 }' | tr '\n' ' ')"
	want="applies applies CONFLICT UPSTREAM "
	n=$((n + 1))
	if [ "$report" = "$want" ]; then ok "in-order application: applies / stacked applies / CONFLICT / UPSTREAM"; else bad "patch report: want [$want] got [$report]"; fi
	n=$((n + 1))
	if [ "$(ep_patch_list "$MOD" | wc -l | tr -d ' ')" -ge 1 ] && [ -z "$(ep_patch_list "$MOD" | while read -r p; do [ -f "proxy/$p" ] || echo "$p"; done)" ]; then
		ok "the real patch list parses ($(ep_patch_list "$MOD" | wc -l | tr -d ' ') patches, every file exists)"
	else
		bad "the patch list in ${MOD} does not parse to existing files"
	fi

	echo "bump: upstream .bazelrc diff:"
	printf 'build --copt=-a\ncommon --@x//y:z=1\nbuild:asan --copt=-fsanitize\ncommon --gone=1\n' >"$work/old.rc"
	printf 'build --copt=-a\ncommon --@x//y:z=2  # why\nbuild:asan --copt=-other\ncommon --new=1\n' >"$work/new.rc"
	printf 'common --@x//y:z=1\n' >"$work/proxy.rc"
	report="$(ep_rc_diff "$work/old.rc" "$work/new.rc" "$work/proxy.rc" | sed -E 's/^ +//' | tr '\n' '|')"
	want="~ common --@x//y:z=1|-> common --@x//y:z=2 [mirrored]|- common --gone=1|+ common --new=1|"
	n=$((n + 1))
	if [ "$report" = "$want" ]; then ok "changed / removed / added, mirrored flags tagged, other configs ignored"; else bad "rc diff: want [$want] got [$report]"; fi

	if [ "$n" -ne 27 ]; then
		echo "::error::ran ${n} cases, expected 27 -- a gate that checks nothing passes" >&2
		exit 2
	fi
	if [ "$fail" -ne 0 ]; then
		echo "::error::the Envoy pin checks are wrong; see cases above" >&2
		exit 1
	fi
	echo "Envoy pin harness: ${n}/${n} cases correct"
}

case "${1:-}" in
"") real 0 ;;
--online) real 1 ;;
--self-test) self_test ;;
*)
	echo "usage: $0 [--online | --self-test]" >&2
	exit 2
	;;
esac
