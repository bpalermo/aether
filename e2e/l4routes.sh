#!/usr/bin/env bash
# Single-cluster kind e2e for proposal 018 Phase 3b (Gateway API L4 routes on the
# mesh capture path). It covers ALL THREE route types — TCPRoute, TLSRoute and
# UDPRoute — which is the whole of issue #868's acceptance.
#
# What it proves, on a real data path (kind + the real chart + the real
# aether-proxy + the CNI's transparent capture):
#
#   T1.0  control      no TCPRoute             -> every probe reaches the FLOOR
#   T1.1  split        75 / 25 over 200 probes -> a band, never `floor`
#   T1.2  drain        weight 0 is DRAIN, asserted EXACTLY and in BOTH directions
#   T1.3  all drained  every weight 0          -> back to the FLOOR
#   T1.4  delete       no TCPRoute again       -> back to the FLOOR
#
#   T2.0  control      no TLSRoute             -> every SNI reaches the FLOOR
#   T2.1  sni A        SNI alpha               -> backend A, never B, never floor
#   T2.2  sni B        SNI bravo               -> backend B, never A, never floor
#   T2.3  fall-through a THIRD, unmatched SNI  -> the FLOOR, never A or B
#   T2.4  delete       no TLSRoute again       -> every SNI back to the FLOOR
#
#   T3.0  control      no UDPRoute             -> no datagram is answered at all
#   T3.1  delivery     UDPRoute 100 / 0        -> every datagram answered by A
#   T3.2  delete       no UDPRoute again       -> no datagram is answered again
#
# The `floor` / `no answer` states are assertions, not setup. A suite that only
# ever observes one outcome cannot distinguish "passing" from "not looking"
# (#853), so T1.0 / T1.3 / T1.4, T2.0 / T2.3 / T2.4 and T3.0 / T3.2
# re-demonstrate each probe's discriminating power on EVERY run: the same probe,
# the same client, a different named answer.
#
# The workload is e2e/l4echo (ghcr.io/bpalermo/aether/l4echo:latest), one process
# per mode:
#
#   --mode=tcp  reads one line, writes "<marker> <line>" and CLOSES.
#   --mode=tls  terminates TLS with a self-signed cert and answers "<marker>".
#   --mode=udp  replies "<marker> <datagram>" to the sender.
#
# The marker is the assertion vehicle — no Envoy admin access, and it survives a
# proxy hot restart, which raw counters do not.
#
# -----------------------------------------------------------------------------
# THE TLS LEG DIALS A PORT NO FILTER CHAIN CLAIMS, AND THAT IS NOT COSMETIC.
#
# BuildCaptureTLSRouteFilterChains (agent/internal/xds/proxy/l4route.go) matches
# `prefix_ranges: <ClusterIP>/32` + `server_names` and NO destination_port.
# Proposal 037 then gave every TCP-primary service destination_port-qualified
# floor chains (:18082 and the service's own primary port) plus a portless
# any-port shim. Envoy resolves destination_port FIRST and only consults the
# portless bucket when no chain claims the exact port — so a TLS dial to the
# service's primary port, or to :18082, lands on the port-qualified FLOOR chain
# and the SNI chains are never even considered.
#
# THAT WAS #911, AND IT IS NOW FIXED. The SNI chains are qualified onto every
# port a floor or per-port chain claims, so they tie the floor chain on tiers 1
# and 2 and win on tier 3 (server_names). TLSRoute works on both mesh spellings
# again and no longer depends on the 037 any-port shim — which matters twice
# over, because Phase 4 proposes to REMOVE that shim, and because TLSRoute
# traffic was incrementing the very counter Phase 4 is gated on.
#
# So this leg now dials the service's PRIMARY port, which the floor chain also
# claims. That is deliberate: dialling a port nothing claims would still pass
# whether or not the fix is present, and a check that cannot distinguish those
# two states is not a check. T2.1/T2.2 (SNI wins) and T2.3 (unmatched SNI falls
# through to the floor) are a real comparison at this port — same client, same
# port, three SNIs — and T2.3 now falls to the port-qualified floor chain rather
# than to the shim.
#
# One consequence worth stating rather than discovering later: if the SNI chains
# has to move with it. Filed as #911 rather than worked around here.
#
# THE UDP LEG DIALS :18081, AND ONLY :18081.
#
# The CNI's UDP redirect (programCaptureRedirect, cni/internal/plugin/capture.go)
# matches ClusterIP + dport == ProxyOutboundPort. redirect-all is TCP-ONLY, so
# unlike the TCP leg there is no any-port UDP capture: a datagram to any other
# port leaves the pod unredirected and is never seen by the mesh.
#
# UDP RIDES THE MESH IN PLAINTEXT. mTLS is a TCP/TLS construct and there is no
# DTLS; the "udp:" clusters carry no transport socket at all (asserted in
# //test/envoy_validate, #876). Nothing here expects or asserts mTLS on the UDP
# leg, and a passing T3 is NOT evidence of an authenticated UDP path.
#
# T3 asserts DELIVERY, not selection — deliberately, per #868's scope note. The
# per-pod udp_proxy carries a bare single-cluster RouteSpecifier, so backend
# weights are discarded and a second UDPRoute-backed service on the node is
# dropped entirely (#873, surfaced as a log line and the udp_route_unsupported
# counter by #874/#882). A "UDPRoute picks the right backend" assertion would
# therefore fail by DESIGN rather than find a bug, and belongs to #873's fix.
#
# The 100 / 0 shape T3.1 uses is the one shape both implementations must agree
# on: today's path takes backends[0] (backendRef order is preserved by
# common/l4project, which keeps weight-0 entries), and a weighted path must
# DRAIN the weight-0 backend (#492). So `bravo` appearing is a failure under
# either, and T3.1 does not have to be rewritten when #873 lands. The MIRROR
# (0 / 100) is deliberately absent: it is the #873 case itself.
#
# -----------------------------------------------------------------------------
# SPIRE IS **ON** IN THIS HARNESS, AND THAT IS NOT OPTIONAL.
#
# uds.sh and both conformance jobs run with spire.enabled=false. The TCP floor
# cannot: with SPIRE off the mesh has no TCP path at all, at BOTH ends.
#
#   * source side — SnapshotCache.captureTCPClusters() returns nil unless the
#     node SVID is known (agent/internal/xds/cache/capture.go). SetNodeIdentity
#     is called only by the SPIRE bridge, and the bridge is not wired when
#     --spire-enabled=false. No node SVID => no "tcp:" clusters at all, so the
#     capture listener's cap_tcp_* chain names a cluster that does not exist.
#   * destination side — NewInboundListener(cleartext=true) emits ONE default
#     HCM chain (agent/internal/xds/proxy/ingress.go). The inbound TCP floor
#     chain, which is what a raw-TCP mesh hop lands on, exists only on the mTLS
#     path.
#
# So "L4 routing with SPIRE off" is not a weaker version of this test, it is a
# test of nothing. The upside of paying for SPIRE: this harness DOES exercise the
# "tcp:" cluster's server-SAN pin end to end, which is the shape of the #464
# regression — the thing the original plan had listed as out of scope. Filed as
# issue #877, because "configured, accepted and silently inert" is a bug in its
# own right even when the mTLS requirement is by design.
#
# THE REGISTRY BACKEND IS **etcd**, AND THAT IS NOT OPTIONAL EITHER.
#
# The chart-default `kubernetes` backend has no notion of a TCP service at all:
# KubernetesRegistry.ListEndpoints / ListAllEndpoints return NOTHING for
# PROTOCOL_TCP on purpose (registry/internal/k8s/kubernetes.go) — every managed
# pod is a mesh-inbound HTTP endpoint by construction there, and returning the
# same pods under TCP used to collapse every mesh service to a TCP-only entry.
# So with that backend the pods' endpoint.aether.io/protocol: tcp is accepted,
# registered, and then invisible: the registrar generates an `app-protocol: http`
# mesh Service, the capture reconciler classifies it HTTP, and there is no TCP
# floor chain for a TCPRoute to replace. etcd — what the soak cluster runs — keys
# the registry by name+protocol and is the only backend where an L4 mesh service
# exists at all. It runs here as a plain docker container on kind's network,
# exactly as the multicluster harnesses run theirs. Filed as issue #878.
# -----------------------------------------------------------------------------
#
# Usage: e2e/l4routes.sh {up|test|verify|down}   (bare = up + verify)
#
# Prereqs: kind, docker, kubectl, helm, bazel (for the image build; CI sets
# L4_SKIP_BUILD=1 and pre-loads the images from the nightly build artifact).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER="${L4_CLUSTER:-l4routes}"
CTX="kind-$CLUSTER"
NS="aether-system"
TEST_NS="aether-test"
MESH_DOMAIN="aether.internal"
TRUST_DOMAIN="aether.internal"
# The port every l4echo tcp workload binds, and the pods' endpoint.aether.io/port.
# The probe dials it on the mesh VIP: redirect-all capture has no dport match, so
# ANY outbound TCP port is redirected to the capture listener, which then selects
# the chain on the ORIGINAL DESTINATION IP (the VIP), not on the port. The
# backendRef port is ignored by design — an L4 backend's "tcp:" cluster carries
# its endpoints' own port — so this one value is both ends of the path.
APP_PORT="9000"
# The TLS leg. TLS_PORT is what the l4echo --mode=tls workloads bind and what
# they register as endpoint.aether.io/port, so it is also the port the backends'
# "tcp:" clusters dial, AND the service's primary port.
#
# TLS_DIAL_PORT is what the PROBE dials. It is deliberately the primary port —
# a port the 037 floor chain also claims — so this leg exercises the #911 fix.
# It was 8443 (claimed by nothing) while TLSRoute was shadowed there; dialling
# an unclaimed port passes with or without the fix and so proves nothing about
# it. See the TLS block in the header.
TLS_PORT="9443"
TLS_DIAL_PORT="9443"
# The three SNIs. Two are routed by a TLSRoute; the third matches nothing and
# must reach the floor (#868's corrected fall-through criterion).
SNI_ALPHA="alpha.l4tls.test"
SNI_BRAVO="bravo.l4tls.test"
SNI_NOMATCH="nomatch.l4tls.test"
# The TLS parent's ClusterIP, resolved once by verify_tls and read by
# tls_probe_batch. Declared here so `set -u` cannot trip over it.
TLS_VIP=""
# The UDP leg. UDP_PORT is what the l4echo --mode=udp workloads bind and
# register; MESH_UDP_PORT is meshconst.ProxyOutboundPort, the ONLY UDP
# destination port the CNI redirects into the capture listener.
UDP_PORT="9001"
MESH_UDP_PORT="18081"
GWAPI_VERSION="v1.6.2"
# TCPRoute/TLSRoute/UDPRoute are EXPERIMENTAL-channel in gateway-api v1.6.2 (the
# standard channel stops at GRPCRoute), so the standard bundle uds.sh installs is
# not enough here.
GWAPI_CHANNEL="experimental-install.yaml"
# SPIRE >= 1.15.2 is required since proposal 036 (the SPIFFE Broker API); these
# are the same chart pins e2e/multicluster_waypoint.sh uses.
SPIRE_CHART_VERSION="${SPIRE_CHART_VERSION:-0.30.2}"
SPIRE_CRDS_VERSION="${SPIRE_CRDS_VERSION:-0.6.1}"
SPIRE_CLASS="spire-mgmt-spire" # spire-controller-manager class (namespace-release)
IMAGES=(agent mesh-dns proxy-supervisor cni-install registrar controller l4echo)
# The ETCD registry backend, not the chart-default kubernetes one — see the
# second block in the header. etcd runs as a plain docker container on kind's
# network, exactly as the multicluster harnesses run theirs.
KIND_NET="kind"
REGION="local"
ETCD_NAME="${ETCD_NAME:-aether-l4-etcd}"
ETCD_IMAGE="${ETCD_IMAGE:-quay.io/coreos/etcd:v3.5.16}"

