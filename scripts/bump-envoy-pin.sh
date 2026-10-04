#!/usr/bin/env bash
# Move the aether-proxy Envoy pin to the registry snapshot of an Envoy commit,
# and report what the move means. A maintainer tool: NOT run in CI, never builds
# Envoy.
#
#   scripts/bump-envoy-pin.sh [--write] [--keep-work] <envoy-commit | latest>
#
# <envoy-commit> is 7..40 hex of an upstream envoyproxy/envoy commit the
# bazel-registry cut a snapshot from; `latest` is the newest snapshot at the
# registry's main HEAD.
#
# 1. Resolves the envoyproxy/bazel-registry commit to pin: the NEWEST registry
#    commit at which that snapshot still exists (HEAD, or the parent of the
#    commit that dropped it), from the history of modules/envoy/metadata.json.
# 2. Computes the rewrite of proxy/.bazelrc (registry line) and proxy/MODULE.bazel
#    (envoy, envoy_api, the envoy single_version_override, every mention of the
#    old snapshot, and each `.envoy` sibling moved to the version the new envoy
#    module requests) as ONE edit, and prints it as a diff. Dry run by default;
#    --write applies it.
# 3. Prints upstream Envoy's root .bazelrc diff between the old and new Envoy
#    commits, restricted to the scopes proxy/.bazelrc mirrors (unconditional,
#    :linux, :clang*, :libc++), each line tagged [mirrored] when proxy/.bazelrc
#    sets that flag.
# 4. Applies every carried patch (proxy/bazel/patches, in MODULE.bazel order) to
#    a sparse shallow checkout of the new Envoy commit — and, as a baseline, of
#    the current one — reporting each as applies / UPSTREAM (reverse-applies:
#    drop it) / CONFLICT (refresh it). This is issue #980's question, answered
#    mechanically.
# 5. Prints the manual steps left: the lock refresh, the carried-patch tests, the
#    go-control-plane parity check.
#
# Needs curl, git and (for an explicit commit) an authenticated gh. The pure
# parts live in scripts/envoy-pin-lib.sh and are exercised offline by
# `scripts/check-envoy-pin.sh --self-test`.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck disable=SC1091
. scripts/envoy-pin-lib.sh

MOD=proxy/MODULE.bazel
RC=proxy/.bazelrc
REGISTRY_GIT=https://github.com/envoyproxy/bazel-registry
ENVOY_GIT=https://github.com/envoyproxy/envoy.git
# How far back the registry history is searched for an explicit commit (100
# metadata.json changes per page).
MAX_PAGES="${MAX_PAGES:-5}"

die() {
	echo "bump-envoy-pin.sh: $*" >&2
	exit 1
}

write=0
keep=0
target=""
while [ $# -gt 0 ]; do
	case "$1" in
	--write) write=1 ;;
	--keep-work) keep=1 ;;
	-h | --help)
		sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'
		exit 0
		;;
	-*) die "unknown flag $1" ;;
	*)
		[ -z "$target" ] || die "one target, got [$target] and [$1]"
		target="$1"
		;;
	esac
	shift
done
[ -n "$target" ] || die "usage: $0 [--write] [--keep-work] <envoy-commit | latest>"
if [ "$target" != latest ] && ! grep -Eq '^[0-9a-f]{7,40}$' <<<"$target"; then
	die "target must be 'latest' or 7..40 hex of an Envoy commit, got [$target]"
fi

work="$(mktemp -d)"
if [ "$keep" = 1 ]; then
	echo "work dir (kept): $work"
else
	trap 'rm -rf "$work"' EXIT
fi

# --- the current pin ----------------------------------------------------------
echo "== current pin"
ep_check_offline "$MOD" "$RC" >/dev/null || die "the current pin is not self-consistent: run scripts/check-envoy-pin.sh"
old_ver="$(ep_dep_version "$MOD" envoy)"
old_reg="$(ep_registry_commit "$RC")"
old_full="$(ep_envoy_commit "$old_reg" "$old_ver" || true)"
echo "  envoy ${old_ver}"
echo "  registry ${old_reg}"
echo "  Envoy commit ${old_full:-<source.json unreadable at the pinned registry commit>}"

# --- resolve the target -------------------------------------------------------
echo "== resolving ${target}"
head="$(git ls-remote "$REGISTRY_GIT" refs/heads/main | awk '{ print $1 }')"
grep -Eq '^[0-9a-f]{40}$' <<<"$head" || die "could not read ${REGISTRY_GIT} main"
echo "  registry main HEAD ${head}"
if [ "$target" = latest ]; then
	new_reg="$head"
	new_ver="$(ep_fetch "${EP_REGISTRY_RAW}/${head}/modules/envoy/metadata.json" | ep_metadata_versions | ep_newest_snapshot)"
	[ -n "$new_ver" ] || die "no envoy version in modules/envoy/metadata.json at ${head}"
