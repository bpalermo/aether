#!/usr/bin/env bash
# Single-cluster kind e2e for proposal 038 Phase 4 (east-west QUIC): the
# per-pod HTTP/3 inbound on UDP:18008 (#953, unconditional) and the per-source
# `quic:<svc>.<ns>.<domain>@<ns>/<sa>` clusters selected by the source SPIFFE
# ID (#956, opt-in per destination via agent.eastWestQuicServices).
#
# What it proves, on a real data path (kind + the real chart + the real
# aether-proxy + SPIRE + the CNI's capture):
#
#   E0  preflight   every destination pod has its HTTP/3 inbound listener
#                   (inbound_<pod>_h3) and the per-listener request counter this
#                   suite reads exists — so every "== 0" below is a reading, not
#                   an absent stat (#853)
#   E1  fan-out     twins are DEMAND-SCOPED (aether#1020): before any request
#                   no pair has dialled, so no quic: twin exists (on a fresh
#                   cluster: zero), and none ever for the h2-only destination
#   E2  h3 + id     client-a and client-b (DIFFERENT ServiceAccounts) each call
#                   quic-a and quic-b: every request answers 200, the destination
#                   sees the CALLER'S OWN SPIFFE ID in x-forwarded-client-cert,
#                   and the request rode the caller's own quic: twin over HTTP/3
#                   (never the other source's twin, never the h2 cluster)
#   E3  h2-only     h2only is not allow-listed: no quic: twin, requests succeed
#                   with the caller's identity, and its HTTP/3 inbound stays idle
#   E4  GAMMA       gamma-a (allow-listed) with a weighted HTTPRoute canary to
#                   gamma-a / gamma-b (both allow-listed) stays on h2 — the
#                   matcher action names ONE cluster, so a weighted split has no
#                   per-source form (#961) — and so never fetches a twin
#   E4b GAMMA       a single-backendRef HTTPRoute rule to the parent (gamma-a)
#                   renders as `cluster:` and IS selected: it rides the caller's
#                   own quic: twin over HTTP/3 like the default route (#961)
#   E4c pairs       the node's quic: cluster count EQUALS the (source,
#                   destination) pairs this suite drove over a selecting route
#                   (E2 + E4b = 2 sources x 3 destinations = 6), and the set is
#                   exactly those pairs: a twin is built only for a pair that has
#                   dialled (aether#1020; the pre-#1020 agent built every local
#                   SA x allow-listed destination: 7 x 4 = 28 on this node)
#   E5  Q3          client-a's pod is deleted and comes back with a NEW pod IP;
#                   its requests still carry client-a's identity over HTTP/3, and
#                   client-b's still carry client-b's
#   E6  id gate     a pod under a ServiceAccount that did not exist a second
#                   earlier sends its FIRST request the instant its container
#                   starts (#1053). The webhook-injected aether-identity-ready
#                   init container ran and exited 0 BEFORE the app started, and
#                   that first request answers 200 carrying the new SA's own
#                   SPIFFE ID
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
#   destination side `listener.inbound_<pod>_h3.http.inbound.downstream_rq_2xx` —
#                    the per-listener HCM counter of the pod's QUIC listener
#                    (proxy.NewInboundQUICListener: stat_prefix inbound_<pod>_h3;
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
# SEEN RED WITH THE FEATURE OFF. `up` takes EASTWEST_QUIC (default on). With
# EASTWEST_QUIC=off the chart is installed with NO allow-list — everything else
# identical, including the unconditional HTTP/3 inbound and the DNS SANs — and
# `verify` goes red at E2: nothing is allow-listed, so no route selects a twin,
# the caller's first request never fetches one, and every request rides h2.
# (E1 passes: since aether#1020 "no twin before any request" is the expected
# state either way.) That is the honest name of the failure: it is not a broken
# data path, it is the feature not being asked for. The red-then-green:
#
#   EASTWEST_QUIC=off e2e/eastwest-quic.sh up && e2e/eastwest-quic.sh verify  # RED at E2
#   e2e/eastwest-quic.sh up && e2e/eastwest-quic.sh verify                    # GREEN
#
# (`up` is re-runnable: the second one only `helm upgrade`s aether with the
# allow-list, which rolls the agent. Never --reuse-values: the allow-list must
# come from THIS invocation.)
#
# E6 WITH THE GATE OFF (IDENTITY_GATE=off installs the chart with
# controller.webhook.identityGate.enabled=false) is a report, not a red: no init
# container is injected and the app's t=0 request races SPIRE's entry sync. On
# kind (2026-09-28) SPIRE issued the SVID 3.9-5.9 s after the agent's subscribe
# (two svids=0 updates first, the talos-main shape) and the first request
# STALLED 3.4-5.4 s waiting for the pod's client certificate before answering
# 200; with the gate on it answered in 12-14 ms after the gate held the pod
# 3.6-4.1 s. On talos-main the same window is ~7.5 s, which crosses the mesh
# cluster connect_timeout and becomes 503 UF (#1053). E6 prints the first
# request's status and latency either way and asserts only the gate-on
# contract (init container injected first, exited 0, first request 200 as the
# new ServiceAccount).
#
#   IDENTITY_GATE=off e2e/eastwest-quic.sh up && e2e/eastwest-quic.sh idgate  # the stall
#   e2e/eastwest-quic.sh up && e2e/eastwest-quic.sh idgate                    # GREEN
#
# E4c's pair count is seen red by asserting the pre-#1020 count against a
# #1020 agent: EWQ_EXPECT_TWINS=28 e2e/eastwest-quic.sh verify goes red at E4c
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
# (`<sa>.<ns>.<meshDomain>`, `*.<sa>.<ns>.<meshDomain>`) — without them every
# HTTP/3 handshake fails the QUIC client's hostname check and E2 goes red with
# the twins present (that shape is the one to look for in the proxy log).
#
# Usage: e2e/eastwest-quic.sh {up|test|verify|idgate|down}   (bare = up + verify)
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
# <registry>/<namespace> every aether image is tagged under, from the single
# setting in bazel/img/registry.bzl (proposal 040) -- never a literal.
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
# on (default) = the allow-list below is installed; off = the same install with
# NO allow-list, for the red half of red-then-green (see the header).
EASTWEST_QUIC="${EASTWEST_QUIC:-on}"
# on (default) = the chart's default egress identity gate (#1053); off = the
# same install with controller.webhook.identityGate.enabled=false (E6's red arm).
IDENTITY_GATE="${IDENTITY_GATE:-on}"
# The mesh VIP Service port every client dials through mesh DNS
# (meshconst.ProxyOutboundPort); the capture route claims "<fqdn>:18081".
OUTBOUND_PORT="18081"
# The application port every destination binds and registers.
APP_PORT="8080"
# Destinations. QUIC_DSTS and GAMMA_DSTS are allow-listed; H2_DST is not.
QUIC_DSTS=(quic-a quic-b)
GAMMA_DSTS=(gamma-a gamma-b)
H2_DST="h2only"
# Sources, each its own ServiceAccount (= its own SPIFFE ID).
SOURCES=(client-a client-b)
# Requests per measured batch.
BATCH=10
AGNHOST_IMAGE="registry.k8s.io/e2e-test-images/agnhost:2.53"
CURL_IMAGE="curlimages/curl:8.22.0"
GWAPI_VERSION="v1.6.2"
# SPIRE >= 1.15.2 is required since proposal 036 (the SPIFFE Broker API); the
# same chart pins e2e/l4routes.sh uses.
SPIRE_CHART_VERSION="${SPIRE_CHART_VERSION:-0.30.2}"
SPIRE_CRDS_VERSION="${SPIRE_CRDS_VERSION:-0.6.1}"
SPIRE_CLASS="spire-mgmt-spire" # spire-controller-manager class (namespace-release)
IMAGES=(agent mesh-dns proxy-supervisor cni-install registrar controller)
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

