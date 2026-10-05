#!/usr/bin/env bash
# The aether-proxy Envoy pin: reading it, checking it, moving it.
#
# This file is SOURCED, never executed: `. scripts/envoy-pin-lib.sh`. It is used
# by scripts/check-envoy-pin.sh (the CI gate, and its offline --self-test) and
# by scripts/bump-envoy-pin.sh (the maintainer's bump tool).
#
# THE PIN IS TWO FILES, ONE FACT
#
#   proxy/MODULE.bazel  bazel_dep(name = "envoy",     version = "<V>")
#                       bazel_dep(name = "envoy_api", version = "<V>")
#                       single_version_override(module_name = "envoy", version = "<V>", patches = ...)
#                       every other `.envoy`-suffixed bazel_dep (protobuf, rules_rust, ...)
#   proxy/.bazelrc      common --registry=https://raw.githubusercontent.com/envoyproxy/bazel-registry/<C>
#
# The envoy bazel-registry keeps a rolling dev snapshot per module and DELETES
# the previous version directory when it moves on, so <V> resolves only at a
# registry commit <C> that still carries it. A branch name (`main`) in place of
# <C> builds today and breaks the day upstream publishes; two registry lines let
# whichever answers first win. See proxy/README.md, "Envoy version bumps".
#
# THE ENVOY SOURCE COMMIT is recorded in the workspace only as the short sha in
# <V> (`1.40.0-dev.<date>.<short>.envoy`); the full sha lives in the registry's
# `modules/envoy/<V>/source.json` (the archive URL), which the online check reads.
#
# Network access goes through ep_http_status and ep_fetch only, so the offline
# harness can replace them with fakes after sourcing this file.

EP_REGISTRY_RAW="${EP_REGISTRY_RAW:-https://raw.githubusercontent.com/envoyproxy/bazel-registry}"
EP_ENVOY_RAW="${EP_ENVOY_RAW:-https://raw.githubusercontent.com/envoyproxy/envoy}"

# ep_http_status <url> — the HTTP status code (000 when nothing answered).
ep_http_status() {
	curl -sS -o /dev/null -w '%{http_code}' --retry 3 --retry-delay 2 --max-time 30 "$1" 2>/dev/null || true
}

# ep_fetch <url> — the body on stdout; fails on any non-2xx.
ep_fetch() {
	curl -fsSL --retry 3 --retry-delay 2 --max-time 60 "$1"
}

# ep_deps <MODULE.bazel> — `name version` for every single-line bazel_dep.
ep_deps() {
	sed -n -E 's/^bazel_dep\(name = "([^"]+)", version = "([^"]+)".*$/\1 \2/p' "$1"
}

# ep_dep_version <MODULE.bazel> <name> — the version, iff exactly one bazel_dep
# names the module.
ep_dep_version() {
	local out
	out="$(ep_deps "$1" | awk -v n="$2" '$1 == n { print $2 }')"
	[ -n "$out" ] && [ "$(printf '%s\n' "$out" | wc -l)" -eq 1 ] || return 1
	printf '%s\n' "$out"
}

# ep_override_version <MODULE.bazel> <module> — the `version` of the
# single_version_override block naming <module> (empty when there is none).
ep_override_version() {
	awk -v want="$2" '
		/^single_version_override\(/ { inblk = 1; mod = ""; ver = ""; next }
		inblk && /^[[:space:]]*module_name = "/ { s = $0; sub(/^[^"]*"/, "", s); sub(/".*$/, "", s); mod = s }
		inblk && /^[[:space:]]*version = "/ { s = $0; sub(/^[^"]*"/, "", s); sub(/".*$/, "", s); ver = s }
		inblk && /^\)/ { if (mod == want) print ver; inblk = 0 }
	' "$1"
}

# ep_registry_lines <.bazelrc> — every non-comment line naming the envoy registry.
ep_registry_lines() {
	grep -E '^[[:space:]]*[^#[:space:]].*envoyproxy/bazel-registry' "$1" || true
}

# ep_registry_commit <.bazelrc> — the pinned registry commit (40 hex), iff it is
# the only envoy-registry line and has exactly the canonical shape.
ep_registry_commit() {
	local lines
	lines="$(ep_registry_lines "$1")"
	[ -n "$lines" ] && [ "$(printf '%s\n' "$lines" | wc -l)" -eq 1 ] || return 1
	printf '%s\n' "$lines" | sed -n -E 's#^common --registry=https://raw\.githubusercontent\.com/envoyproxy/bazel-registry/([0-9a-f]{40})$#\1#p' | grep -E '^[0-9a-f]{40}$'
}