# The mesh controllerName the l4route reconciler writes its RouteParentStatus
# under (agent/internal/gatewaystatus). Waiting on it separates "the agent never
# saw the route" from "the agent saw it and routed wrong" in the failure output.
MESH_CONTROLLER="gateway.aether.io/mesh"

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
die() {
	printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2
	dump_state
	exit 1
}

kc() { kubectl --context "$CTX" "$@"; }

# Every failure here is one of "the name did not resolve", "the agent never saw
# the route", "the route was projected wrong", or "the tcp: cluster was never
# published" — dump all four rather than leave the next red run to a bisect
# (the #590 principle).
dump_state() {
	kubectl config get-contexts "$CTX" >/dev/null 2>&1 || return 0
	printf '\033[1;33m  -- pods --\033[0m\n' >&2
	kc get pods -A -o wide 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- tcproutes / tlsroutes / udproutes (spec + our RouteParentStatus) --\033[0m\n' >&2
	kc get tcproutes,tlsroutes,udproutes -A -o yaml 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- mesh services (mesh-DNS answers their ClusterIPs; app-protocol picks the L4 path) --\033[0m\n' >&2
	kc -n "$TEST_NS" get svc -o custom-columns=NAME:.metadata.name,CLUSTERIP:.spec.clusterIP,PROTO:.metadata.annotations.aether\\.io/app-protocol 2>&1 |
		sed 's/^/    /' >&2 || true
	# The UDP path's only voice: #874 logs every UDPRoute input the single-cluster
	# udp_proxy throws away. If T3 is red because a backend was discarded rather
	# than because the data path is broken, it says so here.
	printf '\033[1;33m  -- agent log: UDPRoute inputs discarded (#873/#874) --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=agent --all-containers --tail=400 --prefix 2>&1 |
		grep -i "UDPRoute input discarded" | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- agent log (L4 projection + snapshot pushes) --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=agent --all-containers --tail=120 --prefix 2>&1 |
		sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- proxy log --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=proxy -c proxy --tail=60 --prefix 2>&1 |
		sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- mesh-dns --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=mesh-dns --tail=15 --prefix 2>&1 |
		sed 's/^/    /' >&2 || true
	# The registry backend is load-bearing here (a service only exists under a
	# PROTOCOL_TCP key on etcd), so show what the registrar actually holds.
	printf '\033[1;33m  -- registrar --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=registrar --tail=40 --prefix 2>&1 |
		sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- etcd tcp service keys --\033[0m\n' >&2
	docker exec "$ETCD_NAME" etcdctl get --prefix / --keys-only 2>/dev/null |
		grep -i tcp | sed 's/^/    /' >&2 || true
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
	# CI builds the images itself (bazel image_load via RBE) and sets
	# L4_SKIP_BUILD=1 so this is a no-op — they are already in the local docker
	# daemon under ghcr.io/bpalermo/aether/<img>:latest.
	if [ "${L4_SKIP_BUILD:-0}" = "1" ]; then
		ok "skipping image build (L4_SKIP_BUILD=1; images pre-built)"
		return
	fi
	log "building + loading aether images (incl. the l4echo test backend)"
	local t
	for t in //agent/cmd/agent //agent/cmd/mesh-dns //agent/cmd/proxy-supervisor \
		//cni/cmd/cni-install //registrar/cmd/registrar //controller/cmd/controller \
		//e2e/l4echo; do
		(cd "$REPO_ROOT" && bazel run "$t:image_load" >/dev/null 2>&1) || die "image build failed for $t"
	done
	ok "images built"
}

create_cluster() {
	if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
		ok "kind cluster '$CLUSTER' already exists"
		return
	fi
	log "creating kind cluster '$CLUSTER'"
	local cfg
	cfg="$(mktemp)"
	sed -e "s/CLUSTER_NAME/$CLUSTER/g" \
		-e "s#POD_SUBNET#10.20.0.0/16#g" \
		-e "s#SVC_SUBNET#10.120.0.0/16#g" \
		"$REPO_ROOT/e2e/kind-cluster.yaml" >"$cfg"
	kind create cluster --config "$cfg" --wait 60s >/dev/null
	rm -f "$cfg"
	ok "cluster '$CLUSTER' ready"
}

# etcd on kind's docker network, reachable from the cluster's nodes. Declared and
# assigned separately (SC2155): `local x="$(cmd)"` takes the builtin's exit
# status, so `set -e` cannot see a failed docker inspect and the install would
# silently proceed with "http://:2379".
etcd_ip() { docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$ETCD_NAME"; }

start_etcd() {
	log "starting etcd (the registry backend; see the header) on the '$KIND_NET' docker network"
	docker rm -f "$ETCD_NAME" >/dev/null 2>&1 || true
	docker run -d --name "$ETCD_NAME" --network "$KIND_NET" "$ETCD_IMAGE" \
		etcd --name s1 --data-dir /tmp/etcd \
		--listen-client-urls http://0.0.0.0:2379 --advertise-client-urls http://0.0.0.0:2379 >/dev/null ||
		die "could not start etcd"
	local deadline=$((SECONDS + 60))
	until docker exec "$ETCD_NAME" etcdctl endpoint health >/dev/null 2>&1; do
		[ "$SECONDS" -lt "$deadline" ] || die "etcd never became healthy"
		sleep 2
	done
	local ip
	ip="$(etcd_ip)"
	[ -n "$ip" ] || die "could not determine the etcd container IP"
	ok "etcd healthy at http://$ip:2379"
}

load_images() {
	log "loading images into '$CLUSTER'"
	local img
	for img in "${IMAGES[@]}"; do
		kind load docker-image "ghcr.io/bpalermo/aether/${img}:latest" --name "$CLUSTER" >/dev/null 2>&1 ||
			die "could not load ghcr.io/bpalermo/aether/${img}:latest into kind (was it built?)"
	done
	ok "images loaded"
}

# Gateway API CRDs go in BEFORE the agent starts. Reconciler.SetupWithManager
# gates each L4 route type on a RESTMapper lookup at manager-setup time
# (crdcheck.Present, at gateway.networking.k8s.io/v1 only, no v1alpha2 fallback),
# so a TCPRoute CRD applied after the helm install would need an agent RESTART to
# take effect — the suite would then fail with a perfectly healthy agent that
# simply never watched the type. Same ordering constraint uds.sh has for
# EndpointPolicy.
install_gwapi_crds() {
	log "installing Gateway API CRDs ($GWAPI_VERSION, experimental channel)"
	kc apply --server-side --force-conflicts -f \
		"https://github.com/kubernetes-sigs/gateway-api/releases/download/${GWAPI_VERSION}/${GWAPI_CHANNEL}" >/dev/null ||
		die "Gateway API CRD install failed"
	# All three types, and all three at v1 specifically: the agent looks each one
	# up at v1 (crdcheck.Present, no v1alpha2 fallback) and DISABLES the type with
	# a warning rather than an error if it is missing. A leg whose CRD never
	# arrived would then measure a mesh that was never watching for the route, so
	# this is a hard gate before anything else runs.
	#
	# It is a gate and not a "skip cleanly if absent": `up` installs these, so
	# their absence is a broken environment, and a leg that skips itself green is
	# the #853 defect this suite exists to avoid.
	local kind
	for kind in tcproutes tlsroutes udproutes; do
		kc wait --for=condition=Established "crd/$kind.gateway.networking.k8s.io" --timeout=60s >/dev/null ||
			die "the ${kind%s} CRD never became Established"
		kc get crd "$kind.gateway.networking.k8s.io" \
			-o jsonpath='{.status.storedVersions}' 2>/dev/null | grep -q v1 ||
			die "the ${kind%s} CRD does not serve v1 — the agent's crdcheck is v1-only and would disable that route type silently"
	done
	ok "Gateway API CRDs installed (TCPRoute, TLSRoute, UDPRoute at v1)"
}

# require_crd KIND PLURAL — fail loudly if a route type's CRD is not served at
# v1 by the time its leg runs. install_gwapi_crds already guarantees this, so a
# failure here means something removed the CRD mid-run, or `verify` is being run
# against a cluster that `up` did not build. Either way the leg cannot mean
# anything, and saying so beats reporting green.
require_crd() {
	local kind="$1" plural="$2"
	kc get crd "$plural.gateway.networking.k8s.io" \
		-o jsonpath='{.status.storedVersions}' 2>/dev/null | grep -q v1 ||
		die "$kind is not served at v1 in this cluster, so the agent never watched it — run '$0 up' first (it installs the gateway-api $GWAPI_VERSION experimental bundle); this leg is NOT skipped, because a leg that skips itself green cannot fail (#853)"
}

# SPIRE, single cluster, self-signed (no upstream CA: nothing has to cross a
# cluster boundary here). See the header for why this is mandatory rather than
# an extra.
install_spire() {
	log "installing SPIRE (trust domain $TRUST_DOMAIN)"
	helm --kube-context "$CTX" repo add spiffe https://spiffe.github.io/helm-charts-hardened/ >/dev/null 2>&1 || true
	helm --kube-context "$CTX" repo update >/dev/null 2>&1 || true
	kc create ns spire-mgmt >/dev/null 2>&1 || true
	helm --kube-context "$CTX" upgrade --install spire-crds spiffe/spire-crds \
		-n spire-mgmt --version "$SPIRE_CRDS_VERSION" --wait --timeout 3m >/dev/null ||
		die "spire-crds install failed"
	# Broker API (proposal 036). accessPolicy is set to permissive EXPLICITLY,
	# never left at the chart's `auto`: `auto` resolves to `enforced` for a
	# KubernetesObjectReference, and `enforced` runs a SubjectAccessReview
	# (impersonate-via-spire) per referenced pod whose RBAC the chart only
	# renders alongside impersonation.clusterWidePodsOnly — so `auto` fails
	# CLOSED and every pod gets PermissionDenied. The same broker key must appear
	# in both the brokerAPI and the k8s-attestor blocks.
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
	# Every mesh pod gets spiffe://<td>/ns/<ns>/sa/<sa>; className must match the
	# spire-controller-manager class ($SPIRE_CLASS).
	kc apply -f - >/dev/null <<YAML || die "ClusterSPIFFEID apply failed"
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: aether-workloads
spec:
  className: $SPIRE_CLASS
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  podSelector:
    matchLabels:
      aether.io/managed: "true"
YAML
	ok "SPIRE up"
}

# Render the chart placeholders into a temp copy so the source tree stays clean.
chart_dir() {
	local out
	out="$(mktemp -d)"
	cp -r "$REPO_ROOT/charts" "$out/"
	sed -i -e 's/{GIT_COMMIT}/e2e/' -e 's/{STABLE_GIT_VERSION}/0.0.0-e2e/' \
		"$out/charts/crds/Chart.yaml" "$out/charts/aether/Chart.yaml"
	echo "$out/charts"
}

install_aether() {
	local charts etcd
	charts="$(chart_dir)"
	etcd="http://$(etcd_ip):2379"
	img() { echo "--set $1.image.repository=ghcr.io/bpalermo/aether/$2 --set $1.image.tag=latest --set $1.image.digest= --set $1.image.pullPolicy=Never"; }
	log "installing the aether CRDs (MeshConfig, HTTPFilter, EdgeConfig, EndpointPolicy)"
	helm --kube-context "$CTX" upgrade --install aether-crds "$charts/crds" \
		-n "$NS" --create-namespace --wait --timeout 2m >/dev/null || die "crds chart install failed"

	# Everything except spire is chart default: transparent capture with
	# redirect-all, mesh DNS, the kubernetes registry backend. L4 routing itself
	# has had NO values gate since proposal 031 — the agent enables each route
	# type purely on its CRD being served — so there is deliberately no --set for
	# it here, and no chart change (hence no Chart.yaml bump) for this suite.
	# controller.webhook.spire stays at its default false: the webhook's serving
	# cert is decoupled from mesh mTLS, and a Helm self-signed cert is one fewer
	# thing that can wedge the install.
	log "installing aether (SPIRE ON — see the header; capture + mesh DNS default)"
	# shellcheck disable=SC2046
	helm --kube-context "$CTX" upgrade --install aether "$charts/aether" \
		-n "$NS" --create-namespace \
		--set namespace.create=false \
		--set "meshDomain=$MESH_DOMAIN" \
		--set spire.enabled=true \
		--set edge.enabled=false \
		--set registrar.registryBackend=etcd \
		--set "registrar.region=$REGION" \
		--set "registrar.etcd.region=$REGION" \
		--set "registrar.etcd.endpoints[0]=$etcd" \
		$(img agent agent) $(img agent.meshDnsDaemon mesh-dns) \
		$(img proxy.supervisor proxy-supervisor) $(img cniInstall cni-install) \
		$(img registrar registrar) $(img controller controller) \
		--set proxy.image.pullPolicy=IfNotPresent \
		--timeout 6m >/dev/null || die "aether install failed"
	rm -rf "$(dirname "$charts")"

	kc -n "$NS" rollout status ds/aether-agent --timeout=300s >/dev/null || die "the agent DaemonSet never became Ready"
	kc -n "$NS" rollout status ds/aether-mesh-dns --timeout=180s >/dev/null || die "the mesh-DNS DaemonSet never became Ready"
	kc -n "$NS" rollout status deploy/aether-registrar --timeout=180s >/dev/null || die "the registrar never became Ready"
	kc -n "$NS" rollout status deploy/aether-controller --timeout=180s >/dev/null || die "the controller never became Ready"
	ok "aether up"
}

# The TCPRoute parent, its two backends, and the client.
#
# NO Kubernetes Service objects are created on purpose: the registrar generates
# the selectorless mesh VIP Service (the name mesh-DNS answers, and the object a
# parentRef names) for every registered service, and it deliberately SKIPS a name
# a non-aether Service already owns — hand-writing one here would suppress the
# VIP and break resolution. The registry service name is the pod's ServiceAccount.
#
# endpoint.aether.io/protocol: "tcp" is load-bearing on EVERY workload here, not
# just the parent. It is what makes the registrar stamp aether.io/app-protocol:
# tcp on the generated Service, which is what the capture reconciler classifies
# on. Without it on the PARENT there is no TCP floor chain to replace, so no L4
# chain exists at all; without it on a BACKEND, captureTCPClusters() never emits
# that backend's "tcp:" cluster and the weighted set points at a name Envoy does
# not have.
#
# The TLS and UDP workloads register endpoint.aether.io/protocol: "tcp" as well
# — "http" and "tcp" are the only accepted values (there is no "udp"), and what
# the UDP path actually needs from the registry is the service's EDS plus its
# application port, which the TCP classification supplies. captureUDPClusters
# reads that port off the service entry and builds an app-port load assignment
# for the plaintext "udp:" cluster.
deploy_l4echo_workload() {
	local name="$1" text="$2" mode="$3" port="$4" extra="$5"
	kc apply -f - >/dev/null <<YAML || die "workload $name apply failed"
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
        endpoint.aether.io/port: "$port"
        endpoint.aether.io/protocol: "tcp"
    spec:
      serviceAccountName: $name
      # l4echo is distroless/static and runs as uid 65532 already (asserted by
      # //e2e/l4echo's container test); this states it rather than relying on it,
      # and the read-only root is free — the binary writes nothing to disk, the
      # TLS cert is minted in memory at start-up.
      securityContext:
        runAsNonRoot: true
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: app
          image: ghcr.io/bpalermo/aether/l4echo:latest
          imagePullPolicy: Never
          args: ["--mode=$mode", "--listen=:$port", "--text=$text"$extra]
          ports: [{containerPort: $port}]
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: ["ALL"]}
YAML
}

deploy_workloads() {
	log "deploying the L4 parents, their backends, and the client"
	kc create ns "$TEST_NS" >/dev/null 2>&1 || true

	# TCPRoute leg: the parent (whose floor chain a TCPRoute replaces) and two
	# weighted backends.
	local name text
	for entry in "l4-front:floor" "l4-a:alpha" "l4-b:bravo"; do
		name="${entry%%:*}"
		text="${entry##*:}"
		deploy_l4echo_workload "$name" "$text" tcp "$APP_PORT" ""
	done

	# TLSRoute leg. Every workload speaks TLS, INCLUDING the parent: #868's
	# fall-through criterion is a positive assertion ("an unmatched SNI reaches
	# the floor backend"), which is only observable if the floor backend can
	# complete the handshake the probe started.
	#
	# The cert covers all three SNIs so a strict client would work too; the probe
	# itself passes -k, because this leg is about which BACKEND answered, not
	# about the identity on the cert.
	for entry in "l4tls-front:tlsfloor" "l4tls-a:tlsalpha" "l4tls-b:tlsbravo"; do
		name="${entry%%:*}"
		text="${entry##*:}"
		deploy_l4echo_workload "$name" "$text" tls "$TLS_PORT" \
			", \"--tls-sans=$SNI_ALPHA,$SNI_BRAVO,$SNI_NOMATCH\""
	done

	# UDPRoute leg. l4udp-front is the parentRef — the VIP whose :18081 the CNI
	# redirects — and l4udp-a / l4udp-b are its backends. Its own "udpfloor"
	# marker is unreachable by construction today: there is no UDP floor, so with
	# no UDPRoute there is no listener at all. Seeing it would itself be a
	# finding, which is why the counts below are exhaustive rather than
	# "at least N".
	for entry in "l4udp-front:udpfloor" "l4udp-a:udpalpha" "l4udp-b:udpbravo"; do
		name="${entry%%:*}"
		text="${entry##*:}"
		deploy_l4echo_workload "$name" "$text" udp "$UDP_PORT" ""
	done

	# The upstreams annotation is NOT an optimisation here: there is no ODCDS for
	# tcp_proxy. An undeclared TCP upstream yields a chain that matches and a
	# cluster that is missing, which looks exactly like a routing bug.
	kc apply -f - >/dev/null <<YAML || die "client apply failed"
apiVersion: v1
kind: ServiceAccount
metadata: {name: client, namespace: $TEST_NS}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: client, namespace: $TEST_NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: client}}
  template:
    metadata:
      labels: {app: client, aether.io/managed: "true"}
      annotations:
        config.aether.io/upstreams: "l4-front.$TEST_NS,l4-a.$TEST_NS,l4-b.$TEST_NS,l4tls-front.$TEST_NS,l4tls-a.$TEST_NS,l4tls-b.$TEST_NS,l4udp-front.$TEST_NS,l4udp-a.$TEST_NS,l4udp-b.$TEST_NS"
    spec:
      serviceAccountName: client
      containers:
        - name: curl
          image: curlimages/curl:8.22.0
          command: ["sleep", "infinity"]
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: ["ALL"]}
YAML

	local d
	for d in l4-front l4-a l4-b l4tls-front l4tls-a l4tls-b l4udp-front l4udp-a l4udp-b client; do
		kc -n "$TEST_NS" rollout status "deploy/$d" --timeout=180s >/dev/null ||
			die "workload '$d' never became Ready"
	done
	ok "workloads deployed (tcp: floor/alpha/bravo, tls: tlsfloor/tlsalpha/tlsbravo, udp: udpfloor/udpalpha/udpbravo, client)"
}