# The allow-list as the agent actually received it — the first thing to read
# when E1 is red.
agent_quic_args() {
	kc -n "$NS" get ds aether-agent -o jsonpath='{.spec.template.spec.containers[*].args}' 2>/dev/null |
		tr ',' '\n' | grep -o 'east-west-quic-services=[^"]*' || echo "(none: nothing is allow-listed)"
}

# Every failure here is one of "the twin was never published", "the twin exists
# but the handshake fails" (#957's shape), "the route never selects the twin",
# or "the identity is wrong" — dump what separates the four (#590).
dump_state() {
	kubectl config get-contexts "$CTX" >/dev/null 2>&1 || return 0
	printf '\033[1;33m  -- pods --\033[0m\n' >&2
	kc get pods -A -o wide 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- agent allow-list (EASTWEST_QUIC=%s) --\033[0m\n' "$EASTWEST_QUIC" >&2
	agent_quic_args | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- agent log: east-west QUIC fan-out budget --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=agent --all-containers --tail=600 --prefix 2>&1 |
		grep -i "east-west QUIC" | tail -10 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- envoy: quic: clusters and their host rq_total/cx_connect_fail --\033[0m\n' >&2
	admin /clusters 2>/dev/null | grep -E '^quic:[^:]*::[0-9.]+:[0-9]+::(rq_total|cx_total|cx_connect_fail)::' |
		head -60 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- envoy: HTTP/3 inbound listeners --\033[0m\n' >&2
	admin /listeners 2>/dev/null | grep '_h3::' | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- envoy: _h3 listener + http3 upstream counters --\033[0m\n' >&2
	{
		admin '/stats?filter=_h3' 2>/dev/null
		admin '/stats?filter=upstream_cx_http3_total' 2>/dev/null
	} | grep -E '(downstream_cx_total|downstream_rq_2xx|downstream_rq_5xx|upstream_cx_http3_total)' |
		head -60 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- httproutes --\033[0m\n' >&2
	kc get httproutes -A -o yaml 2>&1 | sed 's/^/    /' >&2 || true
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
		//cni/cmd/cni-install //registrar/cmd/registrar //controller/cmd/controller; do
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
		-e "s#POD_SUBNET#10.30.0.0/16#g" \
		-e "s#SVC_SUBNET#10.130.0.0/16#g" \
		"$REPO_ROOT/e2e/kind-cluster.yaml" >"$cfg"
	if [ "${EWQ_WORKER:-0}" = "1" ]; then
		printf '  - role: worker\n    labels:\n      topology.kubernetes.io/region: local\n      topology.kubernetes.io/zone: %s\n' "$CLUSTER" >>"$cfg"
	fi
	kind create cluster --config "$cfg" --wait 60s >/dev/null
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
		kind load docker-image "${IMAGE_REGISTRY}/${img}:latest" --name "$CLUSTER" >/dev/null 2>&1 ||
			die "could not load ${IMAGE_REGISTRY}/${img}:latest into kind (was it built?)"
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
# the dnsNameTemplates aether#957 requires (see the header).
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
  dnsNameTemplates:
    - "{{ .PodSpec.ServiceAccountName }}.{{ .PodMeta.Namespace }}.$MESH_DOMAIN"
    - "*.{{ .PodSpec.ServiceAccountName }}.{{ .PodMeta.Namespace }}.$MESH_DOMAIN"
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
	local charts
	charts="$(chart_dir)"
	img() { echo "--set $1.image.repository=${IMAGE_REGISTRY}/$2 --set $1.image.tag=latest --set $1.image.digest= --set $1.image.pullPolicy=Never"; }
	# The allow-list: both QUIC destinations AND both GAMMA destinations (E4
	# needs twins present for the GAMMA backends, so that "stays h2" is a choice
	# the route makes, not an absence). h2only is deliberately NOT listed. Each
	# entry renders one --east-west-quic-services=<ns>/<svc> agent arg
	# (charts/aether/templates/agent-daemonset.yaml).
	local quic=() i=0 svc
	case "$EASTWEST_QUIC" in
	on)
		for svc in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}"; do
			quic+=(--set "agent.eastWestQuicServices[$i]=$TEST_NS/$svc")
			i=$((i + 1))
		done
		;;
	off) ;;
	*) die "EASTWEST_QUIC must be 'on' or 'off', got '$EASTWEST_QUIC'" ;;
	esac
	local gate=()
	case "$IDENTITY_GATE" in
	on) ;;
	off) gate=(--set controller.webhook.identityGate.enabled=false) ;;
	*) die "IDENTITY_GATE must be 'on' or 'off', got '$IDENTITY_GATE'" ;;
	esac

	log "installing the aether CRDs"
	helm --kube-context "$CTX" upgrade --install aether-crds "$charts/crds" \
		-n "$NS" --create-namespace --wait --timeout 2m >/dev/null || die "crds chart install failed"

	# Everything except spire and the allow-list is chart default (kubernetes
	# registry backend, capture + mesh DNS, GAMMA on). No --reuse-values: the
	# allow-list must be exactly what THIS invocation says, so on -> off -> on
	# re-runs of `up` really toggle it.
	log "installing aether (SPIRE ON; EASTWEST_QUIC=$EASTWEST_QUIC; IDENTITY_GATE=$IDENTITY_GATE)"
	# shellcheck disable=SC2046
	helm --kube-context "$CTX" upgrade --install aether "$charts/aether" \
		-n "$NS" --create-namespace \
		--set namespace.create=false \
		--set "meshDomain=$MESH_DOMAIN" \
		--set spire.enabled=true \
		--set edge.enabled=false \
		"${quic[@]}" \
		"${gate[@]+"${gate[@]}"}" \
		"${EWQ_EXTRA_HELM_ARGS[@]+"${EWQ_EXTRA_HELM_ARGS[@]}"}" \
		$(img agent agent) $(img agent.meshDnsDaemon mesh-dns) \
		$(img proxy.supervisor proxy-supervisor) $(img cniInstall cni-install) \
		$(img registrar registrar) $(img controller controller) \
		--set proxy.image.pullPolicy=IfNotPresent \
		$([ "${EWQ_LOCAL_PROXY:-0}" = "1" ] && img proxy proxy) \
		--timeout 6m >/dev/null || die "aether install failed"
	rm -rf "$(dirname "$charts")"

	kc -n "$NS" rollout status ds/aether-agent --timeout=300s >/dev/null || die "the agent DaemonSet never became Ready"
	kc -n "$NS" rollout status ds/aether-mesh-dns --timeout=180s >/dev/null || die "the mesh-DNS DaemonSet never became Ready"
	kc -n "$NS" rollout status deploy/aether-registrar --timeout=180s >/dev/null || die "the registrar never became Ready"
	kc -n "$NS" rollout status deploy/aether-controller --timeout=180s >/dev/null || die "the controller never became Ready"
	ok "aether up (allow-list: $(agent_quic_args | tr '\n' ' '))"
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
	for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}" "$H2_DST"; do
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
	log "deploying destinations (${QUIC_DSTS[*]} ${GAMMA_DSTS[*]} $H2_DST) and sources (${SOURCES[*]})"
	kc create ns "$TEST_NS" >/dev/null 2>&1 || true
	local d
	for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}" "$H2_DST"; do deploy_destination "$d"; done
	for d in "${SOURCES[@]}"; do deploy_source "$d"; done
	for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}" "$H2_DST" "${SOURCES[@]}"; do
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
		awk '$2 == "<none>" && $3 == "Running" { print $1; exit }'
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
h3_stat() { printf 'listener.inbound_%s_h3.http.inbound.downstream_rq_2xx' "$1"; }
h3_rq() {
	local name
	name="$(h3_stat "$1")"
	admin "/stats?filter=inbound_$1_h3" 2>/dev/null | awk -v n="$name:" '$1 == n { print $2; exit }'
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
		for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}" "$H2_DST"; do
			pod="$(pod_of "$d")"
			[ -n "$pod" ] || die "E0: no Running pod for destination $d"
			printf '%s\n' "$listeners" | awk -F'::' -v l="inbound_${pod}_h3" '$1 == l { f = 1 } END { exit !f }' || missing="$missing inbound_${pod}_h3"
		done
		[ -z "$missing" ] && break
		[ "$SECONDS" -lt "$deadline" ] ||
			die "E0: HTTP/3 inbound listener(s) still absent after 180s:$missing — #953's inbound is unconditional with SPIRE on, so this is the inbound (or the pod's SVID) missing, not the allow-list and not timing"
		sleep 5
	done
	for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}" "$H2_DST"; do
		pod="$(pod_of "$d")"
		v="$(h3_rq "$pod")"
		[ -n "$v" ] ||
			die "E0: $(h3_stat "$pod") is not in /stats — every 'h3 delta == 0' below would be reading an absent stat; fix the stat name before trusting this suite (#853)"
	done
	ok "HTTP/3 inbound listeners + counters present for every destination (they exist whether or not anything is allow-listed)"
}

