#!/usr/bin/env bash
# Fails if the Envoy hot-restart supervisor can reach the agent's payload.
#
# Guarded target:
#   //agent/cmd/proxy-supervisor:proxy-supervisor - the aether-proxy pod's PID 1 (#772 phase B2)
#
# Until #772 this was `agent proxy-supervisor`, a subcommand of //agent/cmd/agent,
# so the install-supervisor initContainer staged the whole 65MiB agent binary onto
# the proxy pod's shared volume and the proxy container ran it: controller-runtime,
# client-go, go-control-plane, SPIRE, Gateway API and miekg/dns, none of which the
# supervisor executes. Its real work — //agent/internal/proxy/hotrestart plus
# //common/readymarker — needs ~22 modules and forks a child process.
#
# That cost is not hypothetical in this container: Go package init() runs before
# main() is entered, so every linked package is paid on every supervisor start and
# every hot-restart epoch, and #673 measured the same binary's init() at >=31% of
# this container's CPU when it was additionally re-exec'd as the readiness probe.
#
# //agent/cmd/proxy-supervisor:deps_test asserts this on the linked ELF (hermetic,
# no nested bazel). This script is the complementary build-graph check: it catches
# a forbidden dependency declared in BUILD.bazel even before the linker drops it as
# unreachable, which is the earlier and more legible failure.
#
# Usage: scripts/check-proxy-supervisor-deps.sh [extra bazel flags...]
set -euo pipefail

TARGET="//agent/cmd/proxy-supervisor:proxy-supervisor"

# Unlike the readiness probers (scripts/check-proxy-ready-deps.sh) this binary is
# not stdlib-only — cobra, fsnotify and the OTel metric SDK are load-bearing — so
# this names the heavyweights instead of forbidding everything. "k8s.io/" is not
# used as a blanket here for the same reason: it would be an over-broad pattern on
# a target that legitimately links a gRPC/protobuf stack.
FORBIDDEN='controller_runtime|io_k8s_client_go|io_k8s_apimachinery|io_k8s_api//|gateway_api|go_control_plane|go_spiffe|spiffe_go|miekg'

deps="$(bazel query "deps(${TARGET})" --output=label "$@")"

# Control test: a query that returned nothing useful must not pass vacuously.
if ! grep -q '//agent/internal/proxy/hotrestart' <<<"${deps}"; then
	echo "FAIL: deps(${TARGET}) does not contain //agent/internal/proxy/hotrestart." >&2
	echo "      The query result cannot be trusted — did the target move?" >&2
	exit 1
fi

if matches="$(grep -E "${FORBIDDEN}" <<<"${deps}")"; then
	echo "FAIL: ${TARGET} depends on packages it must never link:" >&2
	while IFS= read -r label; do
		echo "  ${label}" >&2
	done <<<"${matches}"
	cat >&2 <<-'EOF'

		The proxy supervisor forks and hot-restarts an Envoy child process. It has no
		Kubernetes client, no xDS server and no SPIRE identity, and linking one undoes
		the #772 carve-out that took the binary the proxy pod runs from 65MiB to single
		digits of MiB — paid in package init() on every start and every epoch. Fix the
		import; do not relax this check.
	EOF
	exit 1
fi

echo "OK: ${TARGET} links none of the forbidden packages."
