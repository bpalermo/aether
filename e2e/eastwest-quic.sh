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
#   E1  fan-out     a quic: twin exists for every (allow-listed destination x
#                   source ServiceAccount), and NONE for the h2-only destination
#   E2  h3 + id     client-a and client-b (DIFFERENT ServiceAccounts) each call
#                   quic-a and quic-b: every request answers 200, the destination
#                   sees the CALLER'S OWN SPIFFE ID in x-forwarded-client-cert,
#                   and the request rode the caller's own quic: twin over HTTP/3
#                   (never the other source's twin, never the h2 cluster)
#   E3  h2-only     h2only is not allow-listed: no quic: twin, requests succeed
#                   with the caller's identity, and its HTTP/3 inbound stays idle
#   E4  GAMMA       gamma-a (allow-listed) with a weighted HTTPRoute canary to
#                   gamma-a / gamma-b (both allow-listed, twins present) stays on
#                   h2 — #956 leaves WeightedClusters routes alone by design
#   E5  Q3          client-a's pod is deleted and comes back with a NEW pod IP;
#                   its requests still carry client-a's identity over HTTP/3, and
#                   client-b's still carry client-b's
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
# `verify` goes red at E1 because nothing is allow-listed, so no quic: cluster
# exists at all. That is the honest name of the failure: it is not a broken
# data path, it is the feature not being asked for. The red-then-green:
#
#   EASTWEST_QUIC=off e2e/eastwest-quic.sh up && e2e/eastwest-quic.sh verify  # RED at E1
#   e2e/eastwest-quic.sh up && e2e/eastwest-quic.sh verify                    # GREEN
#
# (`up` is re-runnable: the second one only `helm upgrade`s aether with the
# allow-list, which rolls the agent. Never --reuse-values: the allow-list must
# come from THIS invocation.) If E1 is forced past, E2 is red too: no twin moves
# and the h3 listener stays at zero.
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
# Usage: e2e/eastwest-quic.sh {up|test|verify|down}   (bare = up + verify)
#
# Prereqs: kind, docker, kubectl, helm, bazel (for the image build; CI sets
# EWQ_SKIP_BUILD=1 and pre-loads the images from the nightly build artifact).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER="${EWQ_CLUSTER:-eastwest-quic}"
CTX="kind-$CLUSTER"
NODE="$CLUSTER-control-plane"
NS="aether-system"
TEST_NS="aether-test"
MESH_DOMAIN="aether.internal"
TRUST_DOMAIN="aether.internal"
# on (default) = the allow-list below is installed; off = the same install with
# NO allow-list, for the red half of red-then-green (see the header).
EASTWEST_QUIC="${EASTWEST_QUIC:-on}"
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
	kind create cluster --config "$cfg" --wait 60s >/dev/null
	rm -f "$cfg"
	ok "cluster '$CLUSTER' ready"
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
	img() { echo "--set $1.image.repository=ghcr.io/bpalermo/aether/$2 --set $1.image.tag=latest --set $1.image.digest= --set $1.image.pullPolicy=Never"; }
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

	log "installing the aether CRDs"
	helm --kube-context "$CTX" upgrade --install aether-crds "$charts/crds" \
		-n "$NS" --create-namespace --wait --timeout 2m >/dev/null || die "crds chart install failed"

	# Everything except spire and the allow-list is chart default (kubernetes
	# registry backend, capture + mesh DNS, GAMMA on). No --reuse-values: the
	# allow-list must be exactly what THIS invocation says, so on -> off -> on
	# re-runs of `up` really toggle it.
	log "installing aether (SPIRE ON; EASTWEST_QUIC=$EASTWEST_QUIC)"
	# shellcheck disable=SC2046
	helm --kube-context "$CTX" upgrade --install aether "$charts/aether" \
		-n "$NS" --create-namespace \
		--set namespace.create=false \
		--set "meshDomain=$MESH_DOMAIN" \
		--set spire.enabled=true \
		--set edge.enabled=false \
		"${quic[@]}" \
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
	ok "aether up (allow-list: $(agent_quic_args | tr '\n' ' '))"
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
	local listeners d pod v
	listeners="$(admin /listeners)"
	for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}" "$H2_DST"; do
		pod="$(pod_of "$d")"
		[ -n "$pod" ] || die "E0: no Running pod for destination $d"
		printf '%s\n' "$listeners" | awk -F'::' -v l="inbound_${pod}_h3" '$1 == l { f = 1 } END { exit !f }' ||
			die "E0: the HTTP/3 inbound listener inbound_${pod}_h3 does not exist — #953's inbound is unconditional with SPIRE on, so this is the inbound (or the pod's SVID) missing, not the allow-list"
		v="$(h3_rq "$pod")"
		[ -n "$v" ] ||
			die "E0: $(h3_stat "$pod") is not in /stats — every 'h3 delta == 0' below would be reading an absent stat; fix the stat name before trusting this suite (#853)"
	done
	ok "HTTP/3 inbound listeners + counters present for every destination (they exist whether or not anything is allow-listed)"
}

verify_fanout() {
	log "E1 fan-out: a quic: twin for every (allow-listed destination x source SA), none for $H2_DST"
	local deadline=$((SECONDS + 180)) dump missing d s
	while true; do
		dump="$(admin /clusters)"
		missing=""
		for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}"; do
			for s in "${SOURCES[@]}"; do
				has_cluster "$dump" "$(twin "$d" "$s")" || missing="$missing $(twin "$d" "$s")"
			done
		done
		[ -z "$missing" ] && break
		[ "$SECONDS" -lt "$deadline" ] ||
			die "E1: quic: twins missing after 180s:$missing — agent allow-list: [$(agent_quic_args | tr '\n' ' ')]. With EASTWEST_QUIC=off this is the EXPECTED red: nothing is allow-listed, so no destination gets a quic: cluster (the header's red-then-green). With it on, read the agent's 'east-west QUIC fan-out' log line: twins exist only once the node identity is served"
		sleep 5
	done
	local strays
	strays="$(printf '%s\n' "$dump" | awk -F'::' -v p="quic:$(fqdn "$H2_DST")@" 'index($1, p) == 1 { print $1 }' | sort -u)"
	[ -z "$strays" ] || die "E1: the NOT allow-listed $H2_DST has quic: twins: $strays"
	ok "$((${#QUIC_DSTS[@]} * ${#SOURCES[@]} + ${#GAMMA_DSTS[@]} * ${#SOURCES[@]})) twins present (e.g. $(twin "${QUIC_DSTS[0]}" "${SOURCES[0]}")); none for $H2_DST"
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
# a WeightedClusters action is left alone, so GAMMA traffic stays h2 in this
# cut. Note what this does NOT cover, by design: a single-backend GAMMA rule
# whose backend is the parent itself renders as `cluster:` and WOULD be
# rewritten to QUIC — the weighted shape is the one #956 promises stays h2.
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

verify() {
	verify_preflight
	verify_fanout
	verify_quic
	verify_h2only
	verify_gamma
	verify_q3
	log "all east-west QUIC assertions passed (fan-out, per-source HTTP/3 + XFCC, h2-only untouched, GAMMA stays h2, identity across a client address change)"
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

case "${1:-}" in
up) up ;;
test) verify ;;
verify) verify ;;
down) down ;;
"") up && verify ;;
*) die "usage: $0 {up|test|verify|down}" ;;
esac
