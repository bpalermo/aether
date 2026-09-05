#!/usr/bin/env bash
# Fails if either exec-readiness prober can reach an init()-heavy package.
#
# Guarded targets (the file keeps its original name so the CI step and any local
# habits stay valid; it has covered both probers since #683):
#   //agent/cmd/proxy-ready:proxy-ready       - the aether-proxy pod's probe (#673)
#   //agent/cmd/mesh-dns-ready:mesh-dns-ready - the aether-mesh-dns pod's probe (#683)
#
# Both replaced a probe that re-exec'd a full daemon on a timer: the proxy's
# re-exec'd /opt/aether/supervisor (the 67MB agent binary) every 2s per pod, with
# runtime.main at 20.27% and runtime.doInit1 at 19.75% of that container's CPU
# (controller-runtime scheme registration, protobuf descriptor registries); the
# mesh-dns one re-exec'd the 16.9MB resolver daemon every 15s per pod, ~10
# core-seconds per 25 minutes fleet-wide of runc/libcontainer exec plus a second
# runtime.main. All of it is package init(), which the Go runtime executes before
# main() is entered and which therefore no argv check can skip. The only fix is
# to not link those packages, so the entire value of these binaries is what they
# do NOT link.
#
# Each target's own :deps_test asserts this on the linked ELF (hermetic, no
# nested bazel). This script is the complementary build-graph check: it catches a
# forbidden dependency declared in BUILD.bazel even before the linker drops it as
# unreachable, which is the earlier and more legible failure.
#
# Usage: scripts/check-proxy-ready-deps.sh [extra bazel flags...]
set -euo pipefail

TARGETS=(
	"//agent/cmd/proxy-ready:proxy-ready"
	"//agent/cmd/mesh-dns-ready:mesh-dns-ready"
)

# Package paths that must never be reachable. "k8s.io/" covers
# sigs.k8s.io/controller-runtime and the client-go/apimachinery family; miekg and
# fsnotify are what the mesh-dns daemon links and its prober must not.
FORBIDDEN='controller-runtime|k8s\.io/|google\.golang\.org/protobuf|go-control-plane|spf13/cobra|opentelemetry|miekg|fsnotify'

failed=0

for target in "${TARGETS[@]}"; do
	deps="$(bazel query "deps(${target})" --output=label "$@")"

	# Control test: a query that returned nothing useful must not pass vacuously.
	if ! grep -q '//common/readymarker' <<<"${deps}"; then
		echo "FAIL: deps(${target}) does not contain //common/readymarker." >&2
		echo "      The query result cannot be trusted — did the target move?" >&2
		failed=1
		continue
	fi

	if matches="$(grep -E "${FORBIDDEN}" <<<"${deps}")"; then
		echo "FAIL: ${target} depends on packages it must never link:" >&2
		while IFS= read -r label; do
			echo "  ${label}" >&2
		done <<<"${matches}"
		failed=1
		continue
	fi

	echo "OK: ${target} links none of the forbidden packages."
done

if ((failed)); then
	cat >&2 <<-'EOF'

		The readiness probers must stay stdlib-only (plus //common/readymarker). An
		init()-heavy dependency here is exactly the per-pod, every-probe cost #673 and
		#683 removed. Fix the import; do not relax this check.
	EOF
	exit 1
fi
