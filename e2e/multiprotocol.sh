#!/usr/bin/env bash
# Single-cluster kind e2e for proposal 037 (multi-protocol ports on one mesh
# service): ONE pod that serves HTTP on :8080 and a raw-TCP protocol on :9000,
# under one ServiceAccount, with both ports on the mesh — in both orders, an
# HTTP primary with a TCP secondary and a TCP primary with an HTTP secondary.
#
# It is the only end-to-end test of those shapes. e2e/l4routes.sh covers
# services that are raw TCP throughout; every HTTP suite covers services with no
# TCP port. Neither builds what a mixed service needs and nothing else does: a
# capture chain per TCP port on a VIP that also serves HTTP, a per-port "tcp:"
# cluster that carries the port as SNI (it is not the pod's primary), a per-port
# floor chain on the destination's inbound, one pod registered under both
# protocols, and a mesh Service labelled by what its PRIMARY port speaks.
#
# The declarations under test are pod annotations:
#
#   mp-mixed     endpoint.aether.io/port: "8080"   ports: "8080,9000=tcp"
#   mp-tcpfirst  endpoint.aether.io/port: "9000"   ports: "9000,8080=h1"   protocol: "tcp"
#
# Three workloads with the SAME two containers and the same two listeners:
#
#   mp-mixed     ports "8080,9000=tcp"   http answers "mixhttp",  tcp answers "mixtcp"
#   mp-tcpfirst  ports "9000,8080=h1"    http answers "revhttp",  tcp answers "revtcp"
#   mp-httponly  ports "8080"            http answers "ctlhttp",  tcp answers "ctltcp"
#
# mp-httponly is the control: its :9000 listener is as alive as the others', and
# is not declared. What a probe reads back is the marker of the container that
# served it, so every assertion names WHICH listener answered, not just that
# something did.
#
#   M0  control       mp-httponly: HTTP on :18081 answers "ctlhttp"; raw TCP to
#                     :9000 and to :18082 is NEVER answered by "ctltcp" — a port
#                     that is not declared is not on the mesh
#   M1  http, mesh    mp-mixed http://<name>:18081/ -> "mixhttp": the HTTP mesh
#                     port still routes on the authority, on a VIP that now
#                     also has TCP chains
#   M2  http, bare    mp-mixed http://<name>/ (dials 80) -> "mixhttp": a
#                     portless URL resolves by its scheme
#   M3  http, port    mp-mixed http://<name>:8080/ -> "mixhttp": the HTTP app
#                     port by number
#   M4  tcp, port     mp-mixed raw TCP to :9000 -> "mixtcp": the per-port chain
#                     (cap_tcp_<svc>_9000 -> tcp:<name>:9000 -> the inbound's
#                     per-port floor chain)
#   M5  tcp, mesh     mp-mixed raw TCP to :18082 is NOT answered: the TCP mesh
#                     port is "the primary port, as raw TCP"
#                     (docs/workload-requirements.md), this primary is HTTP, and
#                     raw bytes are never forwarded to it
#   M6  both keys     the registry holds mp-mixed's pod under the HTTP key AND
#                     the TCP key, and mp-httponly's under the HTTP key only
#   M7  the Service   the generated mesh Service of mp-mixed is classified
#                     http and exposes both mesh ports, 18081 and 18082
#
# The mirror shape, mp-tcpfirst (#1672):
#
#   X1  tcp, mesh     raw TCP to :18082 -> "revtcp": the TCP mesh port is the
#                     primary port, as raw TCP
#   X2  tcp, port     raw TCP to :9000 -> "revtcp": the primary port by number
#   X3  http, port    http://<name>:8080/ -> "revhttp": the HTTP secondary, on
#                     a VIP that also has floor chains
#   X4  http, mesh    http://<name>:18081/ and http://<name>/ are REFUSED, 421
#                     with x-aether-error: primary-not-http — they name the
#                     primary port over HTTP, and this primary is raw TCP. The
#                     mirror of M5, said by name because HTTP can
#   X5  the Service   the generated mesh Service of mp-tcpfirst is classified
#                     tcp: what its primary port speaks, not the first protocol
#                     it is listed under
#   X6  both keys     the registry holds mp-tcpfirst's pod under both keys too
#
# M0 is an assertion and not setup — it is the state in which the TCP probe
# gives the other answer, re-observed on every run, so a green M4 cannot be a
# probe that only ever says one thing (#853). M5 is the same probe on the same
# VIP as M4, one port over: M4 green with M5 answered would mean the per-port
# chain had become a per-VIP one.
#
# THE RED HALF. To see the gate fail, bring the suite up with the mixed
# workload's declaration cut down to its HTTP port, and verify:
#
#   MP_MIXED_PORTS=8080 e2e/multiprotocol.sh up && e2e/multiprotocol.sh verify
#
# M0 to M3 pass (HTTP never depended on the suffix) and M4 fails: no probe to
# :9000 is answered, exactly as on mp-httponly.
#
# The mirror shape has no declaration to cut down: its red half is the build
# from before #1672, on which X1, X2, X4 and X5 fail (no floor chain for the
# service on the source, a 502 where the refusal should be, and a Service
# labelled http) while X3 passes.
#
# SPIRE IS ON AND THE REGISTRY BACKEND IS etcd, for the reasons the header of
# e2e/l4routes.sh gives in full: with SPIRE off the mesh has no TCP path at
# either end (no node SVID => no "tcp:" clusters; the cleartext inbound has no
# floor chain), and etcd is the backend that keys an endpoint by protocol,
# which is what M6 and X6 read. MP_REGISTRY_BACKEND=kubernetes runs the same
# legs on the chart's default backend, which derives the listings from the
# pods (M6 and X6 are skipped there: it has no key to read). CI runs etcd.
#
# Usage: e2e/multiprotocol.sh {up|test|verify|down}   (bare = up + verify)
#
# Prereqs: kind, docker, kubectl, helm, bazel (for the image build; CI sets
# MP_SKIP_BUILD=1 and pre-loads the images from the nightly build artifact).
# The cluster is this script's own; it reads no kube context but the one it
# creates.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# The pinned kind binary / node image / kubectl (#1251): one Kubernetes for every
# e2e surface, local and CI alike.
# shellcheck source=e2e/kind-version.sh
. "$REPO_ROOT/e2e/kind-version.sh"
# <registry>/<namespace> every aether image is tagged under, from the single
# setting in bazel/registry/registry.bzl (proposal 040) -- never a literal.
IMAGE_REGISTRY="$("$REPO_ROOT/scripts/image-registry.sh" prefix)"
CLUSTER="${MP_CLUSTER:-multiprotocol}"
CTX="kind-$CLUSTER"
NS="aether-system"
TEST_NS="aether-test"
MESH_DOMAIN="aether.internal"
TRUST_DOMAIN="aether.internal"
# The two application ports of both workloads, and the two mesh ports
# (meshconst.ProxyOutboundPort, meshconst.ProxyL4OutboundPort).
HTTP_PORT="8080"
TCP_PORT="9000"
MESH_HTTP_PORT="18081"
MESH_TCP_PORT="18082"
# The mixed workload's endpoint.aether.io/ports. The default is the declaration
# under test; the red half (see the header) overrides it. CI never sets it.
MP_MIXED_PORTS="${MP_MIXED_PORTS:-$HTTP_PORT,$TCP_PORT=tcp}"
# How many probes each measured batch sends.
PROBES=20
# SPIRE >= 1.15.2 is required since proposal 036 (the SPIFFE Broker API); these
# are the same chart pins e2e/l4routes.sh uses.
SPIRE_CHART_VERSION="${SPIRE_CHART_VERSION:-0.30.2}"
SPIRE_CRDS_VERSION="${SPIRE_CRDS_VERSION:-0.6.1}"
SPIRE_CLASS="spire-mgmt-spire" # spire-controller-manager class (namespace-release)
IMAGES=(agent mesh-dns proxy-supervisor cni-install registrar controller uds-csi l4echo)
HTTP_ECHO_IMAGE="hashicorp/http-echo:1.0@sha256:fcb75f691c8b0414d670ae570240cbf95502cc18a9ba57e982ecac589760a186"
CURL_IMAGE="curlimages/curl:8.22.0@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777"
KIND_NET="kind"
REGION="local"
ETCD_NAME="${ETCD_NAME:-aether-multiprotocol-etcd}"
# The registrar's registry backend: "etcd" (what CI runs; see the header) or
# "kubernetes", the chart default, which derives endpoints from the pods
# themselves and has no key to read back (M6 is skipped there). CI never sets
# it.
MP_REGISTRY_BACKEND="${MP_REGISTRY_BACKEND:-etcd}"
case "$MP_REGISTRY_BACKEND" in
etcd | kubernetes) ;;
*)
	echo "MP_REGISTRY_BACKEND must be 'etcd' or 'kubernetes', not '$MP_REGISTRY_BACKEND'" >&2
	exit 2
	;;
