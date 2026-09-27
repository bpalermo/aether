#!/usr/bin/env bash
# //bazel/img:registry_test — scripts/image-registry.sh must answer every
# question exactly as registry.bzl's Starlark helpers do (see BUILD.bazel).
#
# Usage: registry_test.sh <expected file> <image-registry.sh> <registry.bzl>
#   expected: `<subcommand> [<arg>] <answer>` per line, written by Bazel.
set -euo pipefail

expected="$1" script="$2" bzl="$3"
export IMAGE_REGISTRY_BZL="$bzl"

n=0
fail=0
while read -r cmd a b; do
	[ -n "$cmd" ] || continue
	if [ -z "$b" ]; then
		want="$a" got="$("$script" "$cmd")"
		label="$cmd"
	else
		want="$b" got="$("$script" "$cmd" "$a")"
		label="$cmd $a"
	fi
	n=$((n + 1))
	if [ "$got" = "$want" ]; then
		printf '  ok    %-28s %s\n' "$label" "$got"
	else
		printf '  FAIL  %-28s starlark=%s shell=%s\n' "$label" "$want" "$got"
		fail=1
	fi
done <"$expected"

# A comparison over nothing is not agreement (#853).
if [ "$n" -lt 20 ]; then
	echo "only ${n} comparisons — the expected file is not what BUILD.bazel writes" >&2
	exit 2
fi
if [ "$fail" -ne 0 ]; then
	echo "scripts/image-registry.sh disagrees with bazel/img/registry.bzl" >&2
	exit 1
fi
echo "registry: shell and Starlark agree on ${n} answers"