# --- data-path probes -------------------------------------------------------

# probe_batch SERVICE N — run N probes from inside the client pod in ONE exec and
# print N lines, each the FIRST TOKEN of the reply ("floor" / "alpha" / "bravo")
# or "NOREPLY". One exec per batch rather than per probe: 200 kubectl round trips
# would dominate the runtime and the failure modes.
#
# Payloads are >= 6 bytes on purpose. The capture listener's http_inspector
# stalls on an INCONCLUSIVE short first write; a <6-byte first chunk is what
# wedged the TCP probes in #466, and it costs a 1s listener-filter timeout per
# connection even now that the timeout continues instead of closing.
probe_batch() {
	local svc="$1" n="$2"
	# SC2016: the single quotes are the point — this body is evaluated by the
	# POD's shell, not by this one. $target/$n/$i/$reply are its variables, passed
	# in as positional arguments after the `sh` argv[0].
	# shellcheck disable=SC2016
	kc -n "$TEST_NS" exec deploy/client -c curl -- sh -c '
		target="$1"; n="$2"; i=1
		while [ "$i" -le "$n" ]; do
			reply=$(printf "probe-%s\n" "$i" | curl -s --max-time 5 "telnet://$target" 2>/dev/null | head -1 | tr -d "\r")
			printf "%s\n" "${reply:-NOREPLY}"
			i=$((i + 1))
		done
	' sh "$svc.$TEST_NS.$MESH_DOMAIN:$APP_PORT" "$n" 2>/dev/null
}

