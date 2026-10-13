#!/usr/bin/env bash
# Single-cluster kind e2e for proposal 037 (several ports, of more than one
# protocol, on one mesh service) and for the addressing rule of proposal 043
# (a mesh service is addressed by its real ports), Phase 1.
#
# It is the only end-to-end test of a service with more than one port.
# e2e/l4routes.sh covers services that are raw TCP throughout; every HTTP suite
# covers services with one HTTP port. Neither builds what this needs: a capture
# chain and a route table per HTTP port number, a capture chain per (service,
# raw-TCP port) on an address that also serves HTTP, a per-port cluster, a
# per-port chain on the destination's inbound, and one pod registered under
# both protocols.
#
# THE WORKLOADS. Namespace aether-test; the registry service name is the pod's
# ServiceAccount; no Kubernetes Service is written here (the registrar
# generates it). Every listener answers with a marker of its own, so what a
# probe reads back names WHICH workload and WHICH port served it: reaching the
# wrong port or the wrong service is a failure, never a 200 that passes.
#
#   workload      /port  /protocol  /ports          listeners -> marker
#   mp-httponly   8080   http       8080            http 8080 -> ctl8080
#                                                   tcp  9000 -> ctltcp  (NOT declared)
#   mp-multi      8080   http       8080,9090       http 8080 -> multi8080
#                                                   http 9090 -> multi9090
#   mp-eighty     8080   http       80,8080,9090    http 80   -> eighty80
#                                                   http 8080 -> eighty8080
#                                                   http 9090 -> eighty9090
#   mp-mixed      8080   http       8080,9000=tcp   http 8080 -> mix8080
#                                                   tcp  9000 -> mixtcp
#   mp-tcpfirst   9000   tcp        9000,8080=h1    tcp  9000 -> revtcp
#                                                   http 8080 -> rev8080
#   mp-nine       9000   http       9000            http 9000 -> nine9000
#
# (/port, /protocol and /ports are the endpoint.aether.io/ annotations.)
# mp-httponly is the control: its :9000 listener is as alive as mp-mixed's and
# is not declared. mp-nine serves HTTP on the port NUMBER mp-mixed declares as
# raw TCP. mp-eighty really listens on 80.
#
# TWO CLIENTS. "client" is captured (the chart default, redirect-all).
# "client-nocapture" carries capture.aether.io/redirect-all: "false" and only
# ever dials the loopback outbound listener, 127.0.0.1:18081, with the service
# in Host.
#
# NAMES. A service answers to its mesh name <svc>.<ns>.aether.internal and to
# its Kubernetes names <svc>.<ns>.svc.cluster.local and <svc>.<ns>.
#
# THE LEGS. Each prints its ID. A refusal is an immediate 421 with the response
# header x-aether-error: <reason>; "immediate" is asserted as a total request
# time under one second.
#
# Proposal 037, the HTTP-first mixed service and its control:
#
#   M0  control       mp-httponly: HTTP on :18081 answers ctl8080; raw TCP to
#                     :9000 and to :18082 is NEVER answered: a port that is not
#                     declared is not on the mesh
#   M1  http, mesh    mp-mixed http://<name>:18081/ -> mix8080
#   M2  http, bare    mp-mixed http://<name>/ (dials 80) -> mix8080
#   M3  http, port    mp-mixed http://<name>:8080/ -> mix8080
#   M4  tcp, port     mp-mixed raw TCP to :9000 -> mixtcp (cap_tcp_<svc>_9000 ->
#                     tcp:<name>:9000 -> the inbound's per-port chain)
#   M5  tcp, mesh     mp-mixed raw TCP to :18082 is NOT answered: the port its
#                     pods declare in endpoint.aether.io/port is HTTP, and raw
#                     bytes are never forwarded to it
#   M6  both keys     the registry holds mp-mixed's pod under the HTTP key AND
#                     the TCP key, and mp-httponly's under the HTTP key only
#                     (etcd only: it reads registry keys)
#   M7  the Service   the generated Service of mp-mixed has its REAL ports
#                     (8080 appProtocol http named p8080-tcp, 9000 appProtocol
#                     tcp named p9000-tcp) beside the four legacy ports (18081,
#                     18082, 80, 443), and aether.io/app-protocol=http, the
#                     value the 2.6 registrar stamped
#
# The mirror shape, mp-tcpfirst (#1672):
#
#   X1  tcp, mesh     raw TCP to :18082 -> revtcp
#   X2  tcp, port     raw TCP to :9000 -> revtcp
#   X3  http, port    http://<name>:8080/ -> rev8080
#   X4  http, legacy  the legacy HTTP spellings are refused: 421 port-required
#                     (this is the mp-tcpfirst half of A12; one implementation)
#   X5  the Service   the generated Service has 9000 appProtocol tcp and 8080
#                     appProtocol http
#   X6  both keys     the registry holds its pod under both keys (etcd only)
#
# Proposal 043 Phase 1, the addressing rule ("P" is each HTTP port a workload
# declares):
#
#   A1  captured   dial <mesh name>:P, Host <mesh name>:P, all six workloads
#                  -> port P's marker (for mp-eighty P=80 the Host carries an
#                  explicit :80, and port 80 answers)
#   A2  captured   dial <svc>.<ns>.svc.cluster.local:P and <svc>.<ns>:P, for
#                  mp-multi, mp-eighty (8080, 9090) and mp-mixed -> port P's
#                  marker (#1682: the declared port used to answer)
#   A3  loopback   Host <mesh name>:P, all six -> port P's marker
#   A4  both       an HTTPRoute with a port-less parentRef on mp-multi adds a
#                  response header: requests to :9090 AND to :8080 carry it and
#                  are still answered by the port that was addressed
#   A5  captured   dial <mesh name>:8080 with Host <mesh name>:9090 -> 421
#                  port-mismatch
#   A6  loopback   Host <mesh name>:9000 for mp-mixed and mp-tcpfirst, where
#                  9000 is raw TCP -> 421 port-not-http
#   A7  both       mp-multi, port 7777: captured dial and Host on the mesh name
#                  and both Kubernetes names, loopback Host -> 421
#                  port-not-declared
#   A8  captured   raw TCP to mp-mixed's <mesh name>:7777 -> no application
#                  answers
#   A9  captured   dial <name>:P with a PORT-LESS Host, P other than 80, for
#                  mp-multi, mp-eighty, mp-mixed, on the mesh name and the
#                  svc.cluster.local name -> port P's marker
#   A10 captured   http://<mesh name of mp-nine>:9000/ -> nine9000, although
#                  9000 is a raw-TCP port of another service
#   A11 captured   an HTTP request written as raw bytes to mp-mixed's :9000
#                  -> mixtcp (the declaration decides, not the bytes); bytes
#                  that are not HTTP to mp-nine's :9000 -> no application
#                  answers
#   A12 both       the legacy spellings, unchanged in this phase: :18081, the
#                  port-less URL, the port-less svc.cluster.local URL, and on
#                  loopback a port-less Host and Host <mesh name>:18081 reach
#                  the port the pods declare in endpoint.aether.io/port when
#                  it is HTTP; for mp-tcpfirst they are refused, 421
#                  port-required, and :18082 reaches its raw TCP (X4, X1)
#
# A13 (the server-first leg of the optional inspector change) is not
# implemented: that change is not part of this phase's work.
#
# M0 is an assertion and not setup: it is the state in which the TCP probe
# gives the other answer, re-observed on every run, so a green M4 cannot be a
# probe that only ever says one thing (#853).
#
# THE RED HALF. Bring the suite up with the mixed workload's declaration cut
# down to its HTTP port, and verify:
#
#   MP_MIXED_PORTS=8080 e2e/multiprotocol.sh up && e2e/multiprotocol.sh verify
#
# M0 to M3 pass (HTTP never depended on the suffix) and M4 fails: no probe to
# :9000 is answered, exactly as on mp-httponly.
#
# A RED RUN. By default the first failed assertion ends the run and prints the
# cluster's state. To see every leg instead:
#
#   MP_KEEP_GOING=1 e2e/multiprotocol.sh verify
#
# runs them all, prints a table (LEG RESULT DETAIL) and exits non-zero if any
# failed. MP_LEGS="A5 A7" runs a subset. MP_TIMEOUT=<seconds> caps how long a
# leg waits for an answer to converge, which is what a failing assertion costs
# (it waits for the answer it will never get).
#
# SPIRE IS ON, for the reason the header of e2e/l4routes.sh gives in full:
# with SPIRE off the mesh has no TCP path at either end. THE REGISTRY BACKEND
# is etcd by default, the backend that keys an endpoint by protocol (what M6
# and X6 read); MP_REGISTRY_BACKEND=kubernetes runs the same legs on the
# chart's default backend, which derives the listings from the pods and starts
# no etcd (M6 and X6 are skipped there, and say so). CI runs both.
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
# The pinned Gateway API release (#1583); leg A4 needs HTTPRoute.
# shellcheck source=e2e/gateway-api-version.sh
. "$REPO_ROOT/e2e/gateway-api-version.sh"
# <registry>/<namespace> every aether image is tagged under, from the single
# setting in bazel/registry/registry.bzl (proposal 040) -- never a literal.
IMAGE_REGISTRY="$("$REPO_ROOT/scripts/image-registry.sh" prefix)"
CLUSTER="${MP_CLUSTER:-multiprotocol}"
CTX="kind-$CLUSTER"
NS="aether-system"
TEST_NS="aether-test"
MESH_DOMAIN="aether.internal"
TRUST_DOMAIN="aether.internal"
# The application ports of the workloads, a port nobody declares, and the two
# mesh ports (meshconst.ProxyOutboundPort, meshconst.ProxyL4OutboundPort).
HTTP_PORT="8080"
ALT_PORT="9090"
LOW_PORT="80"
TCP_PORT="9000"
UNDECLARED_PORT="7777"
MESH_HTTP_PORT="18081"
MESH_TCP_PORT="18082"
# The loopback outbound listener, as a client-nocapture pod dials it.
LOOPBACK_URL="http://127.0.0.1:$MESH_HTTP_PORT/"
# The mixed workload's endpoint.aether.io/ports. The default is the declaration
# under test; the red half (see the header) overrides it. CI never sets it.
MP_MIXED_PORTS="${MP_MIXED_PORTS:-$HTTP_PORT,$TCP_PORT=tcp}"
# The registrar's registry backend: "etcd", or "kubernetes" (the chart
# default), which derives endpoints from the pods themselves and has no key to
# read back. CI runs one leg of each.
MP_REGISTRY_BACKEND="${MP_REGISTRY_BACKEND:-etcd}"
case "$MP_REGISTRY_BACKEND" in
etcd | kubernetes) ;;
*)
	echo "MP_REGISTRY_BACKEND must be 'etcd' or 'kubernetes', not '$MP_REGISTRY_BACKEND'" >&2
	exit 2
	;;
