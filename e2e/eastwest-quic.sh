#!/usr/bin/env bash
# Single-cluster kind e2e for proposal 038 Phase 4 (east-west QUIC): the
# per-pod HTTP/3 inbound on UDP:18008 (#953, unconditional) and the per-source
# `quic:<svc>.<ns>.<domain>@<ns>/<sa>` clusters selected by the source SPIFFE
# ID (#956; unconditional since the proving allow-list was removed, #979:
# EVERY destination in the node's dependency set is QUIC-eligible, and its
# twins are built per (source, destination) pair on first use, #1020).
#
# What it proves, on a real data path (kind + the real chart + the real
# aether-proxy + SPIRE + the CNI's capture):
#
#   E0  preflight   every destination pod has its HTTP/3 inbound listener
#                   (inbound_<namespace>_<pod>_h3) and the per-listener request counter this
#                   suite reads exists — so every "== 0" below is a reading, not
#                   an absent stat (#853)
#   E1  fan-out     twins are DEMAND-SCOPED (aether#1020): before any request
#                   no pair has dialled, so no quic: twin exists (on a fresh
#                   cluster: zero) -- nothing is listed anywhere, and still no
#                   twin exists for a pair that never dialled
#   E2  h3 + id     client-a and client-b (DIFFERENT ServiceAccounts) each call
#                   quic-a and quic-b: every request answers 200, the destination
#                   sees the CALLER'S OWN SPIFFE ID in x-forwarded-client-cert,
#                   and the request rode the caller's own quic: twin over HTTP/3
#                   (never the other source's twin, never the h2 cluster)
#   E4  GAMMA       gamma-a with a weighted HTTPRoute canary to gamma-a /
#                   gamma-b (both QUIC-eligible) stays on h2 — the matcher
#                   action names ONE cluster, so a weighted split has no
#                   per-source form (#961) — and so never fetches a twin.
#                   (There is no E3 any more: it was the h2-only, not
#                   allow-listed destination, and no such destination exists
#                   since #979; a weighted split is the one h2-by-design path.)
#   E4b GAMMA       a single-backendRef HTTPRoute rule to the parent (gamma-a)
#                   renders as `cluster:` and IS selected: it rides the caller's
#                   own quic: twin over HTTP/3 like the default route (#961)
#   E4c pairs       the node's quic: cluster count EQUALS the (source,
#                   destination) pairs this suite drove over a selecting route
#                   (E2 + E4b = 2 sources x 3 destinations = 6), and the set is
#                   exactly those pairs: a twin is built only for a pair that has
#                   dialled (aether#1020; the pre-#1020 up-front fan-out would be
#                   every local SA x QUIC destination: 6 x 4 = 24 on this node)
#   E5  Q3          client-a's pod is deleted and comes back with a NEW pod IP;
#                   its requests still carry client-a's identity over HTTP/3, and
#                   client-b's still carry client-b's
#   E6  id gate     a pod under a ServiceAccount that did not exist a second
#                   earlier sends its FIRST request the instant its container
#                   starts (#1053). The webhook-injected aether-identity-ready
#                   init container ran and exited 0 BEFORE the app started, and
#                   that first request answers 200 carrying the new SA's own
#                   SPIFFE ID
#   E7  burst       a pod under a brand-new ServiceAccount dials BURST_DSTS (10)
#                   QUIC destinations for the first time AT ONCE, BURST_PER_DST
#                   concurrent requests each, the instant its app starts -- the
#                   k6 loader start of aether#1086, where every first request
#                   routes to a `quic:` twin the snapshot does not carry yet and
#                   the node proxy asks for all of them over ODCDS within ~100
#                   ms. Asserts 0 non-200 (the failure was 503 NC
#                   cluster_not_found at the 2 s on_demand timeout) and reports
#                   the first-use latency (p50/p99/max), the admissions, and --
#                   with EWQ_DEBUG=1 at `up`, which turns on the agent's debug
#                   log -- how many snapshot builds the burst cost (one per
#                   admission before #1086; one or two after)
#
# HOW "OVER HTTP/3" IS PROVEN. The application behind the inbound only ever sees
# the proxy's loopback hop, so nothing the app can report distinguishes h2 from
# h3 on the MESH hop. The proof is read from the node proxy's admin, before and
# after each measured batch, at BOTH ends:
#
#   source side      /clusters HOST rows, `<cluster>::<ip:port>::rq_total::N`.
#                    Host counters live on each cluster's own host objects, so
#                    they are per cluster object regardless of stats naming
#                    (since aether#960 each twin also has its own stats key,
#                    "<ns>/<svc>@<ns>/<sa>"; the host rows were the evidence
#                    before that and stay the evidence, so this suite does not
#                    depend on the stats-key shape).
#   destination side `listener.inbound_<namespace>_<pod>_h3.http.inbound.downstream_rq_2xx` —
#                    the per-listener HCM counter of the pod's QUIC listener
#                    (proxy.NewInboundQUICListener: stat_prefix inbound_<namespace>_<pod>_h3;
#                    buildInboundHCM: HCM stat_prefix "inbound"). Only a QUIC
#                    connection can reach that listener, and only a quic: twin
#                    dials one.
#
# HOW "EACH SOURCE'S OWN IDENTITY" IS PROVEN. The destination app is agnhost
# netexec, whose /header?key=X-Forwarded-Client-Cert echoes the XFCC the inbound
# HCM stamps SANITIZE_SET from the VERIFIED peer certificate (buildInboundHCM);
# its `URI=` element must be spiffe://<td>/ns/aether-test/sa/<caller>. Paired
# with the source-side reading (the caller's twin moved, the other's did not),
# that is per-source selection end to end.
#
# ADMIN ACCESS is the one the multicluster harnesses already use: the proxy is
# hostNetwork, so the kind NODE container shares its netns and
# `docker exec <node> curl http://127.0.0.1:9901/...` reaches Envoy's
# loopback-only admin (both aether images are distroless). /clusters and /stats
# render on Envoy's main thread; a handful of reads per phase on a one-node test
# cluster is fine, and nothing here polls them in a tight loop.
#
# THE NEGATIVE CONTROL: QUIC_DNS_SANS=off. With no allow-list there is no
# "feature off" install left to go red, so the red half is the real failure
# the mesh-wide SPIRE prerequisite exists for — aether#957. `up` takes
# QUIC_DNS_SANS (default on). With QUIC_DNS_SANS=off:
#
#   * the workloads' ClusterSPIFFEID is applied WITHOUT dnsNameTemplates, so
#     every SVID is URI-SAN-only (everything else identical: SPIRE, the chart,
#     the twins, the HTTP/3 inbound);
#   * the node proxy's bootstrap gets one runtime layer setting
#     envoy.reloadable_features.quic_hostname_check_deferred_to_explicit_san_match
#     to false. The chart's proxy pin (#972) CARRIES envoyproxy/envoy#47740,
#     which skips the QUIC client's hostname check when a SAN matcher is
#     configured — so on this pin missing DNS SANs alone would NOT fail. The
#     guard restores the check a plain (#47740-less) pin performs, which is
#     exactly the world the SAN prerequisite protects (validated in #973's
#     TestQUICPathAGuardOffRestoresHostnameCheck). `up` asserts the override
#     is live on the node proxy (admin /runtime) so the red cannot be vacuous.
#
# `verify` then goes red at E2 with the twins PRESENT (E0/E1 pass): the QUIC
# handshake fails closed — 503s, the twin's host cx_connect_fail climbing, and
# the proxy log (quic:info, raised by `up`) saying `Cert chain verification
# failed: Leaf certificate doesn't match hostname: <port>.<sa>.<ns>.<domain>`.
# The red-then-green:
#
#   QUIC_DNS_SANS=off e2e/eastwest-quic.sh up && e2e/eastwest-quic.sh verify  # RED at E2
#   e2e/eastwest-quic.sh down
#   e2e/eastwest-quic.sh up && e2e/eastwest-quic.sh verify                    # GREEN
#
# Tear down between the two: already-issued SVIDs keep their SANs until they
# rotate, so re-running `up` over the red cluster would not be a clean green.
#
# E6 WITH THE GATE OFF (IDENTITY_GATE=off installs the chart with
# controller.webhook.identityGate.enabled=false) is a report, not a red: no init
# container is injected and the app's t=0 request races SPIRE's entry sync. On
# kind (2026-09-28) SPIRE issued the SVID 3.9-5.9 s after the agent's subscribe
# (two svids=0 updates first, the reference-cluster shape) and the first request
# STALLED 3.4-5.4 s waiting for the pod's client certificate before answering
# 200; with the gate on it answered in 12-14 ms after the gate held the pod
# 3.6-4.1 s. On the reference cluster the same window is ~7.5 s, which crosses the mesh
# cluster connect_timeout and becomes 503 UF (#1053). E6 prints the first
# request's status and latency either way and asserts only the gate-on
# contract (init container injected first, exited 0, first request 200 as the
# new ServiceAccount).
#
#   IDENTITY_GATE=off e2e/eastwest-quic.sh up && e2e/eastwest-quic.sh idgate  # the stall
#   e2e/eastwest-quic.sh up && e2e/eastwest-quic.sh idgate                    # GREEN
#
# E4c's pair count is seen red by asserting the pre-#1020 count against a
# #1020 agent: EWQ_EXPECT_TWINS=24 e2e/eastwest-quic.sh verify goes red at E4c
# with 6 twins present (the override exists only for that red run).
#
# Q3 LIMITATION, STATED RATHER THAN DISCOVERED. True QUIC connection migration
# (one connection surviving a client 5-tuple change) cannot be exercised here:
# the QUIC client is the node PROXY in the host netns, not the pod, so a pod's
# address change does not move any QUIC connection's path at all, and kind gives
# no way to rebind the proxy's UDP socket mid-connection. What E5 does prove is
# the property Q3 actually worries about for aether: the per-source binding is
# keyed by the source SPIFFE ID (the filter-state stamp on the new pod's own
# capture chain), not by the pod address — a pod coming back on a new IP is
# still served by ITS OWN twin with ITS OWN identity, never by a twin chosen
# from a stale address.
#
# SPIRE IS ON, AND MUST BE: QUIC mandates TLS, so NewInboundQUICListener returns
# nothing when cleartext and no twin is built without a served node identity.
# The workloads' ClusterSPIFFEID carries the two DNS SANs aether#957 requires
# (`<sa>.<ns>.<meshDomain>`, `*.<sa>.<ns>.<meshDomain>`) — without them (and
# without #47740) every HTTP/3 handshake fails the QUIC client's hostname check
# and E2 goes red with the twins present: the negative control above.
#
# E7 alone, against an `up` cluster: e2e/eastwest-quic.sh burst. Its red is
# main before #1086 on a CPU-starved agent (the reference cluster: 66-141 x 503 NC per loader
# start); on kind the agent is rarely slow enough to cross 2 s, so the leg also
# prints the build count and first-use latency the fix moves.
#
# Usage: e2e/eastwest-quic.sh {up|test|verify|idgate|burst|down}   (bare = up + verify)
#
# Prereqs: kind, docker, kubectl, helm, bazel (for the image build; CI sets
# EWQ_SKIP_BUILD=1 and pre-loads the images from the nightly build artifact).
# EWQ_LOCAL_PROXY=1 runs the proxy image already in the local Docker daemon
# (`make load-proxy-image`: <registry>/proxy:latest, e.g. a carried-patch build)
# instead of the chart's digest-pinned release; read at `up`. Never built here.
# EWQ_WORKER=1 (read at `up`) adds a kind worker node, pins every destination to
# it and every source to the control-plane node, so a destination's proxy can
# hot-restart while the sources' proxy keeps running: the cross-node shape of
# aether#1054 (e2e/eastwest-quic-hotrestart.sh HR_MODE=sparse). This suite's own
# `verify` reads destination-side counters from $NODE and needs the default
# single node.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# The pinned kind binary / node image / kubectl (#1251): one Kubernetes for every
# e2e surface, local and CI alike.
# shellcheck source=e2e/kind-version.sh
. "$REPO_ROOT/e2e/kind-version.sh"
# <registry>/<namespace> every aether image is tagged under, from the single
# setting in bazel/registry/registry.bzl (proposal 040) -- never a literal.
IMAGE_REGISTRY="$("$REPO_ROOT/scripts/image-registry.sh" prefix)"
CLUSTER="${EWQ_CLUSTER:-eastwest-quic}"
CTX="kind-$CLUSTER"
NODE="$CLUSTER-control-plane"
# The node the destinations run on: $NODE, or the worker with EWQ_WORKER=1.
DST_NODE="$NODE"
if [ "${EWQ_WORKER:-0}" = "1" ]; then DST_NODE="$CLUSTER-worker"; fi
NS="aether-system"
TEST_NS="aether-test"
MESH_DOMAIN="aether.internal"
TRUST_DOMAIN="aether.internal"
# on (default) = the workloads' SVIDs carry the aether#957 DNS SANs; off = the
# negative control (no DNS SANs + the #47740 guard off; see the header).
QUIC_DNS_SANS="${QUIC_DNS_SANS:-on}"
case "$QUIC_DNS_SANS" in
on | off) ;;
*)
	printf 'QUIC_DNS_SANS must be on or off, got %s\n' "$QUIC_DNS_SANS" >&2
	exit 1
	;;