# quic_twins DUMP — the distinct quic: cluster names in a /clusters dump, sorted.
quic_twins() { printf '%s\n' "$1" | awk -F'::' 'index($1, "quic:") == 1 { print $1 }' | sort -u; }

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
	log "E1 fan-out: twins are demand-scoped (aether#1020) — before any request, none outside the pairs this suite drives; never one for $H2_DST"
	local dump present strays
	dump="$(admin /clusters)"
	present="$(quic_twins "$dump")"
	# A re-run of verify finds the previous run's pairs (they are persisted
	# in the agent's observed set, by design); anything OUTSIDE the driven set
	# was built for a pair that never dialled.
	strays="$(comm -23 <(printf '%s\n' "$present" | sed '/^$/d') <(driven_pairs))"
	[ -z "$strays" ] ||
		die "E1: quic: twins exist for pairs that never dialled: $strays — the agent is building twins up front (the pre-#1020 fan-out: every local SA x allow-listed destination)"
	printf '%s\n' "$present" | grep -q "^quic:$(fqdn "$H2_DST")@" && die "E1: the NOT allow-listed $H2_DST has a quic: twin"
	ok "$(printf '%s\n' "$present" | sed '/^$/d' | wc -l | tr -d ' ') quic: twins before any request in this run (0 on a fresh cluster); none for a pair that never dialled; none for $H2_DST"
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
			die "$phase: $src -> $dst never converged onto $(twin "$dst" "$src"): replies $(summarize "$replies"); twin rq_total $(host_rq "$d0" "$(twin "$dst" "$src")") -> $(host_rq "$d1" "$(twin "$dst" "$src")"). 200s with the right URI but a still twin = the route never selected the twin (h2 fallback); 503s with a moving twin = the QUIC handshake fails (the #957 DNS-SAN shape — check the proxy log)"
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