# tls_probe_batch SNI N — run N TLS probes from inside the client pod and print
# N lines, each the FIRST TOKEN of the body ("tlsfloor"/"tlsalpha"/"tlsbravo") or
# "NOREPLY".
#
# --resolve pins the connection to the PARENT's VIP while the SNI (and the Host)
# stay whatever we are testing. That separation is the point: filter-chain
# selection here is (original destination IP) x (SNI), and only --resolve lets
# one vary while the other is held.
#
# $TLS_VIP is the parent's ClusterIP, read once by verify_tls. -k because the
# self-signed cert is about completing the handshake, not identity; the marker in
# the body is what is asserted.
tls_probe_batch() {
	local sni="$1" n="$2"
	# shellcheck disable=SC2016  # evaluated by the POD's shell; see probe_batch
	kc -n "$TEST_NS" exec deploy/client -c curl -- sh -c '
		sni="$1"; vip="$2"; port="$3"; n="$4"; i=1
		while [ "$i" -le "$n" ]; do
			body=$(curl -s -k --max-time 5 --resolve "$sni:$port:$vip" "https://$sni:$port/" 2>/dev/null | head -1 | tr -d "\r")
			printf "%s\n" "${body:-NOREPLY}"
			i=$((i + 1))
		done
	' sh "$sni" "$TLS_VIP" "$TLS_DIAL_PORT" "$n" 2>/dev/null
}