esac
# The runtime guard envoyproxy/envoy#47740 registers (carried on the chart's
# proxy pin, #972); off restores the QUIC client's SNI-vs-DNS-SAN check.
QUIC_HOSTNAME_GUARD="envoy.reloadable_features.quic_hostname_check_deferred_to_explicit_san_match"
# on (default) = the chart's default egress identity gate (#1053); off = the
# same install with controller.webhook.identityGate.enabled=false (E6's red arm).
IDENTITY_GATE="${IDENTITY_GATE:-on}"
# The mesh VIP Service port every client dials through mesh DNS
# (meshconst.ProxyOutboundPort); the capture route claims "<fqdn>:18081".
OUTBOUND_PORT="18081"
# The application port every destination binds and registers.
APP_PORT="8080"
# Destinations. Every one of them is QUIC-eligible — nothing is listed.
QUIC_DSTS=(quic-a quic-b)
GAMMA_DSTS=(gamma-a gamma-b)
# Sources, each its own ServiceAccount (= its own SPIFFE ID).
SOURCES=(client-a client-b)
# Requests per measured batch.
BATCH=10
# E7 (aether#1086): first-use fan-out width and per-destination concurrency.
BURST_DSTS="${BURST_DSTS:-10}"
BURST_PER_DST="${BURST_PER_DST:-5}"
AGNHOST_IMAGE="registry.k8s.io/e2e-test-images/agnhost:2.53@sha256:99c6b4bb4a1e1df3f0b3752168c89358794d02258ebebc26bf21c29399011a85"
CURL_IMAGE="curlimages/curl:8.22.0@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777"
# The pinned Gateway API release (#1583): one for every e2e surface.
# shellcheck source=e2e/gateway-api-version.sh
. "$REPO_ROOT/e2e/gateway-api-version.sh"
# SPIRE >= 1.15.2 is required since proposal 036 (the SPIFFE Broker API); the
# same chart pins e2e/l4routes.sh uses.
SPIRE_CHART_VERSION="${SPIRE_CHART_VERSION:-0.30.2}"
SPIRE_CRDS_VERSION="${SPIRE_CRDS_VERSION:-0.6.1}"
SPIRE_CLASS="spire-mgmt-spire" # spire-controller-manager class (namespace-release)
IMAGES=(agent mesh-dns proxy-supervisor cni-install registrar controller uds-csi)
# The tag every aether image is loaded and installed under. `latest` is what
# `bazel run …:image_load` writes; a harness sharing the workstation with other
# builds re-tags its images and sets EWQ_IMAGE_TAG (with EWQ_SKIP_BUILD=1) so a
# concurrent build cannot swap the images under it between build and load.
IMAGE_TAG="${EWQ_IMAGE_TAG:-latest}"
if [ "${EWQ_LOCAL_PROXY:-0}" = "1" ]; then IMAGES+=(proxy); fi
# Extra `helm upgrade aether` arguments for a harness that sources this file
# (e2e/eastwest-quic-hotrestart.sh adds the OTLP collector and access logs).
# Empty for this suite's own runs.
EWQ_EXTRA_HELM_ARGS=("${EWQ_EXTRA_HELM_ARGS[@]+"${EWQ_EXTRA_HELM_ARGS[@]}"}")

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
die() {
	printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2
	dump_state
	exit 1
}

kc() { kubectl --context "$CTX" "$@"; }

# admin PATH — one read of the node proxy's admin (see ADMIN ACCESS above).
admin() { docker exec "$NODE" curl -s --max-time 5 "http://127.0.0.1:9901$1"; }

fqdn() { printf '%s.%s.%s' "$1" "$TEST_NS" "$MESH_DOMAIN"; }
spiffe_id() { printf 'spiffe://%s/ns/%s/sa/%s' "$TRUST_DOMAIN" "$TEST_NS" "$1"; }
# twin DST SRC — proxy.QUICClusterName: quic:<svc>.<ns>.<domain>@<ns>/<sa>.
twin() { printf 'quic:%s@%s/%s' "$(fqdn "$1")" "$TEST_NS" "$2"; }

# The negative-control state as the cluster actually has it — the first thing
# to read when E2 is red: the ClusterSPIFFEID's dnsNameTemplates and the
# #47740 guard as the node proxy's runtime reports it (absent = default true).
quic_sans_state() {
	printf 'ClusterSPIFFEID dnsNameTemplates: %s\n' \
		"$(kc get clusterspiffeid aether-workloads -o jsonpath='{.spec.dnsNameTemplates}' 2>/dev/null || echo '?')"
	printf '%s: %s\n' "$QUIC_HOSTNAME_GUARD" \
		"$(admin /runtime 2>/dev/null | grep -A8 "\"$QUIC_HOSTNAME_GUARD\"" | grep -o '"final_value": *"[^"]*"' || echo '(not overridden: default true)')"
}

