#!/usr/bin/env bash
# Copy the Go sources Bazel generates into the source tree, at the directories
# their import paths name, so that a plain `go build ./...` resolves every
# package in the module.
#
# Why: nothing generated is checked in. The six `go_proto_library` targets under
# //api/aether/... exist only as Bazel outputs, which is all Bazel needs, but a
# tool that reads the module the way the go command does sees six import paths
# with no Go files behind them. CodeQL is such a tool: under default setup its
# Go autobuilder reported "6 packages could not be found" and analysed the 85
# files that import them (the gRPC/API surface, where remote input enters) with
# those types unresolved. .github/workflows/codeql.yaml runs this script before
# the scan; docs/runbook.md, "CodeQL code scanning".
#
#   scripts/materialize-generated-go.sh                  build and copy
#   scripts/materialize-generated-go.sh --clean          remove what a run wrote
#   scripts/materialize-generated-go.sh --check-untracked  fail if git tracks any
#                                                        materialized file (no Bazel)
#
# What it does, in order:
#   1. `bazel query` for every go_proto_library in the root workspace. proxy/ is
#      its own workspace behind .bazelignore and is never touched.
#   2. `bazel query` for any OTHER generated file that a go_library, go_binary
#      or go_test takes as a source or an embedded file. There is none today; if
#      one appears this script fails and names it, because a plain build would
#      be missing it and nothing here knows where it belongs.
#      (x_defs are not such an input: they set string variables at link time,
#      and every variable they set has a default in the source.)
#   3. builds the targets' `go_generated_srcs` output group (rules_go's name for
#      the .pb.go / _grpc.pb.go files protoc wrote) and asks cquery for the
#      paths, so the list is never a guess at rules_go's output layout.
#   4. copies each file to <import path minus the module path>/<file>. A file is
#      written only if it differs, so a second run changes nothing.
#
# It refuses to write a path git tracks (a hand-written file is never
# overwritten) or a path .gitignore does not cover (a materialized file must
# not be able to reach a commit). The files it wrote are listed in
# .materialized-generated-go at the repository root; --clean removes exactly
# those, and a run removes any listed file it no longer generates.
#
# It never runs `go mod tidy` and never touches go.mod or go.sum.
set -euo pipefail

BAZEL="${BAZEL:-bazel}"
MANIFEST_NAME=".materialized-generated-go"

die() {
	echo "materialize-generated-go: $*" >&2
	exit 1
}

mode=materialize
case "${1:-}" in
"") ;;
--clean) mode=clean ;;
--check-untracked) mode=check ;;
-h | --help)
	sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed -e '$d' -e 's/^# \{0,1\}//'
	exit 0
	;;
*) die "unknown argument '$1' (want --clean, --check-untracked or nothing)" ;;
esac
[ "$#" -le 1 ] || die "at most one argument"

root="$(git rev-parse --show-toplevel 2>/dev/null)" || die "not inside a git work tree"
cd "$root"
manifest="$root/$MANIFEST_NAME"

tracked() { git ls-files --error-unmatch -- "$1" >/dev/null 2>&1; }

# Every file a previous run wrote, one repository-relative path per line.
manifest_entries() {
	[ -f "$manifest" ] || return 0
	grep -v -e '^#' -e '^$' "$manifest" || true
}

# --check-untracked: nothing generated may be committed. A tracked *.pb.go
# would be compiled by Bazel twice over (once from the .proto, once never, since
# no go_library lists it) and would silently go stale; a tracked manifest would
# make --clean delete files on someone else's checkout.
if [ "$mode" = check ]; then
	bad="$(git ls-files -- ':(glob)**/*.pb.go' "$MANIFEST_NAME" ':(exclude)proxy')"
	if [ -n "$bad" ]; then
		awk '{ print "  " $0 }' <<<"$bad" >&2
		die "git tracks the generated file(s) above; generated Go is a Bazel output and must stay out of the repository (git rm --cached them)"
	fi
	echo "materialize-generated-go: no generated Go file is tracked"
	exit 0
fi

if [ "$mode" = clean ]; then
	removed=0
	while IFS= read -r rel; do
		[ -n "$rel" ] || continue
		if tracked "$rel"; then
			echo "materialize-generated-go: keeping $rel (git tracks it)" >&2
			continue
		fi
		if [ -f "$rel" ]; then
			rm -f -- "$rel"
			removed=$((removed + 1))
		fi
	done < <(manifest_entries)
	rm -f -- "$manifest"
	echo "materialize-generated-go: removed $removed file(s)"
	exit 0
fi

module="$(sed -nE 's/^module[ \t]+([^ \t]+).*$/\1/p' go.mod | head -n1)"
[ -n "$module" ] || die "no module line in go.mod"

