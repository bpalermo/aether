#!/usr/bin/env bash
# Audit the Go dependency surface of the module: stale go.sum entries, direct
# requires nothing actually uses, and toolchain version floors the build depends
# on but nothing else enforces.
#
# `go mod tidy` is the tool that would normally keep this clean, and it is
# unusable in this repository (see docs/runbook.md, "Go dependency hygiene"):
# the generated proto packages `aethermesh.dev/api/aether/*/v1` exist only as
# Bazel outputs, so the Go tool fails with "no matching versions for query
# latest" on every import of them, and `go mod tidy -e` happily strips modules
# that only generated code or a BUILD file needs. So the audit is driven from
# the module graph (`go list -m all`, which works from go.mod alone) and from
# the Bazel graph (textual `@repo//` references), never from `go mod tidy`.
#
# Four findings are reported:
#
#   1. go.sum modules that are not in the module build list. Dead weight left
#      behind by a removed dependency; `bazel mod tidy` does not prune them.
#      FATAL.
#   2. direct requires that are neither imported by any .go file nor referenced
#      from any BUILD/.bzl file nor named by a `tool` directive. Genuinely
#      unused: drop them with `go get <module>@none`. FATAL.
#   3. direct requires with no Go import that ARE referenced from Bazel and are
#      not annotated `// bazel-only:`. Informational — either annotate them, or
#      (for a pin that exists only to force a CVE-clean version through MVS)
#      move them to the indirect block. Never delete a pin to silence this.
#   4. toolchain version floors: modules the *build* needs at some minimum
#      version because of the Go SDK we pin, checked against the version MVS
#      actually selects. FATAL. See TOOLCHAIN_FLOORS below for why this is not
#      an arbitrary pin (#886).
#
# Findings 2 and 3 audit *direct* requires only, and finding 4 exists because of
# that gap: a version floor lives in the indirect block by design (nothing
# imports it, it is there to raise MVS), which is exactly the category findings
# 2 and 3 skip. Finding 4 audits the resolved version, not the require line, so
# it is unaffected by which block the requirement sits in.
#
# Usage: scripts/go-deps-audit.sh   (or `make deps-audit`)
set -euo pipefail
cd "$(dirname "$0")/.."

BAZEL="${BAZEL:-bazel}"
# The go tool runs as a *subprocess* of `bazel run`, so --repo_env does not
# reach it and a `GOPROXY=direct` in ~/.config/go/env turns every module fetch
# into a git clone. Both spellings, belt and braces.
: "${GOPROXY:=https://proxy.golang.org,direct}"
export GOPROXY

# --- toolchain version floors (finding 4) ------------------------------------
#
# WHY THIS EXISTS — do not delete these as arbitrary pins. nogo runs as a
# build-time validation action on every Go target in the repo (MODULE.bazel:
# `go_sdk.nogo(nogo = "//:nogo")`), and //:nogo pulls in rules_go's TOOLS_NOGO,
# the analyzer set built from `@org_golang_x_tools//...`. An analyzer can only
# read export data in a format it understands, so the x/tools those analyzers
# are compiled from has to be new enough for the SDK whose code they analyze.
#
# On a Go 1.27 SDK that means golang.org/x/tools >= v0.48.0
# (bazel-contrib/rules_go#4701, which postdates rules_go 0.63.0 and only bumped
# rules_go's *own* go.mod, not ours). Below the floor every Go compile in the
# tree fails with:
#
#   nogo: error running analyzers: 1 analyzers skipped due to type-checking
#   error: could not import encoding/json (internal error in importing
#   "encoding/json" (cannot decode "encoding/json", export data version 4 is
#   greater than maximum supported version 2); please report an issue)
#
# — a message that names neither nogo's analyzer dependencies nor x/tools nor
# MVS, so the loud failure points away from its cause. A peer repo on the
# identical version set read it as "nogo cannot work on Go 1.27" and removed
# nogo. This check turns that into a dependency error that names the cause
# (#886). The MODULE.bazel comment beside `go_sdk.nogo` states the same
# invariant in prose; this is the half that enforces it.
#
# Nothing else can: the floor is an *indirect* require (nothing imports x/tools;
# it is there to raise MVS), and findings 2 and 3 deliberately audit only direct
# requires. It would regress silently — a `go get` that reshuffles the graph, a
# bump that drops whatever else pulled x/tools forward, a `-droprequire`.
#
# UPDATING: the rule is "the x/tools TOOLS_NOGO compiles against must understand
# the SDK's export data format", so each row is keyed on the SDK's Go release
# rather than hardcoding one. When `go_sdk.download` moves past the newest row,
# add a row for the new release with the first x/tools that supports it. Keep
# the older rows: they still document the history and cost nothing. If a floor
# ever trips, RAISE the dependency — never lower the floor, and never delete the
# row; the whole tree stops compiling.
#
# Columns, whitespace-separated, one row per line:
#   <minimum Go release> <module path> <minimum version> <reason, rest of line>
# A row applies when the SDK's <major>.<minor> is >= its first column, and its
# reason is quoted verbatim in the failure so the error explains itself to
# someone who has never read this comment.
TOOLCHAIN_FLOORS="$(
	cat <<-'EOF'
		1.27 golang.org/x/tools v0.48.0 nogo runs on every Go target here and rules_go's TOOLS_NOGO analyzers are compiled from this module, so it has to understand the SDK's export data format (bazel-contrib/rules_go#4701). Below the floor EVERY Go compile fails with "could not import encoding/json ... export data version 4 is greater than maximum supported version 2", which names neither nogo nor this module.
	EOF
)"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# --- inputs ------------------------------------------------------------------