# ep_short_sha <version> — the Envoy commit in a -dev snapshot version (empty
# for a release version, which carries none).
ep_short_sha() {
	printf '%s\n' "$1" | sed -n -E 's/^[0-9]+\.[0-9]+\.[0-9]+-dev\.[0-9]{8}\.([0-9a-f]{7,40})\.envoy$/\1/p'
}

# ep_check_offline <MODULE.bazel> <.bazelrc> — the self-consistency checks that
# need no network. Prints one line per check; returns non-zero on any failure.
ep_check_offline() {
	local mod="$1" rc="$2" fail=0 envoy api ovr reg n stale
	envoy="$(ep_dep_version "$mod" envoy)" || envoy=""
	api="$(ep_dep_version "$mod" envoy_api)" || api=""
	if [ -z "$envoy" ] || [ -z "$api" ]; then
		echo "  FAIL  need exactly one bazel_dep each for envoy and envoy_api in ${mod} (got envoy=[${envoy}] envoy_api=[${api}])"
		return 1
	fi
	if [ "$envoy" = "$api" ]; then
		echo "  ok    envoy and envoy_api are the same snapshot: ${envoy}"
	else
		echo "  FAIL  envoy ${envoy} != envoy_api ${api}: the two are published as ONE registry snapshot"
		fail=1
	fi
	if ! grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-dev\.[0-9]{8}\.[0-9a-f]{7,40})?\.envoy$' <<<"$envoy"; then
		echo "  FAIL  envoy version ${envoy} is neither <x.y.z>.envoy nor <x.y.z>-dev.<yyyymmdd>.<sha>.envoy"
		fail=1
	fi
	ovr="$(ep_override_version "$mod" envoy)"
	if grep -Eq '^[[:space:]]*module_name = "envoy",' "$mod"; then
		if [ "$ovr" = "$envoy" ]; then
			echo "  ok    single_version_override(envoy) patches the pinned version"
		else
			echo "  FAIL  single_version_override(envoy) names version [${ovr}], the bazel_dep pins ${envoy}: the carried patches would not apply to the module the build resolves"
			fail=1
		fi
	fi
	n="$(ep_registry_lines "$rc" | wc -l | tr -d ' ')"
	if [ "$n" -ne 1 ]; then
		echo "  FAIL  ${rc}: ${n} envoyproxy/bazel-registry lines, want exactly 1 (two pins let either answer; none leaves ${envoy} unresolvable)"
		fail=1
	elif reg="$(ep_registry_commit "$rc")"; then
		echo "  ok    registry pinned by commit: ${reg}"
	else
		echo "  FAIL  ${rc}: the registry line is not common --registry=${EP_REGISTRY_RAW}/<40-hex commit>: $(ep_registry_lines "$rc")"
		fail=1
	fi
	# Every snapshot version the two files spell out (comments included) must be
	# the pinned one: a stale `envoy@<old>` in the pin's own explanation is how a
	# half-done bump reads as finished.
	stale="$(grep -hoE '[0-9]+\.[0-9]+\.[0-9]+-dev\.[0-9]{8}\.[0-9a-f]{7,40}' "$mod" "$rc" | sort -u | grep -vxF "${envoy%.envoy}" || true)"
	if [ -n "$stale" ]; then
		echo "  FAIL  snapshot versions other than the pin are spelled out in ${mod} / ${rc}: $(printf '%s' "$stale" | tr '\n' ' ')"
		fail=1
	else
		echo "  ok    every snapshot version mentioned in ${mod} and ${rc} is ${envoy%.envoy}"
	fi
	return "$fail"
}

