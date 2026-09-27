#!/usr/bin/env bash
# The image registry is ONE setting: bazel/img/registry.bzl (proposal 040).
#
# Phase 2 of the Quay migration flipped that setting (quay.io since then). It
# was only a one-file flip because nothing else spelled the registry out, and
# that is not something a review can be trusted to keep true — a literal pasted
# into a workflow or an e2e script keeps working until the day a flip leaves it
# pointing at the old registry. So this asserts, on every PR:
#
#   1. scripts/image-registry.sh parses the setting (the strict parse the
#      workflows, verifiers and e2e scripts depend on) and every value is
#      non-empty.
#   2. proxy/bazel/registry.bzl — the //proxy workspace's copy, which cannot
#      load() from this module — is byte-identical to bazel/img/registry.bzl.
#   3. The aether-proxy pin in charts/aether/values.yaml names exactly one of
#      proxy_pin_references(): image_reference("proxy"), or a
#      PROXY_PIN_LEGACY_REFERENCES entry. The pin is DATA (proxy-release's
#      bump-chart job rewrites it), so it cannot be derived, and the flip could
#      not move it: it names the pre-cut-over (ghcr.io) image until the first
#      proxy release after the flip re-pins it — reported below as a notice,
#      never silently. Any other registry is an error.
#   4. No tracked file outside the allow-list below contains the registry
#      literal: `<IMAGE_REGISTRY>/<IMAGE_NAMESPACE>`, or the namespace path
#      standing alone (`bpalermo/aether/agent` in a repo list) — both computed
#      from the setting, not typed here. A positive control first proves the
#      pattern matches the pin in values.yaml, so the hunt cannot go vacuous
#      (#853: a gate that finds nothing because it looks for nothing).
#   5. No tracked file outside the LEGACY allow-list below mentions the
#      pre-cut-over registry at all (LEGACY_PREFIXES: the `<host>/<namespace>`
#      it was before proposal 040 phase 2): not a coordinate, not the bare host
#      (other projects' images on that host, `<host>/<someone else>/…`, are not
#      ours and do not count), not the namespace path standing alone. The
#      sweep still reads the old registry for old commits — from git history,
#      never from a literal — and everything user-facing points at the new one;
#      the legacy allow-list is exactly the footprint phase 4 ("decommission
#      ghcr") removes. Same positive and negative controls.
#
# ALLOW-LIST — where the literal is legitimate:
#   bazel/img/registry.bzl, proxy/bazel/registry.bzl   the setting itself
#   charts/aether/values.yaml       the proxy pin (data; asserted by 3)
#   docs/                           runbooks, and proposals' history text
#   website/                        the published site (user-facing install docs)
#   README.md, charts/README.md, proxy/README.md   user-facing install docs
#   .devcontainer/                  devcontainer features (other registries' refs)
#   scripts/check-registry-config.sh    this file
# The user-facing docs moved to the new coordinates with the flip (phase 2).
#
# LEGACY ALLOW-LIST — where the pre-cut-over registry (ghcr.io) may appear:
#   bazel/img/registry.bzl, proxy/bazel/registry.bzl   the setting's history
#                                   note and PROXY_PIN_LEGACY_REFERENCES
#   bazel/proxy_pin/extensions.bzl  documents accepting that legacy pin
#   charts/aether/values.yaml       the proxy pin, until the next proxy release
#   docs/proposals/                 history: every proposal records the
#                                   coordinates of its time (010, 040's mapping)
#   docs/runbook.md                 incident/validation records that cite the
#                                   coordinates in force then, and the
#                                   ghcr.io→quay.io migration section
#   docs/observability/             symbol-upload records from before the flip
#   README.md, charts/README.md, docs/getting-started.md, docs/configuration.md
#                                   the chart-consumer migration note (where
#                                   releases before 1.0.0 live)
#   scripts/registry-lib.sh, scripts/verify-published-artifacts.sh,
#   scripts/verify-image-signatures.sh, scripts/proxy-pin-lib.sh
#                                   the split sweep: pre-cut-over heads are still
#                                   read on ghcr.io (GHCR_TOKEN, tag layouts)
#   .github/workflows/{ci,publish,publish-verify,proxy-release}.y*ml
#                                   comments on that split, and publish-verify's
#                                   GHCR_TOKEN for pre-cut-over reads
#   scripts/check-registry-lookup.sh, scripts/check-publish-verify-control.sh,
#   scripts/check-signature-layout.sh, scripts/check-index-children.sh
#                                   the offline harnesses play the pre-cut-over
#                                   registry to exercise the split
#   scripts/quay-smoke.sh           its history note (it gated the flip)
#   scripts/check-registry-config.sh    this file (LEGACY_PREFIXES)
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
if ! pin_refs="$(scripts/image-registry.sh proxy-pin-refs)"; then
	echo "::error::scripts/image-registry.sh cannot read proxy_pin_references() from bazel/img/registry.bzl" >&2
	exit 2
fi
n_pin=0
pinned=""
while read -r ref; do
	[ -n "$ref" ] || continue
	c="$(grep -cE "^[[:space:]]*repository:[[:space:]]*\"?${ref//./\\.}\"?[[:space:]]*$" charts/aether/values.yaml || true)"
	n_pin=$((n_pin + c))
	[ "$c" = 0 ] || pinned="$ref"
