#!/usr/bin/env bash
# Fails if //agent/cmd/proxy-ready:proxy-ready can reach an init()-heavy package.
#
# The aether-proxy pod's readiness probe (#673) used to re-exec
# /opt/aether/supervisor — which IS the 67MB agent binary — once every 2s per
# pod. Profiling put runtime.main at 20.27% and runtime.doInit1 at 19.75% of the
# supervisor container's CPU: controller-runtime scheme registration and protobuf
# descriptor registries, all of it package init() that the Go runtime executes
# before main() is entered and that therefore no argv check can skip. The only
# fix is to not link those packages, so the entire value of the replacement
# binary is what it does NOT link.
#
# //agent/cmd/proxy-ready:deps_test asserts this on the linked ELF (hermetic, no
# nested bazel). This script is the complementary build-graph check: it catches a
# forbidden dependency declared in BUILD.bazel even before the linker drops it as
# unreachable, which is the earlier and more legible failure.
#
# Usage: scripts/check-proxy-ready-deps.sh [extra bazel flags...]
set -euo pipefail

TARGET="//agent/cmd/proxy-ready:proxy-ready"

# Package paths that must never be reachable. "k8s.io/" covers
# sigs.k8s.io/controller-runtime and the client-go/apimachinery family.
FORBIDDEN='controller-runtime|k8s\.io/|google\.golang\.org/protobuf|go-control-plane|spf13/cobra|opentelemetry'

deps="$(bazel query "deps(${TARGET})" --output=label "$@")"

# Control test: a query that returned nothing useful must not pass vacuously.
if ! grep -q '//common/readymarker' <<<"${deps}"; then
	echo "FAIL: deps(${TARGET}) does not contain //common/readymarker." >&2
	echo "      The query result cannot be trusted — did the target move?" >&2
	exit 1
fi

if matches="$(grep -E "${FORBIDDEN}" <<<"${deps}")"; then
	echo "FAIL: ${TARGET} depends on packages it must never link:" >&2
	while IFS= read -r label; do
		echo "  ${label}" >&2
	done <<<"${matches}"
	cat >&2 <<-'EOF'

		proxy-ready must stay stdlib-only (plus //common/readymarker). An init()-heavy
		dependency here is exactly the per-pod, every-2s cost #673 removed. Fix the
		import; do not relax this check.
	EOF
	exit 1
fi

echo "OK: ${TARGET} links none of the forbidden packages."