esac
# shellcheck source=e2e/etcd-image.sh
. "$REPO_ROOT/e2e/etcd-image.sh"

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
die() {
	printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2
	dump_state
	exit 1
}

kc() { kubectl --context "$CTX" "$@"; }

# A red run is one of: the pod never registered under the TCP key, the source
# has no chain for the port, the chain names a cluster that was never
# published, or the destination has no floor chain for it. Print what tells
# them apart (the #590 principle).
dump_state() {
	kubectl config get-contexts "$CTX" >/dev/null 2>&1 || return 0
	printf '\033[1;33m  -- pods --\033[0m\n' >&2
	kc get pods -A -o wide 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- the workloads'"'"' declarations (endpoint.aether.io/ports) --\033[0m\n' >&2
	kc -n "$TEST_NS" get pods -o custom-columns=NAME:.metadata.name,PORT:.metadata.annotations.endpoint\\.aether\\.io/port,PORTS:.metadata.annotations.endpoint\\.aether\\.io/ports 2>&1 |
		sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- mesh services (classification + ports) --\033[0m\n' >&2
	kc -n "$TEST_NS" get svc -o custom-columns=NAME:.metadata.name,CLUSTERIP:.spec.clusterIP,PROTO:.metadata.annotations.aether\\.io/app-protocol,PORTS:.spec.ports[*].port 2>&1 |
		sed 's/^/    /' >&2 || true
	if [ "$MP_REGISTRY_BACKEND" = "etcd" ]; then
		printf '\033[1;33m  -- registry keys of the test services (one line per protocol the pod is registered under) --\033[0m\n' >&2
		docker exec "$ETCD_NAME" etcdctl get --prefix / --keys-only 2>/dev/null |
			grep -E '/services/mp-(mixed|tcpfirst|httponly)/' | sed 's/^/    /' >&2 || true
	fi
	# The agent and the proxy are hostNetwork, so the kind node shares their
	# netns and reaches Envoy's loopback-only admin. Names only: which chains and
	# which "tcp:" clusters exist is the whole question.
	local node
	node="$(kc -n "$TEST_NS" get pod -l app=client -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)"
	if [ -n "$node" ]; then
		printf '\033[1;33m  -- envoy: TCP filter chains (cap_tcp_* on the source, in_tcp_* on the destination) --\033[0m\n' >&2
		docker exec "$node" curl -s --max-time 5 "http://127.0.0.1:9901/config_dump?resource=dynamic_listeners" 2>/dev/null |
			grep -oE '"(cap_tcp_|in_tcp_)[^"]*"' | sort | uniq -c | sed 's/^/    /' >&2 || true
		printf '\033[1;33m  -- envoy: tcp: clusters and their hosts --\033[0m\n' >&2
		docker exec "$node" curl -s --max-time 5 http://127.0.0.1:9901/clusters 2>/dev/null |
			grep -E '^tcp:.*(::health_flags|::cx_connect_fail|::rq_total)' | sed -n '1,40s/^/    /p' >&2 || true
	fi
	printf '\033[1;33m  -- agent log --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=agent --all-containers --tail=120 --prefix 2>&1 |
		sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- proxy log --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=proxy -c proxy --tail=60 --prefix 2>&1 |
		sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- registrar --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=registrar --tail=40 --prefix 2>&1 |
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
	# CI builds the images itself (bazel image_load via RBE) and sets
	# MP_SKIP_BUILD=1 so this is a no-op — they are already in the local docker
	# daemon under <IMAGE_REGISTRY>/<img>:latest.
	if [ "${MP_SKIP_BUILD:-0}" = "1" ]; then
		ok "skipping image build (MP_SKIP_BUILD=1; images pre-built)"
		return
	fi
	log "building + loading aether images (incl. the l4echo test backend)"
	local t
	for t in //agent/cmd/agent //agent/cmd/mesh-dns //agent/cmd/proxy-supervisor \
		//cni/cmd/cni-install //registrar/cmd/registrar //controller/cmd/controller //agent/cmd/uds-csi \
		//e2e/l4echo; do
		(cd "$REPO_ROOT" && bazel run "$t:image_load" >/dev/null 2>&1) || die "image build failed for $t"
	done
	ok "images built"
}

create_cluster() {
	# SIGPIPE rule (#1121, e2e/README.md): this script runs under pipefail, so
	# no pipeline may end in a reader that exits before its writer is done
	# (`head`, `grep -q`/`-m`, `awk '...; exit'`): the writer dies of SIGPIPE
	# and the pipeline fails with 141 at a random point. Read to EOF instead
	# (`grep -c ... >/dev/null`). The pods' own `sh` scripts run without
	# pipefail.
	if kind get clusters 2>/dev/null | grep -cx "$CLUSTER" >/dev/null; then
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
	kind_require_binary || die "kind binary does not match e2e/kind-version.sh (see above)"
	kind create cluster --image "$KIND_NODE_IMAGE" --config "$cfg" --wait 60s >/dev/null
	rm -f "$cfg"
	ok "cluster '$CLUSTER' ready"
}

# etcd on kind's docker network, reachable from the cluster's node.
etcd_ip() { docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$ETCD_NAME"; }

start_etcd() {
	if [ "$MP_REGISTRY_BACKEND" != "etcd" ]; then
		ok "no etcd: the registry backend is '$MP_REGISTRY_BACKEND'"
		return
	fi
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
		kind load docker-image "${IMAGE_REGISTRY}/${img}:latest" --name "$CLUSTER" >/dev/null 2>&1 ||
			die "could not load ${IMAGE_REGISTRY}/${img}:latest into kind (was it built?)"
	done
	ok "images loaded"
}

# SPIRE, single cluster, self-signed. Mandatory here (see the header); the
# values are e2e/l4routes.sh's, whose comments say why each one is set.
install_spire() {
	log "installing SPIRE (trust domain $TRUST_DOMAIN)"
	helm --kube-context "$CTX" repo add spiffe https://spiffe.github.io/helm-charts-hardened/ >/dev/null 2>&1 || true
	helm --kube-context "$CTX" repo update >/dev/null 2>&1 || true
	kc create ns spire-mgmt >/dev/null 2>&1 || true
	helm --kube-context "$CTX" upgrade --install spire-crds spiffe/spire-crds \
		-n spire-mgmt --version "$SPIRE_CRDS_VERSION" --wait --timeout 3m >/dev/null ||
		die "spire-crds install failed"
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
	# Declared and assigned separately (SC2155), so `set -e` sees etcd_ip fail.
	local charts
	local -a backend
	charts="$(chart_dir)"
	backend=(--set "registrar.registryBackend=$MP_REGISTRY_BACKEND")
	if [ "$MP_REGISTRY_BACKEND" = "etcd" ]; then
		backend+=(--set "registrar.region=$REGION" --set "registrar.etcd.region=$REGION"
			--set "registrar.etcd.endpoints[0]=http://$(etcd_ip):2379")
	fi
	img() { echo "--set $1.image.repository=${IMAGE_REGISTRY}/$2 --set $1.image.tag=latest --set $1.image.digest= --set $1.image.pullPolicy=Never"; }
	log "installing the aether CRDs"
	helm --kube-context "$CTX" upgrade --install aether-crds "$charts/crds" \
		-n "$NS" --create-namespace --wait --timeout 2m >/dev/null || die "crds chart install failed"

	# Everything except SPIRE and the registry backend is chart default:
	# transparent capture with redirect-all (which is what captures a dial to
	# :9000 and to :80), mesh DNS. Per-port protocols have no values gate.
	log "installing aether (SPIRE ON, $MP_REGISTRY_BACKEND registry — see the header)"
	# shellcheck disable=SC2046
	helm --kube-context "$CTX" upgrade --install aether "$charts/aether" \
		-n "$NS" --create-namespace \
		--set namespace.create=false \
		--set "clusterName=$CLUSTER" \
		--set "meshDomain=$MESH_DOMAIN" \
		--set spire.enabled=true \
		--set edge.enabled=false \
		"${backend[@]}" \
		$(img agent agent) $(img agent.meshDnsDaemon mesh-dns) \
		$(img proxy.supervisor proxy-supervisor) $(img cniInstall cni-install) \
		$(img registrar registrar) $(img controller controller) $(img udsCsi uds-csi) \
		--set proxy.image.pullPolicy=IfNotPresent \
		--timeout 6m >/dev/null || die "aether install failed"
	rm -rf "$(dirname "$charts")"

	kc -n "$NS" rollout status ds/aether-agent --timeout=300s >/dev/null || die "the agent DaemonSet never became Ready"
	kc -n "$NS" rollout status ds/aether-mesh-dns --timeout=180s >/dev/null || die "the mesh-DNS DaemonSet never became Ready"
	kc -n "$NS" rollout status deploy/aether-registrar --timeout=180s >/dev/null || die "the registrar never became Ready"
	kc -n "$NS" rollout status deploy/aether-controller --timeout=180s >/dev/null || die "the controller never became Ready"
	ok "aether up"
}

# deploy_two_port_workload NAME PORTS HTTP_TEXT TCP_TEXT [PRIMARY PROTOCOL] —
# one pod, two listeners: an HTTP server on :$HTTP_PORT and e2e/l4echo (reads
# one line, writes "<text> <line>", closes) on :$TCP_PORT. PORTS is the pod's
# endpoint.aether.io/ports; PRIMARY and PROTOCOL are its endpoint.aether.io/port
# and /protocol (the class of an unsuffixed port), HTTP on :$HTTP_PORT unless
# given. The declaration is the ONLY thing that differs between the workloads
# besides their markers.
#
# No Kubernetes Service is written: the registrar generates the selectorless
# mesh Service (the name mesh DNS answers) for every registered service, and
# skips a name some other Service already owns. The registry service name is
# the pod's ServiceAccount.
deploy_two_port_workload() {
	local name="$1" ports="$2" http_text="$3" tcp_text="$4" primary="${5:-$HTTP_PORT}" protocol="${6:-http}"
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
        endpoint.aether.io/port: "$primary"
        endpoint.aether.io/protocol: "$protocol"
        endpoint.aether.io/ports: "$ports"
    spec:
      serviceAccountName: $name
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: http
          image: $HTTP_ECHO_IMAGE
          args: ["-text=$http_text", "-listen=:$HTTP_PORT"]
          ports: [{containerPort: $HTTP_PORT}]
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: ["ALL"]}
        - name: tcp
          image: ${IMAGE_REGISTRY}/l4echo:latest
          imagePullPolicy: Never
          args: ["--mode=tcp", "--listen=:$TCP_PORT", "--text=$tcp_text"]
          ports: [{containerPort: $TCP_PORT}]
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: ["ALL"]}
YAML
}