esac
# Red-run aids (see "A RED RUN" in the header). CI sets none of them.
ALL_LEGS=(M0 M1 M2 M3 M4 M5 M6 M7 X1 X2 X3 X4 X5 X6 A1 A2 A3 A4 A5 A6 A7 A8 A9 A10 A11 A12)
MP_LEGS="${MP_LEGS:-${ALL_LEGS[*]}}"
MP_KEEP_GOING="${MP_KEEP_GOING:-0}"
MP_TIMEOUT="${MP_TIMEOUT:-}"
# How many probes a measured batch sends: PROBES for the 037 legs, A_PROBES for
# each of the many cases of the addressing legs. AWAIT_PROBES is the batch the
# convergence wait repeats until every probe of one batch gives the answer.
PROBES=20
A_PROBES=5
AWAIT_PROBES=3
# How long an addressing case waits for its answer. Short: by then the suite
# has converged (await_converged), so this is only the time a route or a
# refusal takes to be published.
A_TIMEOUT=45
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
# shellcheck source=e2e/etcd-image.sh
. "$REPO_ROOT/e2e/etcd-image.sh"
# The HTTPRoute of leg A4 and the response header it adds.
A4_ROUTE="mp-a4"
A4_HEADER_VALUE="a4"

WORKLOADS=(mp-httponly mp-multi mp-eighty mp-mixed mp-tcpfirst mp-nine)
CLIENTS=(client client-nocapture)

# wl_tag WORKLOAD: the prefix of its markers.
wl_tag() {
	case "$1" in
	mp-httponly) echo ctl ;;
	mp-multi) echo multi ;;
	mp-eighty) echo eighty ;;
	mp-mixed) echo mix ;;
	mp-tcpfirst) echo rev ;;
	mp-nine) echo nine ;;
	*) echo "unknown workload '$1'" >&2 && return 1 ;;
	esac
}

# wl_http_ports WORKLOAD: the HTTP ports it declares, space-separated.
wl_http_ports() {
	case "$1" in
	mp-httponly | mp-mixed | mp-tcpfirst) echo "$HTTP_PORT" ;;
	mp-multi) echo "$HTTP_PORT $ALT_PORT" ;;
	mp-eighty) echo "$LOW_PORT $HTTP_PORT $ALT_PORT" ;;
	mp-nine) echo "$TCP_PORT" ;;
	*) echo "unknown workload '$1'" >&2 && return 1 ;;
	esac
}

# wl_legacy_port WORKLOAD: its legacy target, the port its pods declare in
# endpoint.aether.io/port (proposal 043, "The legacy spellings during Phases 1
# and 2"). HTTP for every workload but mp-tcpfirst.
wl_legacy_port() {
	case "$1" in
	mp-tcpfirst | mp-nine) echo "$TCP_PORT" ;;
	*) echo "$HTTP_PORT" ;;
	esac
}

# marker WORKLOAD PORT: what the HTTP listener of WORKLOAD on PORT answers.
# tcp_marker WORKLOAD: what its l4echo answers.
marker() { printf '%s%s' "$(wl_tag "$1")" "$2"; }
tcp_marker() { printf '%stcp' "$(wl_tag "$1")"; }

mesh_name() { printf '%s.%s.%s' "$1" "$TEST_NS" "$MESH_DOMAIN"; }
k8s_fqdn() { printf '%s.%s.svc.cluster.local' "$1" "$TEST_NS"; }
k8s_short() { printf '%s.%s' "$1" "$TEST_NS"; }

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
note() { printf '\033[1;33m  %s\033[0m\n' "$*"; }
die() {
	printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2
	dump_state
	exit 1
}

kc() { kubectl --context "$CTX" "$@"; }

# live_backend: the registry backend of the cluster as it is installed, so a
# `verify` run without the MP_REGISTRY_BACKEND that `up` had still knows
# whether there are registry keys to read.
live_backend() {
	local args
	args="$(kc -n "$NS" get deploy aether-registrar -o jsonpath='{.spec.template.spec.containers[*].args}' 2>/dev/null || true)"
	case "$args" in
	*--registry-backend=kubernetes*) echo kubernetes ;;
	*--registry-backend=etcd*) echo etcd ;;
	*) echo "$MP_REGISTRY_BACKEND" ;;
	esac
}

# envoy_admin PATH: the node proxy's admin. The agent and the proxy are
# hostNetwork, so the kind node shares their netns and reaches Envoy's
# loopback-only admin.
envoy_admin() {
	local node
	node="$(kc -n "$TEST_NS" get pod -l app=client -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)"
	[ -n "$node" ] || return 0
	docker exec "$node" curl -s --max-time 5 "http://127.0.0.1:9901/$1" 2>/dev/null || true
}