# Every failure here is one of "the twin was never published", "the twin exists
# but the handshake fails" (#957's shape), "the route never selects the twin",
# or "the identity is wrong" — dump what separates the four (#590).
dump_state() {
	kubectl config get-contexts "$CTX" >/dev/null 2>&1 || return 0
	printf '\033[1;33m  -- pods --\033[0m\n' >&2
	kc get pods -A -o wide 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- DNS SANs + hostname guard (QUIC_DNS_SANS=%s) --\033[0m\n' "$QUIC_DNS_SANS" >&2
	quic_sans_state | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- agent log: east-west QUIC fan-out budget --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=agent --all-containers --tail=600 --prefix 2>&1 |
		grep -i "east-west QUIC" | tail -10 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- envoy: quic: clusters and their host rq_total/cx_connect_fail --\033[0m\n' >&2
	# SIGPIPE rule (#1121, e2e/README.md): this script runs under pipefail, so
	# no pipeline may end in a reader that exits before its writer is done
	# (`head`, `grep -q`/`-m`, `awk '...; exit'`): the writer dies of SIGPIPE
	# and the pipeline fails with 141 at a random point. Read to EOF instead.
	admin /clusters 2>/dev/null | grep -E '^quic:[^:]*::[0-9.]+:[0-9]+::(rq_total|cx_total|cx_connect_fail)::' |
		sed -n '1,60s/^/    /p' >&2 || true
	printf '\033[1;33m  -- envoy: HTTP/3 inbound listeners --\033[0m\n' >&2
	admin /listeners 2>/dev/null | grep '_h3::' | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- envoy: _h3 listener + http3 upstream counters --\033[0m\n' >&2
	{
		admin '/stats?filter=_h3' 2>/dev/null
		admin '/stats?filter=upstream_cx_http3_total' 2>/dev/null
	} | grep -E '(downstream_cx_total|downstream_rq_2xx|downstream_rq_5xx|upstream_cx_http3_total)' |
		sed -n '1,60s/^/    /p' >&2 || true
	printf '\033[1;33m  -- httproutes --\033[0m\n' >&2
	kc get httproutes -A -o yaml 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- proxy log: the aether#957 shape (hostname mismatch) --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=proxy -c proxy --tail=2000 --prefix 2>&1 |
		grep -i "match hostname" | tail -5 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- proxy log (quic / handshake / verify errors first) --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=proxy -c proxy --tail=400 --prefix 2>&1 |
		grep -iE 'quic|http3|handshake|verif|san' | tail -40 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- agent log (tail) --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=agent --all-containers --tail=80 --prefix 2>&1 |
		sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- mesh-dns --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=mesh-dns --tail=15 --prefix 2>&1 |
		sed 's/^/    /' >&2 || true
}

raise_inotify() {
	if sudo -n true 2>/dev/null; then
		sudo sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=524288 >/dev/null 2>&1 &&
			ok "inotify limits raised" || echo "  (could not raise inotify limits)"
	else
		echo "  (no passwordless sudo; run: sudo sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=524288)"
	fi
}

build_images() {
	# CI builds the images in its `build` job and sets EWQ_SKIP_BUILD=1.
	if [ "${EWQ_SKIP_BUILD:-0}" = "1" ]; then
		ok "skipping image build (EWQ_SKIP_BUILD=1; images pre-built)"
		return
	fi
	log "building + loading aether images"
	local t
	for t in //agent/cmd/agent //agent/cmd/mesh-dns //agent/cmd/proxy-supervisor \
		//cni/cmd/cni-install //registrar/cmd/registrar //controller/cmd/controller //agent/cmd/uds-csi; do
		(cd "$REPO_ROOT" && bazel run "$t:image_load" >/dev/null 2>&1) || die "image build failed for $t"
	done
	ok "images built"
}

create_cluster() {
	if kind get clusters 2>/dev/null | grep -cx "$CLUSTER" >/dev/null; then
		ok "kind cluster '$CLUSTER' already exists"
		return
	fi
	log "creating kind cluster '$CLUSTER'"
	local cfg
	cfg="$(mktemp)"
	sed -e "s/CLUSTER_NAME/$CLUSTER/g" \
		-e "s#POD_SUBNET#10.30.0.0/16#g" \
		-e "s#SVC_SUBNET#10.130.0.0/16#g" \
		"$REPO_ROOT/e2e/kind-cluster.yaml" >"$cfg"
	if [ "${EWQ_WORKER:-0}" = "1" ]; then
		printf '  - role: worker\n    labels:\n      topology.kubernetes.io/region: local\n      topology.kubernetes.io/zone: %s\n' "$CLUSTER" >>"$cfg"
	fi
	kind_require_binary || die "kind binary does not match e2e/kind-version.sh (see above)"
	kind create cluster --image "$KIND_NODE_IMAGE" --config "$cfg" --wait 60s >/dev/null
	rm -f "$cfg"
	if [ "${EWQ_WORKER:-0}" = "1" ]; then
		# kind taints the control plane once a worker exists; the sources run there.
		kc taint nodes "$NODE" node-role.kubernetes.io/control-plane:NoSchedule- >/dev/null 2>&1 || true
	fi
	ok "cluster '$CLUSTER' ready"
}

load_images() {
	log "loading images into '$CLUSTER'"
	local img
	for img in "${IMAGES[@]}"; do
		kind load docker-image "${IMAGE_REGISTRY}/${img}:${IMAGE_TAG}" --name "$CLUSTER" >/dev/null 2>&1 ||
			die "could not load ${IMAGE_REGISTRY}/${img}:${IMAGE_TAG} into kind (was it built?)"
	done
	ok "images loaded"
}

# HTTPRoute (standard channel) goes in BEFORE the agent starts: the GAMMA
# reconciler is gated on a RESTMapper lookup at manager-setup time, so a CRD
# applied after the helm install would need an agent restart (the same ordering
# constraint e2e/l4routes.sh documents for the L4 types).
install_gwapi_crds() {
	log "installing Gateway API CRDs ($GWAPI_VERSION, standard channel)"
	kc apply --server-side --force-conflicts -f \
		"https://github.com/kubernetes-sigs/gateway-api/releases/download/${GWAPI_VERSION}/standard-install.yaml" >/dev/null ||
		die "Gateway API CRD install failed"
	kc wait --for=condition=Established crd/httproutes.gateway.networking.k8s.io --timeout=60s >/dev/null ||
		die "the HTTPRoute CRD never became Established"
	ok "Gateway API CRDs installed"
}

# SPIRE, single cluster, self-signed — verbatim from e2e/l4routes.sh, including
# the dnsNameTemplates aether#957 requires (see the header); QUIC_DNS_SANS=off
# leaves them out (the negative control).
install_spire() {
	log "installing SPIRE (trust domain $TRUST_DOMAIN)"
	helm --kube-context "$CTX" repo add spiffe https://spiffe.github.io/helm-charts-hardened/ >/dev/null 2>&1 || true
	helm --kube-context "$CTX" repo update >/dev/null 2>&1 || true
	kc create ns spire-mgmt >/dev/null 2>&1 || true
	helm --kube-context "$CTX" upgrade --install spire-crds spiffe/spire-crds \
		-n spire-mgmt --version "$SPIRE_CRDS_VERSION" --wait --timeout 3m >/dev/null ||
		die "spire-crds install failed"
	# accessPolicy=permissive EXPLICITLY (never `auto`, which fails closed for a
	# KubernetesObjectReference — see e2e/l4routes.sh).
	helm --kube-context "$CTX" upgrade --install spire spiffe/spire \
		-n spire-mgmt --version "$SPIRE_CHART_VERSION" \
		--set global.spire.trustDomain="$TRUST_DOMAIN" \
		--set global.spire.clusterName="$CLUSTER" \
		--set "spiffe-oidc-discovery-provider.enabled=false" \
		--set "spire-agent.sockets.broker.enabled=true" \
		--set "spire-agent.sockets.broker.mountOnHost=true" \
		--set "spire-agent.brokerAPI.brokers.aether-agent.enabled=true" \
		--set "spire-agent.brokerAPI.brokers.aether-agent.idTemplate=spiffe://$TRUST_DOMAIN/ns/$NS/sa/aether-agent" \
		--set "spire-agent.brokerAPI.brokers.aether-agent.allowedReferenceTypes[0].typeURL=type.googleapis.com/spiffe.broker.KubernetesObjectReference" \
		--set "spire-agent.brokerAPI.brokers.aether-agent.allowedReferenceTypes[0].allowOverTCP=false" \
		--set "spire-agent.workloadAttestors.k8s.brokerAPI.accessPolicy=permissive" \
		--set "spire-agent.workloadAttestors.k8s.brokerAPI.brokers.aether-agent.enabled=true" \
		--wait --timeout 8m >/dev/null || die "SPIRE install failed"
	local sans=""
	if [ "$QUIC_DNS_SANS" = on ]; then
		sans="  dnsNameTemplates:
    - \"{{ .PodSpec.ServiceAccountName }}.{{ .PodMeta.Namespace }}.$MESH_DOMAIN\"
    - \"*.{{ .PodSpec.ServiceAccountName }}.{{ .PodMeta.Namespace }}.$MESH_DOMAIN\""
	fi
	kc apply -f - >/dev/null <<YAML || die "ClusterSPIFFEID apply failed"
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: aether-workloads
spec:
  className: $SPIRE_CLASS
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  # The workload's mesh authority and every "<port>." prefix of it, as DNS SANs:
  # Envoy's QUIC client verifies the leaf against the SNI as a hostname after
  # the SPIFFE SAN pin (aether#957), and the east-west QUIC SNI is
  # "<port>.<sa>.<ns>.<mesh domain>" (proxy.QUICServerName). Identity is still
  # the URI SAN; these only satisfy the QUIC client's hostname check.
  # QUIC_DNS_SANS=$QUIC_DNS_SANS
$sans
  podSelector:
    matchLabels:
      aether.io/managed: "true"
YAML
	ok "SPIRE up (DNS SANs: $QUIC_DNS_SANS)"
}

# Render the chart placeholders into a temp copy so the source tree stays clean.
chart_dir() {
	local out
	out="$(mktemp -d)"
	cp -r "$REPO_ROOT/charts" "$out/"
	sed -i -e 's/{GIT_COMMIT}/e2e/' -e 's/{STABLE_GIT_VERSION}/0.0.0-e2e/' \
		"$out/charts/crds/Chart.yaml" "$out/charts/aether/Chart.yaml"
	if [ "$QUIC_DNS_SANS" = off ]; then
		# The negative control's runtime layer, spliced into the COPY's proxy
		# bootstrap as a top-level key just before dynamic_resources (the chart
		# has no runtime value, and this must never be one). Checked below so a
		# template change cannot make the splice silently miss.
		local cm="$out/charts/aether/templates/agent-proxy-configmap.yaml"
		sed -i "/^    dynamic_resources:\$/i\\
    layered_runtime:\\
      layers:\\
        - name: e2e-quic-negative-control\\
          static_layer:\\
            $QUIC_HOSTNAME_GUARD: false\\
" "$cm"
		grep -q "^            $QUIC_HOSTNAME_GUARD: false\$" "$cm" ||
			die "could not splice the $QUIC_HOSTNAME_GUARD override into $cm"
	fi
	echo "$out/charts"
}

install_aether() {
	local charts
	charts="$(chart_dir)"
	img() { echo "--set $1.image.repository=${IMAGE_REGISTRY}/$2 --set $1.image.tag=${IMAGE_TAG} --set $1.image.digest= --set $1.image.pullPolicy=Never"; }
	local gate=()
	case "$IDENTITY_GATE" in
	on) ;;
	off) gate=(--set controller.webhook.identityGate.enabled=false) ;;
	*) die "IDENTITY_GATE must be 'on' or 'off', got '$IDENTITY_GATE'" ;;
	esac

	log "installing the aether CRDs"
	helm --kube-context "$CTX" upgrade --install aether-crds "$charts/crds" \
		-n "$NS" --create-namespace --wait --timeout 2m >/dev/null || die "crds chart install failed"

	# Everything except spire (and the identity-gate / harness overrides) is
	# chart default (kubernetes registry backend, capture + mesh DNS, GAMMA on,
	# east-west QUIC unconditional). Never --reuse-values: the overrides must be
	# exactly what THIS invocation says, so re-runs of `up` really toggle them.
	log "installing aether (SPIRE ON; QUIC_DNS_SANS=$QUIC_DNS_SANS; IDENTITY_GATE=$IDENTITY_GATE)"
	# shellcheck disable=SC2046
	helm --kube-context "$CTX" upgrade --install aether "$charts/aether" \
		-n "$NS" --create-namespace \
		--set namespace.create=false \
		--set "meshDomain=$MESH_DOMAIN" \
		--set spire.enabled=true \
		--set edge.enabled=false \
		"${gate[@]+"${gate[@]}"}" \
		--set "debug=$([ "${EWQ_DEBUG:-0}" = "1" ] && echo true || echo false)" \
		"${EWQ_EXTRA_HELM_ARGS[@]+"${EWQ_EXTRA_HELM_ARGS[@]}"}" \
		$(img agent agent) $(img agent.meshDnsDaemon mesh-dns) \
		$(img proxy.supervisor proxy-supervisor) $(img cniInstall cni-install) \
		$(img registrar registrar) $(img controller controller) $(img udsCsi uds-csi) \
		--set proxy.image.pullPolicy=IfNotPresent \
		$([ "${EWQ_LOCAL_PROXY:-0}" = "1" ] && img proxy proxy) \
		--timeout 6m >/dev/null || die "aether install failed"
	rm -rf "$(dirname "$charts")"

	kc -n "$NS" rollout status ds/aether-agent --timeout=300s >/dev/null || die "the agent DaemonSet never became Ready"
	kc -n "$NS" rollout status ds/aether-mesh-dns --timeout=180s >/dev/null || die "the mesh-DNS DaemonSet never became Ready"
	kc -n "$NS" rollout status deploy/aether-registrar --timeout=180s >/dev/null || die "the registrar never became Ready"
	kc -n "$NS" rollout status deploy/aether-controller --timeout=180s >/dev/null || die "the controller never became Ready"
	ok "aether up"
	if [ "$QUIC_DNS_SANS" = off ]; then
		arm_negative_control
	fi
}