deploy_workloads() {
	log "deploying mp-mixed (ports: $MP_MIXED_PORTS), mp-tcpfirst (TCP primary :$TCP_PORT, ports: $TCP_PORT,$HTTP_PORT=h1), mp-httponly (ports: $HTTP_PORT) and the client"
	kc create ns "$TEST_NS" >/dev/null 2>&1 || true
	deploy_two_port_workload mp-mixed "$MP_MIXED_PORTS" mixhttp mixtcp
	deploy_two_port_workload mp-tcpfirst "$TCP_PORT,$HTTP_PORT=h1" revhttp revtcp "$TCP_PORT" tcp
	deploy_two_port_workload mp-httponly "$HTTP_PORT" ctlhttp ctltcp
	local workloads="mp-mixed mp-tcpfirst mp-httponly"
	local upstreams="mp-mixed.$TEST_NS,mp-tcpfirst.$TEST_NS,mp-httponly.$TEST_NS"

	# The upstreams annotation is NOT an optimisation: there is no on-demand
	# discovery for tcp_proxy. An undeclared TCP upstream yields a chain that
	# matches and a cluster that is missing, which looks exactly like a routing
	# bug.
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
        config.aether.io/upstreams: "$upstreams"
    spec:
      serviceAccountName: client
      containers:
        - name: curl
          image: $CURL_IMAGE
          command: ["sleep", "infinity"]
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: ["ALL"]}
YAML
	local d
	for d in $workloads client; do
		kc -n "$TEST_NS" rollout status "deploy/$d" --timeout=180s >/dev/null ||
			die "workload '$d' never became Ready"
	done
	ok "workloads deployed"
}