# A red run is one of: the pod never registered under a key, the source has no
# chain for the port, the chain names a route table or a cluster that was never
# published, the table has no virtual host for the name, or the destination
# has no chain for the port. Print what tells them apart (the #590 principle).
dump_state() {
	kubectl config get-contexts "$CTX" >/dev/null 2>&1 || return 0
	printf '\033[1;33m  -- pods --\033[0m\n' >&2
	kc get pods -A -o wide 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- the workloads'"'"' declarations (endpoint.aether.io/port, /protocol, /ports) and capture mode --\033[0m\n' >&2
	kc -n "$TEST_NS" get pods -o custom-columns=NAME:.metadata.name,PORT:.metadata.annotations.endpoint\\.aether\\.io/port,PROTOCOL:.metadata.annotations.endpoint\\.aether\\.io/protocol,PORTS:.metadata.annotations.endpoint\\.aether\\.io/ports,REDIRECT-ALL:.metadata.annotations.capture\\.aether\\.io/redirect-all 2>&1 |
		sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- mesh services (frozen classification) --\033[0m\n' >&2
	kc -n "$TEST_NS" get svc -o custom-columns=NAME:.metadata.name,CLUSTERIP:.spec.clusterIP,PROTO:.metadata.annotations.aether\\.io/app-protocol,PORT:.metadata.annotations.aether\\.io/port 2>&1 |
		sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- mesh services: every port as <service> <port>/<name>/<appProtocol> --\033[0m\n' >&2
	kc -n "$TEST_NS" get svc -o jsonpath='{range .items[*]}{.metadata.name}{":"}{range .spec.ports[*]}{" "}{.port}{"/"}{.name}{"/"}{.appProtocol}{end}{"\n"}{end}' 2>&1 |
		sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- HTTPRoutes in %s (leg A4 applies and removes %s) --\033[0m\n' "$TEST_NS" "$A4_ROUTE" >&2
	kc -n "$TEST_NS" get httproutes.gateway.networking.k8s.io -o wide 2>&1 | sed 's/^/    /' >&2 || true
	if [ "$(live_backend)" = "etcd" ]; then
		printf '\033[1;33m  -- registry keys of the test services (one line per protocol the pod is registered under) --\033[0m\n' >&2
		docker exec "$ETCD_NAME" etcdctl get --prefix / --keys-only 2>/dev/null |
			grep -E '/services/mp-[a-z]+/' | sed 's/^/    /' >&2 || true
	fi
	# Names only: which chains, which route tables and which clusters exist is
	# the whole question.
	local listeners
	listeners="$(envoy_admin "config_dump?resource=dynamic_listeners")"
	printf '\033[1;33m  -- envoy: capture HTTP chains and the route tables they name (cap_http_*; one per HTTP port number) --\033[0m\n' >&2
	printf '%s\n' "$listeners" | grep -oE '"(cap_http|out_http)[^"]*"' | sort | uniq -c | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- envoy: TCP filter chains (cap_tcp_* on the source, in_tcp_* on the destination) --\033[0m\n' >&2
	printf '%s\n' "$listeners" | grep -oE '"(cap_tcp_|in_tcp_)[^"]*"' | sort | uniq -c | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- envoy: route configurations published (a chain that names one missing here never serves) --\033[0m\n' >&2
	envoy_admin "config_dump?resource=dynamic_route_configs" |
		awk '/envoy\.config\.route\.v3\.RouteConfiguration"/ { want = 1; next }
			want && /"name"/ { gsub(/[",]/, ""); print $2; want = 0 }' |
		sort | uniq -c | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- envoy: clusters of the test services, with their hosts'"'"' health --\033[0m\n' >&2
	envoy_admin clusters | grep -E '^[a-z:]*mp-[a-z]+\..*::health_flags' | sed -n '1,60s/^/    /p' >&2 || true
	printf '\033[1;33m  -- envoy: tcp: clusters, connect failures and totals --\033[0m\n' >&2
	envoy_admin clusters | grep -E '^tcp:.*(::cx_connect_fail|::rq_total)' | sed -n '1,40s/^/    /p' >&2 || true
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

# HTTPRoute (standard channel), for leg A4. It goes in BEFORE the agent
# starts: the GAMMA reconciler is gated on a RESTMapper lookup at manager-setup
# time, so a CRD applied after the helm install would need an agent restart
# (the ordering constraint e2e/l4routes.sh documents for the L4 types). GAMMA
# itself is chart default (agent.gamma: true).
install_gwapi_crds() {
	log "installing Gateway API CRDs ($GWAPI_VERSION, standard channel)"
	kc apply --server-side --force-conflicts -f \
		"https://github.com/kubernetes-sigs/gateway-api/releases/download/${GWAPI_VERSION}/standard-install.yaml" >/dev/null ||
		die "Gateway API CRD install failed"
	kc wait --for=condition=Established crd/httproutes.gateway.networking.k8s.io --timeout=60s >/dev/null ||
		die "the HTTPRoute CRD never became Established"
	ok "Gateway API CRDs installed"
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
	# Declared and assigned separately (SC2155), so `set -e` sees a failure.
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
	# any port of a mesh address), mesh DNS, GAMMA. Per-port protocols have no
	# values gate.
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

# container_yaml WORKLOAD KIND PORT: one container of a workload, as a YAML
# list item at the indentation of spec.template.spec.containers. KIND "http"
# is hashicorp/http-echo answering marker(WORKLOAD, PORT); KIND "tcp" is
# e2e/l4echo (reads one line, writes "<text> <line>", closes) answering
# tcp_marker(WORKLOAD). One container per port.
container_yaml() {
	local name="$1" kind="$2" port="$3"
	case "$kind" in
	http)
		cat <<YAML
        - name: http-$port
          image: $HTTP_ECHO_IMAGE
          args: ["-text=$(marker "$name" "$port")", "-listen=:$port"]
          ports: [{containerPort: $port}]
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: ["ALL"]}
YAML
		;;
	tcp)
		cat <<YAML
        - name: tcp-$port
          image: ${IMAGE_REGISTRY}/l4echo:latest
          imagePullPolicy: Never
          args: ["--mode=tcp", "--listen=:$port", "--text=$(tcp_marker "$name")"]
          ports: [{containerPort: $port}]
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: ["ALL"]}
YAML
		;;
	*) die "container_yaml: unknown listener kind '$kind'" ;;
	esac
}

# deploy_workload NAME PRIMARY PROTOCOL PORTS LISTENER... — one pod. PRIMARY,
# PROTOCOL and PORTS are its endpoint.aether.io/port, /protocol (the class of
# an unsuffixed port) and /ports; each LISTENER is "http:<port>" or
# "tcp:<port>", one container each (container_yaml). A listener is on the mesh
# only if PORTS declares it.
#
# A listener below 1024 (mp-eighty's :80) is bound by a NON-ROOT container
# with every capability dropped, through the pod-level sysctl
# net.ipv4.ip_unprivileged_port_start=0. It is one of Kubernetes' safe sysctls
# and is per network namespace, so it changes nothing outside the pod. The
# alternative, adding NET_BIND_SERVICE, does not work here: a capability added
# to a container that runs as a non-root user lands in its bounding set only
# (Kubernetes sets no ambient capabilities), and allowPrivilegeEscalation:
# false rules out a file capability on the binary.
#
# No Kubernetes Service is written: the registrar generates the selectorless
# mesh Service (the name mesh DNS and cluster DNS answer) for every registered
# service, and skips a name some other Service already owns. The registry
# service name is the pod's ServiceAccount.
deploy_workload() {
	local name="$1" primary="$2" protocol="$3" ports="$4" spec containers="" sysctls=""
	shift 4
	for spec in "$@"; do
		if [ "${spec#*:}" -lt 1024 ]; then
			sysctls='sysctls: [{name: net.ipv4.ip_unprivileged_port_start, value: "0"}]'
		fi
		containers+="$(container_yaml "$name" "${spec%%:*}" "${spec#*:}")"$'\n'
	done
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
        $sysctls
      containers:
$containers
YAML
}

# deploy_client NAME UPSTREAMS [REDIRECT_ALL] — a managed pod that only sleeps;
# the probes are exec'd into it. REDIRECT_ALL, when given, is its
# capture.aether.io/redirect-all annotation.
deploy_client() {
	local name="$1" upstreams="$2" redirect_all="${3:-}" capture=""
	if [ -n "$redirect_all" ]; then
		capture="capture.aether.io/redirect-all: \"$redirect_all\""
	fi
	kc apply -f - >/dev/null <<YAML || die "client $name apply failed"
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
        config.aether.io/upstreams: "$upstreams"
        $capture
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
	log "deploying ${WORKLOADS[*]} (mp-mixed ports: $MP_MIXED_PORTS) and ${CLIENTS[*]}"
	kc create ns "$TEST_NS" >/dev/null 2>&1 || true
	deploy_workload mp-httponly "$HTTP_PORT" http "$HTTP_PORT" "http:$HTTP_PORT" "tcp:$TCP_PORT"
	deploy_workload mp-multi "$HTTP_PORT" http "$HTTP_PORT,$ALT_PORT" "http:$HTTP_PORT" "http:$ALT_PORT"
	deploy_workload mp-eighty "$HTTP_PORT" http "$LOW_PORT,$HTTP_PORT,$ALT_PORT" "http:$LOW_PORT" "http:$HTTP_PORT" "http:$ALT_PORT"
	deploy_workload mp-mixed "$HTTP_PORT" http "$MP_MIXED_PORTS" "http:$HTTP_PORT" "tcp:$TCP_PORT"
	deploy_workload mp-tcpfirst "$TCP_PORT" tcp "$TCP_PORT,$HTTP_PORT=h1" "tcp:$TCP_PORT" "http:$HTTP_PORT"
	deploy_workload mp-nine "$TCP_PORT" http "$TCP_PORT" "http:$TCP_PORT"

	# The upstreams annotation is NOT an optimisation: there is no on-demand
	# discovery for tcp_proxy. An undeclared TCP upstream yields a chain that
	# matches and a cluster that is missing, which looks exactly like a routing
	# bug. Both clients name every workload.
	local w upstreams=""
	for w in "${WORKLOADS[@]}"; do
		upstreams+="${upstreams:+,}$w.$TEST_NS"
	done
	deploy_client client "$upstreams"
	deploy_client client-nocapture "$upstreams" false

	local d
	for d in "${WORKLOADS[@]}" "${CLIENTS[@]}"; do
		kc -n "$TEST_NS" rollout status "deploy/$d" --timeout=180s >/dev/null ||
			die "workload '$d' never became Ready"
	done
	ok "workloads deployed"
}

# --- probes -------------------------------------------------------------------

# The body of an HTTP probe batch, run by the CLIENT POD's sh (busybox ash: no
# bash, no pipefail). Arguments: URL, HOST (empty = the Host curl writes
# itself, which drops a default port), N. Prints one line per request:
#
#   <status> <first token of the body> <x-aether-error> <x-mp-route> <total s> <dns s>
#
# with "-" for what is absent and status 000 when there was no HTTP response.
HTTP_POD_SCRIPT="$(
	cat <<'SH'
url="$1"; host="$2"; n="$3"; i=1
while [ "$i" -le "$n" ]; do
	if [ -n "$host" ]; then
		curl -s --max-time 5 -D - -w "\nMPSTAT %{http_code} %{time_total} %{time_namelookup}\n" -H "Host: $host" "$url" 2>/dev/null
	else
		curl -s --max-time 5 -D - -w "\nMPSTAT %{http_code} %{time_total} %{time_namelookup}\n" "$url" 2>/dev/null
	fi | tr -d "\r" | awk '
		BEGIN { st = "000"; t = "0"; dns = "0"; err = "-"; rt = "-"; body = "-"; inbody = 0 }
		/^MPSTAT / { st = $2; t = $3; dns = $4; next }
		!inbody && /^$/ { inbody = 1; next }
		!inbody {
			k = tolower($1)
			if (k == "x-aether-error:" && $2 != "") err = $2
			if (k == "x-mp-route:" && $2 != "") rt = $2
			next
		}
		body == "-" && NF { body = $1 }
		END { print st, body, err, rt, t, dns }'
	i=$((i + 1))
done
SH
)"

# The body of a raw-TCP probe batch, run by the client pod's sh. Arguments:
# HOST:PORT, KIND, N. KIND "line" writes "probe-<i>\n"; KIND "http" writes a
# whole HTTP/1.1 request as raw bytes. Prints one line per probe: the first
# token of the reply (l4echo answers "<marker> <line>" and closes), or NOREPLY.
#
# The "line" payload is >= 6 bytes on purpose: the capture listener's
# http_inspector waits on an inconclusive short first write (#466).
TCP_POD_SCRIPT="$(
	cat <<'SH'
target="$1"; kind="$2"; n="$3"; i=1
while [ "$i" -le "$n" ]; do
	if [ "$kind" = "http" ]; then
		reply=$(printf "GET / HTTP/1.1\r\nHost: x\r\n\r\n" | curl -s --max-time 5 "telnet://$target" 2>/dev/null | sed -n "1p" | tr -d "\r")
	else
		reply=$(printf "probe-%s\n" "$i" | curl -s --max-time 5 "telnet://$target" 2>/dev/null | sed -n "1p" | tr -d "\r")
	fi
	reply="${reply%% *}"
	printf "%s\n" "${reply:-NOREPLY}"
	i=$((i + 1))
done
SH
)"

# http_tokens CLIENT URL HOST N — N GETs of URL from deploy/CLIENT in ONE exec,
# one token per request:
#
#   <marker>                      200, the first token of the body
#   <marker>+<value>              ... and the response carried x-mp-route: <value>
#   HTTP<status>                  any other status
#   HTTP<status>:<reason>         ... with x-aether-error: <reason>
#   HTTP421:<reason>:slow         a refusal that took a second or more in total
#   NOREPLY                       no HTTP response
#
# so every HTTP assertion is "every token of the batch is exactly WANT", and
# "the refusal is immediate" is part of the token.
http_tokens() {
	local client="$1" url="$2" host="$3" n="$4"
	kc -n "$TEST_NS" exec "deploy/$client" -c curl -- sh -c "$HTTP_POD_SCRIPT" sh "$url" "$host" "$n" 2>/dev/null |
		awk 'NF >= 5 {
			if ($1 == "000") tok = "NOREPLY"
			else if ($1 == "200") tok = $2
			else tok = "HTTP" $1
			if ($3 != "-") tok = tok ":" $3
			if ($4 != "-") tok = tok "+" $4
			if ($1 == "421" && $5 + 0 >= 1.0) tok = tok ":slow"
			print tok
		}' || true
}

# tcp_tokens HOST:PORT KIND N — N raw-TCP probes from the captured client in ONE
# exec; see TCP_POD_SCRIPT.
tcp_tokens() {
	local target="$1" kind="$2" n="$3"
	kc -n "$TEST_NS" exec deploy/client -c curl -- sh -c "$TCP_POD_SCRIPT" sh "$target" "$kind" "$n" 2>/dev/null || true
}

# show_exchange CLIENT URL HOST — one request, with its response headers, body
# and timings, on stderr: what a failed HTTP assertion looked like on the wire.
show_exchange() {
	local client="$1" url="$2" host="$3"
	printf '\033[1;33m  -- one request from %s: GET %s%s --\033[0m\n' "$client" "$url" "${host:+  (Host: $host)}" >&2
	# SC2016: the single quotes are the point — the body is evaluated by the
	# POD's shell; $1 and $2 are its arguments.
	# shellcheck disable=SC2016
	kc -n "$TEST_NS" exec "deploy/$client" -c curl -- sh -c '
		fmt="\n-- status %{http_code}, total %{time_total}s, dns %{time_namelookup}s, connect %{time_connect}s, to %{remote_ip}:%{remote_port}\n"
		if [ -n "$2" ]; then
			curl -s -i --max-time 5 -w "$fmt" -H "Host: $2" "$1"
		else
			curl -s -i --max-time 5 -w "$fmt" "$1"
		fi 2>&1
		echo "-- curl exit $?"
	' sh "$url" "$host" 2>&1 | sed -n '1,40s/^/    /p' >&2 || true
}

count_token() { printf '%s\n' "$1" | awk -v want="$2" '$1 == want { n++ } END { print n + 0 }'; }
histogram() { printf '%s\n' "$1" | awk 'NF { c[$1]++ } END { for (k in c) printf "%s=%d ", k, c[k] }'; }

# cap_timeout SECONDS — SECONDS, or MP_TIMEOUT when that is set and smaller.
cap_timeout() {
	if [ -n "$MP_TIMEOUT" ] && [ "$MP_TIMEOUT" -lt "$1" ]; then
		echo "$MP_TIMEOUT"
	else
		echo "$1"
	fi
}

# --- legs and their results ---------------------------------------------------

# The leg that is running, how many assertions it has made, and the ones that
# failed. `fail` ends the run at once (the default, and what CI does) or, under
# MP_KEEP_GOING=1, records the failure and lets the leg and the run go on.
CURRENT_LEG=""
LEG_CHECKS=0
LEG_FAILS=()
LEG_SKIP=""
RES_ID=()
RES_STATUS=()
RES_DETAIL=()

passed() {
	LEG_CHECKS=$((LEG_CHECKS + 1))
	ok "$*"
}

# fail WHAT [MEANING] — WHAT is what was seen (it goes in the table); MEANING is
# how to read it (printed with the failure, not repeated in the table).
fail() {
	local what="$1" meaning="${2:+ — $2}"
	LEG_CHECKS=$((LEG_CHECKS + 1))
	if [ "$MP_KEEP_GOING" != "1" ]; then
		die "$CURRENT_LEG: $what$meaning"
	fi
	printf '\033[1;31m  ✗ %s: %s%s\033[0m\n' "$CURRENT_LEG" "$what" "$meaning" >&2
	LEG_FAILS+=("$what")
}

skip() {
	LEG_SKIP="$*"
	note "SKIP $CURRENT_LEG: $*"
}

# expect WHAT WANT TIMEOUT N MEANING FN ARG... — FN ARG... <n> prints one token
# per probe (http_tokens, tcp_tokens). Poll small batches until every probe of
# one batch answers WANT, then one measured batch of N that must be WANT on
# every probe. The wait is the convergence gate: registration, the generated
# Service, the clusters, the chains and the route tables arrive one after
# another, and none of them has a bounded duration (#919). MEANING says how to
# read a wrong answer. Always returns 0: a failure is recorded by `fail`.
expect() {
	local what="$1" want="$2" timeout="$3" n="$4" meaning="$5" replies deadline
	shift 5
	timeout="$(cap_timeout "$timeout")"
	deadline=$((SECONDS + timeout))
	while true; do
		replies="$("$@" "$AWAIT_PROBES")"
		if [ "$(count_token "$replies" "$want")" -eq "$AWAIT_PROBES" ]; then
			break
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			if [ "$1" = "http_tokens" ]; then show_exchange "$2" "$3" "$4"; fi
			fail "$what never answered '$want' in ${timeout}s, last batch: $(histogram "$replies")" "$meaning"
			return 0
		fi
		sleep 3
	done
	replies="$("$@" "$n")"
	if [ "$(count_token "$replies" "$want")" -ne "$n" ]; then
		if [ "$1" = "http_tokens" ]; then show_exchange "$2" "$3" "$4"; fi
		fail "$what: expected $n/$n '$want', got $(histogram "$replies")" "$meaning"
		return 0
	fi
	passed "$what -> $n/$n $want"
}

# expect_unanswered WHAT HOST:PORT KIND MEANING — three raw-TCP probes, none of
# which may read anything back. Few probes: an unanswered one costs the client
# its whole timeout. No wait: this is a state that must hold whenever it is
# looked at.
expect_unanswered() {
	local what="$1" target="$2" kind="$3" meaning="$4" replies
	replies="$(tcp_tokens "$target" "$kind" 3)"
	if [ "$(count_token "$replies" NOREPLY)" -ne 3 ]; then
		fail "$what was answered ($(histogram "$replies"))" "$meaning"
		return 0
	fi
	passed "$what -> unanswered (3/3)"
}

# captured WANT AUTHORITY HOST MEANING — GET http://AUTHORITY/ from the captured
# client. HOST empty = the Host curl writes itself (AUTHORITY, without a
# default port); otherwise it is sent with -H, which is the only way to put an
# explicit ":80" or a port that differs from the dialled one on the wire.
captured() {
	local want="$1" authority="$2" host="$3" meaning="$4"
	expect "captured http://$authority/${host:+ with Host: $host}" "$want" "$A_TIMEOUT" "$A_PROBES" "$meaning" \
		http_tokens client "http://$authority/" "$host"
}

# loopback WANT HOST MEANING — GET the loopback outbound listener from the
# client that is not captured, with HOST naming the service.
loopback() {
	local want="$1" host="$2" meaning="$3"
	expect "loopback $LOOPBACK_URL with Host: $host" "$want" "$A_TIMEOUT" "$A_PROBES" "$meaning" \
		http_tokens client-nocapture "$LOOPBACK_URL" "$host"
}

# shared KEY FN — an assertion two legs own (X4 is the mp-tcpfirst half of
# A12). The first leg to reach it runs FN; the other reports what the first
# one saw instead of sending the same requests again.
shared() {
	local fn="$2" by="SHARED_${1}_BY" fails="SHARED_${1}_FAILS" before i joined=""
	if [ -n "${!by:-}" ]; then
		if [ -n "${!fails:-}" ]; then
			fail "failed when leg ${!by} asserted it in this run: ${!fails}"
		else
			passed "asserted by leg ${!by} in this run, not sent again"
		fi
		return 0
	fi
	before="${#LEG_FAILS[@]}"
	"$fn"
	for ((i = before; i < ${#LEG_FAILS[@]}; i++)); do
		joined+="${joined:+ | }${LEG_FAILS[$i]}"
	done
	printf -v "$by" '%s' "$CURRENT_LEG"
	printf -v "$fails" '%s' "$joined"
}

# svc_ports SERVICE — the generated Service's ports, one "<port>/<name>/<appProtocol>"
# per line.
svc_ports() {
	kc -n "$TEST_NS" get svc "$1" -o jsonpath='{range .spec.ports[*]}{.port}{"/"}{.name}{"/"}{.appProtocol}{"\n"}{end}' 2>/dev/null || true
}

# expect_svc_port SERVICE PORTS PORT NAME APPPROTOCOL — PORTS (svc_ports) has
# PORT; when NAME / APPPROTOCOL are not empty, with that name / appProtocol.
expect_svc_port() {
	local svc="$1" ports="$2" port="$3" name="$4" proto="$5" got
	got="$(printf '%s\n' "$ports" | awk -F/ -v p="$port" '$1 == p { print }')"
	if [ -z "$got" ]; then
		fail "the generated Service $svc has no port $port (ports: $(printf '%s' "$ports" | tr '\n' ' '))"
	elif [ -n "$name" ] && [ "$(printf '%s\n' "$got" | awk -F/ '{ print $2 }')" != "$name" ]; then
		fail "the generated Service $svc names port $port '$(printf '%s\n' "$got" | awk -F/ '{ print $2 }')', want '$name'"
	elif [ -n "$proto" ] && [ "$(printf '%s\n' "$got" | awk -F/ '{ print $3 }')" != "$proto" ]; then
		fail "the generated Service $svc has port $port with appProtocol '$(printf '%s\n' "$got" | awk -F/ '{ print $3 }')', want '$proto'" "the class the pods declare for that port"
	else
		passed "Service $svc: port $got"
	fi
}

# registry_protocols SERVICE — the protocols SERVICE has an endpoint under, one
# per line, sorted. The etcd key is
# .../services/<svc>/protocols/<PROTOCOL_X>/endpoints/<ip>.
registry_protocols() {
	docker exec "$ETCD_NAME" etcdctl get --prefix / --keys-only 2>/dev/null |
		sed -n "s#.*/services/$1/protocols/\\([A-Z_]*\\)/endpoints/.*#\\1#p" | sort -u || true
}

# expect_registered SERVICE WANT MEANING — WANT is the space-terminated sorted
# list of protocol keys, e.g. "PROTOCOL_HTTP PROTOCOL_TCP ".
expect_registered() {
	local svc="$1" want="$2" meaning="$3" got
	got="$(registry_protocols "$svc" | tr '\n' ' ')"
	if [ "$got" != "$want" ]; then
		fail "$svc is registered under '${got:-<nothing>}', want '$want'" "$meaning"
		return 0
	fi
	passed "$svc registered under: $got"
}

# --- proposal 037: the HTTP-first mixed service and its control ---------------

# M0 — the control, and it runs first: the state in which the TCP probe is not
# answered. Its HTTP half is what makes the TCP half mean something: the
# workload is up, registered, resolvable and reachable, and its :9000 listener
# is still not on the mesh.
leg_M0() {
	log "M0 control: mp-httponly declares only its HTTP port — its :$TCP_PORT listener must not be reachable through the mesh"
	local name target
	name="$(mesh_name mp-httponly)"
	expect "captured http://$name:$MESH_HTTP_PORT/" "$(marker mp-httponly "$HTTP_PORT")" 240 "$PROBES" \
		"the control workload does not serve HTTP through the mesh, so nothing below could mean anything" \
		http_tokens client "http://$name:$MESH_HTTP_PORT/" ""
	for target in "$name:$TCP_PORT" "$name:$MESH_TCP_PORT"; do
		expect_unanswered "raw TCP to $target" "$target" line \
			"the pod does not declare a TCP port: the probe cannot tell a declared port from an undeclared one, or the mesh forwards a port nobody registered"
	done
}

# M1..M3 — HTTP on a service that also has a TCP port. The TCP chains are
# matched on destination port and address ahead of everything else; these are
# the three HTTP spellings they must not shadow.
leg_M1() {
	local name
	name="$(mesh_name mp-mixed)"
	log "M1 http on the mesh port: http://$name:$MESH_HTTP_PORT/"
	expect "captured http://$name:$MESH_HTTP_PORT/" "$(marker mp-mixed "$HTTP_PORT")" 240 "$PROBES" \
		"HTTP to the mesh port of a mixed service is not served by its HTTP port" \
		http_tokens client "http://$name:$MESH_HTTP_PORT/" ""
}

leg_M2() {
	local name
	name="$(mesh_name mp-mixed)"
	log "M2 http, portless: http://$name/ (the client dials 80)"
	expect "captured http://$name/" "$(marker mp-mixed "$HTTP_PORT")" 90 "$PROBES" \
		"a portless http:// URL does not reach the service's HTTP port" \
		http_tokens client "http://$name/" ""
}

leg_M3() {
	local name
	name="$(mesh_name mp-mixed)"
	log "M3 http on the app port: http://$name:$HTTP_PORT/"
	expect "captured http://$name:$HTTP_PORT/" "$(marker mp-mixed "$HTTP_PORT")" 90 "$PROBES" \
		"HTTP to the service's own HTTP port is not served by it" \
		http_tokens client "http://$name:$HTTP_PORT/" ""
}

# M4, M5 — raw TCP to the HTTP-first service.
leg_M4() {
	local name
	name="$(mesh_name mp-mixed)"
	log "M4 raw TCP on the service's own TCP port: $name:$TCP_PORT"
	expect "raw TCP to $name:$TCP_PORT" "$(tcp_marker mp-mixed)" 180 "$PROBES" \
		"NOREPLY means no chain claims the port on the source (the pod is not registered with $TCP_PORT=tcp, or the per-port chain is missing); an HTTP marker or status means the connection was delivered to the HTTP port" \
		tcp_tokens "$name:$TCP_PORT" line
}

# :18082 reaches the port the pods declare in endpoint.aether.io/port if that
# port is raw TCP (proposal 043, the legacy rule), and this one is HTTP: the
# spelling names nothing. Asserted so that the answer is a decision.
leg_M5() {
	local name
	name="$(mesh_name mp-mixed)"
	log "M5 the TCP mesh port of a service whose declared port is HTTP: $name:$MESH_TCP_PORT names no port"
	expect_unanswered "raw TCP to $name:$MESH_TCP_PORT" "$name:$MESH_TCP_PORT" line \
		"an HTTP status would be raw bytes forwarded to the HTTP port; '$(tcp_marker mp-mixed)' means :$MESH_TCP_PORT now reaches a TCP port that is not the declared one: update this leg and the proposal's legacy rule together"
}

# M6 — one pod, two keys. This replaces a check that read the pod's own
# annotation back, which the pod carries whether or not anything honoured it.
leg_M6() {
	log "M6 registration: mp-mixed under BOTH protocol keys, mp-httponly under one"
	if [ "$BACKEND" != "etcd" ]; then
		skip "the '$BACKEND' registry backend derives endpoints from the pods and has no per-protocol key to read"
		return 0
	fi
	expect_registered mp-mixed "PROTOCOL_HTTP PROTOCOL_TCP " "a pod with an HTTP and a TCP port registers once per protocol"
	expect_registered mp-httponly "PROTOCOL_HTTP " "its TCP listener is not declared"
}

# M7 — the generated mesh Service: the real ports with their class, beside the
# four legacy ports, and the annotation frozen at what the 2.6 registrar
# stamped (no 2.7 component reads it; 2.6 agents do).
leg_M7() {
	log "M7 the generated mesh Service of mp-mixed: real ports beside the legacy four"
	local proto ports p
	ports="$(svc_ports mp-mixed)"
	expect_svc_port mp-mixed "$ports" "$HTTP_PORT" "p$HTTP_PORT-tcp" http
	expect_svc_port mp-mixed "$ports" "$TCP_PORT" "p$TCP_PORT-tcp" tcp
	for p in "$MESH_HTTP_PORT" "$MESH_TCP_PORT" 80 443; do
		expect_svc_port mp-mixed "$ports" "$p" "" ""
	done
	proto="$(kc -n "$TEST_NS" get svc mp-mixed -o jsonpath='{.metadata.annotations.aether\.io/app-protocol}' 2>/dev/null || true)"
	if [ "$proto" != "http" ]; then
		fail "the mesh Service mp-mixed carries aether.io/app-protocol='${proto:-<unset>}', want 'http'" "the annotation is frozen at its 2.6 value while 2.6 agents may still read it"
	else
		passed "Service mp-mixed: aether.io/app-protocol=http"
	fi
}

# --- the mirror shape: a raw-TCP declared port with an HTTP port (#1672) ------

leg_X1() {
	local name
	name="$(mesh_name mp-tcpfirst)"
	log "X1 the TCP mesh port of a service whose declared port is raw TCP and that also serves HTTP: $name:$MESH_TCP_PORT"
	shared tcpfirst_meshtcp check_tcpfirst_meshtcp
}

check_tcpfirst_meshtcp() {
	local name
	name="$(mesh_name mp-tcpfirst)"
	expect "raw TCP to $name:$MESH_TCP_PORT" "$(tcp_marker mp-tcpfirst)" 180 "$PROBES" \
		"NOREPLY means the source has no :$MESH_TCP_PORT chain for the service; an HTTP status means the bytes were forwarded to the HTTP port" \
		tcp_tokens "$name:$MESH_TCP_PORT" line
}

leg_X2() {
	local name
	name="$(mesh_name mp-tcpfirst)"
	log "X2 raw TCP on its declared port: $name:$TCP_PORT"
	expect "raw TCP to $name:$TCP_PORT" "$(tcp_marker mp-tcpfirst)" 90 "$PROBES" \
		"the raw-TCP port of the service is not served by its own chain" \
		tcp_tokens "$name:$TCP_PORT" line
}

leg_X3() {
	local name
	name="$(mesh_name mp-tcpfirst)"
	log "X3 HTTP on its HTTP port: http://$name:$HTTP_PORT/"
	expect "captured http://$name:$HTTP_PORT/" "$(marker mp-tcpfirst "$HTTP_PORT")" 90 "$PROBES" \
		"the HTTP port of the service is not served as HTTP (NOREPLY: a chain with no port took the connection from the HTTP chain)" \
		http_tokens client "http://$name:$HTTP_PORT/" ""
}

# X4, and the mp-tcpfirst half of A12 — the legacy HTTP spellings reach the
# port the pods declare in endpoint.aether.io/port IF that port is HTTP. This
# one is raw TCP, so they name nothing, and HTTP can say so: 421 with
# x-aether-error: port-required, at once. A 502 is the request forwarded to
# the raw-TCP port as h2; '<marker>' means the spelling now reaches an HTTP
# port that is not the declared one.
leg_X4() {
	log "X4 (= A12 for mp-tcpfirst) the legacy HTTP spellings of a service whose declared port is raw TCP are refused by name"
	shared tcpfirst_refused check_tcpfirst_refused
}

check_tcpfirst_refused() {
	local name want meaning
	name="$(mesh_name mp-tcpfirst)"
	want="HTTP421:port-required"
	meaning="HTTP502 is the request sent to the raw-TCP port; '$(marker mp-tcpfirst "$HTTP_PORT")' means the legacy spelling reaches the HTTP port; ':slow' is a refusal that was not immediate"
	captured "$want" "$name:$MESH_HTTP_PORT" "" "$meaning"
	captured "$want" "$name" "" "$meaning"
	captured "$want" "$(k8s_fqdn mp-tcpfirst)" "" "$meaning"
	loopback "$want" "$name" "$meaning"
	loopback "$want" "$name:$MESH_HTTP_PORT" "$meaning"
}

leg_X5() {
	log "X5 the generated mesh Service of mp-tcpfirst: its real ports, each with its own class"
	local ports
	ports="$(svc_ports mp-tcpfirst)"
	expect_svc_port mp-tcpfirst "$ports" "$TCP_PORT" "" tcp
	expect_svc_port mp-tcpfirst "$ports" "$HTTP_PORT" "" http
}

leg_X6() {
	log "X6 registration: mp-tcpfirst under BOTH protocol keys"
	if [ "$BACKEND" != "etcd" ]; then
		skip "the '$BACKEND' registry backend has no per-protocol key to read (see M6)"
		return 0
	fi
	expect_registered mp-tcpfirst "PROTOCOL_HTTP PROTOCOL_TCP " "a pod with a TCP and an HTTP port registers once per protocol"
}

# --- proposal 043 Phase 1: the addressing rule --------------------------------

# A1 — the dialled port is the port. Host is sent explicitly because curl drops
# a default port: for mp-eighty this is the request whose Host says ":80", and
# port 80 must answer it (the PORT-LESS spelling of the same dial is A12).
leg_A1() {
	log "A1 captured: dial <mesh name>:P with Host <mesh name>:P, every HTTP port of every workload"
	local w p name
	for w in "${WORKLOADS[@]}"; do
		name="$(mesh_name "$w")"
		for p in $(wl_http_ports "$w"); do
			captured "$(marker "$w" "$p")" "$name:$p" "$name:$p" \
				"another marker is another port or another service answering; NOREPLY on a port number that some service declares as raw TCP is the request leaving through the passthrough (A10)"
		done
	done
}

# A2 — the Kubernetes names obey the same rule as the mesh name (#1682: with a
# port that is not the declared one they used to reach the declared one).
# mp-eighty's port 80 is left to A12: curl cannot dial it with a port in Host
# unless told to, and the port-less form is a legacy spelling.
leg_A2() {
	log "A2 captured: dial <svc>.<ns>.svc.cluster.local:P and <svc>.<ns>:P (#1682)"
	local w p name
	for w in mp-multi mp-eighty mp-mixed; do
		for p in $(wl_http_ports "$w"); do
			[ "$p" != "$LOW_PORT" ] || continue
			for name in "$(k8s_fqdn "$w")" "$(k8s_short "$w")"; do
				captured "$(marker "$w" "$p")" "$name:$p" "" \
					"'$(marker "$w" "$(wl_legacy_port "$w")")' for another port is #1682: a Kubernetes name sent to the declared port whatever port was dialled"
			done
		done
	done
}

# A3 — the loopback listener: nothing is dialled, so the port in Host is the
# port (decision 5). The same with capture off, which is this client.
leg_A3() {
	log "A3 loopback (client without capture): Host <mesh name>:P, every HTTP port of every workload"
	local w p name
	for w in "${WORKLOADS[@]}"; do
		name="$(mesh_name "$w")"
		for p in $(wl_http_ports "$w"); do
			loopback "$(marker "$w" "$p")" "$name:$p" \
				"the loopback listener has no virtual host for that port of the service, or sent it to another port"
		done
	done
}

# A4 — a parentRef with no port applies to every HTTP port of the Service.
#
# The rule has a filter and NO backendRefs. That is the form whose destination
# is "the parent Service, on the port that was addressed": the agent routes a
# rule without backends to the cluster of the virtual host it sits on
# (gammaRouteAction), where a backendRef would name one port and move a request
# for the other one to it. So each request must both carry the header (the rule
# is on that port's virtual host) and be answered by the port it was sent to
# (the rule did not move it).
leg_A4() {
	log "A4 an HTTPRoute with a port-less parentRef on mp-multi reaches every HTTP port of it, captured and loopback"
	local name p base
	name="$(mesh_name mp-multi)"
	if ! kc get crd httproutes.gateway.networking.k8s.io >/dev/null 2>&1; then
		fail "the HTTPRoute CRD is not installed, so the agent never watched it" "run '$0 up' first (it installs Gateway API $GWAPI_VERSION); the leg is NOT skipped, because a leg that skips itself green cannot fail (#853)"
		return 0
	fi
	if ! apply_a4_route; then
		fail "the HTTPRoute $A4_ROUTE was not accepted by the API server"
		return 0
	fi
	for p in $(wl_http_ports mp-multi); do
		base="$(marker mp-multi "$p")"
		captured "$base+$A4_HEADER_VALUE" "$name:$p" "" \
			"'$base' alone is the port answering WITHOUT the route's header: the rule is not on that port's virtual host; another marker is the rule moving the request to another port"
		loopback "$base+$A4_HEADER_VALUE" "$name:$p" \
			"'$base' alone is the port answering WITHOUT the route's header on the loopback listener; another marker is the rule moving the request to another port"
	done
	# Remove it, and wait until it is gone from the data plane: every later leg
	# compares whole tokens, and a leftover header would fail them all.
	remove_a4_route
	for p in $(wl_http_ports mp-multi); do
		base="$(marker mp-multi "$p")"
		captured "$base" "$name:$p" "" "the HTTPRoute was deleted and its header is still added"
		loopback "$base" "$name:$p" "the HTTPRoute was deleted and its header is still added"
	done
}

apply_a4_route() {
	kc apply -f - >/dev/null <<YAML
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: $A4_ROUTE, namespace: $TEST_NS}
spec:
  parentRefs:
    - {group: "", kind: Service, name: mp-multi}
  rules:
    - filters:
        - type: ResponseHeaderModifier
          responseHeaderModifier:
            add:
              - {name: x-mp-route, value: $A4_HEADER_VALUE}
YAML
}

remove_a4_route() {
	kc -n "$TEST_NS" delete httproutes.gateway.networking.k8s.io "$A4_ROUTE" --ignore-not-found --wait=true >/dev/null 2>&1 || true
}

# A5 — a Host port that disagrees with the dialled port is refused, not obeyed:
# a request is never answered by a port the client did not connect to.
leg_A5() {
	log "A5 captured: dial mp-multi:$HTTP_PORT with Host mp-multi:$ALT_PORT is refused, port-mismatch"
	local name
	name="$(mesh_name mp-multi)"
	captured "HTTP421:port-mismatch" "$name:$HTTP_PORT" "$name:$ALT_PORT" \
		"'$(marker mp-multi "$ALT_PORT")' is Host deciding the port; '$(marker mp-multi "$HTTP_PORT")' is the Host port being ignored; ':slow' is a refusal that was not immediate"
}

# A6 — on loopback the Host port is the port, and a port declared raw TCP is
# not HTTP.
leg_A6() {
	log "A6 loopback: Host <mesh name>:$TCP_PORT, a raw-TCP port, is refused, port-not-http"
	local w
	for w in mp-mixed mp-tcpfirst; do
		loopback "HTTP421:port-not-http" "$(mesh_name "$w"):$TCP_PORT" \
			"HTTP502/503 is the request forwarded to the raw-TCP port or to a cluster nobody published; ':slow' is a refusal that was not immediate"
	done
}

# A7 — a port the Service does not declare is refused at once, by name, on both
# name families and both paths. Not a 503 after the on-demand timeout, and not
# another port's answer.
leg_A7() {
	log "A7 mp-multi port $UNDECLARED_PORT, which it does not declare, is refused, port-not-declared"
	local name meaning
	meaning="HTTP503 (and ':slow') is the on-demand lookup of a cluster that will never exist; a marker is another port answering for an undeclared one; NOREPLY is the request leaving through the passthrough"
	for name in "$(mesh_name mp-multi)" "$(k8s_fqdn mp-multi)" "$(k8s_short mp-multi)"; do
		captured "HTTP421:port-not-declared" "$name:$UNDECLARED_PORT" "$name:$UNDECLARED_PORT" "$meaning"
	done
	loopback "HTTP421:port-not-declared" "$(mesh_name mp-multi):$UNDECLARED_PORT" "$meaning"
}

# A8 — raw TCP has no name to refuse by: an undeclared port has no chain and
# gets the cluster's own answer (decision 10). Only "no application answers"
# is asserted.
leg_A8() {
	log "A8 captured: raw TCP to mp-mixed:$UNDECLARED_PORT, which it does not declare, reaches no application"
	local name
	name="$(mesh_name mp-mixed)"
	expect_unanswered "raw TCP to $name:$UNDECLARED_PORT" "$name:$UNDECLARED_PORT" line \
		"a marker is a listener the mesh forwarded an undeclared port to"
}

# A9 — a port-less Host means the port that was dialled, as it does to every
# HTTP server. Port 80 is A12's.
leg_A9() {
	log "A9 captured: dial <name>:P with a PORT-LESS Host, every HTTP port but $LOW_PORT"
	local w p name
	for w in mp-multi mp-eighty mp-mixed; do
		for p in $(wl_http_ports "$w"); do
			[ "$p" != "$LOW_PORT" ] || continue
			for name in "$(mesh_name "$w")" "$(k8s_fqdn "$w")"; do
				captured "$(marker "$w" "$p")" "$name:$p" "$name" \
					"'$(marker "$w" "$(wl_legacy_port "$w")")' for another port is the port-less Host sent to the declared port instead of the dialled one"
			done
		done
	done
}

# A10 — one service's raw-TCP declaration of a port number must not take that
# number away from another service's HTTP. Envoy's filter-chain match does not
# backtrack: with a single port-less HTTP chain, mp-mixed's chain on 9000 sends
# every other connection to 9000 to the passthrough.
leg_A10() {
	log "A10 captured: HTTP to mp-nine:$TCP_PORT, a port number mp-mixed declares as raw TCP"
	local name
	name="$(mesh_name mp-nine)"
	captured "$(marker mp-nine "$TCP_PORT")" "$name:$TCP_PORT" "" \
		"NOREPLY is the request leaving through the passthrough because another service's raw-TCP chain names port $TCP_PORT; '$(tcp_marker mp-mixed)' is the wrong service"
}

# A11 — the declaration decides a port's class, never the bytes. HTTP bytes to
# a port declared raw TCP are carried as bytes (l4echo echoes the request line
# after its marker); bytes that are not HTTP to a port declared HTTP reach no
# application.
leg_A11() {
	log "A11 captured: the declaration decides, not the bytes"
	local name
	name="$(mesh_name mp-mixed)"
	expect "an HTTP request as raw bytes to $name:$TCP_PORT" "$(tcp_marker mp-mixed)" "$A_TIMEOUT" "$A_PROBES" \
		"'HTTP/1.1' is an HTTP response: the bytes were sniffed and handed to an HTTP chain although the port is declared raw TCP; NOREPLY is no chain for the port" \
		tcp_tokens "$name:$TCP_PORT" http
	name="$(mesh_name mp-nine)"
	expect_unanswered "bytes that are not HTTP to $name:$TCP_PORT" "$name:$TCP_PORT" line \
		"'HTTP/1.1' is mp-nine's HTTP server answering raw bytes that were forwarded to it; '$(tcp_marker mp-mixed)' is another service's TCP port"
}

# A12 — the legacy spellings, which this phase deliberately leaves alone. The
# legacy target of a service is the port its pods declare in
# endpoint.aether.io/port; :18081, the port-less URL (a dial to 80) and a
# port-less or :18081 Host on loopback reach it when it is HTTP.
#
# mp-eighty is the case to read twice: it DECLARES port 80, and a port-less
# dial of port 80 still answers from 8080, its legacy target (proposal 043,
# decision 11: the legacy meaning of a dial to port 80 is kept through chart
# 2.8, because changing it is from one working answer to another). Port 80 by
# name, with ":80" in Host, is A1.
leg_A12() {
	log "A12 the legacy spellings reach the port declared in endpoint.aether.io/port, unchanged in this phase"
	local w name want meaning
	for w in "${WORKLOADS[@]}"; do
		[ "$w" != "mp-tcpfirst" ] || continue
		name="$(mesh_name "$w")"
		want="$(marker "$w" "$(wl_legacy_port "$w")")"
		meaning="the legacy target of $w is :$(wl_legacy_port "$w"); another marker is a legacy spelling that changed meaning in this phase"
		captured "$want" "$name:$MESH_HTTP_PORT" "" "$meaning"
		captured "$want" "$name" "" "$meaning"
		captured "$want" "$(k8s_fqdn "$w")" "" "$meaning"
		loopback "$want" "$name" "$meaning"
		loopback "$want" "$name:$MESH_HTTP_PORT" "$meaning"
	done
	note "mp-tcpfirst, whose legacy target is raw TCP: its HTTP spellings are refused (X4) and :$MESH_TCP_PORT reaches it (X1)"
	shared tcpfirst_refused check_tcpfirst_refused
	shared tcpfirst_meshtcp check_tcpfirst_meshtcp
}

# --- verify -------------------------------------------------------------------

# await_converged — before any leg: wait until every workload answers on one
# HTTP spelling that works with and without proposal 043 (its HTTP port by
# number; for mp-nine the mesh port, because its port number is the one A10 is
# about). It asserts nothing: a workload that never answers is reported and
# left to the legs, which say what is wrong with it. What it buys is that a
# leg run alone (MP_LEGS) or first does not need a registration-sized timeout.
await_converged() {
	log "waiting for every workload to answer once through the mesh"
	local w name authority want deadline replies
	for w in "${WORKLOADS[@]}"; do
		name="$(mesh_name "$w")"
		authority="$name:$HTTP_PORT"
		want="$(marker "$w" "$HTTP_PORT")"
		if [ "$w" = "mp-nine" ]; then
			authority="$name:$MESH_HTTP_PORT"
			want="$(marker "$w" "$TCP_PORT")"
		fi
		deadline=$((SECONDS + $(cap_timeout 240)))
		while true; do
			replies="$(http_tokens client "http://$authority/" "" "$AWAIT_PROBES")"
			if [ "$(count_token "$replies" "$want")" -eq "$AWAIT_PROBES" ]; then
				ok "$w answers ($want)"
				break
			fi
			if [ "$SECONDS" -ge "$deadline" ]; then
				note "$w did not answer '$want' on http://$authority/ (last batch: $(histogram "$replies")); going on, the legs will say why"
				break
			fi
			sleep 3
		done
	done
}

leg_selected() {
	case " $MP_LEGS " in
	*" $1 "*) return 0 ;;
	*) return 1 ;;
	esac
}

# run_leg ID — run leg_<ID> and record PASS, FAIL or SKIP with a detail.
run_leg() {
	local id="$1" i detail=""
	CURRENT_LEG="$id"
	LEG_CHECKS=0
	LEG_FAILS=()
	LEG_SKIP=""
	"leg_$id"
	RES_ID+=("$id")
	if [ -n "$LEG_SKIP" ]; then
		RES_STATUS+=(SKIP)
		RES_DETAIL+=("$LEG_SKIP")
	elif [ "${#LEG_FAILS[@]}" -gt 0 ]; then
		for ((i = 0; i < ${#LEG_FAILS[@]}; i++)); do
			detail+="${detail:+ | }${LEG_FAILS[$i]}"
		done
		RES_STATUS+=(FAIL)
		RES_DETAIL+=("${#LEG_FAILS[@]} of $LEG_CHECKS assertions failed: $detail")
	else
		RES_STATUS+=(PASS)
		RES_DETAIL+=("$LEG_CHECKS assertions")
	fi
}

print_results() {
	local i
	printf '\n%-4s %-6s %s\n' LEG RESULT DETAIL
	for ((i = 0; i < ${#RES_ID[@]}; i++)); do
		printf '%-4s %-6s %s\n' "${RES_ID[$i]}" "${RES_STATUS[$i]}" "${RES_DETAIL[$i]}"
	done
}

verify() {
	local d id i failed=0 known
	# Commas or spaces; every ID must be one this suite has.
	MP_LEGS="$(printf '%s' "$MP_LEGS" | tr ',' ' ')"
	for id in $MP_LEGS; do
		known=0
		for d in "${ALL_LEGS[@]}"; do
			if [ "$d" = "$id" ]; then known=1; fi
		done
		if [ "$known" -ne 1 ]; then
			echo "MP_LEGS names '$id', which is not a leg of this suite (${ALL_LEGS[*]})" >&2
			exit 2
		fi
	done
	for d in "${WORKLOADS[@]}" "${CLIENTS[@]}"; do
		kc -n "$TEST_NS" get deploy "$d" >/dev/null 2>&1 ||
			die "'$d' is not deployed in '$CLUSTER': run '$0 up' first"
	done
	BACKEND="$(live_backend)"
	log "verifying on the '$BACKEND' registry backend; legs: $MP_LEGS"
	# A route left by a run that was interrupted inside A4 would add its header
	# to every answer of mp-multi.
	remove_a4_route
	await_converged
	for id in "${ALL_LEGS[@]}"; do
		if leg_selected "$id"; then run_leg "$id"; fi
	done
	print_results
	for ((i = 0; i < ${#RES_ID[@]}; i++)); do
		if [ "${RES_STATUS[$i]}" = "FAIL" ]; then failed=$((failed + 1)); fi
	done
	if [ "$failed" -gt 0 ]; then
		# Only reached under MP_KEEP_GOING=1: without it the first failure ended
		# the run in `die`.
		printf '\033[1;31m\n  ✗ %s leg(s) failed\033[0m\n' "$failed" >&2
		dump_state
		exit 1
	fi
	log "all selected multi-protocol and addressing legs passed on the '$BACKEND' backend (see the table above for the ones skipped)"
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

# The registry backend `verify` found in the cluster (live_backend).
BACKEND="$MP_REGISTRY_BACKEND"

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