# arm_negative_control — prove the #47740 guard override is LIVE on the node
# proxy (never a vacuous red) and raise the loggers that print the handshake
# verdict: both verify lines come from quiche's tls_handshaker at quic:info /
# pool:debug (#957), which the default level hides.
arm_negative_control() {
	kc -n "$NS" rollout status ds/aether-proxy --timeout=300s >/dev/null || die "the proxy DaemonSet never became Ready"
	local deadline=$((SECONDS + 120))
	until admin /runtime 2>/dev/null | grep -A8 "\"$QUIC_HOSTNAME_GUARD\"" | grep -c '"final_value": *"false"' >/dev/null; do
		[ "$SECONDS" -lt "$deadline" ] ||
			die "negative control: the node proxy's /runtime never reported $QUIC_HOSTNAME_GUARD=false — a missing-SAN run would then be GREEN on the #47740 pin, not the aether#957 red"
		sleep 5
	done
	local logger
	for logger in quic=info pool=debug; do
		docker exec "$NODE" curl -sf --max-time 5 -X POST "http://127.0.0.1:9901/logging?$logger" >/dev/null ||
			die "negative control: could not set $logger on the node proxy's logging"
	done
	ok "negative control armed: SVIDs without DNS SANs, $QUIC_HOSTNAME_GUARD=false on the node proxy, quic:info pool:debug"
}

# node_pin NODE — a pod-spec nodeSelector line pinning to NODE with
# EWQ_WORKER=1, and an empty line otherwise (one node: nothing to pin).
node_pin() {
	if [ "${EWQ_WORKER:-0}" = "1" ]; then
		printf 'nodeSelector: {kubernetes.io/hostname: %s}' "$1"
	fi
}

# A destination: agnhost netexec on :$APP_PORT, its own ServiceAccount (= its
# mesh service name). No Kubernetes Service is written by hand: the registrar
# generates the mesh VIP Service and skips a name another Service owns.
deploy_destination() {
	local name="$1"
	kc apply -f - >/dev/null <<YAML || die "destination $name apply failed"
apiVersion: v1
kind: ServiceAccount
metadata: {name: $name, namespace: $TEST_NS}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: $name, namespace: $TEST_NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: $name}}
  template:
    metadata:
      labels: {app: $name, aether.io/managed: "true"}
      annotations:
        endpoint.aether.io/port: "$APP_PORT"
    spec:
      serviceAccountName: $name
      $(node_pin "$DST_NODE")
      containers:
        - name: app
          image: $AGNHOST_IMAGE
          args: ["netexec", "--http-port=$APP_PORT"]
          ports: [{containerPort: $APP_PORT}]
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: ["ALL"]}
YAML
}

deploy_source() {
	local name="$1" ups="" d
	for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}"; do
		ups="${ups:+$ups,}$d.$TEST_NS"
	done
	kc apply -f - >/dev/null <<YAML || die "source $name apply failed"
apiVersion: v1
kind: ServiceAccount
metadata: {name: $name, namespace: $TEST_NS}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: $name, namespace: $TEST_NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: $name}}
  template:
    metadata:
      labels: {app: $name, aether.io/managed: "true"}
      annotations:
        config.aether.io/upstreams: "$ups"
    spec:
      serviceAccountName: $name
      $(node_pin "$NODE")
      containers:
        - name: curl
          image: $CURL_IMAGE
          command: ["sleep", "infinity"]
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: ["ALL"]}
YAML
}

deploy_workloads() {
	log "deploying destinations (${QUIC_DSTS[*]} ${GAMMA_DSTS[*]}) and sources (${SOURCES[*]})"
	kc create ns "$TEST_NS" >/dev/null 2>&1 || true
	local d
	for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}"; do deploy_destination "$d"; done
	for d in "${SOURCES[@]}"; do deploy_source "$d"; done
	for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}" "${SOURCES[@]}"; do
		kc -n "$TEST_NS" rollout status "deploy/$d" --timeout=180s >/dev/null ||
			die "workload '$d' never became Ready"
	done
	ok "workloads deployed"
}

# --- readings ----------------------------------------------------------------

# pod_of APP — the one Running, not-terminating pod of a Deployment. Explicit
# rather than `exec deploy/...`, which can pick a Terminating pod during E5.
pod_of() {
	kc -n "$TEST_NS" get pod -l "app=$1" --no-headers \
		-o custom-columns=N:.metadata.name,D:.metadata.deletionTimestamp,P:.status.phase 2>/dev/null |
		awk '!f && $2 == "<none>" && $3 == "Running" { print $1; f = 1 }'
}