# ep_check_online <MODULE.bazel> <.bazelrc> — the pinned registry commit serves
# envoy, envoy_api and every `.envoy` sibling at the declared versions, and the
# envoy snapshot's source.json names a full Envoy commit matching the short sha
# in the version. Any answer but 200 fails (fail closed: an unreachable registry
# is not evidence the pin resolves).
ep_check_online() {
	local mod="$1" rc="$2" fail=0 envoy reg short url code name ver src full
	envoy="$(ep_dep_version "$mod" envoy)" || {
		echo "  FAIL  no single envoy bazel_dep in ${mod}"
		return 1
	}
	reg="$(ep_registry_commit "$rc")" || {
		echo "  FAIL  no single commit-pinned registry line in ${rc}"
		return 1
	}
	while read -r name ver; do
		case "$ver" in *.envoy) ;; *) continue ;; esac
		url="${EP_REGISTRY_RAW}/${reg}/modules/${name}/${ver}/MODULE.bazel"
		code="$(ep_http_status "$url")"
		if [ "$code" = 200 ]; then
			echo "  ok    ${name}@${ver} at registry ${reg:0:12}"
		else
			echo "  FAIL  ${name}@${ver}: HTTP ${code} for ${url}"
			fail=1
		fi
	done < <(ep_deps "$mod")
	short="$(ep_short_sha "$envoy")"
	if [ -z "$short" ]; then
		echo "  ok    ${envoy} is a release version: no embedded Envoy commit to match"
		return "$fail"
	fi
	src="$(ep_fetch "${EP_REGISTRY_RAW}/${reg}/modules/envoy/${envoy}/source.json" 2>/dev/null)" || src=""
	full="$(printf '%s\n' "$src" | sed -n -E 's#^.*"url": *"https://github\.com/envoyproxy/envoy/archive/([0-9a-f]{40})\.tar\.gz".*$#\1#p' | head -1)"
	if [ -z "$full" ]; then
		echo "  FAIL  modules/envoy/${envoy}/source.json at ${reg:0:12}: no envoyproxy/envoy archive URL with a 40-hex commit"
		fail=1
	elif [ "${full#"$short"}" = "$full" ]; then
		echo "  FAIL  source.json archives Envoy ${full}, but the version names ${short}"
		fail=1
	else
		echo "  ok    Envoy source commit ${full} (matches ${short} in the version)"
	fi
	return "$fail"
}

# ep_envoy_commit <registry commit> <version> — the full Envoy commit from the
# snapshot's source.json.
ep_envoy_commit() {
	ep_fetch "${EP_REGISTRY_RAW}/$1/modules/envoy/$2/source.json" |
		sed -n -E 's#^.*"url": *"https://github\.com/envoyproxy/envoy/archive/([0-9a-f]{40})\.tar\.gz".*$#\1#p' | head -1
}

# ep_metadata_versions — the `versions` of a registry metadata.json on stdin,
# one per line.
ep_metadata_versions() {
	tr -d '\n' | sed -n -E 's/^.*"versions": *\[([^]]*)\].*$/\1/p' | tr ',' '\n' | sed -n -E 's/^[[:space:]]*"([^"]+)"[[:space:]]*$/\1/p'
}

# ep_newest_snapshot — the newest -dev snapshot among the versions on stdin (by
# its <yyyymmdd>, then version order). Release versions sort after every
# snapshot of the same x.y.z by `sort -V`, which is the order wanted.
ep_newest_snapshot() {
	sed -E 's/^(([0-9]+\.[0-9]+\.[0-9]+)-dev\.([0-9]{8})\..*)$/\2 \3 \1/; t; s/^(([0-9]+\.[0-9]+\.[0-9]+)\..*)$/\2 99999999 \1/' |
		sort -V -k1,1 -k2,2 | tail -1 | awk '{ print $3 }'
}

# ep_pick_registry_commit <envoy commit (>=7 hex)> <registry HEAD> — reads the
# history of modules/envoy/metadata.json on stdin, newest first, one commit per
# line: `<commit> <parent> <version> [<version>...]`. Prints `<registry commit>
# <version>`: the NEWEST registry commit at which the snapshot cut from that
# Envoy commit still exists. That is HEAD if no newer metadata change dropped it,
# else the parent of the commit that did.
ep_pick_registry_commit() {
	awk -v want="$1" -v head="$2" '
		function matches(v,    s) {
			s = v
			if (!sub(/^[0-9]+\.[0-9]+\.[0-9]+-dev\.[0-9]+\./, "", s)) return 0
			sub(/\.envoy$/, "", s)
			return (index(want, s) == 1 || index(s, want) == 1)
		}
		{
			hit = ""
			for (i = 3; i <= NF; i++) if (matches($i)) hit = $i
			if (hit != "") {
				print ((prev == "") ? head : prevparent), hit
				found = 1
				exit
			}
			prev = $1; prevparent = $2
		}
		END { if (!found) exit 1 }
	'
}

