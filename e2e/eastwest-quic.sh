#!/usr/bin/env bash
# Single-cluster kind e2e for proposal 038 Phase 4 (east-west QUIC): the
# per-pod HTTP/3 inbound on UDP:18008 (#953, unconditional) and the per-source
# `quic:<svc>.<ns>.<domain>@<ns>/<sa>` clusters selected by the source SPIFFE
# ID (#956; unconditional since the proving allow-list was removed: EVERY
# destination in the node's dependency set gets its twins).
#
# What it proves, on a real data path (kind + the real chart + the real
# aether-proxy + SPIRE + the CNI's capture):
#
#   E0  preflight   every destination pod has its HTTP/3 inbound listener
#                   (inbound_<pod>_h3) and the per-listener request counter this
#                   suite reads exists — so every "== 0" below is a reading, not
#                   an absent stat (#853)
#   E1  fan-out     a quic: twin exists for every (destination x source
#                   ServiceAccount) -- nothing is listed anywhere, so this is
#                   the unconditional fan-out read off the real proxy
#   E2  h3 + id     client-a and client-b (DIFFERENT ServiceAccounts) each call
#                   quic-a and quic-b: every request answers 200, the destination
#                   sees the CALLER'S OWN SPIFFE ID in x-forwarded-client-cert,
#                   and the request rode the caller's own quic: twin over HTTP/3
#                   (never the other source's twin, never the h2 cluster)
#   E4  GAMMA       gamma-a with a weighted HTTPRoute canary to gamma-a /
#                   gamma-b (twins present for both) stays on h2 — the matcher
#                   action names ONE cluster, so a weighted split has no
#                   per-source form (#961). (There is no E3 any more: it was the
#                   h2-only, not-allow-listed destination, and no such
#                   destination exists now.)
#   E4b GAMMA       a single-backendRef HTTPRoute rule to the parent (gamma-a)
#                   renders as `cluster:` and IS selected: it rides the caller's
#                   own quic: twin over HTTP/3 like the default route (#961)
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
# Usage: e2e/eastwest-quic.sh {up|test|verify|down}   (bare = up + verify)
#
# Prereqs: kind, docker, kubectl, helm, bazel (for the image build; CI sets
# EWQ_SKIP_BUILD=1 and pre-loads the images from the nightly build artifact).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# <registry>/<namespace> every aether image is tagged under, from the single
# setting in bazel/img/registry.bzl (proposal 040) -- never a literal.
IMAGE_REGISTRY="$("$REPO_ROOT/scripts/image-registry.sh" prefix)"
CLUSTER="${EWQ_CLUSTER:-eastwest-quic}"
CTX="kind-$CLUSTER"
NODE="$CLUSTER-control-plane"
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
# The mesh VIP Service port every client dials through mesh DNS
# (meshconst.ProxyOutboundPort); the capture route claims "<fqdn>:18081".
OUTBOUND_PORT="18081"
# The application port every destination binds and registers.
APP_PORT="8080"
# Destinations. Every one of them gets quic: twins — nothing is listed.
QUIC_DSTS=(quic-a quic-b)
GAMMA_DSTS=(gamma-a gamma-b)
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
	img() { echo "--set $1.image.repository=${IMAGE_REGISTRY}/$2 --set $1.image.tag=latest --set $1.image.digest= --set $1.image.pullPolicy=Never"; }
	log "installing the aether CRDs"
	helm --kube-context "$CTX" upgrade --install aether-crds "$charts/crds" \
		-n "$NS" --create-namespace --wait --timeout 2m >/dev/null || die "crds chart install failed"

	# Everything except spire is chart default (kubernetes registry backend,
	# capture + mesh DNS, GAMMA on, east-west QUIC unconditional). Never
	# --reuse-values.
	log "installing aether (SPIRE ON; QUIC_DNS_SANS=$QUIC_DNS_SANS)"
	# shellcheck disable=SC2046
	helm --kube-context "$CTX" upgrade --install aether "$charts/aether" \
		-n "$NS" --create-namespace \
		--set namespace.create=false \
		--set "meshDomain=$MESH_DOMAIN" \
		--set spire.enabled=true \
		--set edge.enabled=false \
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
	until admin /runtime 2>/dev/null | grep -A8 "\"$QUIC_HOSTNAME_GUARD\"" | grep -q '"final_value": *"false"'; do
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
		for d in "${QUIC_DSTS[@]}" "${GAMMA_DSTS[@]}"; do
			pod="$(pod_of "$d")"
			[ -n "$pod" ] || die "E0: no Running pod for destination $d"
			printf '%s\n' "$listeners" | awk -F'::' -v l="inbound_${pod}_h3" '$1 == l { f = 1 } END { exit !f }' || missing="$missing inbound_${pod}_h3"
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

verify_fanout() {
	log "E1 fan-out: a quic: twin for every (destination x source SA) — nothing is listed"
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
			die "E1: quic: twins missing after 180s:$missing — read the agent's 'east-west QUIC fan-out' log line: twins exist only once the node identity is served, and only for services in the node's dependency set (the sources declare every destination in config.aether.io/upstreams)"
		sleep 5
	done
	ok "$((${#QUIC_DSTS[@]} * ${#SOURCES[@]} + ${#GAMMA_DSTS[@]} * ${#SOURCES[@]})) twins present (e.g. $(twin "${QUIC_DSTS[0]}" "${SOURCES[0]}"))"
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

# A weighted canary parented to gamma-a (both backends have twins). #956
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
	log "E4 GAMMA: weighted HTTPRoute on ${GAMMA_DSTS[0]} (${GAMMA_DSTS[0]} 50 / ${GAMMA_DSTS[1]} 50, both with twins) — stays h2"
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

verify() {
	verify_preflight
	verify_fanout
	verify_quic
	verify_gamma
	verify_gamma_single
	verify_q3
	log "all east-west QUIC assertions passed (unconditional fan-out, per-source HTTP/3 + XFCC, GAMMA weighted stays h2 / single-backend rides h3, identity across a client address change)"
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