# --- probes -------------------------------------------------------------------

fqdn() { printf '%s.%s.%s' "$1" "$TEST_NS" "$MESH_DOMAIN"; }

# http_probe_batch AUTHORITY N — N GETs of http://AUTHORITY/ from the client, in
# ONE exec. Prints N lines: the body's first token (a workload's marker), or
# HTTP<status> when there is a response without one, or NOREPLY.
http_probe_batch() {
	local authority="$1" n="$2"
	# SC2016: the single quotes are the point — the body is evaluated by the
	# POD's shell; $url/$n/$i/$out are its variables.
	# shellcheck disable=SC2016
	kc -n "$TEST_NS" exec deploy/client -c curl -- sh -c '
		url="$1"; n="$2"; i=1
		while [ "$i" -le "$n" ]; do
			out=$(curl -s --max-time 5 -w "\nHTTP%{http_code}\n" "$url" 2>/dev/null | tr -d "\r")
			first=$(printf "%s\n" "$out" | sed -n "1p")
			case "$first" in
			mixhttp | revhttp | ctlhttp | mixtcp* | revtcp* | ctltcp*) printf "%s\n" "${first%% *}" ;;
			*) printf "%s\n" "$(printf "%s\n" "$out" | grep "^HTTP[1-9]" | sed -n "1p" | grep . || echo NOREPLY)" ;;
			esac
			i=$((i + 1))
		done
	' sh "http://$authority/" "$n" 2>/dev/null || true
}