# ep_rewrite <MODULE.bazel> <.bazelrc> <old version> <new version> <new registry
# commit> [<name>=<version> ...] — moves the whole pin in place: the registry
# line, envoy, envoy_api, the envoy single_version_override, every mention of the
# old snapshot version, and the named `.envoy` siblings. All or nothing: any
# precondition that does not hold leaves both files untouched.
ep_rewrite() {
	local mod="$1" rc="$2" old="$3" new="$4" reg="$5" tmpm tmpr kv name ver cur
	shift 5
	grep -Eq '^[0-9a-f]{40}$' <<<"$reg" || {
		echo "ep_rewrite: registry commit [${reg}] is not 40 hex" >&2
		return 1
	}
	grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-dev\.[0-9]{8}\.[0-9a-f]{7,40})?\.envoy$' <<<"$new" || {
		echo "ep_rewrite: [${new}] is not an envoy registry version" >&2
		return 1
	}
	[ "$(ep_dep_version "$mod" envoy 2>/dev/null || true)" = "$old" ] &&
		[ "$(ep_dep_version "$mod" envoy_api 2>/dev/null || true)" = "$old" ] || {
		echo "ep_rewrite: ${mod} does not pin envoy and envoy_api at ${old}" >&2
		return 1
	}
	local old_reg
	old_reg="$(ep_registry_commit "$rc")" || {
		echo "ep_rewrite: ${rc} has no single commit-pinned registry line" >&2
		return 1
	}
	tmpm="$(mktemp)"
	tmpr="$(mktemp)"
	# Version strings are [0-9a-z.-]; the dots are the only regex metacharacter.
	# The registry commit moves on its line and wherever a comment abbreviates it
	# to 8 hex ("263a259e is the commit at which ..."); the rest of that comment
	# (why this bump, which registry PRs) is the maintainer's to rewrite.
	sed -E "s/${old//./\\.}/${new}/g" "$mod" >"$tmpm"
	sed -E -e "s#^(common --registry=https://raw\.githubusercontent\.com/envoyproxy/bazel-registry/)[0-9a-f]{40}\$#\1${reg}#" \
		-e "s/${old//./\\.}/${new}/g" -e "s/\\b${old_reg:0:8}\\b/${reg:0:8}/g" "$rc" >"$tmpr"
	for kv in "$@"; do
		name="${kv%%=*}"
		ver="${kv#*=}"
		cur="$(ep_dep_version "$tmpm" "$name" 2>/dev/null)" || {
			echo "ep_rewrite: ${mod} has no single bazel_dep for ${name}" >&2
			rm -f "$tmpm" "$tmpr"
			return 1
		}
		awk -v n="$name" -v v="$ver" '
			index($0, "bazel_dep(name = \"" n "\", version = \"") == 1 { sub(/version = "[^"]+"/, "version = \"" v "\"") }
			{ print }
		' "$tmpm" >"${tmpm}.next" && mv "${tmpm}.next" "$tmpm"
		[ "$(ep_dep_version "$tmpm" "$name")" = "$ver" ] || {
			echo "ep_rewrite: could not move ${name} ${cur} -> ${ver}" >&2
			rm -f "$tmpm" "$tmpr"
			return 1
		}
	done
	cat "$tmpm" >"$mod"
	cat "$tmpr" >"$rc"
	rm -f "$tmpm" "$tmpr"
}

# ep_rc_flags — the flag lines of an Envoy root .bazelrc on stdin that a
# downstream build mirrors: unconditional `common`/`build`/`test` lines and the
# `:linux`, `:clang*` and `:libc++` configs (the scopes proxy/.bazelrc copies).
# Whitespace is normalized and trailing comments dropped.
ep_rc_flags() {
	sed -E 's/[[:space:]]+#.*$//; s/[[:space:]]+/ /g; s/ $//' |
		grep -E '^(common|build|test)(:(linux|clang|clang-common|libc\+\+))? --' | LC_ALL=C sort -u || true
}

# ep_rc_key <flag line> — what makes two lines "the same setting": the config
# plus the flag name for `--@label=value` / `--name=value` settings whose name is
# not a repeatable list flag; repeatable flags (copt, linkopt, ...) key on the
# whole line.
ep_rc_key() {
	local cfg="${1%% *}" flag="${1#* }"
	case "$flag" in
	--copt=* | --cxxopt=* | --conlyopt=* | --host_cxxopt=* | --linkopt=* | --per_file_copt=* | --action_env=* | --repo_env=* | --define=* | --define\ * | --features=* | --extra_toolchains=*)
		printf '%s %s\n' "$cfg" "$flag"
		;;
	*) printf '%s %s\n' "$cfg" "${flag%%=*}" ;;
	esac
}