echo "==> module build list (go list -m all)"
"$BAZEL" run @rules_go//go --repo_env="GOPROXY=$GOPROXY" -- list -m all >"$tmp/buildlist.txt"
awk 'NF {print $1}' "$tmp/buildlist.txt" | sort -u >"$tmp/build_modules.txt"

awk 'NF {print $1}' go.sum | sort -u >"$tmp/gosum_modules.txt"

# Every require line, as "<module>\t<direct|indirect>\t<annotated|plain>".
awk '
	/^require[ \t]*\(/ { inblk = 1; next }
	inblk && /^\)/     { inblk = 0; next }
	{
		line = $0
		if (inblk) {
			sub(/^[ \t]+/, "", line)
		} else if (line ~ /^require[ \t]+[^(]/) {
			sub(/^require[ \t]+/, "", line)
		} else {
			next
		}
		if (line == "" || line ~ /^\/\//) next
		split(line, f, /[ \t]+/)
		kind = (line ~ /\/\/[ \t]*indirect/) ? "indirect" : "direct"
		note = (line ~ /\/\/[ \t]*bazel-only:/) ? "annotated" : "plain"
		print f[1] "\t" kind "\t" note
	}
' go.mod >"$tmp/requires.tsv"

# Module paths named by a `tool` directive (Go keeps these direct by design).
sed -nE 's|^tool[ \t]+(.*)$|\1|p' go.mod | sort -u >"$tmp/tools.txt"

# The Go SDK the build actually compiles with, from MODULE.bazel. Not go.mod's
# `go` line: that is a language-version floor for the module, whereas the export
# data format that finding 4 is about comes from the toolchain rules_go
# downloads, which is `go_sdk.download`. Read out of that call however
# buildifier happens to have wrapped it.
sdk_version="$(
	awk '
		/^[ \t]*go_sdk\.download\(/ { inblk = 1 }
		inblk {
			buf = buf $0
			if (/\)/) {
				if (match(buf, /version[ \t]*=[ \t]*"[^"]+"/)) {
					v = substr(buf, RSTART, RLENGTH)
					sub(/^version[ \t]*=[ \t]*"/, "", v)
					sub(/"$/, "", v)
					print v
				}
				exit
			}
		}
	' MODULE.bazel
)"

# Every import-path-shaped string literal in the module's Go sources. A module
# counts as imported if a literal equals it or descends from it.
grep -rhoE '"[A-Za-z0-9_.~-]+\.[A-Za-z]{2,}/[^"]*"' \
	--include='*.go' --exclude-dir='bazel-*' . 2>/dev/null |
	tr -d '"' | sort -u >"$tmp/imports.txt" || true

# --- helpers -----------------------------------------------------------------

# gazelle's repo naming: reverse the domain, then join the remaining path
# segments, lowercased, with every non-alphanumeric turned into an underscore.
# github.com/go-jose/go-jose/v4 -> com_github_go_jose_go_jose_v4
repo_name() {
	local path="$1" domain rest out
	domain="${path%%/*}"
	rest="${path#"$domain"}"
	out="$(awk -F. '{ for (i = NF; i > 0; i--) printf "%s%s", $i, (i > 1 ? "_" : "") }' <<<"$domain")"
	out="${out}${rest//\//_}"
	out="${out//-/_}"
	out="${out//./_}"
	tr '[:upper:]' '[:lower:]' <<<"$out"
}

is_imported() {
	local mod="$1"
	grep -qxF "$mod" "$tmp/imports.txt" || grep -q "^${mod}/" "$tmp/imports.txt"
}

is_bazel_referenced() {
	local repo
	repo="$(repo_name "$1")"
	grep -rlqF "@${repo}//" \
		--include='BUILD' --include='BUILD.bazel' --include='*.bzl' \
		--exclude-dir='bazel-*' . 2>/dev/null
}

is_tool() {
	local mod="$1"
	grep -qxF "$mod" "$tmp/tools.txt" || grep -q "^${mod}/" "$tmp/tools.txt"
}

# The version MVS actually selects for a module, read from the build list rather
# than from the go.mod require line: MVS routinely selects higher than what we
# ask for, so the require line is not what the build uses.
#
# Exact match on the module path, never a prefix: `golang.org/x/tools/go/expect`
# is a *separate* module sitting immediately below `golang.org/x/tools` in the
# list at a wildly different version (v0.1.1-deprecated), so a prefix match here
# would silently read the wrong number. Empty output means "not resolvable",
# which the caller treats as a failure rather than a pass.
resolved_version() {
	awk -v m="$1" '
		$1 != m { next }
		{
			v = ($3 == "=>") ? $NF : $2
			if (v !~ /^v[0-9]/) v = ""
			print v
			exit
		}
	' "$tmp/buildlist.txt"
}

# True when version $1 is at least version $2. Compares the numeric
# v<major>.<minor>[.<patch>...] prefix component by component, so it is right
# for both Go module versions (v0.49.0) and bare Go releases (1.27.1). A
# pre-release or build suffix is ignored; the floors here are all plain tagged
# releases, so that distinction never arises in practice.
version_ge() {
	awk -v have="$1" -v want="$2" 'BEGIN {
		sub(/^v/, "", have); sub(/[-+].*$/, "", have)
		sub(/^v/, "", want); sub(/[-+].*$/, "", want)
		nh = split(have, h, "."); nw = split(want, w, ".")
		n = (nh > nw) ? nh : nw
		for (i = 1; i <= n; i++) {
			a = (i <= nh) ? h[i] + 0 : 0
			b = (i <= nw) ? w[i] + 0 : 0
			if (a != b) exit (a > b) ? 0 : 1
		}
		exit 0
	}'
}

# --- finding 1: stale go.sum modules -----------------------------------------

comm -23 "$tmp/gosum_modules.txt" "$tmp/build_modules.txt" >"$tmp/stale.txt"
stale_lines=0
echo
echo "==> stale go.sum modules (not in the build list)"
if [ -s "$tmp/stale.txt" ]; then
	while read -r mod; do
		n="$(grep -cE "^${mod//./\\.} " go.sum || true)"
		stale_lines=$((stale_lines + n))
		echo "  ${mod} (${n} lines)"
	done <"$tmp/stale.txt"
	echo "::error file=go.sum::$(wc -l <"$tmp/stale.txt" | tr -d ' ') stale module(s) / ${stale_lines} lines in go.sum. They are not in the module build list; delete their lines (see docs/runbook.md, 'Go dependency hygiene')."
else
	echo "  none"
fi

# --- findings 2 and 3: direct requires nothing uses --------------------------

unused=0
unannotated=0
listed=0
echo
echo "==> direct requires with no Go import"
while IFS=$'\t' read -r mod kind note; do
	[ "$kind" = "direct" ] || continue
	if is_imported "$mod"; then
		continue
	fi
	listed=$((listed + 1))
	if is_tool "$mod"; then
		echo "  ${mod} — tool directive (kept direct by the go tool)"
		continue
	fi
	if is_bazel_referenced "$mod"; then
		if [ "$note" = "annotated" ]; then
			echo "  ${mod} — bazel-only (annotated), @$(repo_name "$mod")//… is referenced from BUILD/.bzl"
		else
			unannotated=$((unannotated + 1))
			echo "::warning file=go.mod::${mod} is a direct require with no Go import but @$(repo_name "$mod")//… IS referenced from BUILD/.bzl. Annotate it '// bazel-only: referenced from BUILD/.bzl, no Go import', or move it to the indirect block if it is only a version pin."
		fi
		continue
	fi
	unused=$((unused + 1))
	echo "::error file=go.mod::${mod} is a direct require that nothing uses: no Go import, no BUILD/.bzl reference, no tool directive. If it only exists to force a CVE-clean version through MVS, move it to the indirect block (reclassify, do not drop); otherwise remove it with 'bazel run @rules_go//go -- get ${mod}@none'."
done <"$tmp/requires.tsv"
[ "$listed" -ne 0 ] || echo "  none (every direct require is imported by a .go file)"

# Indirect requires are not audited for imports: an indirect require with no
# import of its own is the normal case (it is either a transitive dependency or
# a deliberate version floor), so it is informational at most. The floors that
# the *build* depends on are checked by name below instead.

# --- finding 4: toolchain version floors -------------------------------------

below_floor=0
echo
echo "==> toolchain version floors (Go SDK ${sdk_version:-UNKNOWN})"
if [ -z "$sdk_version" ]; then
	below_floor=$((below_floor + 1))
	echo "::error file=MODULE.bazel::could not read the Go SDK version from go_sdk.download(...) in MODULE.bazel, so the toolchain version floors in scripts/go-deps-audit.sh (TOOLCHAIN_FLOORS) could not be checked at all. Fix the parse rather than dropping the check: one of those floors is what keeps nogo working (#886)."
fi
while read -r min_sdk mod min_ver reason; do
	[ -n "${min_sdk:-}" ] || continue # blank line
	# A typo'd row must not quietly become a floor that is never checked.
	if [ -z "${mod:-}" ] || [ -z "${min_ver:-}" ]; then
		below_floor=$((below_floor + 1))
		echo "::error file=scripts/go-deps-audit.sh::malformed TOOLCHAIN_FLOORS row '${min_sdk} ${mod:-} ${min_ver:-}': expected '<minimum Go release> <module path> <minimum version> <reason>'."
		continue
	fi
	[ -n "$sdk_version" ] || break
	if ! version_ge "$sdk_version" "$min_sdk"; then
		echo "  ${mod} >= ${min_ver}: not applicable (applies from Go ${min_sdk}; SDK is ${sdk_version})"
		continue
	fi
	have="$(resolved_version "$mod")"
	if [ -n "$have" ] && version_ge "$have" "$min_ver"; then
		echo "  ${mod} ${have} >= ${min_ver} (floor for the Go ${sdk_version} SDK)"
		continue
	fi
	# Same remedy either way: raise the module. Reclassify, never drop — the
	# indirect block is the right home for a floor that nothing imports.
	fix="Raise it with 'bazel run @rules_go//go -- get ${mod}@${min_ver}' (the indirect block is the right home for a floor nothing imports — reclassify, do not drop), then 'make tidy'. Never lower the floor in TOOLCHAIN_FLOORS (scripts/go-deps-audit.sh) to make this pass; see aether #886 and the version note beside go_sdk.nogo in MODULE.bazel."
	below_floor=$((below_floor + 1))
	if [ -z "$have" ]; then
		echo "::error file=go.mod::${mod} must resolve to >= ${min_ver} on the Go ${sdk_version} SDK, but it is not in the module build list with a usable version. ${reason} ${fix}"
	else
		echo "::error file=go.mod::${mod} resolves to ${have}, below the ${min_ver} floor that the Go ${sdk_version} SDK requires. ${reason} ${fix}"
	fi
done <<<"$TOOLCHAIN_FLOORS"

echo
if [ -s "$tmp/stale.txt" ] || [ "$unused" -ne 0 ] || [ "$below_floor" -ne 0 ]; then
	echo "go-deps-audit: FAIL ($(wc -l <"$tmp/stale.txt" | tr -d ' ') stale go.sum module(s), ${unused} unused direct require(s), ${below_floor} toolchain floor violation(s), ${unannotated} unannotated bazel-only require(s))"
	exit 1
fi
echo "go-deps-audit: OK (go.sum matches the build list; every direct require is used; every toolchain floor met; ${unannotated} unannotated bazel-only require(s))"