# tcp_probe_batch HOST:PORT N — N raw-TCP probes from the client in ONE exec.
# Prints N lines: the first token of the reply (l4echo answers
# "<marker> <line>" and closes), or NOREPLY.
#
# The payload is >= 6 bytes on purpose: the capture listener's http_inspector
# waits on an inconclusive short first write (#466).
tcp_probe_batch() {
	local target="$1" n="$2"
	# shellcheck disable=SC2016  # evaluated by the POD's shell; see http_probe_batch
	kc -n "$TEST_NS" exec deploy/client -c curl -- sh -c '
		target="$1"; n="$2"; i=1
		while [ "$i" -le "$n" ]; do
			reply=$(printf "probe-%s\n" "$i" | curl -s --max-time 5 "telnet://$target" 2>/dev/null | sed -n "1p" | tr -d "\r")
			reply="${reply%% *}"
			printf "%s\n" "${reply:-NOREPLY}"
			i=$((i + 1))
		done
	' sh "$target" "$n" 2>/dev/null || true
}

count_token() { printf '%s\n' "$1" | awk -v want="$2" '$1 == want { n++ } END { print n + 0 }'; }
histogram() { printf '%s\n' "$1" | awk 'NF { c[$1]++ } END { for (k in c) printf "%s=%d ", k, c[k] }'; }