# ep_rc_diff <old upstream .bazelrc> <new upstream .bazelrc> <proxy .bazelrc> —
# what changed upstream between the two pins, among the lines a downstream build
# mirrors. `~` changed (same setting, new value), `-` removed, `+` added; each
# tagged [mirrored] when proxy/.bazelrc sets that flag, so the lines that need a
# decision are the [mirrored] ones and the `+` ones.
ep_rc_diff() {
	local old new proxy_flags line key tag changed=0
	old="$(ep_rc_flags <"$1")"
	new="$(ep_rc_flags <"$2")"
	proxy_flags="$(ep_rc_flags <"$3")"
	local removed added match a
	removed="$(LC_ALL=C comm -23 <(printf '%s\n' "$old") <(printf '%s\n' "$new") | sed '/^$/d')"
	added="$(LC_ALL=C comm -13 <(printf '%s\n' "$old") <(printf '%s\n' "$new") | sed '/^$/d')"
	# A here-string, not `printf | grep -q`: under pipefail an early match
	# SIGPIPEs the producer and the pipeline reports 141, i.e. "no match".
	mirrored() {
		local k="${1#* }"
		k="${k%%=*}"
		if grep -qF -- "$k" <<<"$proxy_flags"; then echo " [mirrored]"; fi
	}
	while IFS= read -r line; do
		[ -n "$line" ] || continue
		key="$(ep_rc_key "$line")"
		tag="$(mirrored "$line")"
		match=""
		while IFS= read -r a; do
			[ -n "$a" ] || continue
			if [ "$(ep_rc_key "$a")" = "$key" ] && [ "$a" != "$line" ]; then match="$a"; fi
		done <<<"$added"
		if [ -n "$match" ]; then
			printf '  ~ %s\n      -> %s%s\n' "$line" "$match" "$tag"
			added="$(printf '%s\n' "$added" | grep -vxF -- "$match" || true)"
		else
			printf '  - %s%s\n' "$line" "$tag"
		fi
		changed=1
	done <<<"$removed"
	while IFS= read -r line; do
		[ -n "$line" ] || continue
		printf '  + %s%s\n' "$line" "$(mirrored "$line")"
		changed=1
	done <<<"$added"
	[ "$changed" = 1 ] || echo "  (no change in the mirrored scopes)"
}

# ep_patch_report <tree> <patch>... — applies the carried patches IN ORDER to a
# scratch tree (they stack: 47740 sits on 47743, 1054b on 1054, ...) and prints
# per patch: `applies` (still needed, applied for the next one), `UPSTREAM`
# (reverse-applies cleanly: already in this Envoy, drop it) or `CONFLICT` (needs
# a refresh; not applied, so a later patch stacked on it may conflict too).
# Returns 0; the caller reads the verdicts.
ep_patch_report() {
	local tree="$1" p name
	shift
	for p in "$@"; do
		name="$(basename "$p")"
		# git -C changes directory: a relative patch path would not be found,
		# and "not found" must never read as CONFLICT.
		case "$p" in /*) ;; *) p="$PWD/$p" ;; esac
		[ -f "$p" ] || {
			printf '  MISSING   %s\n' "$name"
			continue
		}
		if git -C "$tree" apply --check -p1 "$p" 2>/dev/null; then
			git -C "$tree" apply -p1 "$p"
			printf '  applies   %s\n' "$name"
		elif git -C "$tree" apply --check -R -p1 "$p" 2>/dev/null; then
			printf '  UPSTREAM  %s (reverse-applies: already in this Envoy, drop it)\n' "$name"
		else
			printf '  CONFLICT  %s (neither applies nor reverse-applies: refresh it)\n' "$name"
		fi
	done
}

# ep_patch_paths <patch>... — the repository paths the patches touch.
ep_patch_paths() {
	sed -n -E 's#^(\+\+\+ b/|--- a/)([^[:space:]]+).*$#\2#p' "$@" | sort -u
}

# ep_patch_list <MODULE.bazel> — the patch files of the envoy
# single_version_override, in application order, as paths under proxy/.
ep_patch_list() {
	awk '
		/^single_version_override\(/ { inblk = 1; mod = ""; n = 0; next }
		inblk && /^[[:space:]]*module_name = "envoy",/ { mod = "envoy" }
		inblk && /"\/\/bazel\/patches:[^"]+\.patch"/ { s = $0; sub(/^[^"]*"\/\/bazel\/patches:/, "", s); sub(/".*$/, "", s); p[++n] = s }
		inblk && /^\)/ { if (mod == "envoy") for (i = 1; i <= n; i++) print "bazel/patches/" p[i]; inblk = 0 }
	' "$1"
}
