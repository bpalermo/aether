#!/usr/bin/env bash
# Build the whole module with the plain go command, the way a tool that knows
# nothing about Bazel does. This is the property CodeQL's Go extraction needs
# (.github/workflows/codeql.yaml runs this script twice: once before the scan,
# as the proof, and once under CodeQL's tracer); docs/runbook.md, "CodeQL code
# scanning".
#
#   scripts/go-build-plain.sh           go build ./...
#   scripts/go-build-plain.sh --vet     the same, then go vet ./...
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
# The whole module, with no exclusion: `./...` is every package under the
# module root, test/ and e2e/ included, and there is no list of packages to
# skip. A package the go command cannot load (an import of another tree's
# internal/ package, which Bazel's `visibility` would let through; a generated
# file that was not materialized) fails this script. Keep it that way: fix the
# package, do not exclude it (#1311 moved the three that once were).
# //scripts:go_build_plain_test holds the two invocations to exactly `./...`.
#
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

echo "go-build-plain: $have, GOOS=$GOOS GOARCH=$(go env GOARCH) CGO_ENABLED=$CGO_ENABLED GOFLAGS=$GOFLAGS"
# Under CodeQL's tracer this is also what puts every non-test file in the
# database: the extractor runs on the packages `go build` is given.
go build ./...
echo "go-build-plain: go build ./... ok: $(go list ./... | grep -c '') packages, none excluded"

if [ "$vet" -eq 1 ]; then
	go vet ./...
	echo "go-build-plain: go vet ./... ok, none excluded"
fi

[ "$(git hash-object go.mod go.sum)" = "$sums_before" ] || die "the go command changed go.mod or go.sum"
if ! git diff --quiet -- go.mod go.sum; then
	die "go.mod or go.sum differs from the index"
fi