# await_probe FN TARGET WANT TIMEOUT — poll small batches until every probe of
# one batch answers WANT. The convergence gate before a measured batch:
# registration, the generated Service, the clusters and the chains arrive one
# after another, and none of them has a bounded duration (#919). On a timeout it
# prints what it last saw.
await_probe() {
	local fn="$1" target="$2" want="$3" timeout="$4" replies deadline probes=5
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

# expect_all ID FN TARGET WANT TIMEOUT MEANING — converge, then one measured
# batch that must be WANT on every probe.
expect_all() {
	local id="$1" fn="$2" target="$3" want="$4" timeout="$5" meaning="$6" replies got
	await_probe "$fn" "$target" "$want" "$timeout" >/dev/null ||
		die "$id: $target never answered '$want': $(await_probe "$fn" "$target" "$want" 1) — $meaning"
	replies="$("$fn" "$target" "$PROBES")"
	got="$(count_token "$replies" "$want")"
	[ "$got" -eq "$PROBES" ] ||
		die "$id: expected $PROBES/$PROBES '$want' from $target, got $(histogram "$replies") — $meaning"
}

# --- assertions ---------------------------------------------------------------

# M0 — the control, and it runs first: the state in which the TCP probe is not
# answered. Its HTTP half is what makes the TCP half mean something: the
# workload is up, registered, resolvable and reachable, and its :9000 listener
# is still not on the mesh.
verify_control() {
	log "M0 control: mp-httponly declares only its HTTP port — its :$TCP_PORT listener must not be reachable through the mesh"
	local name replies target got
	name="$(fqdn mp-httponly)"
	expect_all M0 http_probe_batch "$name:$MESH_HTTP_PORT" ctlhttp 240 \
		"the control workload does not serve HTTP through the mesh, so nothing below could mean anything"
	ok "$PROBES/$PROBES HTTP requests to :$MESH_HTTP_PORT answered by its HTTP container"
	# Few probes: an unanswered one costs the client its whole timeout.
	for target in "$name:$TCP_PORT" "$name:$MESH_TCP_PORT"; do
		replies="$(tcp_probe_batch "$target" 3)"
		got="$(count_token "$replies" NOREPLY)"
		[ "$got" -eq 3 ] ||
			die "M0: raw TCP to $target was answered ($(histogram "$replies")) although the pod does not declare a TCP port — the probe cannot tell a declared port from an undeclared one, or the mesh forwards a port nobody registered"
	done
	ok "raw TCP to :$TCP_PORT and to :$MESH_TCP_PORT unanswered (3/3 each): a port that is not declared is not on the mesh"
}

# M1..M3 — HTTP on a service that also has a TCP port. The TCP chains are
# matched on destination port ahead of everything else; these are the three
# HTTP spellings they must not shadow.
verify_http() {
	local name
	name="$(fqdn mp-mixed)"
	log "M1 http on the mesh port: http://$name:$MESH_HTTP_PORT/"
	expect_all M1 http_probe_batch "$name:$MESH_HTTP_PORT" mixhttp 240 \
		"HTTP to the mesh port of a mixed service is not served by its HTTP port"
	ok "$PROBES/$PROBES answered by the HTTP container"

	log "M2 http, portless: http://$name/ (the client dials 80)"
	expect_all M2 http_probe_batch "$name" mixhttp 90 \
		"a portless http:// URL does not resolve to the service's HTTP port"
	ok "$PROBES/$PROBES answered by the HTTP container"

	log "M3 http on the app port: http://$name:$HTTP_PORT/"
	expect_all M3 http_probe_batch "$name:$HTTP_PORT" mixhttp 90 \
		"HTTP to the service's own HTTP port is not served by it"
	ok "$PROBES/$PROBES answered by the HTTP container"
}

# M4, M5 — raw TCP to the HTTP-primary service.
verify_tcp() {
	local name replies got
	name="$(fqdn mp-mixed)"
	log "M4 raw TCP on the service's own TCP port: $name:$TCP_PORT"
	expect_all M4 tcp_probe_batch "$name:$TCP_PORT" mixtcp 180 \
		"NOREPLY means no chain claims the port on the source (the pod is not registered with $TCP_PORT=tcp, or the per-port chain is missing); an HTTP marker or status means the connection was delivered to the HTTP port"
	ok "$PROBES/$PROBES answered by the TCP container: the per-port chain carries :$TCP_PORT"

	# :18082 is "the primary port, as raw TCP" (docs/workload-requirements.md,
	# "How a spelling resolves"), and this service's primary is HTTP: there is
	# nothing for it to name. Asserted so that the answer is a decision: proposal
	# 037's text has :18082 resolve to the lowest TCP port of such a service
	# ("default TCP port"), which is not what shipped. If that is built, this
	# becomes $PROBES/$PROBES 'mixtcp' on purpose, with the document.
	log "M5 the TCP mesh port of an HTTP-primary service: $name:$MESH_TCP_PORT names no port"
	replies="$(tcp_probe_batch "$name:$MESH_TCP_PORT" 3)"
	got="$(count_token "$replies" NOREPLY)"
	[ "$got" -eq 3 ] ||
		die "M5: raw TCP to $name:$MESH_TCP_PORT was answered ($(histogram "$replies")) — 'mixhttp' would be raw bytes forwarded to the HTTP primary; 'mixtcp' means :$MESH_TCP_PORT now resolves to a non-primary TCP port: update this leg and docs/workload-requirements.md together"
	ok "unanswered (3/3): the primary is HTTP, and raw TCP is never forwarded to it"
}

# http_refusal AUTHORITY — one GET of http://AUTHORITY/ from the client. Prints
# "<status> <x-aether-error value>" ("000 " when there is no response).
http_refusal() {
	# shellcheck disable=SC2016  # evaluated by the POD's shell; see http_probe_batch
	kc -n "$TEST_NS" exec deploy/client -c curl -- sh -c '
		out=$(curl -s --max-time 5 -o /dev/null -D - -w "STATUS %{http_code}\n" "$1" 2>/dev/null | tr -d "\r")
		status=$(printf "%s\n" "$out" | sed -n "s/^STATUS //p")
		reason=$(printf "%s\n" "$out" | sed -n "s/^[Xx]-[Aa]ether-[Ee]rror: *//p" | sed -n "1p")
		printf "%s %s\n" "${status:-000}" "$reason"
	' sh "http://$1/" 2>/dev/null || true
}

# X1..X6 — the mirror shape, a TCP primary with an HTTP secondary (#1672).
verify_tcpfirst() {
	local name target got proto deadline
	name="$(fqdn mp-tcpfirst)"
	log "X1 the TCP mesh port of a TCP-primary service with an HTTP port: $name:$MESH_TCP_PORT"
	expect_all X1 tcp_probe_batch "$name:$MESH_TCP_PORT" revtcp 180 \
		"NOREPLY means the source has no :$MESH_TCP_PORT chain for the service; 'revhttp' means the floor forwarded to the HTTP port"
	ok "$PROBES/$PROBES answered by the TCP container: :$MESH_TCP_PORT is the primary port, as raw TCP"

	log "X2 raw TCP on its primary port: $name:$TCP_PORT"
	expect_all X2 tcp_probe_batch "$name:$TCP_PORT" revtcp 90 \
		"the primary port of a TCP-primary service is not served by the floor"
	ok "$PROBES/$PROBES answered by the TCP container"

	log "X3 HTTP on its secondary port: http://$name:$HTTP_PORT/"
	expect_all X3 http_probe_batch "$name:$HTTP_PORT" revhttp 90 \
		"the HTTP port of a TCP-primary service is not served as HTTP ('revtcp' or NOREPLY: a per-VIP chain with no port took the connection from the HTTP chain)"
	ok "$PROBES/$PROBES answered by the HTTP container"

	# :18081 and the portless URL are "the primary port, over HTTP"
	# (docs/workload-requirements.md, "How a spelling resolves"), and this
	# primary is raw TCP: they name no port. HTTP can say so, so it does: 421 and
	# a reason, at once. A 502 is the request forwarded to the raw-TCP port as
	# h2; a 200 'revhttp' means these spellings now resolve to a non-primary HTTP
	# port: update this leg and the document together.
	log "X4 the HTTP mesh port and the portless URL of a TCP-primary service are refused by name"
	for target in "$name:$MESH_HTTP_PORT" "$name"; do
		deadline=$((SECONDS + 60))
		while true; do
			got="$(http_refusal "$target")"
			[ "$got" = "421 primary-not-http" ] && break
			[ "$SECONDS" -lt "$deadline" ] ||
				die "X4: http://$target/ answered '${got:-<nothing>}', want '421 primary-not-http' — 502 is the request sent to the raw-TCP primary; 200 means the spelling resolves to the HTTP secondary"
			sleep 5
		done
	done
	ok "http://$name:$MESH_HTTP_PORT/ and http://$name/: 421, x-aether-error: primary-not-http"

	log "X5 the generated mesh Service of mp-tcpfirst"
	proto="$(kc -n "$TEST_NS" get svc mp-tcpfirst -o jsonpath='{.metadata.annotations.aether\.io/app-protocol}' 2>/dev/null || true)"
	[ "$proto" = "tcp" ] ||
		die "X5: the mesh Service mp-tcpfirst carries aether.io/app-protocol='${proto:-<unset>}', want 'tcp' — its primary port is raw TCP, whatever protocols it is listed under"
	ok "app-protocol tcp"

	if [ "$MP_REGISTRY_BACKEND" != "etcd" ]; then
		log "X6 registration: skipped — see M6"
		return
	fi
	log "X6 registration: mp-tcpfirst under BOTH protocol keys"
	got="$(registry_protocols mp-tcpfirst | tr '\n' ' ')"
	[ "$got" = "PROTOCOL_HTTP PROTOCOL_TCP " ] ||
		die "X6: mp-tcpfirst is registered under '${got:-<nothing>}', want 'PROTOCOL_HTTP PROTOCOL_TCP'"
	ok "mp-tcpfirst: PROTOCOL_HTTP + PROTOCOL_TCP"
}

# registry_protocols SERVICE — the protocols SERVICE has an endpoint under, one
# per line, sorted. The etcd key is
# .../services/<svc>/protocols/<PROTOCOL_X>/endpoints/<ip>.
registry_protocols() {
	docker exec "$ETCD_NAME" etcdctl get --prefix / --keys-only 2>/dev/null |
		sed -n "s#.*/services/$1/protocols/\\([A-Z_]*\\)/endpoints/.*#\\1#p" | sort -u || true
}

# M6 — one pod, two keys. This replaces a check that read the pod's own
# annotation back, which the pod carries whether or not anything honoured it.
verify_registration() {
	if [ "$MP_REGISTRY_BACKEND" != "etcd" ]; then
		log "M6 registration: skipped — the '$MP_REGISTRY_BACKEND' backend derives endpoints from the pods and has no per-protocol key to read"
		return
	fi
	log "M6 registration: mp-mixed under BOTH protocol keys, mp-httponly under one"
	local got
	got="$(registry_protocols mp-mixed | tr '\n' ' ')"
	[ "$got" = "PROTOCOL_HTTP PROTOCOL_TCP " ] ||
		die "M6: mp-mixed is registered under '${got:-<nothing>}', want 'PROTOCOL_HTTP PROTOCOL_TCP' — a pod with an HTTP and a TCP port registers once per protocol"
	got="$(registry_protocols mp-httponly | tr '\n' ' ')"
	[ "$got" = "PROTOCOL_HTTP " ] ||
		die "M6: mp-httponly is registered under '${got:-<nothing>}', want 'PROTOCOL_HTTP' only"
	ok "mp-mixed: PROTOCOL_HTTP + PROTOCOL_TCP; mp-httponly: PROTOCOL_HTTP"
}

# M7 — the generated mesh Service.
verify_service() {
	log "M7 the generated mesh Service of mp-mixed"
	local proto ports
	proto="$(kc -n "$TEST_NS" get svc mp-mixed -o jsonpath='{.metadata.annotations.aether\.io/app-protocol}' 2>/dev/null || true)"
	[ "$proto" = "http" ] ||
		die "M7: the mesh Service mp-mixed carries aether.io/app-protocol='${proto:-<unset>}', want 'http' — its primary port is HTTP"
	ports=" $(kc -n "$TEST_NS" get svc mp-mixed -o jsonpath='{.spec.ports[*].port}' 2>/dev/null || true) "
	case "$ports" in
	*" $MESH_HTTP_PORT "*) ;;
	*) die "M7: the mesh Service mp-mixed does not expose $MESH_HTTP_PORT (ports:$ports)" ;;
	esac
	case "$ports" in
	*" $MESH_TCP_PORT "*) ;;
	*) die "M7: the mesh Service mp-mixed does not expose the TCP mesh port $MESH_TCP_PORT (ports:$ports)" ;;
	esac
	ok "app-protocol http; ports:$ports"
}

verify() {
	kc -n "$TEST_NS" get deploy mp-mixed >/dev/null 2>&1 ||
		die "the workloads are not deployed in '$CLUSTER': run '$0 up' first"
	verify_control
	verify_http
	verify_tcp
	verify_registration
	verify_service
	verify_tcpfirst
	log "all multi-protocol assertions passed (control; HTTP primary + TCP secondary: three HTTP spellings, the TCP port, the TCP mesh port, dual registration, the mesh Service; TCP primary + HTTP secondary: the TCP mesh port, the primary port, the HTTP port, the refused HTTP spellings, the mesh Service, dual registration)"
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
*)
	echo "usage: $0 {up|test|verify|down}" >&2
	exit 2
	;;
esac