else
	command -v gh >/dev/null || die "an explicit commit needs gh (the registry's commit history)"
	hist="$work/history"
	: >"$hist"
	found=0
	for page in $(seq 1 "$MAX_PAGES"); do
		commits="$(gh api "repos/envoyproxy/bazel-registry/commits?path=modules/envoy/metadata.json&sha=main&per_page=100&page=${page}" \
			--jq '.[] | .sha + " " + .parents[0].sha')" || die "gh api: registry history page ${page}"
		[ -n "$commits" ] || break
		while read -r c p; do
			vs="$(ep_fetch "${EP_REGISTRY_RAW}/${c}/modules/envoy/metadata.json" | ep_metadata_versions | tr '\n' ' ')" || vs=""
			echo "$c $p $vs" >>"$hist"
			if pick="$(ep_pick_registry_commit "$target" "$head" <"$hist")"; then
				found=1
				break 2
			fi
		done <<<"$commits"
	done
	[ "$found" = 1 ] || die "no registry snapshot was cut from Envoy ${target} in the last $(wc -l <"$hist" | tr -d ' ') metadata.json changes (MAX_PAGES=${MAX_PAGES})"
	new_reg="${pick%% *}"
	new_ver="${pick#* }"
fi
new_full="$(ep_envoy_commit "$new_reg" "$new_ver" || true)"
[ -n "$new_full" ] || die "modules/envoy/${new_ver}/source.json at ${new_reg} names no Envoy commit"
short="$(ep_short_sha "$new_ver")"
[ -z "$short" ] || [ "${new_full#"$short"}" != "$new_full" ] || die "source.json archives ${new_full}, the version names ${short}"
echo "  envoy ${new_ver}"
echo "  registry ${new_reg}"
echo "  Envoy commit ${new_full}"
[ "$(ep_http_status "${EP_REGISTRY_RAW}/${new_reg}/modules/envoy_api/${new_ver}/MODULE.bazel")" = 200 ] ||
	die "envoy_api@${new_ver} is not served at ${new_reg}: the registry is not self-consistent there (proxy/README.md, \"Envoy version bumps\")"

# --- the .envoy siblings ------------------------------------------------------
echo "== .envoy siblings declared in ${MOD}"
ep_fetch "${EP_REGISTRY_RAW}/${new_reg}/modules/envoy/${new_ver}/MODULE.bazel" >"$work/envoy.MODULE.bazel" ||
	die "cannot fetch modules/envoy/${new_ver}/MODULE.bazel at ${new_reg}"
ep_fetch "${EP_REGISTRY_RAW}/${new_reg}/modules/envoy_api/${new_ver}/MODULE.bazel" >"$work/envoy_api.MODULE.bazel" ||
	die "cannot fetch modules/envoy_api/${new_ver}/MODULE.bazel at ${new_reg}"
moves=()
while read -r name cur; do
	case "$name" in envoy | envoy_api) continue ;; esac
	case "$cur" in *.envoy) ;; *) continue ;; esac
	want="$(ep_deps "$work/envoy.MODULE.bazel" | awk -v n="$name" '$1 == n { print $2; exit }')"
	why="requested by envoy@${new_ver}"
	if [ -z "$want" ]; then
		if [ "$(ep_http_status "${EP_REGISTRY_RAW}/${new_reg}/modules/${name}/${cur}/MODULE.bazel")" = 200 ]; then
			want="$cur"
			why="not requested by envoy; still served"
		else
			want="$(ep_fetch "${EP_REGISTRY_RAW}/${new_reg}/modules/${name}/metadata.json" | ep_metadata_versions | sort -V | tail -1 || true)"
			why="not requested by envoy; ${cur} is gone, newest served"
		fi
	fi
	[ -n "$want" ] || die "${name}: no version to move to at ${new_reg}"
	if [ "$want" = "$cur" ]; then
		printf '  %-22s %s (unchanged; %s)\n' "$name" "$cur" "$why"
	else
		printf '  %-22s %s -> %s (%s)\n' "$name" "$cur" "$want" "$why"
		moves+=("${name}=${want}")
	fi
done < <(ep_deps "$MOD")

# Every `.envoy` module envoy/envoy_api request directly must exist at the new
# registry commit (one level; proxy/README.md's loop walks the full closure).
misses=0
while read -r name ver; do
	case "$ver" in *.envoy) ;; *) continue ;; esac
	if [ "$(ep_http_status "${EP_REGISTRY_RAW}/${new_reg}/modules/${name}/${ver}/MODULE.bazel")" != 200 ]; then
		echo "  MISSING at ${new_reg:0:12}: ${name}@${ver} (requested by envoy/envoy_api)"
		misses=$((misses + 1))
	fi
done < <(cat "$work/envoy.MODULE.bazel" "$work/envoy_api.MODULE.bazel" | sed -n -E 's/^bazel_dep\(name = "([^"]+)", version = "([^"]+)".*$/\1 \2/p' | sort -u)
if [ "$misses" -eq 0 ]; then
	echo "  every .envoy module envoy/envoy_api request is served at ${new_reg:0:12}"
else
	echo "  WARNING: ${misses} requested module(s) missing: the registry is not self-consistent at this commit; an older commit (or the next snapshot) may be (proxy/README.md, \"Envoy version bumps\")"
fi

