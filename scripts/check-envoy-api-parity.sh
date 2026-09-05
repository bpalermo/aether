#!/usr/bin/env bash
# Keep the agent's xDS API bindings aligned with the Envoy the proxy is built from.
#
# The agent generates Envoy config with github.com/envoyproxy/go-control-plane/envoy
# (go.mod), which go-control-plane publishes per Envoy release (envoy/v1.NN.0). The
# proxy is built from the envoy_api module pinned in proxy/MODULE.bazel, which is
# either a release (1.NN.0.envoy) or a main snapshot (1.NN.0-dev.<date>.<sha>.envoy).
# Fields added between the two are invisible to the agent, and deprecated ones can
# vanish from the proxy first, so the bindings must be the release matching the
# proxy, or — while the proxy tracks a -dev snapshot of the NEXT release, which has
# no bindings yet — the latest release before it (exactly one minor behind).
set -euo pipefail
cd "$(dirname "$0")/.."

bindings=$(sed -nE 's|^\s*github.com/envoyproxy/go-control-plane/envoy v([0-9]+\.[0-9]+)\.[0-9]+.*$|\1|p' go.mod | head -1)
proxy_line=$(sed -nE 's|^bazel_dep\(name = "envoy_api", version = "([^"]+)"\)|\1|p' proxy/MODULE.bazel | head -1)
[ -n "$bindings" ] || {
	echo "::error::go.mod: no github.com/envoyproxy/go-control-plane/envoy requirement found"
	exit 1
}
[ -n "$proxy_line" ] || {
	echo "::error::proxy/MODULE.bazel: no bazel_dep(name = \"envoy_api\", version = ...) found"
	exit 1
}

proxy_mm=$(sed -nE 's|^([0-9]+\.[0-9]+)\.[0-9]+.*$|\1|p' <<<"$proxy_line")
case "$proxy_line" in *-dev.*) dev=1 ;; *) dev=0 ;; esac

b_major=${bindings%%.*}
b_minor=${bindings#*.}
p_major=${proxy_mm%%.*}
p_minor=${proxy_mm#*.}

echo "bindings: go-control-plane/envoy v${bindings}  proxy: envoy_api ${proxy_line} (dev=${dev})"
if [ "$b_major" != "$p_major" ]; then
	echo "::error::Envoy major version mismatch: bindings ${bindings} vs proxy ${proxy_mm}"
	exit 1
fi
if [ "$dev" = 1 ]; then
	# A -dev snapshot of 1.NN has no bindings yet; 1.(NN-1) is the newest possible.
	if [ "$b_minor" -ne "$((p_minor - 1))" ] && [ "$b_minor" -ne "$p_minor" ]; then
		echo "::error::bindings v${bindings} are not the latest release before the proxy's ${proxy_line} snapshot (want ${p_major}.$((p_minor - 1)) or ${proxy_mm}): run 'bazel run @rules_go//go -- get github.com/envoyproxy/go-control-plane/envoy@v${p_major}.$((p_minor - 1)).0 && make tidy'"
		exit 1
	fi
elif [ "$b_minor" -ne "$p_minor" ]; then
	echo "::error::bindings v${bindings} do not match the proxy's release ${proxy_line}: run 'bazel run @rules_go//go -- get github.com/envoyproxy/go-control-plane/envoy@v${proxy_mm}.0 && make tidy'"
	exit 1
fi
echo "OK: xDS bindings are aligned with the proxy's Envoy"