# host_rq DUMP CLUSTER — sum of rq_total over CLUSTER's host rows in a /clusters
# dump (`<cluster>::<ip:port>::rq_total::N`). Exact cluster-name match.
host_rq() {
	printf '%s\n' "$1" | awk -F'::' -v c="$2" '$1 == c && $3 == "rq_total" { s += $4 } END { print s + 0 }'
}

# h2_rq DUMP DST — the h2 family of DST: the default cluster "<fqdn>" plus any
# "<fqdn>:<port>" alias/per-port cluster. Never a quic: twin (different prefix).
h2_rq() {
	printf '%s\n' "$1" | awk -F'::' -v c="$(fqdn "$2")" \
		'($1 == c || index($1, c ":") == 1) && $3 == "rq_total" { s += $4 } END { print s + 0 }'
}

# has_cluster DUMP CLUSTER — 0 iff CLUSTER appears in a /clusters dump.
has_cluster() { printf '%s\n' "$1" | awk -F'::' -v c="$2" '$1 == c { f = 1 } END { exit !f }'; }

# h3_stat POD / h3_rq POD — the pod's HTTP/3 inbound 2xx count; EMPTY when the stat does not
# exist, so a caller can refuse to read an absent stat as zero.
h3_stat() { printf 'listener.inbound_%s_%s_h3.http.inbound.downstream_rq_2xx' "$TEST_NS" "$1"; }
h3_rq() {
	local name
	name="$(h3_stat "$1")"
	admin "/stats?filter=inbound_${TEST_NS}_$1_h3" 2>/dev/null | awk -v n="$name:" '!f && $1 == n { print $2; f = 1 }'
}

# req_batch SRC DST PATH N — N requests from SRC's pod to DST's mesh name, one
# exec; prints N lines "<http_code> <first body line or EMPTY>".
req_batch() {
	local src="$1" dst="$2" path="$3" n="$4" pod
	pod="$(pod_of "$src")"
	[ -n "$pod" ] || {
		printf '000 NO_SOURCE_POD\n'
		return
	}
	# shellcheck disable=SC2016  # evaluated by the POD's shell
	kc -n "$TEST_NS" exec "$pod" -c curl -- sh -c '
		url="$1"; n="$2"; i=1
		while [ "$i" -le "$n" ]; do
			out=$(curl -s --max-time 5 -w "\n%{http_code}" "$url" 2>/dev/null)
			code=$(printf "%s\n" "$out" | tail -n 1)
			body=$(printf "%s\n" "$out" | sed "\$d" | head -n 1 | tr -d "\r")
			printf "%s %s\n" "${code:-000}" "${body:-EMPTY}"
			i=$((i + 1))
		done
	' sh "http://$(fqdn "$dst"):$OUTBOUND_PORT$path" "$n" 2>/dev/null || true
}

xfcc_batch() { req_batch "$1" "$2" "/header?key=X-Forwarded-Client-Cert" "$3"; }

