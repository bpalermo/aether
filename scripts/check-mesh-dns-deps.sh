#!/usr/bin/env bash
# Fails if the standalone mesh-DNS daemon can reach the agent's payload.
#
# Guarded target:
#   //agent/cmd/mesh-dns:mesh-dns - the aether-mesh-dns DaemonSet's daemon (#583)
#
# mesh-dns was carved out of the agent so the resolver every managed pod's :53 is
# DNAT'd to survives agent rolls (#578), and so every node stops pulling
# controller-runtime, go-control-plane, the CNI server and SPIRE for a process
# that answers DNS from a snapshot file and executes none of them.
#
# //agent/cmd/mesh-dns:deps_test pins the full module set (an allow-list and a
# module budget) on the linked ELF (hermetic, no nested bazel). This script is the
# complementary build-graph check: it catches a forbidden dependency declared in
# BUILD.bazel even before the linker drops it as unreachable, which is the earlier
# and more legible failure.
#
# Usage: scripts/check-mesh-dns-deps.sh [extra bazel flags...]
set -euo pipefail

TARGET="//agent/cmd/mesh-dns:mesh-dns"

# miekg/dns, cobra, fsnotify and the OTel SDK are load-bearing, so this names
# the heavyweights (the scripts/check-proxy-supervisor-deps.sh shape) rather than
# forbidding everything.
FORBIDDEN='controller_runtime|io_k8s_client_go|io_k8s_apimachinery|io_k8s_api//|gateway_api|go_control_plane|go_spiffe|spiffe_go|containernetworking_cni'

deps="$(bazel query "deps(${TARGET})" --output=label "$@")"

# Control test: a query that returned nothing useful must not pass vacuously.
for want in '//agent/internal/meshdns' '@com_github_miekg_dns//'; do
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

		mesh-dns answers DNS from a snapshot file the node agent writes. It has no
		Kubernetes client, no xDS server, no CNI and no SPIRE identity, and linking
		one undoes the #583 carve-out that lets the node's resolver survive agent
		rolls without carrying the agent's payload. Fix the import; do not relax
		this check.
	EOF2
	exit 1
fi

echo "OK: ${TARGET} links none of the forbidden packages."
