#!/usr/bin/env bash
# Fails if the CNI plugin can reach a Kubernetes client, the xDS stack or SPIRE.
#
# Guarded target:
#   //cni/cmd/cni:cni - the CNI plugin the container runtime execs per pod ADD/DEL
#
# The plugin is exec'd for every pod ADD and DEL on every node, so every package
# it links is paid in Go package init() on each invocation (init() runs before
# main(); no argv check can skip it). It talks to the node agent over the CNI gRPC
# socket and to the kernel (netlink/nftables); the agent owns the Kubernetes
# client, the xDS server and the SPIRE identity.
#
# //cni/cmd/cni:deps_test pins the full module set (an allow-list and a module
# budget) on the linked ELF (hermetic, no nested bazel). This script is the
# complementary build-graph check: it catches a forbidden dependency declared in
# BUILD.bazel even before the linker drops it as unreachable, which is the earlier
# and more legible failure.
#
# Usage: scripts/check-cni-deps.sh [extra bazel flags...]
set -euo pipefail

TARGET="//cni/cmd/cni:cni"

# grpc, protobuf, netlink/nftables, zap and (for now) the OTel SDK are
# load-bearing, so this names the heavyweights (the
# scripts/check-proxy-supervisor-deps.sh shape) rather than forbidding everything.
FORBIDDEN='controller_runtime|io_k8s_client_go|io_k8s_apimachinery|io_k8s_api//|gateway_api|go_control_plane|go_spiffe|spiffe_go|miekg|spf13_cobra'

deps="$(bazel query "deps(${TARGET})" --output=label "$@")"

# Control test: a query that returned nothing useful must not pass vacuously.
for want in '//cni/internal/plugin' '@com_github_containernetworking_cni//pkg/skel'; do
	if ! grep -qF "${want}" <<<"${deps}"; then
		echo "FAIL: deps(${TARGET}) does not contain ${want}." >&2
		echo "      The query result cannot be trusted — did the target move?" >&2
		exit 1
	fi
done

if matches="$(grep -E "${FORBIDDEN}" <<<"${deps}")"; then
	echo "FAIL: ${TARGET} depends on packages it must never link:" >&2
	while IFS= read -r label; do
		echo "  ${label}" >&2
	done <<<"${matches}"
	cat >&2 <<-'EOF2'

		The CNI plugin is exec'd for every pod ADD/DEL and talks only to the node
		agent (the CNI gRPC socket) and the kernel. A Kubernetes client, the xDS
		stack or SPIRE in its link set is paid in package init() on every pod
		start and stop. Fix the import; do not relax this check.
	EOF2
	exit 1
fi

echo "OK: ${TARGET} links none of the forbidden packages."