# ids_ok REPLIES SRC — 0 iff every reply is a 200 whose XFCC URI element is
# exactly SRC's SPIFFE ID. SANITIZE_SET leaves exactly one element, whose `By=`
# is the destination and whose `URI=` is the verified caller.
ids_ok() {
	printf '%s\n' "$1" | awk -v want="$(spiffe_id "$2")" '
		{ n++; code = $1; rest = substr($0, index($0, " ") + 1)
		  uri = ""; if (match(rest, /;URI=[^;,"]*/)) uri = substr(rest, RSTART + 5, RLENGTH - 5)
		  if (code == "200" && uri == want) good++ }
		END { exit !(n > 0 && good == n) }'
}

# summarize REPLIES — "200:URI=spiffe://…/sa/x ×N ..." for failure messages.
summarize() {
	printf '%s\n' "$1" | awk '
		{ rest = substr($0, index($0, " ") + 1); uri = rest
		  if (match(rest, /;URI=[^;,"]*/)) uri = substr(rest, RSTART + 1, RLENGTH - 1)
		  c[$1 ":" uri]++ }
		END { for (k in c) printf "[%s x%d] ", k, c[k] }'
}

# --- E0 / E1 -----------------------------------------------------------------

verify_preflight() {
	log "E0 preflight: node proxy admin reachable; every destination pod has its HTTP/3 inbound and a readable h3 counter"
	admin /ready >/dev/null 2>&1 || die "E0: the node proxy admin is not reachable via 'docker exec $NODE curl 127.0.0.1:9901' — is this the cluster '$0 up' built?"
	# Converge, like every other phase: `up` returns when the pods are Ready, but
	# the proxy acks each pod's LDS push a little later (the 2026-09-27 nightly
	# died here on a slow runner while the agent still logged "envoy did not ack
	# listener" for every pod; the re-run passed). A listener that never shows up
	# within the deadline is still the real failure this phase exists to catch.
	local deadline=$((SECONDS + 180)) listeners d pod v missing
	while true; do
		listeners="$(admin /listeners)"
		missing=""
		for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}"; do
			pod="$(pod_of "$d")"
			[ -n "$pod" ] || die "E0: no Running pod for destination $d"
			printf '%s\n' "$listeners" | awk -F'::' -v l="inbound_${TEST_NS}_${pod}_h3" '$1 == l { f = 1 } END { exit !f }' || missing="$missing inbound_${TEST_NS}_${pod}_h3"
		done
		[ -z "$missing" ] && break
		[ "$SECONDS" -lt "$deadline" ] ||
			die "E0: HTTP/3 inbound listener(s) still absent after 180s:$missing — #953's inbound is unconditional with SPIRE on, so this is the inbound (or the pod's SVID) missing, not timing"
		sleep 5
	done
	for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}"; do
		pod="$(pod_of "$d")"
		v="$(h3_rq "$pod")"
		[ -n "$v" ] ||
			die "E0: $(h3_stat "$pod") is not in /stats — every 'h3 delta == 0' below would be reading an absent stat; fix the stat name before trusting this suite (#853)"
	done
	ok "HTTP/3 inbound listeners + counters present for every destination"
}

# quic_twins DUMP — the distinct quic: cluster names in a /clusters dump, sorted.
# E6's brand-new idgate-<ts> and E7's burst-<ts> ServiceAccounts are left out: since #979 their
# first request is QUIC-eligible like any other, and their pods (and so their
# pairs) outlive the run, so a re-run of verify would count them as strays.
quic_twins() {
	printf '%s\n' "$1" | awk -F'::' -v g="@$TEST_NS/idgate-" -v b="@$TEST_NS/burst-" \
		'index($1, "quic:") == 1 && index($1, g) == 0 && index($1, b) == 0 { print $1 }' | sort -u
}

# driven_pairs — the twins E2 + E4b dial over a SELECTING route, sorted: every
# source x (each QUIC destination + the GAMMA parent's single-backend rule).
# E4's weighted split never selects, so gamma-b is never a pair.
driven_pairs() {
	local s d
	for s in "${SOURCES[@]}"; do
		for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[0]}"; do
			twin "$d" "$s"
			printf '\n'
		done
	done | sort -u
}

verify_fanout() {
	log "E1 fan-out: twins are demand-scoped (aether#1020) — before any request, none outside the pairs this suite drives; nothing is listed (#979)"
	local dump present strays
	dump="$(admin /clusters)"
	present="$(quic_twins "$dump")"
	# A re-run of verify finds the previous run's pairs (they are persisted
	# in the agent's observed set, by design); anything OUTSIDE the driven set
	# was built for a pair that never dialled.
	strays="$(comm -23 <(printf '%s\n' "$present" | sed '/^$/d') <(driven_pairs))"
	[ -z "$strays" ] ||
		die "E1: quic: twins exist for pairs that never dialled: $strays — the agent is building twins up front (the pre-#1020 fan-out: every local SA x QUIC destination)"
	ok "$(printf '%s\n' "$present" | sed '/^$/d' | wc -l | tr -d ' ') quic: twins before any request in this run (0 on a fresh cluster); none for a pair that never dialled"
}

# verify_pairs — E4c: the node's twin set IS the set of pairs driven so far.
verify_pairs() {
	local want present n expect deadline=$((SECONDS + 60))
	want="$(driven_pairs)"
	expect="${EWQ_EXPECT_TWINS:-$(printf '%s\n' "$want" | wc -l | tr -d ' ')}"
	log "E4c pairs: the node's quic: cluster count must equal the $expect (source, destination) pairs E2 + E4b drove"
	while true; do
		present="$(quic_twins "$(admin /clusters)")"
		n="$(printf '%s\n' "$present" | sed '/^$/d' | wc -l | tr -d ' ')"
		[ "$n" -eq "$expect" ] && [ "$present" = "$want" ] && break
		[ "$SECONDS" -lt "$deadline" ] ||
			die "E4c: $n quic: clusters on the node, want $expect = the driven pairs [$(printf '%s\n' "$want" | tr '\n' ' ')]; present: [$(printf '%s\n' "$present" | tr '\n' ' ')]. More than the pairs = a twin built for a pair that never dialled (the pre-#1020 SA x destination fan-out); fewer = a pair that dialled lost its twin"
		sleep 5
	done
	ok "E4c: $n quic: clusters = $expect driven pairs, exactly [$(printf '%s\n' "$present" | tr '\n' ' ')]"
}

# --- E2 / E5: per-source HTTP/3 with the caller's own identity ---------------

# assert_quic PHASE SRC DST — converge, then one measured batch of $BATCH.
assert_quic() {
	local phase="$1" src="$2" dst="$3" other pod replies deadline
	other="${SOURCES[0]}"
	[ "$other" = "$src" ] && other="${SOURCES[1]}"
	pod="$(pod_of "$dst")"

	# Convergence: the caller's twin must be carrying ITS requests with ITS
	# identity before anything is measured (route push + twin warm-up).
	deadline=$((SECONDS + 180))
	local d0 d1
	while true; do
		d0="$(admin /clusters)"
		replies="$(xfcc_batch "$src" "$dst" 3)"
		d1="$(admin /clusters)"
		if ids_ok "$replies" "$src" &&
			[ "$(($(host_rq "$d1" "$(twin "$dst" "$src")") - $(host_rq "$d0" "$(twin "$dst" "$src")")))" -ge 3 ]; then
			break
		fi
		[ "$SECONDS" -lt "$deadline" ] ||
			die "$phase: $src -> $dst never converged onto $(twin "$dst" "$src"): replies $(summarize "$replies"); twin rq_total $(host_rq "$d0" "$(twin "$dst" "$src")") -> $(host_rq "$d1" "$(twin "$dst" "$src")"). 200s with the right URI but a still twin = the route never selected the twin (h2 fallback); 503s with a moving twin = the QUIC handshake fails (the #957 DNS-SAN shape — the EXPECTED red under QUIC_DNS_SANS=off; check 'match hostname' in the proxy log and the DNS SANs + hostname guard state in the dump)"
		sleep 5
	done

	local c0 c1 h0 h1 tw ot h2 h3
	c0="$(admin /clusters)"
	h0="$(h3_rq "$pod")"
	replies="$(xfcc_batch "$src" "$dst" "$BATCH")"
	c1="$(admin /clusters)"
	h1="$(h3_rq "$pod")"
	[ -n "$h0" ] && [ -n "$h1" ] || die "$phase: $(h3_stat "$pod") vanished mid-phase"

	ids_ok "$replies" "$src" ||
		die "$phase: $src -> $dst: expected $BATCH x 200 with URI=$(spiffe_id "$src"), got $(summarize "$replies") — a URI naming $other means the destination saw the OTHER source's certificate (the per-source selection picked the wrong twin)"
	tw=$(($(host_rq "$c1" "$(twin "$dst" "$src")") - $(host_rq "$c0" "$(twin "$dst" "$src")")))
	ot=$(($(host_rq "$c1" "$(twin "$dst" "$other")") - $(host_rq "$c0" "$(twin "$dst" "$other")")))
	h2=$(($(h2_rq "$c1" "$dst") - $(h2_rq "$c0" "$dst")))
	h3=$((h1 - h0))
	[ "$tw" -ge "$BATCH" ] ||
		die "$phase: $src -> $dst: its twin $(twin "$dst" "$src") carried $tw/$BATCH requests"
	[ "$ot" -eq 0 ] ||
		die "$phase: $src -> $dst: the OTHER source's twin $(twin "$dst" "$other") carried $ot requests while only $src was calling — selection is not keyed on the source identity"
	[ "$h2" -eq 0 ] ||
		die "$phase: $src -> $dst: the h2 cluster $(fqdn "$dst") carried $h2 requests — some requests took on_no_match (no identity stamp, or no arm for $(spiffe_id "$src"))"
	[ "$h3" -ge "$BATCH" ] ||
		die "$phase: $src -> $dst: $(h3_stat "$pod") moved by $h3 (< $BATCH) — the requests did not arrive over the destination's HTTP/3 inbound"
	ok "$phase: $src -> $dst: $BATCH/$BATCH x 200, XFCC URI=$(spiffe_id "$src"); own twin +$tw, other twin +0, h2 +0, dest h3 inbound +$h3"
}

verify_quic() {
	log "E2 per-source HTTP/3: ${SOURCES[*]} x ${QUIC_DSTS[*]}"
	local s d
	for s in "${SOURCES[@]}"; do
		for d in "${QUIC_DSTS[@]}"; do
			assert_quic E2 "$s" "$d"
		done
	done
}

# --- E4 helper: a destination that must stay on h2 ---------------------------

# assert_h2 PHASE SRC DST... — one measured batch from SRC to the FIRST DST;
# every listed DST (the GAMMA split's backends) must show no quic: twin traffic
# and no HTTP/3 inbound traffic, and together at least $BATCH h2 requests.
assert_h2() {
	local phase="$1" src="$2" dst="$3"
	shift 2
	local d c0 c1 replies pod
	declare -A h0=()
	c0="$(admin /clusters)"
	for d in "$@"; do
		pod="$(pod_of "$d")"
		h0[$d]="$(h3_rq "$pod")"
		[ -n "${h0[$d]}" ] || die "$phase: $(h3_stat "$pod") is absent — refusing to read it as zero"
	done
	replies="$(xfcc_batch "$src" "$dst" "$BATCH")"
	c1="$(admin /clusters)"
	ids_ok "$replies" "$src" ||
		die "$phase: $src -> $dst: expected $BATCH x 200 with URI=$(spiffe_id "$src") over h2, got $(summarize "$replies")"
	local h2=0 s tw h3 v
	for d in "$@"; do
		h2=$((h2 + $(h2_rq "$c1" "$d") - $(h2_rq "$c0" "$d")))
		for s in "${SOURCES[@]}"; do
			tw=$(($(host_rq "$c1" "$(twin "$d" "$s")") - $(host_rq "$c0" "$(twin "$d" "$s")")))
			[ "$tw" -eq 0 ] || die "$phase: $src -> $dst: quic: twin $(twin "$d" "$s") carried $tw requests — this destination must stay on h2"
		done
		v="$(h3_rq "$(pod_of "$d")")"
		h3=$((v - ${h0[$d]}))
		[ "$h3" -eq 0 ] || die "$phase: $src -> $dst: $d's HTTP/3 inbound moved by $h3 — this destination must stay on h2"
	done
	[ "$h2" -ge "$BATCH" ] ||
		die "$phase: $src -> $dst: the h2 cluster(s) of [$*] carried only $h2/$BATCH requests — the 200s came from somewhere this suite is not reading"
	ok "$phase: $src -> $dst: $BATCH/$BATCH x 200, XFCC URI=$(spiffe_id "$src"); h2 +$h2, every quic: twin +0, HTTP/3 inbound +0 on [$*]"
}

# --- E4: GAMMA-routed destination stays on h2 --------------------------------

# A weighted canary parented to gamma-a (both backends QUIC-eligible). #956
# rewrites only routes whose action is `cluster: <this service's h2 cluster>`;
# a WeightedClusters action is left alone because the matcher plugin's action
# names ONE cluster (#961) -- so a GAMMA split stays h2. E4b below covers the
# other GAMMA shape: a single-backendRef rule to the parent renders as
# `cluster:` and IS selected (rides QUIC), which is the documented behaviour.
apply_gamma_route() {
	kc apply -f - >/dev/null <<YAML || die "HTTPRoute apply failed"
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: gamma-canary, namespace: $TEST_NS}
spec:
  parentRefs:
    - {group: "", kind: Service, name: ${GAMMA_DSTS[0]}}
  rules:
    - backendRefs:
        - {group: "", kind: Service, name: ${GAMMA_DSTS[0]}, port: $APP_PORT, weight: 50}
        - {group: "", kind: Service, name: ${GAMMA_DSTS[1]}, port: $APP_PORT, weight: 50}
YAML
}

# gamma_split_up PHASE — apply the weighted canary and wait until it is live.
# Converge on the DATA, not the status: the route is live once a request to the
# parent is answered by the second backend's pod (agnhost /hostname). Since #979
# this split is the one h2-by-design route, so e2e/eastwest-quic-hotrestart.sh
# uses it as its h2 control too.
gamma_split_up() {
	local phase="$1" b_pod deadline=$((SECONDS + 180)) replies
	apply_gamma_route
	b_pod="$(pod_of "${GAMMA_DSTS[1]}")"
	while true; do
		replies="$(req_batch "${SOURCES[0]}" "${GAMMA_DSTS[0]}" /hostname 20)"
		printf '%s\n' "$replies" | awk -v p="$b_pod" '$1 == "200" && $2 == p { f = 1 } END { exit !f }' && break
		[ "$SECONDS" -lt "$deadline" ] ||
			die "$phase: the HTTPRoute never took effect — no request to ${GAMMA_DSTS[0]} reached ${GAMMA_DSTS[1]} ($b_pod) in 180s: $(summarize "$replies")"
		sleep 5
	done
	ok "the weighted route is live (requests to ${GAMMA_DSTS[0]} reach $b_pod)"
}

verify_gamma() {
	log "E4 GAMMA: weighted HTTPRoute on ${GAMMA_DSTS[0]} (${GAMMA_DSTS[0]} 50 / ${GAMMA_DSTS[1]} 50, both QUIC-eligible) — stays h2"
	gamma_split_up E4
	local s
	for s in "${SOURCES[@]}"; do
		assert_h2 E4 "$s" "${GAMMA_DSTS[@]}"
	done
	kc -n "$TEST_NS" delete httproute gamma-canary --ignore-not-found >/dev/null || die "could not delete the HTTPRoute"
	ok "HTTPRoute removed (verify is re-runnable)"
}

# --- E4b: a single-backendRef GAMMA rule to the parent rides QUIC (#961) -------

# The rule adds a request header so its liveness is observable on the DATA
# (agnhost /header echoes it): the backend is the parent itself, so /hostname
# cannot tell "the rule took effect" from "the default route answered".
apply_gamma_single_route() {
	kc apply -f - >/dev/null <<YAML || die "HTTPRoute apply failed"
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: gamma-single, namespace: $TEST_NS}
spec:
  parentRefs:
    - {group: "", kind: Service, name: ${GAMMA_DSTS[0]}}
  rules:
    - filters:
        - type: RequestHeaderModifier
          requestHeaderModifier:
            add:
              - {name: x-gamma-single, value: "1"}
      backendRefs:
        - {group: "", kind: Service, name: ${GAMMA_DSTS[0]}, port: $APP_PORT}
YAML
}

verify_gamma_single() {
	log "E4b GAMMA: single-backendRef HTTPRoute on ${GAMMA_DSTS[0]} to itself — selected, rides HTTP/3 (#961)"
	apply_gamma_single_route
	local deadline=$((SECONDS + 180)) replies
	while true; do
		replies="$(req_batch "${SOURCES[0]}" "${GAMMA_DSTS[0]}" "/header?key=X-Gamma-Single" 5)"
		printf '%s\n' "$replies" | awk '$1 == "200" && $2 == "1" { f = 1 } END { exit !f }' && break
		[ "$SECONDS" -lt "$deadline" ] ||
			die "E4b: the HTTPRoute never took effect — no request to ${GAMMA_DSTS[0]} carried x-gamma-single in 180s: $(summarize "$replies")"
		sleep 5
	done
	ok "the single-backend route is live (x-gamma-single reaches ${GAMMA_DSTS[0]})"
	local s
	for s in "${SOURCES[@]}"; do
		assert_quic E4b "$s" "${GAMMA_DSTS[0]}"
	done
	kc -n "$TEST_NS" delete httproute gamma-single --ignore-not-found >/dev/null || die "could not delete the HTTPRoute"
	ok "HTTPRoute removed (verify is re-runnable)"
}

# --- E5: Q3 — identity survives a client address change ----------------------

verify_q3() {
	local src="${SOURCES[0]}" old_pod old_ip new_pod new_ip deadline
	log "E5 Q3: delete $src's pod so it returns on a NEW IP — still its own identity, still its own twin, over HTTP/3"
	old_pod="$(pod_of "$src")"
	old_ip="$(kc -n "$TEST_NS" get pod "$old_pod" -o jsonpath='{.status.podIP}')"
	kc -n "$TEST_NS" delete pod "$old_pod" --wait=true --timeout=120s >/dev/null || die "E5: could not delete $old_pod"
	kc -n "$TEST_NS" rollout status "deploy/$src" --timeout=180s >/dev/null || die "E5: $src never came back"
	deadline=$((SECONDS + 60))
	while true; do
		new_pod="$(pod_of "$src")"
		[ -n "$new_pod" ] && [ "$new_pod" != "$old_pod" ] && break
		[ "$SECONDS" -lt "$deadline" ] || die "E5: no replacement pod for $src"
		sleep 2
	done
	new_ip="$(kc -n "$TEST_NS" get pod "$new_pod" -o jsonpath='{.status.podIP}')"
	# If the address did not change, the phase would prove nothing about Q3.
	[ -n "$new_ip" ] && [ "$new_ip" != "$old_ip" ] ||
		die "E5: $src came back on the SAME pod IP ($old_ip) — the address change this phase exists for did not happen, so it cannot pass"
	ok "$src: $old_pod ($old_ip) -> $new_pod ($new_ip)"
	local d
	for d in "${QUIC_DSTS[@]}"; do
		assert_quic E5 "$src" "$d"
		assert_quic E5 "${SOURCES[1]}" "$d"
	done
}

# --- E6: the egress identity gate (#1053) ------------------------------------

# verify_identity_gate — a client under a brand-new ServiceAccount whose app
# sends at t=0. The pod is a bare Pod (restartPolicy Never) so it runs exactly
# once: a restart would send a second "first" request under an identity that is
# no longer new.
verify_identity_gate() {
	local sa pod want first init_exit init_ran started
	sa="idgate-$(date +%s)"
	pod="$sa"
	want="$(spiffe_id "$sa")"
	log "E6 identity gate (IDENTITY_GATE=$IDENTITY_GATE): new ServiceAccount $sa; the app's first request leaves at container start"
	kc apply -f - >/dev/null <<YAML || die "E6: client apply failed"
apiVersion: v1
kind: ServiceAccount
metadata: {name: $sa, namespace: $TEST_NS}
---
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  namespace: $TEST_NS
  labels: {app: $sa, aether.io/managed: "true"}
  annotations:
    config.aether.io/upstreams: "${QUIC_DSTS[0]}.$TEST_NS"
spec:
  serviceAccountName: $sa
  restartPolicy: Never
  containers:
    - name: curl
      image: $CURL_IMAGE
      command: ["sh", "-c"]
      args:
        - |
          out=\$(curl -s --max-time 30 -w '\\n%{http_code} %{time_total}' "http://$(fqdn "${QUIC_DSTS[0]}"):$OUTBOUND_PORT/header?key=X-Forwarded-Client-Cert")
          last=\$(printf '%s\\n' "\$out" | tail -n 1)
          body=\$(printf '%s\\n' "\$out" | sed '\$d' | head -n 1)
          echo "AETHER_IDGATE_FIRST code=\${last%% *} took=\${last#* }s xfcc=\$body"
          exec sleep 3600
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: {drop: ["ALL"]}
YAML
	# The first request has a 30s ceiling of its own; wait for its line.
	local deadline=$((SECONDS + 180))
	first=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		first="$(kc -n "$TEST_NS" logs "$pod" -c curl 2>/dev/null | grep '^AETHER_IDGATE_FIRST' || true)"
		[ -n "$first" ] && break
		sleep 2
	done
	init_ran="$(kc -n "$TEST_NS" get pod "$pod" -o jsonpath='{.spec.initContainers[*].name}' 2>/dev/null || true)"
	init_exit="$(kc -n "$TEST_NS" get pod "$pod" \
		-o jsonpath='{.status.initContainerStatuses[?(@.name=="aether-identity-ready")].state.terminated.exitCode}' 2>/dev/null || true)"
	started="$(kc -n "$TEST_NS" get pod "$pod" \
		-o jsonpath='{.status.initContainerStatuses[?(@.name=="aether-identity-ready")].state.terminated.startedAt}..{.status.initContainerStatuses[?(@.name=="aether-identity-ready")].state.terminated.finishedAt}' 2>/dev/null || true)"
	printf '    init containers: [%s]  aether-identity-ready exit=%s (%s)\n' "${init_ran:-none}" "${init_exit:-n/a}" "${started:-n/a}"
	printf '    gate log: %s\n' "$(kc -n "$TEST_NS" logs "$pod" -c aether-identity-ready 2>/dev/null | tail -n 1 || echo n/a)"
	printf '    %s\n' "${first:-AETHER_IDGATE_FIRST (none within 180s)}"

	[ -n "$first" ] || die "E6: the client never reported its first request"
	if [ "$IDENTITY_GATE" = "off" ]; then
		case "$init_ran" in *aether-identity-ready*) die "E6: IDENTITY_GATE=off but the gate was injected" ;; esac
		ok "E6 (gate OFF): first request, with no gate holding the app: ${first#AETHER_IDGATE_FIRST }"
		return
	fi
	case "$init_ran" in
	aether-identity-ready*) ;;
	*) die "E6: the webhook did not inject aether-identity-ready FIRST (init containers: [${init_ran:-none}])" ;;
	esac
	[ "$init_exit" = "0" ] || die "E6: aether-identity-ready did not exit 0 (exit=${init_exit:-still running})"
	case "$first" in
	*"code=200 "*) ;;
	*) die "E6: the pod's FIRST request did not answer 200 with the gate on: $first" ;;
	esac
	case "$first" in
	*"URI=$want"*) ;;
	*) die "E6: the first request did not carry the new ServiceAccount's own identity ($want): $first" ;;
	esac
	ok "E6: aether-identity-ready held the app until the SVID existed; its t=0 request answered 200 as $want"
}

