#!/usr/bin/env bash
# Fails if the csi.aether.io node plugin can reach a Kubernetes client, the xDS
# stack or SPIRE.
#
# Guarded target:
#   //agent/cmd/uds-csi:uds-csi - the csi.aether.io CSI node plugin (proposal 039)
#
# The plugin runs PRIVILEGED, as root, with Bidirectional mount propagation into
# the host on every node: it is the most privileged process aether runs, so every
# package it links is attack surface, not just megabytes. Its whole job is two
# gRPC services on Unix sockets (CSI Identity/Node, and the kubelet's plugin
# registration API) plus mount(2)/umount2(2) through
# golang.org/x/sys/unix. It makes no Kubernetes API calls (no RBAC, no token)
# and SPIRE is not involved at all.
#
# //agent/cmd/uds-csi:deps_test asserts this on the linked ELF (hermetic, no
# nested bazel). This script is the complementary build-graph check: it catches a
# forbidden dependency declared in BUILD.bazel even before the linker drops it as
# unreachable, which is the earlier and more legible failure.
#
# Usage: scripts/check-uds-csi-deps.sh [extra bazel flags...]
set -euo pipefail

TARGET="//agent/cmd/uds-csi:uds-csi"

# The CSI spec module, grpc and protobuf are load-bearing, so this names the
# heavyweights (the scripts/check-proxy-supervisor-deps.sh shape) rather than
# forbidding everything. cobra and the OTel SDK are listed too: the plugin parses
# its flags with the stdlib and exports no telemetry.
FORBIDDEN='controller_runtime|io_k8s_client_go|io_k8s_apimachinery|io_k8s_api//|gateway_api|go_control_plane|go_spiffe|spiffe_go|miekg|spf13_cobra|otel_sdk'

deps="$(bazel query "deps(${TARGET})" --output=label "$@")"

# Control test: a query that returned nothing useful must not pass vacuously.
for want in '//agent/internal/udscsi' '@com_github_container_storage_interface_spec//lib/go/csi'; do
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
	cat >&2 <<-'EOF'

		The csi.aether.io node plugin is privileged on every node and talks only to
		the kubelet (two Unix sockets) and the kernel. A Kubernetes client, the xDS
		stack or SPIRE in its link set is attack surface in the most privileged
		aether process there is. Fix the import; do not relax this check.
	EOF
	exit 1
fi

echo "OK: ${TARGET} links none of the forbidden packages."
