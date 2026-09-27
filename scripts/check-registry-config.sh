#!/usr/bin/env bash
# The image registry is ONE setting: bazel/img/registry.bzl (proposal 040).
#
# Phase 2 of the Quay migration flips that setting. It is only a one-file flip
# if nothing else spells the registry out, and that is not something a review
# can be trusted to keep true — a literal pasted into a workflow or an e2e
# script keeps working until the day the flip leaves it pointing at the old
# registry. So this asserts, on every PR:
#
#   1. scripts/image-registry.sh parses the setting (the strict parse the
#      workflows, verifiers and e2e scripts depend on) and every value is
#      non-empty.
#   2. proxy/bazel/registry.bzl — the //proxy workspace's copy, which cannot
#      load() from this module — is byte-identical to bazel/img/registry.bzl.
#   3. The aether-proxy pin in charts/aether/values.yaml names exactly
#      image_reference("proxy"): the pin is DATA (proxy-release's bump-chart job
#      rewrites it), so it cannot be derived, but it must agree.
#   4. No tracked file outside the allow-list below contains the registry
#      literal: `<IMAGE_REGISTRY>/<IMAGE_NAMESPACE>`, or the namespace path
#      standing alone (`bpalermo/aether/agent` in a repo list) — both computed
#      from the setting, not typed here. A positive control first proves the
#      pattern matches the pin in values.yaml, so the hunt cannot go vacuous
#      (#853: a gate that finds nothing because it looks for nothing).
#
# ALLOW-LIST — where the literal is legitimate:
#   bazel/img/registry.bzl, proxy/bazel/registry.bzl   the setting itself
#   charts/aether/values.yaml       the proxy pin (data; asserted by 3)
#   docs/                           runbooks, and proposals' history text
#   website/                        the published site (user-facing install docs)
#   charts/README.md, proxy/README.md   user-facing install docs
#   .devcontainer/                  devcontainer features (other registries' refs)
#   scripts/check-registry-config.sh    this file
# The user-facing docs move to the new coordinates in phase 2 with the flip,
# not before: until then they are true.
#
# No network, no Bazel. Exit 0 clean, 1 on a violation, 2 when the check
# itself cannot run.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

fail=0
bad() {
	echo "::error::$*"
	fail=1
}

# --- 1. the setting parses ---------------------------------------------------
if ! env_out="$(scripts/image-registry.sh)"; then
	echo "::error::scripts/image-registry.sh cannot parse bazel/img/registry.bzl" >&2
	exit 2
fi
IMAGE_REGISTRY_HOST="" IMAGE_NAMESPACE="" PROXY_IMAGE=""
while IFS='=' read -r k v; do
	case "$k" in
	IMAGE_REGISTRY_HOST) IMAGE_REGISTRY_HOST="$v" ;;
	IMAGE_NAMESPACE) IMAGE_NAMESPACE="$v" ;;
	PROXY_IMAGE) PROXY_IMAGE="$v" ;;
	esac
done <<<"$env_out"
if [ -z "$IMAGE_REGISTRY_HOST" ] || [ -z "$IMAGE_NAMESPACE" ] || [ -z "$PROXY_IMAGE" ]; then
	echo "::error::scripts/image-registry.sh printed an incomplete setting:" >&2
	printf '%s\n' "$env_out" >&2
	exit 2
fi
echo "setting: ${IMAGE_REGISTRY_HOST}/${IMAGE_NAMESPACE} (proxy: ${PROXY_IMAGE})"

# --- 2. the proxy workspace's copy -------------------------------------------
if cmp -s bazel/img/registry.bzl proxy/bazel/registry.bzl; then
	echo "ok: proxy/bazel/registry.bzl is identical to bazel/img/registry.bzl"
else
	bad "proxy/bazel/registry.bzl differs from bazel/img/registry.bzl — the //proxy workspace would publish somewhere else. Copy it: cp bazel/img/registry.bzl proxy/bazel/registry.bzl"
	diff -u bazel/img/registry.bzl proxy/bazel/registry.bzl | head -20
fi

# --- 3. the proxy pin agrees --------------------------------------------------
n_pin="$(grep -cE "^[[:space:]]*repository:[[:space:]]*\"?${PROXY_IMAGE//./\\.}\"?[[:space:]]*$" charts/aether/values.yaml || true)"
if [ "$n_pin" = 1 ]; then
	echo "ok: charts/aether/values.yaml pins the proxy as ${PROXY_IMAGE}"
else
	bad "charts/aether/values.yaml has ${n_pin} \`repository: ${PROXY_IMAGE}\` line(s), want exactly 1 — the proxy pin disagrees with bazel/img/registry.bzl"
fi

# --- 4. no literal outside the allow-list ------------------------------------
allow=(
	':(exclude)bazel/img/registry.bzl'
	':(exclude)proxy/bazel/registry.bzl'
	':(exclude)charts/aether/values.yaml'
	':(exclude)docs/'
	':(exclude)website/'
	':(exclude)charts/README.md'
	':(exclude)proxy/README.md'
	':(exclude).devcontainer/'
	':(exclude)scripts/check-registry-config.sh'
)
esc() { printf '%s' "$1" | sed -E 's/[][\.^$*+?(){}|/]/\\&/g'; }
host_ns="$(esc "${IMAGE_REGISTRY_HOST}/${IMAGE_NAMESPACE}")(?![[:alnum:]_.-])"
# The namespace path on its own, not glued to a host, a URL path or a word
# (github.com/<owner>/<repo>/... is the source repository, not an image).
bare_ns="(?<![[:alnum:]_./-])$(esc "${IMAGE_NAMESPACE}")/"
pattern="${host_ns}|${bare_ns}"

# Positive control: the same pattern must see both forms it hunts for, in the
# shapes they were actually written in before proposal 040. (Not values.yaml:
# on the phase-2 flip its pin still names the OLD registry until it is re-pinned,
# which is check 3's finding to report, not a broken pattern.)
for probe in \
	"  IMAGE_REGISTRY: ${IMAGE_REGISTRY_HOST}/${IMAGE_NAMESPACE}" \
	"    repository: ${PROXY_IMAGE}" \
	"	${IMAGE_NAMESPACE}/agent"; do
	if ! printf '%s\n' "$probe" | grep -qP -- "$pattern"; then
		echo "::error::the literal hunt does not match '${probe}' — the pattern is broken, and a hunt that cannot match proves nothing" >&2
		exit 2
	fi
done
# …and must NOT see the source repository's URLs, or the allow-list would have
# to swallow every file that links to GitHub.
if printf '%s\n' "https://github.com/${IMAGE_NAMESPACE}/blob/main/x" | grep -qP -- "$bare_ns"; then
	echo "::error::the literal hunt matches a github.com URL — it would flag every source link" >&2
	exit 2
fi

hits="$(git grep -nP -- "$pattern" -- . "${allow[@]}")"
rc=$?
case "$rc" in
0)
	bad "the registry is spelled out outside bazel/img/registry.bzl. Derive it (Bazel: load //bazel/img:registry.bzl; shell/workflows: scripts/image-registry.sh), or — if it is documentation — add the path to the allow-list in scripts/check-registry-config.sh with a reason:"
	printf '%s\n' "$hits" | sed 's/^/  /'
	;;
1) echo "ok: no registry literal outside the allow-list" ;;
*)
	echo "::error::git grep failed (exit ${rc})" >&2
	exit 2
	;;
esac

if [ "$fail" -ne 0 ]; then
	exit 1
fi
echo "registry config: one setting, bazel/img/registry.bzl"