# --- E3: the h2-only destination ---------------------------------------------

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

verify_h2only() {
	log "E3 h2-only: $H2_DST is not allow-listed — no twin, still served, over h2"
	local s
	for s in "${SOURCES[@]}"; do
		assert_h2 E3 "$s" "$H2_DST"
	done
}

# --- E4: GAMMA-routed destination stays on h2 --------------------------------

# A weighted canary parented to gamma-a (both backends allow-listed). #956
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

verify_gamma() {
	log "E4 GAMMA: weighted HTTPRoute on ${GAMMA_DSTS[0]} (${GAMMA_DSTS[0]} 50 / ${GAMMA_DSTS[1]} 50, both allow-listed) — stays h2"
	apply_gamma_route
	# Converge on the DATA, not the status: the route is live once a request to
	# the parent is answered by the second backend's pod (agnhost /hostname).
	local b_pod deadline=$((SECONDS + 180)) replies
	b_pod="$(pod_of "${GAMMA_DSTS[1]}")"
	while true; do
		replies="$(req_batch "${SOURCES[0]}" "${GAMMA_DSTS[0]}" /hostname 20)"
		printf '%s\n' "$replies" | awk -v p="$b_pod" '$1 == "200" && $2 == p { f = 1 } END { exit !f }' && break
		[ "$SECONDS" -lt "$deadline" ] ||
			die "E4: the HTTPRoute never took effect — no request to ${GAMMA_DSTS[0]} reached ${GAMMA_DSTS[1]} ($b_pod) in 180s: $(summarize "$replies")"
		sleep 5
	done
	ok "the weighted route is live (requests to ${GAMMA_DSTS[0]} reach $b_pod)"
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
    config.aether.io/upstreams: "$H2_DST.$TEST_NS"
spec:
  serviceAccountName: $sa
  restartPolicy: Never
  containers:
    - name: curl
      image: $CURL_IMAGE
      command: ["sh", "-c"]
      args:
        - |
          out=\$(curl -s --max-time 30 -w '\\n%{http_code} %{time_total}' "http://$(fqdn "$H2_DST"):$OUTBOUND_PORT/header?key=X-Forwarded-Client-Cert")
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

verify() {
	verify_preflight
	verify_fanout
	verify_quic
	verify_h2only
	verify_gamma
	verify_gamma_single
	verify_pairs
	verify_q3
	verify_identity_gate
	log "all east-west QUIC assertions passed (demand-scoped twins, per-source HTTP/3 + XFCC, h2-only untouched, GAMMA stays h2, twins = driven pairs, identity across a client address change, the identity gate holds a new pod until its SVID exists)"
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
down) down ;;
"") up && verify ;;
*) die "usage: $0 {up|test|verify|idgate|down}" ;;
esac