# --- E7: a brand-new identity's first use of many QUIC destinations at once ---

# burst_dsts — E7's destinations, burst-dst-0 .. burst-dst-<BURST_DSTS-1>.
burst_dsts() {
	local i
	for ((i = 0; i < BURST_DSTS; i++)); do printf 'burst-dst-%d\n' "$i"; done
}

# percentiles — "p50=… p99=… max=…" of the seconds on stdin.
percentiles() {
	sort -n | awk '{ v[NR] = $1 } END {
		if (NR == 0) { print "n/a"; exit }
		printf "p50=%.3fs p99=%.3fs max=%.3fs", v[int((NR - 1) * 0.50) + 1], v[int((NR - 1) * 0.99) + 1], v[NR] }'
}

# verify_burst — E7, aether#1086. The client is a bare Pod (it runs once) under
# a ServiceAccount that did not exist a moment earlier, so no (source,
# destination) pair has dialled and no twin exists: EVERY one of its first
# requests needs an on-demand twin. The identity gate (#1053) holds the app
# until its SVID exists, so what is measured is twin admission + publish +
# warm, never the SVID wait. All BURST_DSTS x BURST_PER_DST requests leave
# together (curl --parallel-immediate), as k6's VUs do.
verify_burst() {
	local sa pod d i ups="" urls="" since result lines n bad admitted published builds alog
	log "E7 burst (aether#1086): a new ServiceAccount's first requests to $BURST_DSTS QUIC destinations at once, $BURST_PER_DST each"
	kc create ns "$TEST_NS" >/dev/null 2>&1 || true
	for d in $(burst_dsts); do deploy_destination "$d"; done
	for d in $(burst_dsts); do
		kc -n "$TEST_NS" rollout status "deploy/$d" --timeout=180s >/dev/null || die "E7: destination '$d' never became Ready"
	done
	# Every destination's HTTP/3 inbound must be up before the burst, or a
	# listener still warming would read as a #1086 failure.
	local deadline=$((SECONDS + 120)) missing
	while true; do
		missing=""
		for d in $(burst_dsts); do
			[ -n "$(h3_rq "$(pod_of "$d")")" ] || missing="$missing $d"
		done
		[ -z "$missing" ] && break
		[ "$SECONDS" -lt "$deadline" ] || die "E7: no HTTP/3 inbound for:$missing"
		sleep 3
	done
	# And resolvable: the registrar generates each destination's mesh VIP
	# Service, and mesh DNS serves it only after the agent projects the record
	# into its snapshot file. A burst sent before that fails DNS (curl 000) on
	# the not-yet-known names, which is not #1086 -- so wait for every Service,
	# then give the record projection a fixed settle.
	deadline=$((SECONDS + 120))
	while true; do
		missing=""
		for d in $(burst_dsts); do
			kc -n "$TEST_NS" get service "$d" >/dev/null 2>&1 || missing="$missing $d"
		done
		[ -z "$missing" ] && break
		[ "$SECONDS" -lt "$deadline" ] || die "E7: no mesh VIP Service for:$missing"
		sleep 3
	done
	sleep "${BURST_DNS_SETTLE:-15}"
	ok "E7: $BURST_DSTS destinations Ready with their HTTP/3 inbound and mesh VIP Service"

	sa="burst-$(date +%s)"
	pod="$sa"
	for d in $(burst_dsts); do
		ups="${ups:+$ups,}$d.$TEST_NS"
		for ((i = 0; i < BURST_PER_DST; i++)); do
			# -o binds to ONE url: every request needs its own.
			urls="$urls -o /dev/null http://$(fqdn "$d"):$OUTBOUND_PORT/hostname"
		done
	done
	since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	kc apply -f - >/dev/null <<YAML || die "E7: client apply failed"
apiVersion: v1
kind: ServiceAccount
metadata: {name: $sa, namespace: $TEST_NS}
---
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  namespace: $TEST_NS
  labels: {app: $sa, aether.io/managed: "true"}
  annotations:
    config.aether.io/upstreams: "$ups"
spec:
  serviceAccountName: $sa
  restartPolicy: Never
  $(node_pin "$NODE")
  containers:
    - name: curl
      image: $CURL_IMAGE
      command: ["sh", "-c"]
      args:
        - |
          echo AETHER_BURST_START
          curl -s --parallel --parallel-immediate --parallel-max 200 --max-time 10 -w 'AETHER_BURST %{http_code} %{time_total} %{url}\\n' $urls
          echo AETHER_BURST_DONE
          exec sleep 3600
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: {drop: ["ALL"]}
YAML
	deadline=$((SECONDS + 240))
	result=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		result="$(kc -n "$TEST_NS" logs "$pod" -c curl 2>/dev/null || true)"
		case "$result" in *AETHER_BURST_DONE*) break ;; esac
		sleep 2
	done
	case "$result" in
	*AETHER_BURST_DONE*) ;;
	*) die "E7: the burst client never finished (log tail: $(printf '%s\n' "$result" | tail -n 3 | tr '\n' ' '))" ;;
	esac
	lines="$(printf '%s\n' "$result" | grep '^AETHER_BURST [0-9]' || true)"
	n="$(printf '%s\n' "$lines" | sed '/^$/d' | wc -l | tr -d ' ')"
	bad="$(printf '%s\n' "$lines" | sed '/^$/d' | awk '$2 != "200"' | wc -l | tr -d ' ')"
	printf '    first-use requests: %s, non-200: %s; latency %s\n' "$n" "$bad" \
		"$(printf '%s\n' "$lines" | sed '/^$/d' | awk '{ print $3 }' | percentiles)"
	printf '%s\n' "$lines" | sed '/^$/d' | awk '$2 != "200" { c[$2]++ } END { for (k in c) printf "    status %s x%d\n", k, c[k] }'

	# What the agent did for the burst: one admission line per twin, then the
	# snapshot builds (the "setting snapshot" line is debug: EWQ_DEBUG=1 at up).
	alog="$(kc -n "$NS" logs -l app.kubernetes.io/component=agent -c agent --since-time="$since" --tail=-1 2>/dev/null || true)"
	admitted="$(printf '%s\n' "$alog" | grep -c "observed east-west QUIC pair (ODCDS).*@$TEST_NS/$sa" || true)"
	published="$(printf '%s\n' "$alog" | grep -c 'published observed east-west QUIC pairs' || true)"
	# Builds from the burst's first admission on (the pod's own arrival --
	# listeners, dependency set -- costs builds of its own before that). The
	# agent logs JSON; "timestamp" is RFC 3339, so a prefix compare orders it.
	builds="$(printf '%s\n' "$alog" | awk -v sa="@$TEST_NS/$sa\"" '
		{ ts = ""; if (match($0, /"timestamp":"[^"]*"/)) ts = substr($0, RSTART + 13, 23) }
		index($0, "observed east-west QUIC pair (ODCDS)") && index($0, sa) && first == "" { first = ts }
		/setting snapshot/ && first != "" && ts >= first { n++ }
		END { print n + 0 }')"
	printf '    agent: %s admissions for %s; from the first of them on, %s coalesced publishes and %s snapshot builds%s\n' \
		"$admitted" "$sa" "$published" "$builds" "$([ "${EWQ_DEBUG:-0}" = "1" ] || echo ' (EWQ_DEBUG off: builds are not logged)')"
	printf '%s\n' "$alog" | grep -E "observed east-west QUIC pair \(ODCDS\).*@$TEST_NS/$sa|published observed east-west QUIC pairs|setting snapshot" |
		cut -c1-240 | sed 's/^/    /' || true
	# The client ran once; remove it so repeated runs do not pile pods (and
	# their listeners, SVIDs and pairs) onto the one node's agent.
	kc -n "$TEST_NS" delete pod "$pod" --wait=false >/dev/null 2>&1 || true
	kc -n "$TEST_NS" delete serviceaccount "$sa" >/dev/null 2>&1 || true

	[ "$n" -eq $((BURST_DSTS * BURST_PER_DST)) ] || die "E7: $n first-use results, want $((BURST_DSTS * BURST_PER_DST))"
	[ "$bad" -eq 0 ] ||
		die "E7: $bad of $n first-use requests did not answer 200 (aether#1086: 503 NC = the twin was not admitted, published and warmed inside the on_demand timeout; 000 = the client never got a response)"
	ok "E7: $n/$n first-use requests to $BURST_DSTS fresh QUIC destinations answered 200"
}

verify() {
	verify_preflight
	verify_fanout
	verify_quic
	verify_gamma
	verify_gamma_single
	verify_pairs
	verify_q3
	verify_identity_gate
	verify_burst
	log "all east-west QUIC assertions passed (demand-scoped twins with nothing listed, per-source HTTP/3 + XFCC, GAMMA weighted stays h2 / single-backend rides h3, twins = driven pairs, identity across a client address change, the identity gate holds a new pod until its SVID exists, a new identity's first use of $BURST_DSTS destinations at once answers 200)"
}

down() {
	log "tearing down"
	kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	ok "cluster '$CLUSTER' removed"
}

up() {
	raise_inotify
	build_images
	create_cluster
	load_images
	install_gwapi_crds
	install_spire
	install_aether
	deploy_workloads
}

# Sourced (e2e/eastwest-quic-hotrestart.sh reuses the bring-up and readings):
# define everything, run nothing.
if [ "${BASH_SOURCE[0]}" != "$0" ]; then
	return 0
fi

case "${1:-}" in
up) up ;;
test) verify ;;
verify) verify ;;
idgate) verify_identity_gate ;;
burst) verify_burst ;;
down) down ;;
"") up && verify ;;
*) die "usage: $0 {up|test|verify|idgate|burst|down}" ;;
esac
