#!/usr/bin/env bash
# Build the whole module with the plain go command, the way a tool that knows
# nothing about Bazel does. This is the property CodeQL's Go extraction needs
# (.github/workflows/codeql.yaml runs this script twice: once before the scan,
# as the proof, and once under CodeQL's tracer); docs/runbook.md, "CodeQL code
# scanning".
#
#   scripts/go-build-plain.sh           go build <every package but the excluded>
#   scripts/go-build-plain.sh --vet     the same, then go vet over them
#                                       (vet also type-checks every _test.go)
#
# Run scripts/materialize-generated-go.sh first: without the generated proto
# packages in the tree the build cannot resolve aethermesh.dev/api/...
#
# The environment is fixed so the result does not depend on the caller's:
#   GOFLAGS=-mod=readonly  the go command may not edit go.mod or go.sum, and
#                          fails instead of fetching a module go.sum lacks.
#                          This repository never runs `go mod tidy`.
#   GOTOOLCHAIN=local      no toolchain download: the go on PATH must be the
#                          pinned one, and this script checks that it is.
#   GOWORK=off             a stray go.work above the checkout changes nothing.
#   GOOS=linux             every component is Linux-only at run time (netns,
#                          netlink, mount); a darwin build would drop the files
#                          behind `//go:build linux`.
#   CGO_ENABLED=0          what Bazel links: the images are static distroless.
#                          No package in the module uses cgo.
#   GOARCH                 left to the host: no file in the module is
#                          architecture-constrained.
#
# Packages the plain go command cannot build. The list is explicit, each entry
# carries its reason, and an entry that starts building fails this script
# (remove it then):
#
#   test/envoy_validate, test/envoy_validate/generate
#       builders.go imports aethermesh.dev/agent/internal/xds/{config,proxy}
#       from outside agent/. Bazel allows that through `visibility`; the go
#       command enforces Go's internal-package rule and refuses to load the
#       package ("use of internal package ... not allowed"). Two non-test
#       files, both test tooling (the `envoy --mode validate` config builders
#       and their generator); neither is linked into a shipped binary. They
#       are still handed to CodeQL: see the loop after the main build.
#   test/mtlspool (--vet only)
#       has only _test.go files, which break the same rule. `go build` has
#       nothing to compile there; `go vet` type-checks tests and cannot load it.
#
# Everything else in the module is built, test/ and e2e/ included.
# Afterwards go.mod and go.sum must be byte-identical to what git has.
set -euo pipefail

die() {
	echo "go-build-plain: $*" >&2
	exit 1
}

vet=0
case "${1:-}" in
"") ;;
--vet) vet=1 ;;
*) die "unknown argument '$1' (want --vet or nothing)" ;;
esac

root="$(git rev-parse --show-toplevel 2>/dev/null)" || die "not inside a git work tree"
cd "$root"

export GOFLAGS=-mod=readonly GOTOOLCHAIN=local GOWORK=off GOOS=linux CGO_ENABLED=0

# The pin is go.mod's go directive, which //e2e:go_pin_test holds equal to
# MODULE.bazel's go_sdk.download version.
want="$(sed -nE 's/^go[ \t]+([^ \t]+).*$/\1/p' go.mod | head -n1)"
have="$(go env GOVERSION)"
[ "$have" = "go$want" ] ||
	die "go on PATH is $have, the repository pins go$want (go.mod); install that version, or in CI use actions/setup-go with go-version-file: go.mod"

[ -f .materialized-generated-go ] ||
	die "the generated proto packages are not in the tree; run scripts/materialize-generated-go.sh first"

sums_before="$(git hash-object go.mod go.sum)"

module="$(sed -nE 's/^module[ \t]+([^ \t]+).*$/\1/p' go.mod | head -n1)"
build_excluded=("$module/test/envoy_validate" "$module/test/envoy_validate/generate")
vet_excluded=("${build_excluded[@]}" "$module/test/mtlspool")

# packages_except <go list template> <excluded>...: every package of the module
# the template prints, except the excluded ones. `go list -e` so a package that
# cannot be loaded is still listed: only the explicit exclusions are dropped,
# and anything else that is broken fails the build below instead of vanishing
# from it.
packages_except() {
	local all pkg skip tmpl="$1"
	shift
	all="$(go list -e -f "$tmpl" ./... | grep -v '^$')"
	for skip in "$@"; do
		[ -d "${skip#"$module/"}" ] || die "excluded package $skip is not in the module; drop it from this script"
	done
	while IFS= read -r pkg; do
		for skip in "$@"; do
			[ "$pkg" = "$skip" ] && continue 2
		done
		echo "$pkg"
	done <<<"$all"
}

echo "go-build-plain: $have, GOOS=$GOOS GOARCH=$(go env GOARCH) CGO_ENABLED=$CGO_ENABLED GOFLAGS=$GOFLAGS"
# go build: the packages that have a non-test file. (`./...` skips a
# test-only directory silently; naming one is an error.)
mapfile -t pkgs < <(packages_except '{{if .GoFiles}}{{.ImportPath}}{{end}}' "${build_excluded[@]}")
[ "${#pkgs[@]}" -gt 0 ] || die "go list found no package"
go build "${pkgs[@]}"
echo "go-build-plain: go build ok: ${#pkgs[@]} packages"

# The excluded packages, in an invocation of their own that MUST fail, and fail
# for the documented reason. Two purposes:
#   - an exclusion is only legitimate while the go command still refuses the
#     package; the day it builds, this script fails until the entry is removed.
#   - CodeQL's tracer runs its extractor on every `go build` it sees, before
#     the build and whatever the build's outcome, so naming the packages here
#     is what puts their files in the database (with one extraction error each
#     for the internal import, which scripts/codeql-go-diagnostics.sh allows by
#     name). Without this they would simply not be scanned.
for pkg in "${build_excluded[@]}"; do
	if out="$(go build "$pkg" 2>&1)"; then
		die "$pkg is excluded but now builds; remove it from build_excluded so it is part of the real build"
	fi
	case "$out" in
	*"use of internal package"*) ;;
	*) die "$pkg is excluded for its internal-package import but fails for another reason: $out" ;;
	esac
done
echo "go-build-plain: ${#build_excluded[@]} excluded package(s) still refused by the go command, as documented: ${build_excluded[*]}"

if [ "$vet" -eq 1 ]; then
	# `-test` so a package whose tests are what breaks counts as failing.
	for pkg in "${vet_excluded[@]}"; do
		errs="$(go list -e -test -f '{{if .Error}}{{.Error.Err}}{{end}}{{range .DepsErrors}}{{.Err}}{{end}}' "$pkg")"
		[ -n "$errs" ] || die "$pkg is excluded from vet but now loads cleanly; remove it from vet_excluded"
	done
	mapfile -t pkgs < <(packages_except '{{.ImportPath}}' "${vet_excluded[@]}")
	go vet "${pkgs[@]}"
	echo "go-build-plain: go vet ok: ${#pkgs[@]} packages, ${#vet_excluded[@]} excluded (${vet_excluded[*]})"
fi

[ "$(git hash-object go.mod go.sum)" = "$sums_before" ] || die "the go command changed go.mod or go.sum"
if ! git diff --quiet -- go.mod go.sum; then
	die "go.mod or go.sum differs from the index"
fi