# udp_probe_batch SERVICE N — send N datagrams to SERVICE's mesh name on
# :$MESH_UDP_PORT and print N lines, each the first token of the reply or
# "NOREPLY".
#
# busybox nc's -w is an IDLE timeout, so a probe that IS answered still costs a
# second; `timeout` bounds the pathological case where nc neither reads nor
# exits. Both are cheap compared with a wedged batch.
#
# NOREPLY is the expected answer whenever no UDPRoute exists, and it is expected
# for two different reasons that this probe cannot tell apart: the CNI redirect
# sends the datagram to :18001/udp where nothing is bound, and without the
# redirect the generated mesh Service has no UDP port for kube-proxy to serve.
# That is stated rather than hidden — T3.0 is a weaker control than T1.0, which
# is why T3.1 does not rest on it alone.
udp_probe_batch() {
	local svc="$1" n="$2"
	# shellcheck disable=SC2016  # evaluated by the POD's shell; see probe_batch
	kc -n "$TEST_NS" exec deploy/client -c curl -- sh -c '
		target="$1"; port="$2"; n="$3"; i=1
		while [ "$i" -le "$n" ]; do
			reply=$(printf "probe-%s\n" "$i" | timeout 4 nc -u -w 1 "$target" "$port" 2>/dev/null | head -1 | tr -d "\r")
			printf "%s\n" "${reply:-NOREPLY}"
			i=$((i + 1))
		done
	' sh "$svc.$TEST_NS.$MESH_DOMAIN" "$MESH_UDP_PORT" "$n" 2>/dev/null
}

# svc_vip NAME — the ClusterIP of a registrar-generated mesh Service. Declared
# and assigned separately (SC2155) so `set -e` can see a failed kubectl.
svc_vip() { kc -n "$TEST_NS" get svc "$1" -o jsonpath='{.spec.clusterIP}' 2>/dev/null; }

# count_token REPLIES TOKEN — how many replies start with TOKEN.
count_token() {
	printf '%s\n' "$1" | awk -v want="$2" '$1 == want { n++ } END { print n + 0 }'
}

# histogram REPLIES — a compact "floor=3 alpha=150 ..." for failure messages.
histogram() {
	printf '%s\n' "$1" | awk '{ c[$1]++ } END { for (k in c) printf "%s=%d ", k, c[k] }'
}

# await_all SERVICE WANT TIMEOUT — poll small batches until ALL of them answer
# WANT, then return. This is the convergence gate before every measured batch:
# a route apply/patch is eventually consistent (reconcile -> xDS push -> Envoy
# filter-chain swap), and measuring across the transition would mix two
# configurations into one exact assertion. Failing here is itself an assertion —
# "the data plane never reached the expected state" — and says what it saw.
#
# await_probe takes the batch function by name so the TCP, TLS and UDP legs share
# one convergence gate rather than three that can drift; await_all is the TCP
# spelling and keeps its original signature.
await_probe() {
	local fn="$1" target="$2" want="$3" timeout="$4" replies deadline probes=10
	deadline=$((SECONDS + timeout))
	while true; do
		replies="$("$fn" "$target" "$probes")"
		if [ "$(count_token "$replies" "$want")" -eq "$probes" ]; then
			return 0
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			printf '%s' "$(histogram "$replies")"
			return 1
		fi
		sleep 5
	done
}

await_all() { await_probe probe_batch "$1" "$2" "$3"; }

# await_mixed SERVICE A B TIMEOUT — poll small batches until BOTH A and B appear
# and nothing else does. The convergence gate for the weighted-split phase, where
# no single reply is the expected one.
await_mixed() {
	local svc="$1" a="$2" b="$3" timeout="$4" replies deadline probes=20
	deadline=$((SECONDS + timeout))
	while true; do
		replies="$(probe_batch "$svc" "$probes")"
		if [ "$(count_token "$replies" "$a")" -ge 1 ] &&
			[ "$(count_token "$replies" "$b")" -ge 1 ] &&
			[ "$(($(count_token "$replies" "$a") + $(count_token "$replies" "$b")))" -eq "$probes" ]; then
			return 0
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			printf '%s' "$(histogram "$replies")"
			return 1
		fi
		sleep 5
	done
}

# await_route_observed NAME TIMEOUT — wait until OUR controller has published an
# Accepted=True RouteParentStatus whose observedGeneration matches the object's
# current generation. Proves the agent processed THIS version of the route, which
# separates "the agent never saw it" from "the agent saw it and routed wrong".
#
# It is a GATE, not an assertion: ResolvedRefs here does not check that the
# backend Services exist (backendsResolve does not look them up), so status
# alone never proves the data path.
# Takes the route KIND first so the TLSRoute and UDPRoute legs get the same gate
# (their reconciler writes the same RouteParentStatus under the same
# controllerName).
await_route_observed() {
	local kind="$1" name="$2" timeout="$3" gen observed status deadline
	deadline=$((SECONDS + timeout))
	while true; do
		gen="$(kc -n "$TEST_NS" get "$kind" "$name" -o jsonpath='{.metadata.generation}' 2>/dev/null || true)"
		status="$(kc -n "$TEST_NS" get "$kind" "$name" \
			-o jsonpath="{range .status.parents[?(@.controllerName=='$MESH_CONTROLLER')]}{range .conditions[?(@.type=='Accepted')]}{.status}{'\n'}{end}{end}" 2>/dev/null || true)"
		observed="$(kc -n "$TEST_NS" get "$kind" "$name" \
			-o jsonpath="{range .status.parents[?(@.controllerName=='$MESH_CONTROLLER')]}{range .conditions[?(@.type=='Accepted')]}{.observedGeneration}{'\n'}{end}{end}" 2>/dev/null || true)"
		if [ "$status" = "True" ] && [ -n "$gen" ] && [ "$observed" = "$gen" ]; then
			return 0
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			printf 'generation=%s accepted=%s observedGeneration=%s' "${gen:-<none>}" "${status:-<none>}" "${observed:-<none>}"
			return 1
		fi
		sleep 3
	done
}

# apply_route WEIGHT_A WEIGHT_B — (re)apply the TCPRoute with the given weights.
#
# group: "" is REQUIRED and is the subtle one: ParentReference.group defaults to
# gateway.networking.k8s.io in the CRD schema, and the reconciler skips any
# parentRef with a non-empty group, so an omitted group silently yields a route
# that is never attached to anything.
#
# backendRef.port is set because the CRD requires it for a Service backend; the
# projector IGNORES it (an L4 backend's cluster carries its endpoints' own port),
# so do not read anything into the value.
apply_route() {
	local wa="$1" wb="$2"
	kc apply -f - >/dev/null <<YAML || die "TCPRoute apply failed (weights $wa/$wb)"
apiVersion: gateway.networking.k8s.io/v1
kind: TCPRoute
metadata: {name: l4-split, namespace: $TEST_NS}
spec:
  parentRefs:
    - group: ""
      kind: Service
      name: l4-front
  rules:
    - backendRefs:
        - {group: "", kind: Service, name: l4-a, port: $APP_PORT, weight: $wa}
        - {group: "", kind: Service, name: l4-b, port: $APP_PORT, weight: $wb}
YAML
	await_route_observed tcproute l4-split 90 ||
		die "the agent never published Accepted=True for this generation of TCPRoute l4-split (weights $wa/$wb): $(await_route_observed tcproute l4-split 1)"
}

