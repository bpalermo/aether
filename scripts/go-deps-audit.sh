#!/usr/bin/env bash
# Audit the Go dependency surface of the module: stale go.sum entries and
# direct requires nothing actually uses.
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
# Three findings are reported:
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
echo
echo "==> direct requires with no Go import"
while IFS=$'\t' read -r mod kind note; do
	[ "$kind" = "direct" ] || continue
	if is_imported "$mod"; then
		continue
	fi
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
[ "$unused" -ne 0 ] || [ "$unannotated" -ne 0 ] || echo "  (every direct require is imported, a tool, or annotated bazel-only)"

# Indirect requires are not audited for imports: an indirect require with no
# import of its own is the normal case (it is either a transitive dependency or
# a deliberate version floor), so it is informational at most.

echo
if [ -s "$tmp/stale.txt" ] || [ "$unused" -ne 0 ]; then
	echo "go-deps-audit: FAIL ($(wc -l <"$tmp/stale.txt" | tr -d ' ') stale go.sum module(s), ${unused} unused direct require(s), ${unannotated} unannotated bazel-only require(s))"
	exit 1
fi
echo "go-deps-audit: OK (go.sum matches the build list; every direct require is used; ${unannotated} unannotated bazel-only require(s))"