done <<<"$pin_refs"
if [ "$n_pin" != 1 ]; then
	bad "charts/aether/values.yaml has ${n_pin} \`repository:\` line(s) naming one of proxy_pin_references() ($(printf '%s' "$pin_refs" | paste -sd, -)), want exactly 1 — the proxy pin disagrees with bazel/img/registry.bzl"
elif [ "$pinned" = "$PROXY_IMAGE" ]; then
	echo "ok: charts/aether/values.yaml pins the proxy as ${PROXY_IMAGE}"
else
	echo "ok: charts/aether/values.yaml pins the proxy as ${pinned} — a PRE-cut-over reference (PROXY_PIN_LEGACY_REFERENCES); the next proxy release re-pins it to ${PROXY_IMAGE}"
	echo "::notice::the aether-proxy pin still names ${pinned}; it moves to ${PROXY_IMAGE} with the next proxy release (proxy-release.yml bump-chart)"
fi

# --- 4. no literal outside the allow-list ------------------------------------
allow=(
	':(exclude)bazel/img/registry.bzl'
	':(exclude)proxy/bazel/registry.bzl'
	':(exclude)charts/aether/values.yaml'
	':(exclude)docs/'
	':(exclude)website/'
	':(exclude)README.md'
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

# --- 5. no pre-cut-over coordinate outside the legacy allow-list ------------
# The registry before proposal 040 phase 2, as `<host>/<namespace>`: the one
# place (with registry.bzl's history note) that spells it out.
LEGACY_PREFIXES=(
	"ghcr.io/bpalermo/aether"
)
legacy_allow=(
	':(exclude)bazel/img/registry.bzl'
	':(exclude)proxy/bazel/registry.bzl'
	':(exclude)bazel/proxy_pin/extensions.bzl'
	':(exclude)charts/aether/values.yaml'
	':(exclude)docs/proposals/'
	':(exclude)docs/runbook.md'
	':(exclude)docs/observability/'
	':(exclude)README.md'
	':(exclude)charts/README.md'
	':(exclude)docs/getting-started.md'
	':(exclude)docs/configuration.md'
	':(exclude)scripts/registry-lib.sh'
	':(exclude)scripts/verify-published-artifacts.sh'
	':(exclude)scripts/verify-image-signatures.sh'
	':(exclude)scripts/proxy-pin-lib.sh'
	':(exclude).github/workflows/ci.yaml'
	':(exclude).github/workflows/publish.yaml'
	':(exclude).github/workflows/publish-verify.yaml'
	':(exclude).github/workflows/proxy-release.yml'
	':(exclude)scripts/check-registry-lookup.sh'
	':(exclude)scripts/check-publish-verify-control.sh'
	':(exclude)scripts/check-signature-layout.sh'
	':(exclude)scripts/check-index-children.sh'
	':(exclude)scripts/quay-smoke.sh'
	':(exclude)scripts/check-registry-config.sh'
)
legacy_pattern=""
for lp in "${LEGACY_PREFIXES[@]}"; do
	l_host="${lp%%/*}" l_ns="${lp#*/}"
	if [ "$l_host" = "$IMAGE_REGISTRY_HOST" ]; then
		echo "::error::LEGACY_PREFIXES names the CURRENT registry host (${l_host})" >&2
		exit 2
	fi
	# The host, unless another project's namespace follows it
	# (`<host>/<someone else>/...`: a third-party image, not ours).
	l_host_re="(?<![[:alnum:]_.-])$(esc "$l_host")(?!/(?!$(esc "${l_ns%%/*}")/)[[:alnum:]_.-]+/)(?![[:alnum:]_-])"
	l_bare_ns="(?<![[:alnum:]_./-])$(esc "$l_ns")/"
	legacy_pattern="${legacy_pattern:+${legacy_pattern}|}${l_host_re}|${l_bare_ns}"
	for probe in "  IMAGE_REGISTRY: ${lp}" "helm install x oci://${lp}/charts/aether" "	${l_ns}/agent" \
		"# signatures as tags on ${l_host}." "registry: ${l_host}"; do
		if ! printf '%s\n' "$probe" | grep -qP -- "$legacy_pattern"; then
			echo "::error::the legacy hunt does not match '${probe}' — the pattern is broken" >&2
			exit 2
		fi
	done
	for probe in "image: ${l_host}/open-telemetry/opentelemetry-collector:1" \
		"https://github.com/${l_ns}/blob/main/x" "notghcr.io.example"; do
		if printf '%s\n' "$probe" | grep -qP -- "$legacy_pattern"; then
			echo "::error::the legacy hunt matches '${probe}' — another project's image or a source link is not ours" >&2
			exit 2
		fi
	done
done
hits="$(git grep -nP -- "$legacy_pattern" -- . "${legacy_allow[@]}")"
rc=$?
case "$rc" in
0)
	bad "the pre-cut-over registry (${LEGACY_PREFIXES[*]}) is still mentioned. Point it at the setting (quay.io since proposal 040 phase 2), or — if it is a historical record or part of the split sweep — add the path to the LEGACY allow-list in scripts/check-registry-config.sh with a reason:"
	printf '%s\n' "$hits" | sed 's/^/  /'
	;;
1) echo "ok: the pre-cut-over registry (${LEGACY_PREFIXES[*]}) appears nowhere outside the legacy allow-list" ;;
*)
	echo "::error::git grep failed (exit ${rc})" >&2
	exit 2
	;;
esac

if [ "$fail" -ne 0 ]; then
	exit 1
fi
echo "registry config: one setting, bazel/img/registry.bzl"