# apply_tls_routes — one TLSRoute per SNI, both parented to l4tls-front.
#
# Two objects rather than one: TLSRoute carries its hostnames at SPEC level, not
# per rule, so "SNI alpha to backend A and SNI bravo to backend B" cannot be
# expressed in a single route. The reconciler appends every Service-parented
# TLSRoute's rules to the same per-service list, which becomes one filter chain
# per rule (cap_tls_<svc>_<i>), each with its own server_names.
apply_tls_routes() {
	kc apply -f - >/dev/null <<YAML || die "TLSRoute apply failed"
apiVersion: gateway.networking.k8s.io/v1
kind: TLSRoute
metadata: {name: l4tls-alpha, namespace: $TEST_NS}
spec:
  parentRefs:
    - group: ""
      kind: Service
      name: l4tls-front
  hostnames: ["$SNI_ALPHA"]
  rules:
    - backendRefs:
        - {group: "", kind: Service, name: l4tls-a, port: $TLS_PORT, weight: 1}
---
apiVersion: gateway.networking.k8s.io/v1
kind: TLSRoute
metadata: {name: l4tls-bravo, namespace: $TEST_NS}
spec:
  parentRefs:
    - group: ""
      kind: Service
      name: l4tls-front
  hostnames: ["$SNI_BRAVO"]
  rules:
    - backendRefs:
        - {group: "", kind: Service, name: l4tls-b, port: $TLS_PORT, weight: 1}
YAML
	local r
	for r in l4tls-alpha l4tls-bravo; do
		await_route_observed tlsroute "$r" 90 ||
			die "the agent never published Accepted=True for this generation of TLSRoute $r: $(await_route_observed tlsroute "$r" 1)"
	done
}

# apply_udp_route WEIGHT_A WEIGHT_B — the UDPRoute for l4udp-front.
#
# backendRef ORDER is load-bearing for this leg and not only aesthetic: the
# single-cluster udp_proxy takes backends[0], and common/l4project preserves
# backendRef order (it defaults weights and keeps weight-0 entries rather than
# dropping them). l4udp-a is first so today's "backends[0]" and a future
# weight-honouring path name the same backend — see the UDP block in the header.
apply_udp_route() {
	local wa="$1" wb="$2"
	kc apply -f - >/dev/null <<YAML || die "UDPRoute apply failed (weights $wa/$wb)"
apiVersion: gateway.networking.k8s.io/v1
kind: UDPRoute
metadata: {name: l4udp-split, namespace: $TEST_NS}
spec:
  parentRefs:
    - group: ""
      kind: Service
      name: l4udp-front
  rules:
    - backendRefs:
        - {group: "", kind: Service, name: l4udp-a, port: $UDP_PORT, weight: $wa}
        - {group: "", kind: Service, name: l4udp-b, port: $UDP_PORT, weight: $wb}
YAML
	await_route_observed udproute l4udp-split 90 ||
		die "the agent never published Accepted=True for this generation of UDPRoute l4udp-split (weights $wa/$wb): $(await_route_observed udproute l4udp-split 1)"
}

# --- assertions -------------------------------------------------------------

# T1.0 — the permanent built-in negative control, and it runs FIRST.
#
# Without it the rest of this suite is unfalsifiable: if the probe could not tell
# the parent apart from its backends, every later count would be meaningless and
# still green. This is the state the suite returns to twice more (T1.3, T1.4).
verify_control() {
	log "T1.0 control: no TCPRoute — every probe must reach the FLOOR (l4-front itself)"
	local replies floor
	await_all l4-front floor 240 ||
		die "l4-front never answered 'floor' with no TCPRoute applied: $(await_all l4-front floor 1) — the plain TCP floor chain is not carrying traffic, so nothing below this line could mean anything"
	replies="$(probe_batch l4-front 20)"
	floor="$(count_token "$replies" floor)"
	[ "$floor" -eq 20 ] ||
		die "T1.0: expected 20/20 'floor', got $(histogram "$replies")"
	ok "20/20 probes reached the floor: the probe distinguishes the parent from its backends"
}

# T1.1 — the weighted split, asserted as a BAND.
#
# The sampling maths, so the band is justified rather than guessed. tcp_proxy's
# weighted_clusters picks a cluster per CONNECTION with an independent random
# draw, so alpha's count over n=200 probes is Binomial(200, 0.75):
#
#   mean   = 200 * 0.75              = 150
#   sd     = sqrt(200 * 0.75 * 0.25) = 6.12 connections = 3.06 percentage points
#
# The accepted band is alpha/200 in [0.60, 0.90], i.e. [120, 180] connections:
#   * width      = +/- 0.15 = +/- 4.9 sd  -> a correct implementation flakes with
#                  probability ~1e-6 per run, which over a nightly schedule is
#                  once every ~2700 years.
#   * 50/50 bug  = p 0.50, sd 3.54 points; 0.60 is 2.8 sd away, so an
#                  equal-weight implementation is caught ~99.7% of the time.
#   * 100/0 bug  = p 1.00, outside the band deterministically.
#
# Going below ~200 samples breaks that separation (at n=50 a 50/50 split lands
# inside the band about 8% of the time), which is why the count is not smaller
# even though each probe is a full mesh round trip.
verify_split() {
	log "T1.1 split: TCPRoute l4-split, l4-a weight 75 / l4-b weight 25, 200 probes"
	apply_route 75 25
	await_mixed l4-front alpha bravo 180 ||
		die "the 75/25 TCPRoute never took effect: $(await_mixed l4-front alpha bravo 1) — 'floor' here means the route was accepted but never replaced the floor chain"

	local replies alpha bravo floor other
	replies="$(probe_batch l4-front 200)"
	alpha="$(count_token "$replies" alpha)"
	bravo="$(count_token "$replies" bravo)"
	floor="$(count_token "$replies" floor)"
	other=$((200 - alpha - bravo - floor))

	[ "$floor" -eq 0 ] ||
		die "T1.1: $floor/200 probes still reached the FLOOR with a TCPRoute attached — the route did not replace the floor chain: $(histogram "$replies")"
	[ "$other" -eq 0 ] ||
		die "T1.1: $other/200 probes did not answer at all: $(histogram "$replies")"
	[ "$alpha" -ge 1 ] && [ "$bravo" -ge 1 ] ||
		die "T1.1: the split is not a split — one backend received nothing: $(histogram "$replies")"
	{ [ "$alpha" -ge 120 ] && [ "$alpha" -le 180 ]; } ||
		die "T1.1: alpha took $alpha/200 = $((alpha * 100 / 200))%, outside the [60%, 90%] band for a 75/25 split (see the sampling maths above): $(histogram "$replies")"
	ok "alpha $alpha/200, bravo $bravo/200, floor 0 — inside the 75/25 band"
}