# --- the rewrite --------------------------------------------------------------
echo "== rewrite (proxy/MODULE.bazel + proxy/.bazelrc, one edit)"
if [ "$new_ver" = "$old_ver" ] && [ "$new_reg" = "$old_reg" ] && [ "${#moves[@]}" -eq 0 ]; then
	echo "  already pinned: nothing to rewrite"
else
	mkdir -p "$work/rw/proxy"
	cp "$MOD" "$work/rw/proxy/MODULE.bazel"
	cp "$RC" "$work/rw/proxy/.bazelrc"
	ep_rewrite "$work/rw/proxy/MODULE.bazel" "$work/rw/proxy/.bazelrc" "$old_ver" "$new_ver" "$new_reg" "${moves[@]}" ||
		die "the rewrite refused (see above); nothing changed"
	ep_check_offline "$work/rw/proxy/MODULE.bazel" "$work/rw/proxy/.bazelrc" >"$work/rw.check" ||
		{
			cat "$work/rw.check"
			die "the rewritten pin fails the offline check; nothing changed"
		}
	diff -u --label "a/$MOD" --label "b/$MOD" "$MOD" "$work/rw/proxy/MODULE.bazel" || true
	diff -u --label "a/$RC" --label "b/$RC" "$RC" "$work/rw/proxy/.bazelrc" || true
	if [ "$write" = 1 ]; then
		cp "$work/rw/proxy/MODULE.bazel" "$MOD"
		cp "$work/rw/proxy/.bazelrc" "$RC"
		echo "  WRITTEN. The pin's explanatory comment in ${RC} (\"<sha> is the commit at which ...\", \"Why this bump\") still describes the old pin: rewrite it by hand."
	else
		echo "  dry run: pass --write to apply"
	fi
fi

# --- upstream .bazelrc diff ---------------------------------------------------
echo "== upstream Envoy .bazelrc, ${old_full:0:12} -> ${new_full:0:12} (scopes proxy/.bazelrc mirrors)"
if [ -z "$old_full" ]; then
	echo "  skipped: the current pin's Envoy commit is unknown"
elif [ "$old_full" = "$new_full" ]; then
	echo "  same Envoy commit: no change"
else
	ep_fetch "${EP_ENVOY_RAW}/${old_full}/.bazelrc" >"$work/old.bazelrc" || die "cannot fetch .bazelrc at ${old_full}"
	ep_fetch "${EP_ENVOY_RAW}/${new_full}/.bazelrc" >"$work/new.bazelrc" || die "cannot fetch .bazelrc at ${new_full}"
	ep_rc_diff "$work/old.bazelrc" "$work/new.bazelrc" "$RC"
fi

# --- carried patches ----------------------------------------------------------
mapfile -t patches < <(ep_patch_list "$MOD" | sed 's#^#proxy/#')
[ "${#patches[@]}" -gt 0 ] || die "no carried patches parsed from ${MOD}"
ep_patch_paths "${patches[@]}" | sed 's#^#/#' >"$work/sparse"

# checkout <full sha> <dir> — a sparse, blobless, depth-1 checkout of only the
# paths the patches touch (a full Envoy clone is ~1 GB).
checkout() {
	git init -q "$2"
	git -C "$2" remote add origin "$ENVOY_GIT"
	git -C "$2" sparse-checkout set --no-cone --stdin <"$work/sparse"
	git -C "$2" fetch -q --depth 1 --filter=blob:none origin "$1"
	git -C "$2" checkout -q FETCH_HEAD
}
report() {
	echo "== carried patches vs Envoy $1 ($2)"
	checkout "$1" "$work/envoy-$1" || die "cannot check out Envoy $1"
	ep_patch_report "$work/envoy-$1" "${patches[@]}"
}
if [ -n "$old_full" ] && [ "$old_full" != "$new_full" ]; then
	report "$old_full" "baseline: the current pin; every patch should apply"
fi
report "$new_full" "the new pin"

if [ "$write" = 1 ]; then step1="Rewrite the pin's comment in proxy/.bazelrc by hand (why this bump)."; else step1="Apply the rewrite: re-run with --write, then rewrite the pin's comment in proxy/.bazelrc."; fi
cat <<EOF
== next steps (manual)
  1. ${step1}
  2. Drop every UPSTREAM patch from proxy/bazel/patches/, its single_version_override
     entry and its proxy/README.md row; refresh every CONFLICT patch against ${new_full:0:12}.
  3. Re-diff proxy/.bazelrc against the [mirrored] / + lines above (and the filter-cc
     template named at the top of that file).
  4. Refresh the lock and check resolution with the proxy workspace's own Bazel
     (module resolution only, no Envoy build; proxy/README.md "Build"):
       cd proxy && bazel mod graph --depth=1 --lockfile_mode=update
       bazel mod explain @quiche @protobuf @abseil-cpp
  5. Run the carried-patch tests (builds the patched sources only):
       cd proxy && bazel test //bazel/patches:carried_patch_tests
  6. scripts/check-envoy-pin.sh --online && scripts/check-envoy-api-parity.sh
     (a new minor may need go-control-plane/envoy bumped: the parity script prints the command).
  7. One PR; CI's proxy workflow builds both arches. Merging it starts proxy-release.
EOF