# --- 1. the generated-Go targets.
mapfile -t targets < <("$BAZEL" query --noshow_progress 'kind("go_proto_library rule", //...)' | LC_ALL=C sort)
[ "${#targets[@]}" -gt 0 ] || die "bazel query found no go_proto_library; refusing to conclude there is nothing to generate"
for t in "${targets[@]}"; do
	case "$t" in
	//proxy/* | //proxy:*) die "$t is in the proxy workspace, which this script must not build" ;;
	esac
done

# --- 2. any other generated file a Go rule compiles or embeds.
go_rules='kind("go_(library|binary|test) rule", //...)'
other="$("$BAZEL" query --noshow_progress \
	"kind(\"generated file\", labels(srcs, $go_rules) union labels(embedsrcs, $go_rules))")"
if [ -n "$other" ]; then
	awk '{ print "  " $0 }' <<<"$other" >&2
	die "the generated file(s) above are sources of a Go rule but are not go_proto_library outputs; a plain go build needs them and this script does not know how to place them. Teach it, then update docs/runbook.md (\"CodeQL code scanning\")"
fi

# --- 3. build them, and read the output paths back.
"$BAZEL" build --noshow_progress --output_groups=go_generated_srcs -- "${targets[@]}"
set_expr="set(${targets[*]})"
mapfile -t outputs < <("$BAZEL" cquery --noshow_progress "$set_expr" --output=starlark \
	--starlark:expr='"\n".join([f.path for f in target.output_groups.go_generated_srcs.to_list()])' | LC_ALL=C sort -u)
[ "${#outputs[@]}" -gt 0 ] || die "the go_generated_srcs output group is empty for every target (did rules_go rename it?)"
execroot="$("$BAZEL" info --noshow_progress execution_root)"

# --- 4. check every file, then copy. Two passes, so a refusal on the last file
# leaves the tree exactly as it was instead of half-written and unlisted.
placeholders=0
srcs=()
rels=()
for out in "${outputs[@]}"; do
	[ -n "$out" ] || continue
	# rules_go writes <bin>/<pkg>/<target>_/<import path>/<file>.
	case "$out" in
	*"_/$module/"*.go) rel="${out#*"_/$module/"}" ;;
	*) die "unexpected generated output '$out': not a .go file under <target>_/$module/" ;;
	esac
	src="$execroot/$out"
	[ -f "$src" ] || die "bazel reported $out but $src does not exist"
	# A plugin that had nothing to emit for a .proto (protoc-gen-go-grpc on a
	# file with no service) leaves no file, and rules_go fills the declared
	# output with a two-line `package ignore` stub behind an `ignore` build
	# constraint. It is not part of the package; copying it would only plant a
	# file with a foreign package clause in the directory.
	if [ "$(head -n 1 "$src")" = "// +build ignore" ] && grep -qx 'package ignore' "$src"; then
		placeholders=$((placeholders + 1))
		continue
	fi
	head -n 5 "$src" | grep '^// Code generated .* DO NOT EDIT\.$' >/dev/null ||
		die "$out has no 'Code generated ... DO NOT EDIT.' header; refusing to copy something that does not look generated"
	if tracked "$rel"; then
		die "refusing to overwrite $rel: git tracks it"
	fi
	git check-ignore -q -- "$rel" ||
		die "$rel is not git-ignored: extend the generated-Go pattern in .gitignore to its directory first, so it cannot be committed"
	srcs+=("$src")
	rels+=("$rel")
done
[ "${#rels[@]}" -gt 0 ] || die "every generated output was an empty stub; nothing to materialize"

written=0
unchanged=0
new_manifest="$(mktemp)"
trap 'rm -f "$new_manifest"' EXIT
for i in "${!rels[@]}"; do
	src="${srcs[$i]}"
	rel="${rels[$i]}"
	if [ -f "$rel" ] && cmp -s -- "$src" "$rel"; then
		unchanged=$((unchanged + 1))
	else
		mkdir -p -- "$(dirname -- "$rel")"
		# Bazel outputs are read-only; install gives the copy a writable mode
		# so the next run can replace it.
		install -m 0644 -- "$src" "$rel"
		written=$((written + 1))
	fi
	echo "$rel" >>"$new_manifest"
done

# A file an earlier run wrote that is no longer generated (a renamed .proto)
# would otherwise stay behind and keep compiling.
stale=0
while IFS= read -r rel; do
	[ -n "$rel" ] || continue
	grep -qxF -- "$rel" "$new_manifest" && continue
	tracked "$rel" && continue
	if [ -f "$rel" ]; then
		rm -f -- "$rel"
		stale=$((stale + 1))
	fi
done < <(manifest_entries)

{
	echo "# Written by scripts/materialize-generated-go.sh: the generated Go files it copied"
	echo "# into the source tree. Remove them with: scripts/materialize-generated-go.sh --clean"
	LC_ALL=C sort "$new_manifest"
} >"$manifest"

total=$((written + unchanged))
echo "materialize-generated-go: ${#targets[@]} target(s), $total file(s): $written written, $unchanged unchanged, $stale stale removed, $placeholders empty rules_go stub(s) skipped"