# T1.2 — weight 0 is DRAIN, asserted EXACTLY and in BOTH directions.
#
# This is the #492 assertion. The bug that shipped normalised an explicit 0 to 1
# in l4RulesToWeightedClusters, which made "drain" mean "equal share": the
# drained backend kept taking ~50% of connections. Nothing catches that at the
# unit layer alone, and no upstream conformance profile tests it.
#
# Exact, not a band: drain is absolute, so a single connection to the drained
# backend is a failure.
#
# Mirrored, because a one-direction assertion is nearly free to pass by accident
# — if the projector were dropping l4-b for some unrelated reason (a bad
# ReferenceGrant decision, a name typo, an unclassified backend with no tcp:
# cluster), "bravo == 0" would be green while nothing about weights worked. The
# mirror makes each backend prove it can be both the only target and the only
# drained one.
#
# Power, measured against the real bug rather than assumed: with the #492 hunk
# reverted the weights become 100 and 1, so the drained backend takes ~2 of 200
# connections and this phase alone misses the bug about 14% of the time
# ((100/101)^200). That is why it is not the only drain assertion. T1.3 below is
# DETERMINISTIC under the same bug — 0/0 becomes 1/1 and the traffic splits
# ~50/50 between the backends instead of returning to the floor — so the two
# together catch #492 every time. Observed on 2026-09-20: T1.2a went red with
# "bravo took 1/200", T1.3's state measured alpha=19 bravo=31 out of 50.
verify_drain() {
	local replies alpha bravo

	log "T1.2a drain: l4-a weight 100 / l4-b weight 0 — bravo must be EXACTLY 0"
	apply_route 100 0
	await_all l4-front alpha 180 ||
		die "after draining l4-b, l4-front did not settle on 'alpha': $(await_all l4-front alpha 1)"
	replies="$(probe_batch l4-front 200)"
	alpha="$(count_token "$replies" alpha)"
	bravo="$(count_token "$replies" bravo)"
	[ "$bravo" -eq 0 ] ||
		die "T1.2a: bravo took $bravo/200 connections at weight 0 — an explicit weight 0 must mean DRAIN, not a share (this is exactly #492): $(histogram "$replies")"
	[ "$alpha" -eq 200 ] ||
		die "T1.2a: expected all 200 probes on alpha, got $(histogram "$replies")"
	ok "alpha 200/200, bravo 0/200 — weight 0 drains l4-b completely"

	log "T1.2b drain, mirrored: l4-a weight 0 / l4-b weight 100 — alpha must be EXACTLY 0"
	apply_route 0 100
	await_all l4-front bravo 180 ||
		die "after draining l4-a, l4-front did not settle on 'bravo': $(await_all l4-front bravo 1)"
	replies="$(probe_batch l4-front 200)"
	alpha="$(count_token "$replies" alpha)"
	bravo="$(count_token "$replies" bravo)"
	[ "$alpha" -eq 0 ] ||
		die "T1.2b: alpha took $alpha/200 connections at weight 0 — drain must work in both directions (#492): $(histogram "$replies")"
	[ "$bravo" -eq 200 ] ||
		die "T1.2b: expected all 200 probes on bravo, got $(histogram "$replies")"
	ok "bravo 200/200, alpha 0/200 — the drain is symmetric, so the counts come from the weights"
}

# T1.3 — every backend drained falls back to the FLOOR.
#
# buildWeightedTCPProxy returns nil for an empty weighted set and
# BuildCaptureTCPRouteFilterChain then falls back to buildCaptureTCPFloorFilterChain.
# That is the behaviour the #492 commit message claims and nothing checks end to
# end; it is also a second negative control, reached from the opposite direction
# to T1.0 (a route that exists but selects nothing).
verify_all_drained() {
	log "T1.3 all drained: both weights 0 — every probe must fall back to the FLOOR"
	apply_route 0 0
	await_all l4-front floor 180 ||
		die "with every backend drained, l4-front did not fall back to the floor: $(await_all l4-front floor 1) — an empty weighted set must collapse to the passthrough floor chain, not to a blackhole or a stale backend"
	local replies floor
	replies="$(probe_batch l4-front 50)"
	floor="$(count_token "$replies" floor)"
	[ "$floor" -eq 50 ] ||
		die "T1.3: expected 50/50 'floor', got $(histogram "$replies")"
	ok "50/50 probes on the floor: an all-zero weighted set collapses to the passthrough chain"
}

# T1.4 — delete the route.
#
# The third negative control, and the one that proves the ROUTE rather than the
# fixture was doing the work: the same client, the same name, the same port, no
# TCPRoute, back to the floor.
verify_delete() {
	log "T1.4 delete: remove the TCPRoute — traffic must return to the FLOOR"
	kc -n "$TEST_NS" delete tcproute l4-split >/dev/null || die "could not delete TCPRoute l4-split"
	await_all l4-front floor 180 ||
		die "l4-front did not return to the floor after the TCPRoute was deleted: $(await_all l4-front floor 1)"
	local replies floor
	replies="$(probe_batch l4-front 20)"
	floor="$(count_token "$replies" floor)"
	[ "$floor" -eq 20 ] ||
		die "T1.4: expected 20/20 'floor' after deleting the route, got $(histogram "$replies")"
	ok "20/20 probes back on the floor: the route, not the fixture, was doing the routing"
}

# --- T2: TLSRoute ------------------------------------------------------------

# tls_assert PHASE SNI WANT N — N TLS probes with SNI, all of which must answer
# WANT and nothing else. Exhaustive rather than "at least one": SNI selection is
# deterministic (a filter-chain match, not a load-balancing draw), so a single
# stray answer is a real defect and not sampling noise.
tls_assert() {
	local phase="$1" sni="$2" want="$3" n="$4" replies got
	replies="$(tls_probe_batch "$sni" "$n")"
	got="$(count_token "$replies" "$want")"
	[ "$got" -eq "$n" ] ||
		die "$phase: expected $n/$n '$want' for SNI $sni, got $(histogram "$replies")"
}

# T2.0 — the TLS negative control, and it runs FIRST, exactly as T1.0 does.
#
# With no TLSRoute the only portless chain on the parent's VIP is the floor, so
# every SNI must reach the parent itself. This is what establishes that the probe
# can distinguish the parent from its backends BEFORE any routing is asserted; if
# it could not, every count below would be meaningless and still green.
verify_tls_control() {
	log "T2.0 control: no TLSRoute — every SNI must reach the TLS FLOOR (l4tls-front itself)"
	require_crd TLSRoute tlsroutes
	# `|| true` so a missing Service produces the die below rather than `set -e`
	# aborting the run with no explanation (an assignment takes the substitution's
	# exit status).
	TLS_VIP="$(svc_vip l4tls-front || true)"
	[ -n "$TLS_VIP" ] ||
		die "the registrar never generated a mesh Service for l4tls-front, so there is no VIP to dial — the TLS workloads did not register"
	ok "l4tls-front VIP is $TLS_VIP (dialled on :$TLS_DIAL_PORT, which no filter chain claims)"

	await_probe tls_probe_batch "$SNI_ALPHA" tlsfloor 240 ||
		die "l4tls-front never answered 'tlsfloor' with no TLSRoute applied: $(await_probe tls_probe_batch "$SNI_ALPHA" tlsfloor 1) — either the TLS floor is not carrying traffic or the handshake never completed, and nothing below this line could mean anything"
	local sni
	for sni in "$SNI_ALPHA" "$SNI_BRAVO" "$SNI_NOMATCH"; do
		tls_assert "T2.0" "$sni" tlsfloor 20
	done
	ok "60/60 probes across three SNIs reached the TLS floor: SNI does not route until a TLSRoute says so"
}

# T2.1 / T2.2 — SNI selects the backend, asserted in BOTH directions.
#
# Both directions because a one-way assertion is nearly free to pass by accident:
# if the SNI chains were ignored entirely and chain 0 always won, "alpha answers
# tlsalpha" would be green while nothing about SNI worked. Each SNI proving it
# reaches a DIFFERENT backend is what makes the match, rather than the ordering,
# the thing being measured (#868's own correction).
verify_tls_sni() {
	log "T2.1/T2.2 SNI selection: $SNI_ALPHA -> l4tls-a, $SNI_BRAVO -> l4tls-b"
	apply_tls_routes
	await_probe tls_probe_batch "$SNI_ALPHA" tlsalpha 180 ||
		die "the TLSRoute for $SNI_ALPHA never took effect: $(await_probe tls_probe_batch "$SNI_ALPHA" tlsalpha 1) — 'tlsfloor' here means the route was accepted but its SNI chain never won the match, which is what happens when a destination_port-qualified chain claims the dialled port (see the header)"
	tls_assert "T2.1" "$SNI_ALPHA" tlsalpha 20
	ok "20/20 on SNI $SNI_ALPHA reached l4tls-a"

	await_probe tls_probe_batch "$SNI_BRAVO" tlsbravo 180 ||
		die "the TLSRoute for $SNI_BRAVO never took effect: $(await_probe tls_probe_batch "$SNI_BRAVO" tlsbravo 1)"
	tls_assert "T2.2" "$SNI_BRAVO" tlsbravo 20
	ok "20/20 on SNI $SNI_BRAVO reached l4tls-b — the SNI, not the chain order, selects the backend"
}

