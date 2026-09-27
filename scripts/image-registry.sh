#!/usr/bin/env bash
# Print the image registry setting, parsed from bazel/img/registry.bzl.
#
# bazel/img/registry.bzl is the ONE place the registry host, namespace and naming
# rules are written down (proposal 040). Bazel loads it; everything that is not
# Bazel -- the workflows, the verifiers, the e2e scripts -- asks this script, so
# a registry flip is an edit to that file and nothing else.
#
# The parse is deliberately strict: each assignment must appear EXACTLY once, on
# one line, in the exact shape the .bzl documents. A setting this script cannot
# read is exit 2 with nothing on stdout, never a default: a default here would
# quietly publish or verify against a registry nobody chose.
# //bazel/img:registry_test asserts the Starlark helpers and this script agree.
#
# Usage:
#   image-registry.sh               KEY=VALUE lines (for "$GITHUB_ENV" or eval):
#       IMAGE_REGISTRY_HOST      ghcr.io
#       IMAGE_NAMESPACE          bpalermo/aether
#       IMAGE_REGISTRY           <host>/<namespace>: the prefix the workflows
#                                append a component name to (not valid for a
#                                component with a name override -- use `ref`)
#       PROXY_IMAGE              image_reference("proxy")
#       CHART_REPOSITORY_PREFIX  charts/
#   image-registry.sh repo <component>   image_repository(component)
#   image-registry.sh ref <component>    image_reference(component)
#   image-registry.sh chart-repo <chart> chart_repository(chart)
#   image-registry.sh host               the registry host alone
#   image-registry.sh prefix             <host>/<namespace> (IMAGE_REGISTRY above)
#
# Every value is validated against [a-z0-9./_-] (plus :port on the host), so the
# KEY=VALUE form is safe to eval and to append to $GITHUB_ENV unquoted.
set -euo pipefail

bzl="${IMAGE_REGISTRY_BZL:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bazel/img/registry.bzl}"

die() {
	echo "image-registry.sh: ${bzl}: $*" >&2
	exit 2
}

[ -r "$bzl" ] || die "not readable"

# one <NAME> <value regex> -> the value of the single `NAME = "value"` line.
one() {
	local name="$1" re="$2" lines
	lines="$(grep -E "^${name}[[:space:]]*=" "$bzl" || true)"
	[ -n "$lines" ] || die "no ${name} assignment"
	[ "$(printf '%s\n' "$lines" | wc -l)" -eq 1 ] || die "${name} is assigned more than once"
	[[ "$lines" =~ ^${name}\ =\ \"(${re})\"$ ]] || die "cannot parse: ${lines}"
	printf '%s\n' "${BASH_REMATCH[1]}"
}

host="$(one IMAGE_REGISTRY '[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?')"
namespace="$(one IMAGE_NAMESPACE '[a-z0-9]([a-z0-9._/-]*[a-z0-9])?')"
chart_prefix="$(one CHART_REPOSITORY_PREFIX '[a-z0-9._/-]*')"

# IMAGE_NAME_OVERRIDES = {"a": "b", "c": "d"}  (or {})
ov_lines="$(grep -E '^IMAGE_NAME_OVERRIDES[[:space:]]*=' "$bzl" || true)"
[ -n "$ov_lines" ] || die "no IMAGE_NAME_OVERRIDES assignment"
[ "$(printf '%s\n' "$ov_lines" | wc -l)" -eq 1 ] || die "IMAGE_NAME_OVERRIDES is assigned more than once"
pair='"[a-z0-9._-]+": "[a-z0-9._-]+"'
[[ "$ov_lines" =~ ^IMAGE_NAME_OVERRIDES\ =\ \{((${pair})(, ${pair})*)?\}$ ]] ||
	die "cannot parse: ${ov_lines}"
# `<component> <name>` per line. A plain list, not an associative array: bash
# 3.2 (macOS) runs the e2e scripts that call this.
overrides=""
body="${BASH_REMATCH[1]}"
while [[ "$body" =~ ^\"([a-z0-9._-]+)\":\ \"([a-z0-9._-]+)\"(,\ )?(.*)$ ]]; do
	case $'\n'"$overrides" in *$'\n'"${BASH_REMATCH[1]} "*) die "IMAGE_NAME_OVERRIDES repeats ${BASH_REMATCH[1]}" ;; esac
	overrides="${overrides}${BASH_REMATCH[1]} ${BASH_REMATCH[2]}"$'\n'
	body="${BASH_REMATCH[4]}"
done
[ -z "$body" ] || die "cannot parse IMAGE_NAME_OVERRIDES near: ${body}"

name_ok() {
	[[ "$1" =~ ^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$ ]] || {
		echo "image-registry.sh: not a component/chart name: '$1'" >&2
		exit 2
	}
}

repo() {
	local c="$1" k v name
	name_ok "$c"
	name="$c"
	while read -r k v; do
		[ "$k" = "$c" ] && name="$v"
	done <<<"$overrides"
	printf '%s/%s\n' "$namespace" "$name"
}

case "${1:-env}" in
env)
	printf 'IMAGE_REGISTRY_HOST=%s\n' "$host"
	printf 'IMAGE_NAMESPACE=%s\n' "$namespace"
	printf 'IMAGE_REGISTRY=%s/%s\n' "$host" "$namespace"
	proxy_repo="$(repo proxy)"
	printf 'PROXY_IMAGE=%s/%s\n' "$host" "$proxy_repo"
	printf 'CHART_REPOSITORY_PREFIX=%s\n' "$chart_prefix"
	;;
host) printf '%s\n' "$host" ;;
prefix) printf '%s/%s\n' "$host" "$namespace" ;;
repo)
	[ "$#" -eq 2 ] || die "usage: repo <component>"
	repo "$2"
	;;
ref)
	[ "$#" -eq 2 ] || die "usage: ref <component>"
	r="$(repo "$2")"
	printf '%s/%s\n' "$host" "$r"
	;;
chart-repo)
	[ "$#" -eq 2 ] || die "usage: chart-repo <chart>"
	name_ok "$2"
	printf '%s/%s%s\n' "$namespace" "$chart_prefix" "$2"
	;;
*)
	echo "usage: $0 [env | host | prefix | repo <component> | ref <component> | chart-repo <chart>]" >&2
	exit 2
	;;
esac