# T2.3 — an unmatched SNI falls through to the TCP floor, asserted POSITIVELY.
#
# This is the designed behaviour, not a gap: the SNI chains match
# ClusterIP/32 + server_names and sit alongside a chain matching the same /32
# with no server_names, so a connection whose SNI matches no rule selects the
# less specific chain. The /32 is what stops a TLSRoute for one service from
# hijacking another's connections carrying the same SNI, since a client can set
# any SNI it likes.
#
# Asserted as "reaches the floor backend", never as "does not reach A or B": an
# absence would also be satisfied by the connection being dropped, which is a
# different outcome entirely.
verify_tls_fallthrough() {
	log "T2.3 fall-through: SNI $SNI_NOMATCH matches no rule — it must reach the TLS FLOOR"
	await_probe tls_probe_batch "$SNI_NOMATCH" tlsfloor 180 ||
		die "an unmatched SNI did not reach the floor: $(await_probe tls_probe_batch "$SNI_NOMATCH" tlsfloor 1) — 'tlsalpha'/'tlsbravo' means an SNI chain matched a name it does not carry; 'NOREPLY' means the connection was dropped instead of falling through"
	tls_assert "T2.3" "$SNI_NOMATCH" tlsfloor 20
	# Re-assert the two routed SNIs in the SAME phase. Without this, a run in
	# which both TLSRoutes had silently stopped matching would read as a passing
	# fall-through: everything reaches the floor, which is exactly what T2.0
	# already showed. The three answers have to be different AT THE SAME TIME for
	# the fall-through to mean anything.
	tls_assert "T2.3" "$SNI_ALPHA" tlsalpha 10
	tls_assert "T2.3" "$SNI_BRAVO" tlsbravo 10
	ok "20/20 on an unmatched SNI reached the floor while the other two SNIs still reached their own backends"
}

# T2.4 — delete both TLSRoutes: every SNI returns to the floor.
#
# The third TLS negative control, and the one that proves the ROUTES rather than
# the fixture were doing the work.
verify_tls_delete() {
	log "T2.4 delete: remove both TLSRoutes — every SNI must return to the FLOOR"
	kc -n "$TEST_NS" delete tlsroute l4tls-alpha l4tls-bravo >/dev/null ||
		die "could not delete the TLSRoutes"
	await_probe tls_probe_batch "$SNI_ALPHA" tlsfloor 180 ||
		die "SNI $SNI_ALPHA did not return to the floor after its TLSRoute was deleted: $(await_probe tls_probe_batch "$SNI_ALPHA" tlsfloor 1)"
	local sni
	for sni in "$SNI_ALPHA" "$SNI_BRAVO"; do
		tls_assert "T2.4" "$sni" tlsfloor 20
	done
	ok "40/40 back on the TLS floor: the routes, not the fixture, were doing the routing"
}

# --- T3: UDPRoute ------------------------------------------------------------

# T3.0 — the UDP negative control: with no UDPRoute there is no UDP capture
# listener at all (GenerateUDPCaptureListener returns nil for an empty route
# set), so nothing answers.
#
# It is a weaker control than T1.0 — see udp_probe_batch for why NOREPLY has two
# possible causes — but it is a real assertion: an ANSWER here means either a
# listener outlived its routes or something outside the mesh is serving the VIP,
# and both are findings.
verify_udp_control() {
	log "T3.0 control: no UDPRoute — no datagram to l4udp-front:$MESH_UDP_PORT may be answered"
	require_crd UDPRoute udproutes
	local replies none
	replies="$(udp_probe_batch l4udp-front 10)"
	none="$(count_token "$replies" NOREPLY)"
	[ "$none" -eq 10 ] ||
		die "T3.0: expected 10/10 'NOREPLY' before any UDPRoute exists, got $(histogram "$replies") — something is answering UDP on the mesh VIP with no route to justify a listener"
	ok "10/10 datagrams unanswered: the UDP listener does not exist until a UDPRoute does"
}

# T3.1 — delivery over the plaintext UDP path, with the one weight shape both
# the current and the fixed implementation must agree on.
#
# 40 datagrams, ALL of which must be answered by l4udp-a and none by l4udp-b. It
# is exhaustive rather than a band because this is not a per-datagram draw under
# either implementation: today udp_proxy carries a single cluster, and under a
# weight-honouring one weight 0 means DRAIN, not a share (#492). A single
# 'udpbravo' therefore fails under both, which is what lets this assertion
# survive #873's fix unchanged.
#
# What it does NOT assert, deliberately: that UDP selects BETWEEN backends or
# between services. That is #873, and asserting it today would fail by design.
verify_udp_delivery() {
	log "T3.1 delivery: UDPRoute l4udp-a weight 100 / l4udp-b weight 0 — 40 datagrams, all to l4udp-a"
	apply_udp_route 100 0
	await_probe udp_probe_batch l4udp-front udpalpha 180 ||
		die "the UDPRoute never carried a datagram: $(await_probe udp_probe_batch l4udp-front udpalpha 1) — 'NOREPLY' is the UDP path failing at one of three places that nothing else here distinguishes: the CNI's nftables UDP REDIRECT inside the pod netns, udp_proxy receiving on the redirected socket, or conntrack un-DNATing the reply back to VIP:$MESH_UDP_PORT so the client's connected socket accepts it. Check the agent log for 'UDPRoute input discarded' first — a discarded backend is a different failure from a broken data path"

	local replies alpha bravo none other
	replies="$(udp_probe_batch l4udp-front 40)"
	alpha="$(count_token "$replies" udpalpha)"
	bravo="$(count_token "$replies" udpbravo)"
	none="$(count_token "$replies" NOREPLY)"
	other=$((40 - alpha - bravo - none))

	[ "$none" -eq 0 ] ||
		die "T3.1: $none/40 datagrams went unanswered — the UDP path is lossy, not absent: $(histogram "$replies")"
	[ "$other" -eq 0 ] ||
		die "T3.1: $other/40 datagrams were answered by something that is neither backend: $(histogram "$replies")"
	[ "$bravo" -eq 0 ] ||
		die "T3.1: l4udp-b answered $bravo/40 datagrams at weight 0 — weight 0 is DRAIN (#492), and today's single-cluster udp_proxy should not reach it either: $(histogram "$replies")"
	[ "$alpha" -eq 40 ] ||
		die "T3.1: expected 40/40 'udpalpha', got $(histogram "$replies")"
	ok "40/40 datagrams answered by l4udp-a, 0 by the weight-0 backend (plaintext — no mTLS on this leg, by design)"
}

# T3.2 — delete the UDPRoute: the listener goes away and nothing answers again.
#
# The second UDP negative control, reached from the opposite direction to T3.0,
# and the one that proves the ROUTE rather than the fixture was carrying the
# datagrams.
verify_udp_delete() {
	log "T3.2 delete: remove the UDPRoute — datagrams must go unanswered again"
	kc -n "$TEST_NS" delete udproute l4udp-split >/dev/null || die "could not delete UDPRoute l4udp-split"
	local replies none deadline=$((SECONDS + 180))
	while true; do
		replies="$(udp_probe_batch l4udp-front 10)"
		none="$(count_token "$replies" NOREPLY)"
		if [ "$none" -eq 10 ]; then
			break
		fi
		[ "$SECONDS" -lt "$deadline" ] ||
			die "T3.2: datagrams are still answered after the UDPRoute was deleted: $(histogram "$replies") — the UDP capture listener outlived its route"
		sleep 5
	done
	ok "10/10 datagrams unanswered again: the route, not the fixture, was carrying them"
}

verify() {
	verify_control
	verify_split
	verify_drain
	verify_all_drained
	verify_delete
	log "all TCPRoute assertions passed (control, 75/25 band, mirrored weight-0 drain, all-drained floor, delete)"

	verify_tls_control
	verify_tls_sni
	verify_tls_fallthrough
	verify_tls_delete
	log "all TLSRoute assertions passed (control, SNI both directions, positive fall-through, delete)"

	verify_udp_control
	verify_udp_delivery
	verify_udp_delete
	log "all UDPRoute assertions passed (control, plaintext delivery with a weight-0 backend drained, delete)"
}

down() {
	log "tearing down"
	kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	docker rm -f "$ETCD_NAME" >/dev/null 2>&1 || true
	ok "cluster '$CLUSTER' and its etcd removed"
}

up() {
	raise_inotify
	build_images
	create_cluster
	start_etcd
	load_images
	install_gwapi_crds
	install_spire
	install_aether
	deploy_workloads
}

case "${1:-}" in
up) up ;;
test) verify ;;
verify) verify ;;
down) down ;;
"") up && verify ;;
*) die "usage: $0 {up|test|verify|down}" ;;
esac
