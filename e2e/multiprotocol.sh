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
#   mp-tcponly    9000   tcp        9000            tcp  9000 -> onlytcp
#                                                   tcp  7777 -> onlytcp7777 (NOT declared)
#
# (/port, /protocol and /ports are the endpoint.aether.io/ annotations.)
# mp-httponly is the control: its :9000 listener is as alive as mp-mixed's and
# is not declared. mp-nine serves HTTP on the port NUMBER mp-mixed declares as
# raw TCP. mp-eighty really listens on 80. mp-tcponly has no HTTP port at all;
# its second listener is not declared and answers with a marker of its own, so
# "the port that was dialled reached the pod" and "the declared port answered
# for another one" are two different answers.
#
# TWO CLIENTS. "client" is captured (the chart default, redirect-all).
# "client-nocapture" carries capture.aether.io/redirect-all: "false" and only
# ever dials the loopback outbound listener, 127.0.0.1:18081, with the service
# in Host, or uses that listener as its HTTP proxy (the P legs; there the
# captured client does the same).
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
#   M1  http, mesh    mp-mixed http://<name>:18081/ -> 421 port-not-declared:
#                     18081 is the proxy's port, not a port of the service
#   M2  http, bare    mp-mixed http://<name>/ (dials 80) -> 421
#                     port-not-declared: it does not declare 80
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
#                     tcp named p9000-tcp) and the two mesh ports (18081,
#                     18082); no port 80 or 443 it does not declare, and
#                     neither aether.io/app-protocol nor aether.io/port
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
#   A12 both       NO spelling means "the port the pods declare" (see leg_A12);
#                  what follows is what the three spellings meant until chart
#                  3.0.0: :18081, the
#                  port-less URL, the port-less svc.cluster.local URL, and on
#                  loopback a port-less Host and Host <mesh name>:18081 reach
#                  the port the pods declare in endpoint.aether.io/port when
#                  it is HTTP; for mp-tcpfirst they are refused, 421
#                  port-required, and :18082 reaches its raw TCP (X4, X1)
#
# A raw-TCP service is reached on the ports it declares (chart 3.0.0 removes
# the chain that sent ANY port of such a service to its declared port):
#
#   R1  routes     an HTTPRoute on mp-httponly whose backendRef names a port
#                  mp-multi does not have (7777, 18081, 80): ResolvedRefs is
#                  False/BackendNotFound and the request is answered 500,
#                  captured and loopback
#   R2  routes     an HTTPRoute whose parentRef names a port mp-httponly does
#                  not have (18081, 80, 7777): Accepted is
#                  False/NoMatchingParent and mp-httponly answers for itself
#   R3  routes     a valid and an invalid backendRef of equal weight: over 60
#                  requests only the valid port's marker and 500 are seen
#   N1  short name mp-short exists in two namespaces. From the first: Host
#                  mp-short:8080 is the first's (shorta), captured, through
#                  the proxy port, and when the other's address is dialled
#   N2  short name the mirror, from the second namespace (shortb); and the
#                  per-port refusals apply to the short name
#   N3  short name from a third namespace, which has no MESH mp-short but a
#                  plain Service of that name: not a mesh name there. It
#                  reaches its own plain Service (shortplain); a dial to the
#                  other namespaces' mesh addresses with the short Host is
#                  answered by neither; on the proxy port, what any name
#                  that is not the mesh's gets
#   V1  reserved   18081 and 18082 are the mesh's: Host <a name nothing
#                  serves>:18081 -> 421 port-not-declared at once, on both
#                  paths; a pod that declares one is denied at admission
#   U1  user svc   mp-own is a mesh service behind a Service of the user's
#                  (port 8000 -> 8080) that no route names. Its Kubernetes
#                  names are not captured mesh names: http://mp-own:8000/
#                  and http://mp-own.<ns>:8000/ reach the pods through the
#                  Service. A service-wide HTTPFilter that targets it (seen
#                  in force on the mesh name: 403 on the loopback listener
#                  and through the proxy port from the captured client)
#                  changes neither, nor a dial to the Service's port with
#                  the mesh name in Host. Two more Services of the user's
#                  select the same pods, on a port number another mesh
#                  service serves as HTTP (9090) and on the pods' own
#                  (8080): with the mesh name in Host, the first passes
#                  through as before the filter, and the second, which was a
#                  mesh delivery before it, is one WITH the filter (403).
#                  None is refused 421
#   U2  named      a Service port whose targetPort is a NAME is refused
#                  until the name resolves, and resolves on the
#                  EndpointSlice's EVENT. (a) A selectorless Service and an
#                  HTTPRoute to it: False/BackendNotFound and 500 on both
#                  paths; an EndpointSlice made by hand names the port and
#                  the status turns True, a second that gives the name
#                  another number turns it False again, and deleting that
#                  one turns it back: nothing but the slices changes, so
#                  only the agent's slice watch can have told it. (b) A
#                  Service of the user's with a named targetPort that NO
#                  route names fronts mesh pods: their mesh name answers on
#                  the proxy port by the Service's port
#   T1  replaced   mp-move serves HTTP on 8080, then is redeployed serving
#                  raw TCP on 9000 only. Host mp-move:8080 on loopback is
#                  refused (421 port-not-declared) within T1_WINDOW of the
#                  rollout, well inside the 90 s a vanished service's
#                  cluster and virtual host are kept for; raw TCP to :9000
#                  answers. Runs last: it changes a workload
#   W1  captured   mp-tcponly: raw TCP to <mesh name>:9000 and to :18082
#                  -> onlytcp
#   W2  captured   mp-tcponly: raw TCP to :7777 (a port it listens on and does
#                  not declare), to :8080 and :9090 (HTTP port numbers of OTHER
#                  services) and to :80 -> no application answers. Any marker
#                  is a failure: onlytcp is the declared port answering for
#                  another one, onlytcp7777 the undeclared listener, anything
#                  else another service. What the client sees instead is
#                  printed, not asserted, one line per port starting with
#                  "W2-OBSERVED": curl's exit code and how long it took
#   W3  both       mp-tcponly over HTTP: captured http://<mesh name>:8080/
#                  -> 421 port-not-declared; captured :18081 and the port-less
#                  URL -> 421 no-http-port; loopback Host <mesh name>:9000
#                  -> 421 port-not-http
#
# :18081 is the mesh's HTTP proxy port. A client that is given a proxy
# connects to it and sends the absolute URL and Host: <name>:<port>; nothing
# about the connection names a service port, so Host decides, port included.
# That holds for the loopback listener 127.0.0.1:18081 and for a captured dial
# to <name>:18081. A dial to a REAL port is unchanged (A5), and so are
# Host <name>:18081 and a port-less Host (A12). "proxied" below is literally
# curl --proxy http://127.0.0.1:18081 <url>, from BOTH clients:
#
#   P1  proxied    http://<mesh name of mp-multi>:8080/ and :9090/ -> multi8080,
#                  multi9090; mp-eighty's port 80 -> eighty80. curl drops a
#                  default port from the request line and from Host, so that
#                  one request is sent with --request-target and -H Host, both
#                  spelling ":80"; what the plain http://<name>:80/ gets (a
#                  port-less authority, the legacy spelling of A12) is printed
#                  as "P1-OBSERVED" and not asserted
#   P2  proxied    http://<mesh name of mp-multi>:7777/ -> 421
#                  port-not-declared; http://<mesh name of mp-mixed>:9000/
#                  -> 421 port-not-http; and, from client-nocapture, a name
#                  that is no mesh service (http://example.invalid:8080/)
#                  -> 404
#   P3  captured   dial http://<name of mp-multi>:18081/ with Host <name>:9090
#                  -> multi9090, Host <name>:8080 -> multi8080, Host
#                  <name>:7777 -> 421 port-not-declared; on the mesh name and
#                  on the svc.cluster.local name (in the URL and in Host)
#   P4  proxied    a service's HTTP filters and route rules are applied on
#                  every path to the service: while the HTTPRoute of A4
#                  exists, http://<mesh name of mp-multi>:9090/ ->
#                  multi9090+a4 and :8080/ -> multi8080+a4. It runs inside
#                  A4's window (one apply, one delete) and is reported under
#                  its own ID; run without A4 it opens the window itself
#   P5  proxied    documentation, never a failure: https://<mesh name of
#                  mp-multi>:8080/, which is a CONNECT. One line per client
#                  starting with "P5-OBSERVED": the status of the CONNECT
#                  response if there was one, curl's exit code and the time.
#                  INFO in the table
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
# VERSION SKEW. The components are installed from two groups of images, each
# with its own tag (default "latest"): MP_TAG_AGENT for agent, mesh-dns,
# proxy-supervisor, cni-install and uds-csi; MP_TAG_REGISTRAR for registrar
# and controller. `up` loads exactly those tags, and builds only the images
# that are used at "latest" (nothing under MP_SKIP_BUILD=1); any other tag
# must already be in the local Docker daemon. `verify` reads the tags back
# from the installed workloads and prints them first, as two lines starting
# with "MP-CELL", so a log names the cell it measured. e2e/README.md has the
# whole procedure. CI runs no skew cell.
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
# The short-name legs (N1-N3): a second namespace with a service of the SAME
# name as one in TEST_NS, and a third with no service of that name.
TEST_NS2="aether-test-2"
TEST_NS3="aether-test-3"
SHORT_NAME="mp-short"
SHORT_MARKER_A="shorta"
SHORT_MARKER_B="shortb"
# A Service of the same name in the third namespace that is NOT a mesh service.
SHORT_MARKER_PLAIN="shortplain"
# Leg T1: a service whose declared port moves from HTTP 8080 to raw TCP 9000.
# T1_WINDOW is how long after the rollout the old port may still be routed. It
# must stay well under the agent's 90 s retention grace: a request that is
# still routed past it is the grace applied to a service the registry says has
# changed shape.
MOVE_NAME="mp-move"
# Leg U1: a mesh service whose name a Service of the user's owns.
OWN_NAME="mp-own"
OWN_MARKER="own8080"
OWN_SERVICE_PORT=8000
# Leg U2: a Service of the user's with a named targetPort, created before its
# pods.
LATE_NAME="mp-late"
LATE_MARKER="late8080"
LATE_SERVICE_PORT=8000
LATE_ROUTE="mp-late-route"
LATE_SLICED="mp-late-sliced"
# How long a slice change may take to show in a route's status (leg U2 says
# why it is this short).
U2_EVENT_WINDOW=8
# Two more Services of the user's that select mp-own's pods: on a port number
# another mesh service declares as HTTP, and on the port the pods serve.
OWN_ALT_SERVICE="mp-own-alt"
OWN_POD_SERVICE="mp-own-pod"
OWN_FILTER="mp-own-deny"
MOVE_MARKER_HTTP="move8080"
MOVE_MARKER_TCP="movetcp"
T1_WINDOW=60
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
# The loopback outbound listener, as a client-nocapture pod dials it, and the
# same listener named as an HTTP proxy (curl --proxy).
LOOPBACK_URL="http://127.0.0.1:$MESH_HTTP_PORT/"
PROXY_URL="http://127.0.0.1:$MESH_HTTP_PORT"
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
# The image tag of each component group (see "VERSION SKEW" in the header).
# CI sets neither.
MP_TAG_AGENT="${MP_TAG_AGENT:-latest}"
MP_TAG_REGISTRAR="${MP_TAG_REGISTRAR:-latest}"
AGENT_GROUP=(agent mesh-dns proxy-supervisor cni-install uds-csi)
REGISTRAR_GROUP=(registrar controller)
# Red-run aids (see "A RED RUN" in the header). CI sets none of them.
ALL_LEGS=(M0 M1 M2 M3 M4 M5 M6 M7 X1 X2 X3 X4 X5 X6 A1 A2 A3 A4 A5 A6 A7 A8 A9 A10 A11 A12
	W1 W2 W3 P1 P2 P3 P4 P5 R1 R2 R3 S1 N1 N2 N3 V1 U1 U2 T1)
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
# The routes of the R legs, and aether's controller name in a route's status.
R_BACKEND_ROUTE="mp-r-backend"
R_PARENT_ROUTE="mp-r-parent"
R_WEIGHTED_ROUTE="mp-r-weighted"
MESH_CONTROLLER="gateway.aether.io/mesh"
A4_HEADER_VALUE="a4"

# WORKLOADS are the six that serve HTTP on at least one declared port: the
# addressing legs iterate them. TCP_ONLY has no HTTP port (the W legs).
WORKLOADS=(mp-httponly mp-multi mp-eighty mp-mixed mp-tcpfirst mp-nine)
TCP_ONLY="mp-tcponly"
CLIENTS=(client client-nocapture)
# What the undeclared listener of TCP_ONLY answers.
STRAY_MARKER="onlytcp$UNDECLARED_PORT"

# wl_tag WORKLOAD: the prefix of its markers.
wl_tag() {
	case "$1" in
	mp-httponly) echo ctl ;;
	mp-multi) echo multi ;;
	mp-eighty) echo eighty ;;
	mp-mixed) echo mix ;;
	mp-tcpfirst) echo rev ;;
	mp-nine) echo nine ;;
	mp-tcponly) echo only ;;
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
	mp-tcponly) ;;
	*) echo "unknown workload '$1'" >&2 && return 1 ;;
	esac
}

# wl_legacy_port WORKLOAD: its legacy target, the port its pods declare in
# endpoint.aether.io/port (proposal 043, "The legacy spellings during Phases 1
# and 2"). HTTP for every workload but mp-tcpfirst.
wl_legacy_port() {
	case "$1" in
	mp-tcpfirst | mp-nine | mp-tcponly) echo "$TCP_PORT" ;;
	*) echo "$HTTP_PORT" ;;
	esac
}

# image_tag IMAGE: the tag IMAGE is loaded and installed at.
image_tag() {
	local i
	for i in "${AGENT_GROUP[@]}"; do
		if [ "$i" = "$1" ]; then echo "$MP_TAG_AGENT" && return 0; fi
	done
	for i in "${REGISTRAR_GROUP[@]}"; do
		if [ "$i" = "$1" ]; then echo "$MP_TAG_REGISTRAR" && return 0; fi
	done
	echo latest
}

# marker WORKLOAD PORT: what the HTTP listener of WORKLOAD on PORT answers.
# tcp_marker WORKLOAD: what its l4echo answers.
marker() { printf '%s%s' "$(wl_tag "$1")" "$2"; }
tcp_marker() { printf '%stcp' "$(wl_tag "$1")"; }

# marker_owner TOKEN: the workload and listener a marker of this suite belongs
# to (every marker there is), or nothing when TOKEN is not one.
marker_owner() {
	local w p
	for w in "${WORKLOADS[@]}"; do
		for p in $(wl_http_ports "$w"); do
			if [ "$1" = "$(marker "$w" "$p")" ]; then echo "$w, HTTP port $p" && return 0; fi
		done
	done
	for w in mp-httponly mp-mixed mp-tcpfirst "$TCP_ONLY"; do
		if [ "$1" = "$(tcp_marker "$w")" ]; then echo "$w, raw-TCP port $TCP_PORT" && return 0; fi
	done
	if [ "$1" = "$STRAY_MARKER" ]; then echo "$TCP_ONLY, its undeclared listener on $UNDECLARED_PORT"; fi
}

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
	printf '\033[1;33m  -- mesh services (the two annotations must be <none>) --\033[0m\n' >&2
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
	printf '\033[1;33m  -- envoy: capture HTTP chains and the route tables they name (cap_http, cap_http_18081) --\033[0m\n' >&2
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
	log "building + loading the aether images used at 'latest' (incl. the l4echo test backend)"
	# Only what is used at "latest" is built: an image of a group that runs
	# another tag (a skew cell) was built from another commit and tagged by
	# hand.
	local t
	for t in //agent/cmd/agent //agent/cmd/mesh-dns //agent/cmd/proxy-supervisor \
		//cni/cmd/cni-install //registrar/cmd/registrar //controller/cmd/controller //agent/cmd/uds-csi \
		//e2e/l4echo; do
		if [ "$(image_tag "${t##*/}")" != "latest" ]; then
			note "not building ${t##*/}: it runs at tag '$(image_tag "${t##*/}")'"
			continue
		fi
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
	log "loading images into '$CLUSTER' (agent group at '$MP_TAG_AGENT', registrar group at '$MP_TAG_REGISTRAR')"
	# Exactly the references in use, each once.
	local img ref loaded=" "
	for img in "${IMAGES[@]}"; do
		ref="${IMAGE_REGISTRY}/${img}:$(image_tag "$img")"
		case "$loaded" in *" $ref "*) continue ;; esac
		kind load docker-image "$ref" --name "$CLUSTER" >/dev/null 2>&1 ||
			die "could not load $ref into kind (was it built, or tagged? see VERSION SKEW in the header)"
		loaded+="$ref "
	done
	ok "images loaded:$loaded"
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
	img() { echo "--set $1.image.repository=${IMAGE_REGISTRY}/$2 --set $1.image.tag=$(image_tag "$2") --set $1.image.digest= --set $1.image.pullPolicy=Never"; }
	log "installing the aether CRDs"
	helm --kube-context "$CTX" upgrade --install aether-crds "$charts/crds" \
		-n "$NS" --create-namespace --wait --timeout 2m >/dev/null || die "crds chart install failed"

	# Everything except SPIRE and the registry backend is chart default:
	# transparent capture with redirect-all (which is what captures a dial to
	# any port of a mesh address), mesh DNS, GAMMA. Per-port protocols have no
	# values gate.
	log "installing aether (SPIRE ON, $MP_REGISTRY_BACKEND registry — see the header; agent group at '$MP_TAG_AGENT', registrar group at '$MP_TAG_REGISTRAR')"
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

# container_yaml WORKLOAD KIND PORT [TEXT]: one container of a workload, as a
# YAML list item at the indentation of spec.template.spec.containers. KIND
# "http" is hashicorp/http-echo answering marker(WORKLOAD, PORT); KIND "tcp" is
# e2e/l4echo (reads one line, writes "<text> <line>", closes) answering TEXT,
# or tcp_marker(WORKLOAD) without one. One container per port.
container_yaml() {
	local name="$1" kind="$2" port="$3" text="${4:-}"
	case "$kind" in
	http)
		cat <<YAML
        - name: http-$port
          image: $HTTP_ECHO_IMAGE
          args: ["-text=${text:-$(marker "$name" "$port")}", "-listen=:$port"]
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
          args: ["--mode=tcp", "--listen=:$port", "--text=${text:-$(tcp_marker "$name")}"]
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
# "tcp:<port>", or "tcp:<port>:<text>" for an l4echo that answers <text>; one
# container each (container_yaml). A listener is on the mesh only if PORTS
# declares it.
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
	local name="$1" primary="$2" protocol="$3" ports="$4" spec kind port text containers="" sysctls=""
	shift 4
	for spec in "$@"; do
		IFS=: read -r kind port text <<<"$spec"
		if [ "$port" -lt 1024 ]; then
			sysctls='sysctls: [{name: net.ipv4.ip_unprivileged_port_start, value: "0"}]'
		fi
		containers+="$(container_yaml "$name" "$kind" "$port" "$text")"$'\n'
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
	log "deploying ${WORKLOADS[*]} $TCP_ONLY (mp-mixed ports: $MP_MIXED_PORTS) and ${CLIENTS[*]}"
	kc create ns "$TEST_NS" >/dev/null 2>&1 || true
	deploy_workload mp-httponly "$HTTP_PORT" http "$HTTP_PORT" "http:$HTTP_PORT" "tcp:$TCP_PORT"
	deploy_workload mp-multi "$HTTP_PORT" http "$HTTP_PORT,$ALT_PORT" "http:$HTTP_PORT" "http:$ALT_PORT"
	deploy_workload mp-eighty "$HTTP_PORT" http "$LOW_PORT,$HTTP_PORT,$ALT_PORT" "http:$LOW_PORT" "http:$HTTP_PORT" "http:$ALT_PORT"
	deploy_workload mp-mixed "$HTTP_PORT" http "$MP_MIXED_PORTS" "http:$HTTP_PORT" "tcp:$TCP_PORT"
	deploy_workload mp-tcpfirst "$TCP_PORT" tcp "$TCP_PORT,$HTTP_PORT=h1" "tcp:$TCP_PORT" "http:$HTTP_PORT"
	deploy_workload mp-nine "$TCP_PORT" http "$TCP_PORT" "http:$TCP_PORT"
	deploy_workload "$TCP_ONLY" "$TCP_PORT" tcp "$TCP_PORT" "tcp:$TCP_PORT" "tcp:$UNDECLARED_PORT:$STRAY_MARKER"

	# The upstreams annotation is NOT an optimisation: there is no on-demand
	# discovery for tcp_proxy. An undeclared TCP upstream yields a chain that
	# matches and a cluster that is missing, which looks exactly like a routing
	# bug. Both clients name every workload.
	local w upstreams=""
	for w in "${WORKLOADS[@]}" "$TCP_ONLY"; do
		upstreams+="${upstreams:+,}$w.$TEST_NS"
	done
	upstreams+=",$SHORT_NAME.$TEST_NS,$MOVE_NAME.$TEST_NS,$OWN_NAME.$TEST_NS,$LATE_NAME.$TEST_NS"
	# The user's Service first: the registrar generates a Service only for a
	# name no other Service owns.
	deploy_user_service "$OWN_NAME" "$OWN_SERVICE_PORT" "$HTTP_PORT"
	deploy_user_service "$OWN_ALT_SERVICE" "$ALT_PORT" "$HTTP_PORT" "$OWN_NAME"
	deploy_user_service "$OWN_POD_SERVICE" "$HTTP_PORT" "$HTTP_PORT" "$OWN_NAME"
	deploy_workload "$OWN_NAME" "$HTTP_PORT" http "$HTTP_PORT" "http:$HTTP_PORT:$OWN_MARKER"
	deploy_workload "$MOVE_NAME" "$HTTP_PORT" http "$HTTP_PORT" "http:$HTTP_PORT:$MOVE_MARKER_HTTP" "tcp:$TCP_PORT:$MOVE_MARKER_TCP"
	deploy_client client "$upstreams"
	deploy_client client-nocapture "$upstreams" false

	# The short-name legs: one name, two namespaces, and a namespace without it.
	# The functions above deploy into TEST_NS, so it is set per call.
	kc create ns "$TEST_NS2" >/dev/null 2>&1 || true
	kc create ns "$TEST_NS3" >/dev/null 2>&1 || true
	deploy_workload "$SHORT_NAME" "$HTTP_PORT" http "$HTTP_PORT" "http:$HTTP_PORT:$SHORT_MARKER_A"
	TEST_NS="$TEST_NS2" deploy_workload "$SHORT_NAME" "$HTTP_PORT" http "$HTTP_PORT" "http:$HTTP_PORT:$SHORT_MARKER_B"
	local short_upstreams="$SHORT_NAME.$TEST_NS2,$SHORT_NAME.$TEST_NS"
	TEST_NS="$TEST_NS2" deploy_client client "$short_upstreams"
	TEST_NS="$TEST_NS3" deploy_client client "$short_upstreams"
	deploy_plain_service "$TEST_NS3" "$SHORT_NAME" "$HTTP_PORT" "$SHORT_MARKER_PLAIN"

	local d
	for d in "${WORKLOADS[@]}" "$TCP_ONLY" "${CLIENTS[@]}" "$SHORT_NAME" "$MOVE_NAME" "$OWN_NAME"; do
		kc -n "$TEST_NS" rollout status "deploy/$d" --timeout=180s >/dev/null ||
			die "workload '$d' never became Ready"
	done
	for d in "$SHORT_NAME" client; do
		kc -n "$TEST_NS2" rollout status "deploy/$d" --timeout=180s >/dev/null || die "workload '$d' in $TEST_NS2 never became Ready"
	done
	kc -n "$TEST_NS3" rollout status deploy/client --timeout=180s >/dev/null || die "the client in $TEST_NS3 never became Ready"
	kc -n "$TEST_NS3" rollout status "deploy/$SHORT_NAME-plain" --timeout=180s >/dev/null || die "the plain $SHORT_NAME in $TEST_NS3 never became Ready"
	ok "workloads deployed"
}

# deploy_user_service NAME PORT TARGET [APP] — a Service of the user's, NAME,
# in TEST_NS, whose PORT targets pod port TARGET of the pods labelled app=APP
# (default NAME: the pods deploy_workload NAME makes, which are mesh pods).
deploy_user_service() {
	kc apply -f - >/dev/null <<YAML || die "user Service $1 apply failed"
apiVersion: v1
kind: Service
metadata: {name: $1, namespace: $TEST_NS}
spec:
  selector: {app: ${4:-$1}}
  ports: [{name: http, port: $2, targetPort: $3}]
YAML
}

# deploy_plain_service NS NAME PORT TEXT — a Service NAME in NS that is NOT a
# mesh service: its pod is not mesh-managed (no aether.io/managed label, no
# annotation), and the Service is an ordinary one with a selector. It answers
# TEXT on PORT.
deploy_plain_service() {
	local ns="$1" name="$2" port="$3" text="$4"
	kc apply -f - >/dev/null <<YAML || die "plain service $name apply failed"
apiVersion: apps/v1
kind: Deployment
metadata: {name: $name-plain, namespace: $ns}
spec:
  replicas: 1
  selector: {matchLabels: {app: $name-plain}}
  template:
    metadata:
      labels: {app: $name-plain}
    spec:
      containers:
$(container_yaml "$name" http "$port" "$text")
---
apiVersion: v1
kind: Service
metadata: {name: $name, namespace: $ns}
spec:
  selector: {app: $name-plain}
  ports: [{name: http, port: $port, targetPort: $port}]
YAML
}

# --- probes -------------------------------------------------------------------

# The body of an HTTP probe batch, run by the CLIENT POD's sh (busybox ash: no
# bash, no pipefail). Arguments: URL, HOST (empty = the Host curl writes
# itself, which drops a default port), N, PROXY (empty = none; otherwise the
# request goes through curl --proxy PROXY, which connects to PROXY and sends
# the absolute URL in the request line). With a PROXY and a HOST the request
# line is forced to URL as it is written (--request-target): curl drops a
# default port there too, and that is the only way to put ":80" in both.
# Prints one line per request:
#
#   <status> <first token of the body> <x-aether-error> <x-mp-route> <total s> <dns s>
#
# with "-" for what is absent and status 000 when there was no HTTP response.
HTTP_POD_SCRIPT="$(
	cat <<'SH'
url="$1"; host="$2"; n="$3"; proxy="${4:-}"; i=1
set -- -s --max-time 5 -D - -w "\nMPSTAT %{http_code} %{time_total} %{time_namelookup}\n"
if [ -n "$host" ]; then set -- "$@" -H "Host: $host"; fi
case "$proxy" in
env:*)
	# The proxy from the environment, as a workload has it: curl reads the
	# lower-case http_proxy for an http:// URL (most other clients read
	# HTTP_PROXY as well).
	http_proxy="${proxy#env:}"
	export http_proxy
	;;
"") ;;
*)
	set -- "$@" --proxy "$proxy"
	if [ -n "$host" ]; then set -- "$@" --request-target "$url"; fi
	;;
esac
while [ "$i" -le "$n" ]; do
	curl "$@" "$url" 2>/dev/null | tr -d "\r" | awk '
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
#
# proxy_tokens CLIENT URL HOST N — the same through curl --proxy PROXY_URL. The
# response is the proxy's own answer to a plain-HTTP request (no CONNECT), so
# the tokens read the same.
http_tokens() { tokens_via "" "$@"; }
proxy_tokens() { tokens_via "$PROXY_URL" "$@"; }

tokens_via() {
	local proxy="$1" client="$2" url="$3" host="$4" n="$5"
	kc -n "$TEST_NS" exec "deploy/$client" -c curl -- sh -c "$HTTP_POD_SCRIPT" sh "$url" "$host" "$n" "$proxy" 2>/dev/null |
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

# show_exchange CLIENT URL HOST [PROXY] — one request, with its response
# headers, body and timings, on stderr: what a failed HTTP assertion looked
# like on the wire. PROXY as in HTTP_POD_SCRIPT.
show_exchange() {
	local client="$1" url="$2" host="$3" proxy="${4:-}"
	printf '\033[1;33m  -- one request from %s: GET %s%s%s --\033[0m\n' "$client" "$url" "${host:+  (Host: $host)}" "${proxy:+  (--proxy $proxy)}" >&2
	# SC2016: the single quotes are the point — the body is evaluated by the
	# POD's shell; $1, $2 and $3 are its arguments.
	# shellcheck disable=SC2016
	kc -n "$TEST_NS" exec "deploy/$client" -c curl -- sh -c '
		url="$1"; host="$2"; proxy="$3"
		set -- -s -i --max-time 5 -w "\n-- status %{http_code}, total %{time_total}s, dns %{time_namelookup}s, connect %{time_connect}s, to %{remote_ip}:%{remote_port}\n"
		if [ -n "$host" ]; then set -- "$@" -H "Host: $host"; fi
		if [ -n "$proxy" ]; then
			set -- "$@" --proxy "$proxy"
			if [ -n "$host" ]; then set -- "$@" --request-target "$url"; fi
		fi
		curl "$@" "$url" 2>&1
		echo "-- curl exit $?"
	' sh "$url" "$host" "$proxy" 2>&1 | sed -n '1,40s/^/    /p' >&2 || true
}

# show_failed FN ARG... — the exchange behind a failed `expect`, when FN is an
# HTTP probe.
show_failed() {
	case "$1" in
	http_tokens) show_exchange "$2" "$3" "$4" ;;
	proxy_tokens) show_exchange "$2" "$3" "$4" "$PROXY_URL" ;;
	esac
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
LEG_INFO=""
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

# info TEXT — the leg asserts nothing: it is reported as INFO with TEXT.
info() { LEG_INFO="$*"; }

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
			show_failed "$@"
			fail "$what never answered '$want' in ${timeout}s, last batch: $(histogram "$replies")" "$meaning"
			return 0
		fi
		sleep 3
	done
	replies="$("$@" "$n")"
	if [ "$(count_token "$replies" "$want")" -ne "$n" ]; then
		show_failed "$@"
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

# proxied WANT CLIENT AUTHORITY MEANING [EXPLICIT] — from deploy/CLIENT,
# literally `curl --proxy http://127.0.0.1:18081 http://AUTHORITY/`: the client
# connects to the loopback listener and sends the absolute URL and
# Host: AUTHORITY. With EXPLICIT the request line and Host are forced to spell
# AUTHORITY as written (see HTTP_POD_SCRIPT), for a default port.
proxied() {
	local want="$1" client="$2" authority="$3" meaning="$4" host=""
	if [ -n "${5:-}" ]; then host="$authority"; fi
	expect "$client: curl --proxy $PROXY_URL http://$authority/${host:+ (request line and Host forced to $host)}" \
		"$want" "$A_TIMEOUT" "$A_PROBES" "$meaning" proxy_tokens "$client" "http://$authority/" "$host"
}

# expect_no_application HOST:PORT — three raw-TCP probes from the captured
# client, none of which may read anything back, like expect_unanswered; a
# reply is named for what it is. Every marker of the suite is known to
# marker_owner: the service's own declared port answering for another one, its
# undeclared listener, and a marker of ANOTHER service are three different
# defects.
expect_no_application() {
	local target="$1" replies tok owner seen=""
	replies="$(tcp_tokens "$target" line 3)"
	if [ "$(count_token "$replies" NOREPLY)" -eq 3 ]; then
		passed "raw TCP to $target -> no application answers (3/3)"
		return 0
	fi
	for tok in $(printf '%s\n' "$replies" | sort -u); do
		[ "$tok" != "NOREPLY" ] || continue
		owner="$(marker_owner "$tok")"
		if [ "$tok" = "$(tcp_marker "$TCP_ONLY")" ]; then
			seen+="; '$tok' is the service's own DECLARED port $TCP_PORT answering for a port that is not it (the any-port chain)"
		elif [ "$tok" = "$STRAY_MARKER" ]; then
			seen+="; '$tok' is the pod's undeclared listener: a port nobody declared was forwarded to the pod"
		elif [ -n "$owner" ]; then
			seen+="; '$tok' is a marker of ANOTHER service ($owner)"
		else
			seen+="; '$tok' is no marker of this suite, and still an answer"
		fi
	done
	fail "raw TCP to $target was answered ($(histogram "$replies"))${seen:-; fewer than 3 probes ran}"
}

# curl_exit_meaning CODE — the curl exit codes a probe that is not answered
# ends with.
curl_exit_meaning() {
	case "$1" in
	0) echo "the peer closed the connection" ;;
	7) echo "could not connect: refused or unreachable" ;;
	28) echo "timed out at --max-time 5" ;;
	52) echo "the peer closed without a reply" ;;
	56) echo "receive failure: reset" ;;
	*) echo "see curl's exit codes" ;;
	esac
}

# observe_tcp HOST:PORT — one raw-TCP probe from the captured client, reported
# and not asserted: curl's exit code, the total time, and the time to connect
# (0.000000 when it never connected). The line starts with W2-OBSERVED.
observe_tcp() {
	local target="$1" rc="" total="" connect=""
	# SC2016: evaluated by the POD's shell. Without pipefail, $? after the
	# substitution is curl's.
	# shellcheck disable=SC2016
	read -r rc total connect < <(kc -n "$TEST_NS" exec deploy/client -c curl -- sh -c '
		out=$(printf "probe-observe\n" | curl -s -o /dev/null -w "%{time_total} %{time_connect}" --max-time 5 "telnet://$1" 2>/dev/null)
		echo "$? ${out:-- -}"
	' sh "$target" 2>/dev/null || true) || true
	if [ -z "$rc" ]; then
		printf 'W2-OBSERVED raw TCP to %s: no observation (the probe did not run)\n' "$target"
		return 0
	fi
	printf 'W2-OBSERVED raw TCP to %s: curl exit %s (%s), total %ss, connected after %ss\n' \
		"$target" "$rc" "$(curl_exit_meaning "$rc")" "$total" "$connect"
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
	expect "captured http://$name:$HTTP_PORT/" "$(marker mp-httponly "$HTTP_PORT")" 240 "$PROBES" \
		"the control workload does not serve HTTP through the mesh, so nothing below could mean anything" \
		http_tokens client "http://$name:$HTTP_PORT/" ""
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
	log "M1 the proxy port is not a port of the service: http://$name:$MESH_HTTP_PORT/ (Host says :$MESH_HTTP_PORT) is refused by name"
	expect "captured http://$name:$MESH_HTTP_PORT/" "HTTP421:port-not-declared" 240 "$PROBES" \
		"'$(marker mp-mixed "$HTTP_PORT")' is :$MESH_HTTP_PORT read as 'the port the pods declare', which it meant until chart 3.0.0" \
		http_tokens client "http://$name:$MESH_HTTP_PORT/" ""
}

leg_M2() {
	local name
	name="$(mesh_name mp-mixed)"
	log "M2 http, portless: http://$name/ dials port 80, which mp-mixed does not declare: refused by name"
	expect "captured http://$name/" "HTTP421:port-not-declared" 90 "$PROBES" \
		"'$(marker mp-mixed "$HTTP_PORT")' is a port-less URL read as 'the port the pods declare', which it meant until chart 3.0.0" \
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

# M7 — the generated mesh Service is what the service serves: its REAL ports,
# each with its class, and the mesh's two ports (the proxy port and the raw-TCP
# one). Nothing else: no "http" 80 or "https" 443 placeholder (a port 80 the
# pods do not declare is not a port of the service), and neither of the two
# annotations a registrar wrote through chart 2.6 for its agents
# (aether.io/app-protocol, aether.io/port).
leg_M7() {
	log "M7 the generated mesh Service of mp-mixed: its real ports and the two mesh ports, no placeholder, no service-level protocol"
	local ports p key value
	ports="$(svc_ports mp-mixed)"
	expect_svc_port mp-mixed "$ports" "$HTTP_PORT" "p$HTTP_PORT-tcp" http
	expect_svc_port mp-mixed "$ports" "$TCP_PORT" "p$TCP_PORT-tcp" tcp
	for p in "$MESH_HTTP_PORT" "$MESH_TCP_PORT"; do
		expect_svc_port mp-mixed "$ports" "$p" "" ""
	done
	for p in 80 443; do
		if printf '%s\n' "$ports" | awk -F/ -v p="$p" '$1 == p { found = 1 } END { exit !found }'; then
			fail "the generated Service mp-mixed has a port $p, which its pods do not declare (ports: $(printf '%s' "$ports" | tr '\n' ' '))" \
				"the placeholder ports were removed in chart 3.0.0: a generated Service has the ports its pods declare"
		else
			passed "Service mp-mixed: no port $p"
		fi
	done
	# mp-eighty DECLARES 80: there it is a real port, named and typed.
	expect_svc_port mp-eighty "$(svc_ports mp-eighty)" "$LOW_PORT" "p$LOW_PORT-tcp" http
	for key in app-protocol port; do
		value="$(kc -n "$TEST_NS" get svc mp-mixed -o jsonpath="{.metadata.annotations.aether\\.io/$key}" 2>/dev/null || true)"
		if [ -n "$value" ]; then
			fail "the mesh Service mp-mixed carries aether.io/$key='$value'" "nothing reads a service-level protocol or port any more; the registrar removes the annotation from a Service that has it"
		else
			passed "Service mp-mixed: no aether.io/$key"
		fi
	done
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
	log "X4 (= A12 for mp-tcpfirst) a service whose declared port is raw TCP: :$MESH_HTTP_PORT and the port-less name are refused by name, like for any service that does not declare 80"
	shared tcpfirst_refused check_tcpfirst_refused
}

check_tcpfirst_refused() {
	local name meaning
	name="$(mesh_name mp-tcpfirst)"
	meaning="HTTP502 is the request sent to the raw-TCP port; '$(marker mp-tcpfirst "$HTTP_PORT")' means a spelling without a declared port reaches the HTTP port; ':slow' is a refusal that was not immediate"
	captured "HTTP421:port-not-declared" "$name:$MESH_HTTP_PORT" "" "$meaning"
	captured "HTTP421:port-not-declared" "$name" "" "$meaning"
	captured "HTTP421:port-not-declared" "$(k8s_fqdn mp-tcpfirst)" "" "$meaning"
	loopback "HTTP421:port-required" "$name" "$meaning"
	loopback "HTTP421:port-not-declared" "$name:$MESH_HTTP_PORT" "$meaning"
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
	# P4 is asserted while the route exists: one apply and one delete for both.
	if leg_selected P4; then p4_in_window check_p4_routed; fi
	# Remove it, and wait until it is gone from the data plane: every later leg
	# compares whole tokens, and a leftover header would fail them all.
	remove_a4_route
	for p in $(wl_http_ports mp-multi); do
		base="$(marker mp-multi "$p")"
		captured "$base" "$name:$p" "" "the HTTPRoute was deleted and its header is still added"
		loopback "$base" "$name:$p" "the HTTPRoute was deleted and its header is still added"
	done
	if leg_selected P4; then p4_in_window check_p4_gone; fi
}

# P4's assertions, made inside A4's window, are kept here until leg P4 reports
# them under its own ID.
P4_RAN=0
P4_CHECKS=0
P4_FAILS=()

# p4_in_window FN — run FN as leg P4 while another leg is running, and put
# that leg's counters back afterwards.
p4_in_window() {
	local leg="$CURRENT_LEG" checks="$LEG_CHECKS"
	local -a fails=(${LEG_FAILS[@]+"${LEG_FAILS[@]}"})
	CURRENT_LEG="P4"
	LEG_CHECKS=0
	LEG_FAILS=()
	"$1"
	P4_RAN=1
	P4_CHECKS=$((P4_CHECKS + LEG_CHECKS))
	P4_FAILS+=(${LEG_FAILS[@]+"${LEG_FAILS[@]}"})
	CURRENT_LEG="$leg"
	LEG_CHECKS="$checks"
	LEG_FAILS=(${fails[@]+"${fails[@]}"})
}

# check_p4_routed — with the HTTPRoute of A4 in place, a request through the
# proxy port carries the route's header and is answered by the port its Host
# names.
check_p4_routed() {
	note "P4 (inside A4's window): the HTTPRoute applies through the proxy port, curl --proxy $PROXY_URL"
	local name client p base
	name="$(mesh_name mp-multi)"
	for client in "${CLIENTS[@]}"; do
		for p in $(wl_http_ports mp-multi); do
			base="$(marker mp-multi "$p")"
			proxied "$base+$A4_HEADER_VALUE" "$client" "$name:$p" \
				"'$base' alone is the port answering WITHOUT the route's header: a request through the proxy port is not served by the virtual host the service's route rules are on; another marker is another port answering"
		done
	done
}

check_p4_gone() {
	local name client p
	name="$(mesh_name mp-multi)"
	for client in "${CLIENTS[@]}"; do
		for p in $(wl_http_ports mp-multi); do
			proxied "$(marker mp-multi "$p")" "$client" "$name:$p" "the HTTPRoute was deleted and its header is still added through the proxy port"
		done
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

# A12 — no spelling means "the port the pods declare". Until chart 3.0.0 three
# did: <name>:18081, the port-less URL (a dial to 80), and a port-less or
# :18081 Host on the loopback listener.
#
#   - 18081 is the proxy's port, not a port of a service: Host "<name>:18081"
#     is a port the service does not declare, on both paths.
#   - A port-less name is port 80 and nothing else. mp-eighty declares 80 and
#     answers from it; every other workload refuses: by the dialled port when
#     captured (port-not-declared), and on the loopback listener, where nothing
#     was dialled, by asking for a port (port-required).
leg_A12() {
	log "A12 :$MESH_HTTP_PORT and the port-less name never mean 'the port the pods declare': port 80 answers when declared, everything else is refused by name"
	local w name meaning portless80 portlessLoop
	for w in "${WORKLOADS[@]}"; do
		[ "$w" != "mp-tcpfirst" ] || continue
		name="$(mesh_name "$w")"
		meaning="'$(marker "$w" "$(wl_legacy_port "$w")")' is the spelling read as the port $w's pods declare (:$(wl_legacy_port "$w")), which it meant until chart 3.0.0"
		portless80="HTTP421:port-not-declared"
		portlessLoop="HTTP421:port-required"
		if [ "$w" = "mp-eighty" ]; then
			portless80="$(marker "$w" "$LOW_PORT")"
			portlessLoop="$portless80"
		fi
		captured "HTTP421:port-not-declared" "$name:$MESH_HTTP_PORT" "" "$meaning"
		captured "$portless80" "$name" "" "$meaning"
		captured "$portless80" "$(k8s_fqdn "$w")" "" "$meaning"
		loopback "$portlessLoop" "$name" "$meaning"
		loopback "HTTP421:port-not-declared" "$name:$MESH_HTTP_PORT" "$meaning"
	done
	note "mp-tcpfirst, whose declared port is raw TCP: the same refusals (X4), and :$MESH_TCP_PORT reaches it (X1)"
	shared tcpfirst_refused check_tcpfirst_refused
	shared tcpfirst_meshtcp check_tcpfirst_meshtcp
}

# --- a route's ports are real ports ---------------------------------------------
#
# A parentRef and a backendRef name a REAL port of a Service. One that does not
# is not given another meaning (until chart 3.0.0, 18081 and an undeclared 80
# meant "the port the pods declare", and any other wrong port fell back to the
# backend's declared port): the data plane refuses, and the route's status says
# why, under aether's own controller name.

# route_condition ROUTE TYPE — "<status>/<reason>" of aether's condition TYPE
# on the route's first parent status that aether owns, "" when there is none
# OR when the condition was written for an earlier generation of the route.
#
# The generation matters: R1 and R2 re-apply one route name with another port.
# A condition read without it is the one the PREVIOUS port left, which is the
# very answer the next port is expected to give, so the leg passed on the first
# poll whatever the agent made of the new spec.
route_condition() {
	local out gen observed rest
	out="$(kc -n "$TEST_NS" get httproutes.gateway.networking.k8s.io "$1" \
		-o jsonpath="{.metadata.generation} {range .status.parents[?(@.controllerName==\"$MESH_CONTROLLER\")]}{range .conditions[?(@.type==\"$2\")]}{.observedGeneration} {.status}/{.reason}{end}{end}" 2>/dev/null || true)"
	read -r gen observed rest <<<"$out" || true
	if [ -n "$gen" ] && [ "$gen" = "$observed" ]; then
		printf '%s' "$rest"
	fi
}

# expect_condition_within SECONDS ROUTE TYPE WANT MEANING — the condition must
# be WANT within SECONDS, polled twice a second. For a change the agent is
# told of by an event, where the long wait of expect_condition would also pass
# an agent that only found out at some later re-read.
expect_condition_within() {
	local within="$1" route="$2" type="$3" want="$4" meaning="$5" got="" start
	start="$(date +%s.%N)"
	while :; do
		got="$(route_condition "$route" "$type")"
		[ "$got" = "$want" ] && break
		awk -v s="$start" -v n="$(date +%s.%N)" -v w="$within" 'BEGIN { exit !(n - s >= w) }' && break
		sleep 0.5
	done
	if [ "$got" = "$want" ]; then
		passed "HTTPRoute $route: $type is $want within ${within}s"
	else
		fail "HTTPRoute $route: $type is '${got:-absent}' after ${within}s, want $want" "$meaning"
	fi
}

# expect_condition ROUTE TYPE WANT MEANING — poll until the condition is WANT.
expect_condition() {
	local route="$1" type="$2" want="$3" meaning="$4" got="" deadline
	deadline=$(($(date +%s) + ${MP_TIMEOUT:-90}))
	while :; do
		got="$(route_condition "$route" "$type")"
		[ "$got" = "$want" ] && break
		[ "$(date +%s)" -ge "$deadline" ] && break
		sleep 2
	done
	if [ "$got" = "$want" ]; then
		passed "HTTPRoute $route: $type is $want"
	else
		fail "HTTPRoute $route: $type is '${got:-absent}', want $want" "$meaning"
	fi
}

remove_route() {
	kc -n "$TEST_NS" delete httproutes.gateway.networking.k8s.io "$1" --ignore-not-found --wait=true >/dev/null 2>&1 || true
}

# apply_backend_route NAME PARENT PORT... — an HTTPRoute on PARENT whose rule
# sends everything to mp-multi on each PORT given as "<port>[=<weight>]".
apply_backend_route() {
	local name="$1" parent="$2" refs="" spec port weight
	shift 2
	for spec in "$@"; do
		port="${spec%%=*}"
		weight=1
		case "$spec" in *=*) weight="${spec#*=}" ;; esac
		refs+="        - {group: \"\", kind: Service, name: mp-multi, port: $port, weight: $weight}"$'\n'
	done
	kc apply -f - >/dev/null <<YAML
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: $name, namespace: $TEST_NS}
spec:
  parentRefs:
    - {group: "", kind: Service, name: $parent}
  rules:
    - backendRefs:
$refs
YAML
}

# R1 — a backendRef to a port the backend does not have: ResolvedRefs=False
# with BackendNotFound, and every request the rule takes is answered 500 on
# both paths. Three wrong ports: one nothing declares, the mesh HTTP port, and
# port 80 of a service that does not declare it. Then the route is removed and
# the parent answers for itself again.
leg_R1() {
	log "R1 an HTTPRoute whose backendRef names a port mp-multi does not have: status says so, and the request is answered 500"
	local name port
	name="$(mesh_name mp-httponly)"
	for port in "$UNDECLARED_PORT" "$MESH_HTTP_PORT" "$LOW_PORT"; do
		# The route is deleted between ports and the parent is seen answering
		# for itself again first: the 500 asserted below is then THIS port's.
		# (Re-applied in place, the second and third port's expected answer
		# was the one the first had left.)
		remove_route "$R_BACKEND_ROUTE"
		captured "$(marker mp-httponly "$HTTP_PORT")" "$name:$HTTP_PORT" "" "before backendRef port $port: the previous port's route is deleted and its 500 is still answered"
		loopback "$(marker mp-httponly "$HTTP_PORT")" "$name:$HTTP_PORT" "before backendRef port $port, loopback: the previous port's route is deleted and its 500 is still answered"
		if ! apply_backend_route "$R_BACKEND_ROUTE" mp-httponly "$port"; then
			fail "the HTTPRoute $R_BACKEND_ROUTE (backendRef port $port) was not accepted by the API server"
			continue
		fi
		expect_condition "$R_BACKEND_ROUTE" ResolvedRefs "False/BackendNotFound" \
			"'True/ResolvedRefs' is the status not reading the backendRef's port (backendRef port $port)"
		captured "HTTP500" "$name:$HTTP_PORT" "" \
			"backendRef port $port: '$(marker mp-multi "$HTTP_PORT")' is the request delivered to the port mp-multi's pods declare, which the route never named; 'HTTP503' is a cluster that does not exist"
		loopback "HTTP500" "$name:$HTTP_PORT" \
			"backendRef port $port, on the loopback listener: a marker is the request delivered to a port the route never named"
	done
	remove_route "$R_BACKEND_ROUTE"
	captured "$(marker mp-httponly "$HTTP_PORT")" "$name:$HTTP_PORT" "" "the HTTPRoute was deleted and its 500 is still answered"
	loopback "$(marker mp-httponly "$HTTP_PORT")" "$name:$HTTP_PORT" "the HTTPRoute was deleted and its 500 is still answered"
}

# apply_parent_route PORT — the HTTPRoute of R2: every request of mp-httponly's
# port PORT to mp-multi's second HTTP port.
apply_parent_route() {
	kc apply -f - >/dev/null <<YAML
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: $R_PARENT_ROUTE, namespace: $TEST_NS}
spec:
  parentRefs:
    - {group: "", kind: Service, name: mp-httponly, port: $1}
  rules:
    - backendRefs:
        - {group: "", kind: Service, name: mp-multi, port: $ALT_PORT}
YAML
}

# R2 — a parentRef to a port the parent does not have attaches the route to
# nothing: Accepted=False with NoMatchingParent, and the parent answers for
# itself on every path, as if the route did not exist. The route's backendRef
# is a real port of ANOTHER service, so a route that did attach would move the
# request there.
leg_R2() {
	log "R2 an HTTPRoute whose parentRef names a port mp-httponly does not have: status says so, and the route is attached to nothing"
	local name port own
	name="$(mesh_name mp-httponly)"
	own="$(marker mp-httponly "$HTTP_PORT")"
	for port in "$MESH_HTTP_PORT" "$LOW_PORT" "$UNDECLARED_PORT"; do
		# First the same route on a port the parent HAS: it attaches, and the
		# request moves to its backend. Only then the wrong port. "The parent
		# answers for itself" is also what a route the agent never read looks
		# like; after this control it is the answer to THIS parentRef.
		if ! apply_parent_route "$HTTP_PORT"; then
			fail "the HTTPRoute $R_PARENT_ROUTE (parentRef port $HTTP_PORT, the control) was not accepted by the API server"
			continue
		fi
		expect_condition "$R_PARENT_ROUTE" Accepted "True/Accepted" "the control: a parentRef to the parent's real port $HTTP_PORT (before parentRef port $port)"
		captured "$(marker mp-multi "$ALT_PORT")" "$name:$HTTP_PORT" "" \
			"the control before parentRef port $port: '$own' is the route not applied to the port it does name"
		if ! apply_parent_route "$port"; then
			fail "the HTTPRoute $R_PARENT_ROUTE (parentRef port $port) was not accepted by the API server"
			continue
		fi
		expect_condition "$R_PARENT_ROUTE" Accepted "False/NoMatchingParent" \
			"'True/Accepted' is the status not reading the parentRef's port (parentRef port $port)"
		captured "$own" "$name:$HTTP_PORT" "" \
			"parentRef port $port: '$(marker mp-multi "$ALT_PORT")' is the route applied to a port its parentRef did not name"
		# The two spellings that used to mean "the port the pods declare" are
		# refused by name whether or not the route exists; what matters here
		# is that the route does not answer for them either.
		captured "HTTP421:port-not-declared" "$name:$MESH_HTTP_PORT" "" \
			"parentRef port $port, dial :$MESH_HTTP_PORT: '$(marker mp-multi "$ALT_PORT")' is the route applied through the proxy port"
		captured "HTTP421:port-not-declared" "$name" "" \
			"parentRef port $port, port-less dial: '$(marker mp-multi "$ALT_PORT")' is the route applied to a port mp-httponly does not declare"
		loopback "$own" "$name:$HTTP_PORT" \
			"parentRef port $port, loopback: '$(marker mp-multi "$ALT_PORT")' is the route applied to a port its parentRef did not name"
		loopback "HTTP421:port-required" "$name" \
			"parentRef port $port, loopback, port-less: '$(marker mp-multi "$ALT_PORT")' is the route applied to a name without a port"
	done
	remove_route "$R_PARENT_ROUTE"
}

# R3 — a rule with one valid and one invalid backendRef of equal weight: the
# invalid one's share is answered 500 and the rest reaches the valid port; no
# request is delivered anywhere else. Counted over 60 requests: both answers
# must be seen, and nothing but those two.
leg_R3() {
	log "R3 an HTTPRoute with a valid and an invalid backendRef of equal weight: half the requests are answered 500, the rest by the valid port"
	local name hist good bad other
	name="$(mesh_name mp-httponly)"
	if ! apply_backend_route "$R_WEIGHTED_ROUTE" mp-httponly "$ALT_PORT=1" "$UNDECLARED_PORT=1"; then
		fail "the HTTPRoute $R_WEIGHTED_ROUTE was not accepted by the API server"
		return 0
	fi
	expect_condition "$R_WEIGHTED_ROUTE" ResolvedRefs "False/BackendNotFound" "one of the two backendRefs names a port mp-multi does not have"
	# Wait until the rule is in force: the valid backend's marker is seen.
	local deadline=$(($(date +%s) + ${MP_TIMEOUT:-90}))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		http_tokens client "http://$name:$HTTP_PORT/" "" 6 | grep -q "^$(marker mp-multi "$ALT_PORT")$" && break
		sleep 2
	done
	hist="$(http_tokens client "http://$name:$HTTP_PORT/" "" 60 | sort | uniq -c | awk '{printf "%s=%s ", $2, $1}')"
	good="$(printf '%s' "$hist" | tr ' ' '\n' | awk -F= -v m="$(marker mp-multi "$ALT_PORT")" '$1==m{print $2}')"
	bad="$(printf '%s' "$hist" | tr ' ' '\n' | awk -F= '$1=="HTTP500"{print $2}')"
	other=$((60 - ${good:-0} - ${bad:-0}))
	note "R3 answers over 60 requests: $hist"
	if [ "${good:-0}" -ge 10 ] && [ "${bad:-0}" -ge 10 ] && [ "$other" -eq 0 ]; then
		passed "captured $name:$HTTP_PORT: ${good} answered by mp-multi:$ALT_PORT, ${bad} answered 500, nothing else"
	else
		fail "captured $name:$HTTP_PORT over 60 requests: $hist" \
			"want only '$(marker mp-multi "$ALT_PORT")' and 'HTTP500', at least 10 of each (the weights are equal); any other token is a request delivered to a port the route did not name"
	fi
	remove_route "$R_WEIGHTED_ROUTE"
	captured "$(marker mp-httponly "$HTTP_PORT")" "$name:$HTTP_PORT" "" "the HTTPRoute was deleted and is still in force"
}

# --- a name without a namespace is the caller's namespace's ---------------------
#
# "mp-short" exists in two namespaces, with different answers (shorta in
# $TEST_NS, shortb in $TEST_NS2), and not in a third. A Host with no namespace
# ("mp-short:8080") means the service of that name in the CALLER's namespace,
# as the cluster's DNS search path does: never another namespace's, whether or
# not the name is unique, and a refusal when the caller's namespace has none.

# ns_tokens NS CLIENT URL HOST N — http_tokens from deploy/CLIENT of NS.
ns_tokens() {
	local ns="$1"
	shift
	TEST_NS="$ns" http_tokens "$@"
}

# ns_proxy_tokens NS CLIENT URL HOST N — the same through the proxy port, as
# an origin-form request to the loopback listener with HOST.
ns_loopback_tokens() {
	local ns="$1" client="$2" host="$3" n="$4"
	TEST_NS="$ns" http_tokens "$client" "$LOOPBACK_URL" "$host" "$n"
}

# N1 — a caller in the first namespace gets the first namespace's service, on
# both paths, although a service of the same name exists elsewhere.
leg_N1() {
	log "N1 Host $SHORT_NAME:$HTTP_PORT from $TEST_NS is $SHORT_NAME.$TEST_NS, captured and through the proxy port"
	local other meaning
	other="$SHORT_NAME.$TEST_NS2.svc.cluster.local"
	meaning="'$SHORT_MARKER_B' is the short name answered by ANOTHER namespace's service; 'HTTP421:caller-namespace-unknown' is the caller's namespace not read off the connection"
	expect "from $TEST_NS, captured http://$SHORT_NAME:$HTTP_PORT/" "$SHORT_MARKER_A" 240 "$A_PROBES" "$meaning" \
		ns_tokens "$TEST_NS" client "http://$SHORT_NAME:$HTTP_PORT/" ""
	expect "from $TEST_NS, loopback, Host $SHORT_NAME:$HTTP_PORT" "$SHORT_MARKER_A" "$A_TIMEOUT" "$A_PROBES" "$meaning" \
		ns_loopback_tokens "$TEST_NS" client-nocapture "$SHORT_NAME:$HTTP_PORT"
	# The address dialled does not choose the namespace either: the other
	# namespace's Service address, with the short name in Host.
	expect "from $TEST_NS, captured dial to $other:$HTTP_PORT with Host $SHORT_NAME:$HTTP_PORT" "$SHORT_MARKER_A" "$A_TIMEOUT" "$A_PROBES" "$meaning" \
		ns_tokens "$TEST_NS" client "http://$other:$HTTP_PORT/" "$SHORT_NAME:$HTTP_PORT"
	# The full names are unaffected, from anywhere.
	expect "from $TEST_NS, captured http://$SHORT_NAME.$TEST_NS2.$MESH_DOMAIN:$HTTP_PORT/" "$SHORT_MARKER_B" "$A_TIMEOUT" "$A_PROBES" \
		"the full name of the other namespace's service does not answer as that service" \
		ns_tokens "$TEST_NS" client "http://$SHORT_NAME.$TEST_NS2.$MESH_DOMAIN:$HTTP_PORT/" ""
}

# N2 — the mirror: a caller in the second namespace gets the second's.
leg_N2() {
	log "N2 Host $SHORT_NAME:$HTTP_PORT from $TEST_NS2 is $SHORT_NAME.$TEST_NS2, captured and through the proxy port"
	local other meaning
	other="$SHORT_NAME.$TEST_NS.svc.cluster.local"
	meaning="'$SHORT_MARKER_A' is the short name answered by ANOTHER namespace's service"
	expect "from $TEST_NS2, captured http://$SHORT_NAME:$HTTP_PORT/" "$SHORT_MARKER_B" 240 "$A_PROBES" "$meaning" \
		ns_tokens "$TEST_NS2" client "http://$SHORT_NAME:$HTTP_PORT/" ""
	expect "from $TEST_NS2, loopback, Host $SHORT_NAME:$HTTP_PORT" "$SHORT_MARKER_B" "$A_TIMEOUT" "$A_PROBES" "$meaning" \
		ns_loopback_tokens "$TEST_NS2" client "$SHORT_NAME:$HTTP_PORT"
	expect "from $TEST_NS2, captured dial to $other:$HTTP_PORT with Host $SHORT_NAME:$HTTP_PORT" "$SHORT_MARKER_B" "$A_TIMEOUT" "$A_PROBES" "$meaning" \
		ns_tokens "$TEST_NS2" client "http://$other:$HTTP_PORT/" "$SHORT_NAME:$HTTP_PORT"
	# The per-port rules apply to the short name as to the full one.
	expect "from $TEST_NS2, loopback, Host $SHORT_NAME:$UNDECLARED_PORT" "HTTP421:port-not-declared" "$A_TIMEOUT" "$A_PROBES" \
		"a marker is the short name answered on a port its service does not declare" \
		ns_loopback_tokens "$TEST_NS2" client "$SHORT_NAME:$UNDECLARED_PORT"
	expect "from $TEST_NS2, loopback, Host $SHORT_NAME (no port)" "HTTP421:port-required" "$A_TIMEOUT" "$A_PROBES" \
		"a marker is a name without a port read as something else than port 80" \
		ns_loopback_tokens "$TEST_NS2" client "$SHORT_NAME"
}

# N3 — a caller in a namespace that has NO MESH service of the name. The name
# is then not a mesh name for that caller (the owner's ruling of 2026-10-12),
# whether or not another namespace has a mesh service called the same: it is
# handled like any destination that is not the mesh's. $TEST_NS3 has a plain
# Service "$SHORT_NAME" of its own, not a mesh service.
#
#   - http://$SHORT_NAME:$HTTP_PORT/ resolves, in the cluster's DNS, to that
#     plain Service, and reaches it: the passthrough to the address dialled.
#     Until the ruling it was refused 421, because two other namespaces have
#     a mesh service of the name.
#   - The passthrough is not a way to another namespace's mesh service. A dial
#     to the address of the mesh service of $TEST_NS or $TEST_NS2 with the
#     short name in Host goes to the address that was dialled, a mesh Service
#     address that nothing but the mesh serves: it is answered by neither
#     service.
#   - On the proxy port nothing was dialled: the name gets what any name that
#     is not the mesh's gets there.
leg_N3() {
	log "N3 Host $SHORT_NAME:$HTTP_PORT from $TEST_NS3, which has no MESH $SHORT_NAME, is not a mesh name there"
	local meaning target
	meaning="'$SHORT_MARKER_A' or '$SHORT_MARKER_B' is the short name answered by a mesh service of a namespace the caller is not in; 'HTTP421' is the name refused instead of treated as not the mesh's"
	expect "from $TEST_NS3, captured http://$SHORT_NAME:$HTTP_PORT/ (its own plain Service)" "$SHORT_MARKER_PLAIN" 240 "$A_PROBES" "$meaning" \
		ns_tokens "$TEST_NS3" client "http://$SHORT_NAME:$HTTP_PORT/" ""
	for target in "$SHORT_NAME.$TEST_NS.svc.cluster.local" "$SHORT_NAME.$TEST_NS2.svc.cluster.local"; do
		expect "from $TEST_NS3, captured dial to $target:$HTTP_PORT with Host $SHORT_NAME:$HTTP_PORT" "HTTP503" "$A_TIMEOUT" "$A_PROBES" \
			"$meaning; '$SHORT_MARKER_PLAIN' cannot be: that address was not dialled" \
			ns_tokens "$TEST_NS3" client "http://$target:$HTTP_PORT/" "$SHORT_NAME:$HTTP_PORT"
	done
	# The proxy port: whatever a name that is not the mesh's gets there, the
	# short name gets too. (Measured, not assumed: 404 on the loopback
	# listener; from a pod on redirect-all the dial to 127.0.0.1:18081 can be
	# the proxy port's capture chain, whose floor is the passthrough.)
	local control
	control="$(ns_loopback_tokens "$TEST_NS3" client "mp-not-a-mesh-name:$HTTP_PORT" 1 | tr -d '[:space:]')"
	case "$control" in
	HTTP404 | HTTP503)
		expect "from $TEST_NS3, proxy port, Host $SHORT_NAME:$HTTP_PORT (a name that is not the mesh's gets $control there)" "$control" "$A_TIMEOUT" "$A_PROBES" "$meaning" \
			ns_loopback_tokens "$TEST_NS3" client "$SHORT_NAME:$HTTP_PORT"
		;;
	*)
		fail "from $TEST_NS3, proxy port: a name that is not the mesh's was answered '$control'" "expected a 404 or the passthrough's failure"
		;;
	esac
	# And its own full-name requests are served as for anyone.
	expect "from $TEST_NS3, captured http://$SHORT_NAME.$TEST_NS.$MESH_DOMAIN:$HTTP_PORT/" "$SHORT_MARKER_A" "$A_TIMEOUT" "$A_PROBES" \
		"the full name must answer from any namespace" \
		ns_tokens "$TEST_NS3" client "http://$SHORT_NAME.$TEST_NS.$MESH_DOMAIN:$HTTP_PORT/" ""
}

# --- the mesh's own two ports are a port of no service --------------------------

# V1 — 18081 and 18082 are the mesh's. An authority that names one is refused
# at once for a name the node knows NOTHING of as well (until this rule it was
# offered on demand, parked, and failed after the on-demand timeout; an alias
# cluster of that name answered it if the service became known meanwhile), and
# a pod that declares one as an application port is denied at admission.
leg_V1() {
	log "V1 the mesh's own ports are a port of no service: an unknown name with :$MESH_HTTP_PORT is refused at once, and a pod cannot declare one"
	local unknown out port
	unknown="mp-nowhere.$TEST_NS.$MESH_DOMAIN"
	for port in "$MESH_HTTP_PORT" "$MESH_TCP_PORT"; do
		expect "loopback, Host $unknown:$port (a name nothing serves)" "HTTP421:port-not-declared" "$A_TIMEOUT" "$A_PROBES" \
			"HTTP503 is the request offered on demand and parked for a cluster named for the mesh's own port" \
			http_tokens client-nocapture "$LOOPBACK_URL" "$unknown:$port"
	done
	expect "captured dial to $(mesh_name mp-httponly):$MESH_HTTP_PORT with Host $unknown:$MESH_HTTP_PORT" "HTTP421:port-not-declared" "$A_TIMEOUT" "$A_PROBES" \
		"HTTP503 is the request parked on the proxy port's capture table" \
		http_tokens client "http://$(mesh_name mp-httponly):$MESH_HTTP_PORT/" "$unknown:$MESH_HTTP_PORT"

	# Admission. A server-side dry run goes through the webhooks and creates
	# nothing.
	for port in "$MESH_HTTP_PORT" "$MESH_TCP_PORT"; do
		if out="$(reserved_pod "$port" | kc apply --dry-run=server -f - 2>&1)"; then
			fail "a pod that declares port $port was admitted: $out" "the controller's pod webhook must deny it"
		elif ! grep -q "port $port as an application port" <<<"$out" || ! grep -q "are the mesh's own ports" <<<"$out"; then
			fail "a pod that declares port $port was refused for another reason: $out" ""
		else
			passed "a pod declaring $port is denied at admission, naming the port"
		fi
	done
	if out="$(reserved_pod "$HTTP_PORT" | kc apply --dry-run=server -f - 2>&1)"; then
		passed "the same pod declaring $HTTP_PORT is admitted"
	else
		fail "the same pod declaring $HTTP_PORT is refused: $out" "the denial above is then not about the port"
	fi
}

# --- a mesh service behind a Service of the user's ------------------------------

# U1 — a mesh service fronted by a user-owned Service is not a captured mesh
# name, and attaching a service-wide filter to it does not make its SHORT name
# one. The Service has no route, so the agent has no port map of it: a dial to
# any of its Kubernetes names leaves through the passthrough to the Service's
# own address and reaches the pods.
#
# On the mesh name the port is the Service's ($OWN_SERVICE_PORT): the agent
# holds the port map of every Service.
#
# The filter is an RBAC policy no caller satisfies. That it is in force is
# seen where the mesh applies it, by an answer only the filter gives: the
# service's MESH name answers 403 on the loopback listener, and 403 through
# the proxy port from the CAPTURED client, the pod whose calls are looked at
# below. No call that worked before the filter
# may be refused after it: the Kubernetes names, and the mesh name in Host on
# a dial to the Service's own port, must all still answer.
# (With the filter, the short name alone was claimed and refused 421
# port-not-declared, while "<name>.<ns>" still passed through; and then the
# mesh name in Host on a dial to the Service's port was, which this leg read
# as its proof that the filter had arrived.)
# u1_dial WANT AUTHORITY HOST WHEN MEANING: a captured dial to AUTHORITY (a
# Service of the user's that selects mp-own's pods) with Host HOST.
u1_dial() {
	expect "captured dial to $2 with Host $3, $4" "$1" "$A_TIMEOUT" "$A_PROBES" "$5" \
		http_tokens client "http://$2/" "$3"
}

leg_U1() {
	local short="$OWN_NAME:$OWN_SERVICE_PORT" two="$OWN_NAME.$TEST_NS:$OWN_SERVICE_PORT" meaning mesh
	mesh="$(mesh_name "$OWN_NAME"):$OWN_SERVICE_PORT"
	log "U1 a mesh service behind a user's Service ($OWN_NAME, $OWN_SERVICE_PORT -> $HTTP_PORT): its Kubernetes names pass through, with a service-wide filter too"
	meaning="HTTP421 is the name claimed by the mesh for a port map the agent does not have"
	captured "$OWN_MARKER" "$short" "" "control, no filter: $meaning"
	captured "$OWN_MARKER" "$two" "" "control, no filter: $meaning"
	loopback "$OWN_MARKER" "$(mesh_name "$OWN_NAME"):$OWN_SERVICE_PORT" "control, no filter: the mesh name reaches the service through the mesh"
	# The table of the dialled port, before the filter: the mesh name in Host
	# on a dial to the Service's address is not claimed there either (the
	# node has no virtual host for the service on this table), and passes
	# through like the Service's own names.
	expect "captured dial to $short with Host $(mesh_name "$OWN_NAME"):$OWN_SERVICE_PORT, no filter" "$OWN_MARKER" "$A_TIMEOUT" "$A_PROBES" \
		"control: the table of the dialled port claims the service's mesh name before any filter exists" \
		http_tokens client "http://$short/" "$mesh"
	proxied "$OWN_MARKER" client "$mesh" "control, no filter: the mesh name reaches the service through the proxy port from the captured client"
	# The same pods behind two more Services of the user's, dialled with the
	# mesh name in Host: on a port number another mesh service serves as HTTP
	# (the table of that port has an on-demand route), and on the pods' own.
	local alt="$OWN_ALT_SERVICE:$ALT_PORT" own_pod="$OWN_POD_SERVICE:$HTTP_PORT" bare pod_host
	bare="$(mesh_name "$OWN_NAME")"
	pod_host="$bare:$HTTP_PORT"
	u1_dial "$OWN_MARKER" "$alt" "$bare" "no filter" "control: the dial does not reach the pods through the user's Service before any filter exists"
	u1_dial "$OWN_MARKER" "$alt" "$pod_host" "no filter" "control: the dial does not reach the pods through the user's Service before any filter exists"
	u1_dial "$OWN_MARKER" "$own_pod" "$pod_host" "no filter" "control: the dial does not reach the pods before any filter exists"
	u1_dial "$OWN_MARKER" "$own_pod" "$bare" "no filter" "control: the dial does not reach the pods before any filter exists"

	kc apply -f - >/dev/null <<YAML || die "U1: the HTTPFilter was not accepted"
apiVersion: config.aether.io/v1
kind: HTTPFilter
metadata: {name: $OWN_FILTER, namespace: $TEST_NS}
spec:
  scope: SCOPE_CHAIN
  targetRefs:
    - kind: Service
      name: $OWN_NAME
  rbac:
    action: ACTION_ALLOW
    policies:
      - name: nobody
        principals:
          - namespace: mp-no-such-namespace
YAML
	expect "loopback, Host $(mesh_name "$OWN_NAME"):$OWN_SERVICE_PORT with the filter" "HTTP403" 90 "$A_PROBES" \
		"the service-wide filter never came into force on the mesh name, so the checks below would show nothing" \
		http_tokens client-nocapture "$LOOPBACK_URL" "$(mesh_name "$OWN_NAME"):$OWN_SERVICE_PORT"
	# And for the captured client, whose three calls below are the point: the
	# proxy port answers it 403 for the mesh name too. (That the filter had
	# reached this pod was read off a refusal: the dial to the Service's port
	# with the mesh name in Host turned 421 port-not-declared when the filter
	# arrived. That refusal was the defect: the call had worked.)
	expect "client: curl --proxy $PROXY_URL http://$mesh/ with the filter" "HTTP403" 90 "$A_PROBES" \
		"'$OWN_MARKER' is the filter not being in force for the captured client, so the three checks below would show nothing" \
		proxy_tokens client "http://$mesh/" ""
	expect "captured dial to $short with Host $mesh, with the filter" "$OWN_MARKER" "$A_TIMEOUT" "$A_PROBES" \
		"HTTP421:port-not-declared is the filter having made the mesh claim, and refuse, a call that passed through to the user's Service before it" \
		http_tokens client "http://$short/" "$mesh"
	captured "$OWN_MARKER" "$short" "" "with the filter: $meaning"
	captured "$OWN_MARKER" "$two" "" "with the filter: $meaning"
	# A port number another mesh service serves as HTTP: not a port of this
	# service, so the dial goes where it went before the filter, to the
	# address that was dialled. (It was 421 port-not-declared, and 421
	# port-mismatch with the pods' port in Host.)
	meaning="HTTP421 is the filter having made the mesh refuse a call that passed through to the user's Service before it; HTTP403 would be the call taken into the mesh on a port the service does not have"
	u1_dial "$OWN_MARKER" "$alt" "$bare" "with the filter" "$meaning"
	u1_dial "$OWN_MARKER" "$alt" "$pod_host" "with the filter" "$meaning"
	# The pods' own port, with a Host that names it or no port: that was a
	# mesh delivery before the filter (fetched on demand), and it is one with
	# the filter now. The filter denies every caller, so it answers 403:
	# refused by the policy the user attached, not by the mesh's addressing
	# (421), and never delivered without the filter.
	meaning="HTTP421 is the mesh refusing the name by its addressing; '$OWN_MARKER' is the request delivered without the service-wide filter"
	u1_dial "HTTP403" "$own_pod" "$pod_host" "with the filter" "$meaning"
	u1_dial "HTTP403" "$own_pod" "$bare" "with the filter" "$meaning"

	kc -n "$TEST_NS" delete httpfilters.config.aether.io "$OWN_FILTER" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	loopback "$OWN_MARKER" "$(mesh_name "$OWN_NAME"):$OWN_SERVICE_PORT" "the HTTPFilter was deleted and is still in force"
}

# --- a named targetPort is refused until it resolves ----------------------------

# U2 — a Service port whose targetPort is a NAME names nothing until an
# EndpointSlice of the Service says which number the name is (owner's ruling,
# 2026-10-13), and the agent learns that from the slice's EVENT.
#
# (a) The event. $LATE_SLICED is a Service with NO selector, port
# $LATE_SERVICE_PORT, targetPort "web": Kubernetes makes no EndpointSlice for
# it, so every slice it has is one this leg writes, and a slice is the only
# thing that changes between two checks. An HTTPRoute sends mp-httponly's
# traffic to it. With no slice the reference is invalid: the status says so
# and the request is answered 500 on the captured path and on the loopback
# listener (it was delivered to the port the backend's pods declare). A slice
# that names the port makes the status True; a second slice that gives the
# same name another number makes it False again; deleting that one makes it
# True. An agent that does not watch EndpointSlices sees none of the three.
# (The first form of this leg created the pods instead, and passed with the
# watch removed: the pods' registration is an event of its own.)
#
# (b) No route. $LATE_NAME is a Service of the user's, port
# $LATE_SERVICE_PORT, targetPort "web", in front of mesh pods whose container
# port is named "web", and NO route names it. The mesh name of the service
# answers on the proxy port by the Service's port. Names were resolved only
# for a Service a route named, so this one stayed unresolved for ever: 421
# "Its ports: none" on every spelling, with nothing logged.
leg_U2() {
	local name
	name="$(mesh_name mp-httponly)"
	log "U2 a named targetPort is refused until it resolves, and resolves on the EndpointSlice's event"
	kc apply -f - >/dev/null <<YAML || die "U2: Service $LATE_SLICED apply failed"
apiVersion: v1
kind: Service
metadata: {name: $LATE_SLICED, namespace: $TEST_NS}
spec:
  ports: [{name: http, port: $LATE_SERVICE_PORT, targetPort: web}]
YAML
	kc apply -f - >/dev/null <<YAML || die "U2: the HTTPRoute was not accepted"
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: $LATE_ROUTE, namespace: $TEST_NS}
spec:
  parentRefs:
    - {group: "", kind: Service, name: mp-httponly}
  rules:
    - backendRefs:
        - {group: "", kind: Service, name: $LATE_SLICED, port: $LATE_SERVICE_PORT}
YAML
	expect_condition "$LATE_ROUTE" ResolvedRefs "False/BackendNotFound" \
		"'True/ResolvedRefs' is a named targetPort that resolves to nothing being taken on trust"
	captured "HTTP500" "$name:$HTTP_PORT" "" \
		"no EndpointSlice of $LATE_SLICED names its port: a marker or HTTP503 is the request sent on to a port nothing named"
	loopback "HTTP500" "$name:$HTTP_PORT" "the same on the loopback listener"

	# Each of the three slice changes must reach the status within
	# $U2_EVENT_WINDOW seconds, and the three follow one another at once.
	# That is what tells the agent's WATCH from a relist: with `list` and
	# `get` on endpointslices but no `watch` (the role before this chart's
	# last change, or one edited by hand) the informer's reflector cannot
	# watch, backs off, and lists again every 30 to 60 seconds once its
	# back-off has reached its cap (client-go: 800 ms doubling to 30 s, with
	# up to as much again in jitter), which every change would be seen by in
	# the end. Three windows of $U2_EVENT_WINDOW s in a row are shorter than
	# one such interval, so a relist can answer one of them at most; with the
	# plain 90 s wait this leg passed 9 of 9 without the verb.
	u2_slice a "$HTTP_PORT"
	expect_condition_within "$U2_EVENT_WINDOW" "$LATE_ROUTE" ResolvedRefs "True/ResolvedRefs" \
		"'False/BackendNotFound' is the agent not having been told of the EndpointSlice by its watch: nothing else changed"
	u2_slice b "$ALT_PORT"
	expect_condition_within "$U2_EVENT_WINDOW" "$LATE_ROUTE" ResolvedRefs "False/BackendNotFound" \
		"'True/ResolvedRefs' is two EndpointSlices that give the name different numbers resolved to one of them, or the second slice not seen by the watch"
	kc -n "$TEST_NS" delete endpointslice "$LATE_SLICED-b" --wait=true >/dev/null 2>&1 || true
	expect_condition_within "$U2_EVENT_WINDOW" "$LATE_ROUTE" ResolvedRefs "True/ResolvedRefs" \
		"'False/BackendNotFound' is the agent not having been told by its watch that the EndpointSlice went"

	remove_route "$LATE_ROUTE"
	kc -n "$TEST_NS" delete endpointslice "$LATE_SLICED-a" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	kc -n "$TEST_NS" delete service "$LATE_SLICED" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	captured "$(marker mp-httponly "$HTTP_PORT")" "$name:$HTTP_PORT" "" "the HTTPRoute was deleted and mp-httponly does not answer for itself"

	# (b) a user's Service with a named targetPort that no route names.
	kc apply -f - >/dev/null <<YAML || die "U2: Service and workload $LATE_NAME apply failed"
apiVersion: v1
kind: Service
metadata: {name: $LATE_NAME, namespace: $TEST_NS}
spec:
  selector: {app: $LATE_NAME}
  ports: [{name: http, port: $LATE_SERVICE_PORT, targetPort: web}]
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: $LATE_NAME, namespace: $TEST_NS}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: $LATE_NAME, namespace: $TEST_NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: $LATE_NAME}}
  template:
    metadata:
      labels: {app: $LATE_NAME, aether.io/managed: "true"}
      annotations:
        endpoint.aether.io/port: "$HTTP_PORT"
        endpoint.aether.io/protocol: "http"
        endpoint.aether.io/ports: "$HTTP_PORT"
    spec:
      serviceAccountName: $LATE_NAME
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: web
          image: $HTTP_ECHO_IMAGE
          args: ["-text=$LATE_MARKER", "-listen=:$HTTP_PORT"]
          ports: [{name: web, containerPort: $HTTP_PORT}]
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: ["ALL"]}
YAML
	kc -n "$TEST_NS" rollout status "deploy/$LATE_NAME" --timeout=180s >/dev/null ||
		die "U2: $LATE_NAME never became ready"
	expect "loopback $LOOPBACK_URL with Host: $(mesh_name "$LATE_NAME"):$LATE_SERVICE_PORT (a named targetPort, no route)" "$LATE_MARKER" 90 "$A_PROBES" \
		"HTTP421 is the Service's named targetPort never resolved because no route names the Service: its mesh service has no port at all on the proxy port" \
		http_tokens client-nocapture "$LOOPBACK_URL" "$(mesh_name "$LATE_NAME"):$LATE_SERVICE_PORT"
	proxied "$LATE_MARKER" client "$(mesh_name "$LATE_NAME"):$LATE_SERVICE_PORT" "the same through the proxy port from the captured client"

	kc -n "$TEST_NS" delete deploy "$LATE_NAME" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	kc -n "$TEST_NS" delete service "$LATE_NAME" --ignore-not-found --wait=true >/dev/null 2>&1 || true
}

# u2_slice SUFFIX PORT — an EndpointSlice of $LATE_SLICED, written by hand,
# that gives its port name "http" the number PORT.
u2_slice() {
	kc apply -f - >/dev/null <<YAML || die "U2: EndpointSlice $LATE_SLICED-$1 apply failed"
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: $LATE_SLICED-$1
  namespace: $TEST_NS
  labels: {kubernetes.io/service-name: $LATE_SLICED}
addressType: IPv4
ports: [{name: http, port: $2, protocol: TCP}]
endpoints: [{addresses: ["10.250.0.9"]}]
YAML
}

# --- a port that stops being HTTP is refused at once ----------------------------

# T1 — the retention grace is for a service that VANISHED, not for one that
# changed shape. mp-move serves HTTP on $HTTP_PORT; it is redeployed with the
# same two listeners and other annotations: raw TCP on $TCP_PORT, nothing on
# $HTTP_PORT. From then on the registry holds rows of the service that do not
# declare $HTTP_PORT, which is evidence, and the node proxy must answer that
# port by it at once. Before this rule the HTTP cluster and its virtual host
# on the loopback listener were kept for the 90 s grace, and their endpoints
# were the remaining pods on their raw-TCP port.
#
# Loopback, because that is the table the retained virtual host was in; the
# captured table of a port number is built from the declared ports alone.
leg_T1() {
	local name
	name="$(mesh_name "$MOVE_NAME")"
	log "T1 a declared port that stops being HTTP is refused at once: $MOVE_NAME moves from HTTP :$HTTP_PORT to raw TCP :$TCP_PORT"
	loopback "$MOVE_MARKER_HTTP" "$name:$HTTP_PORT" "control: the service is not served on its HTTP port before the move"

	deploy_workload "$MOVE_NAME" "$TCP_PORT" tcp "$TCP_PORT" "http:$HTTP_PORT:$MOVE_MARKER_HTTP" "tcp:$TCP_PORT:$MOVE_MARKER_TCP"
	kc -n "$TEST_NS" rollout status "deploy/$MOVE_NAME" --timeout=180s >/dev/null ||
		die "T1: $MOVE_NAME never finished its rollout"

	expect "loopback, Host $name:$HTTP_PORT within ${T1_WINDOW}s of the move" "HTTP421:port-not-declared" "$T1_WINDOW" "$A_PROBES" \
		"HTTP502/503 or $MOVE_MARKER_HTTP for that long is the retained HTTP cluster and virtual host of a port the service's own rows no longer declare" \
		http_tokens client-nocapture "$LOOPBACK_URL" "$name:$HTTP_PORT"
	expect "raw TCP to $name:$TCP_PORT" "$MOVE_MARKER_TCP" 90 "$A_PROBES" \
		"NOREPLY means the port the service now declares has no chain or no tcp: cluster" \
		tcp_tokens "$name:$TCP_PORT" line
}

# reserved_pod PORT — a mesh pod that declares PORT, for an admission dry run.
reserved_pod() {
	cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: mp-reserved-$1
  namespace: $TEST_NS
  labels: {app: mp-reserved, aether.io/managed: "true"}
  annotations:
    endpoint.aether.io/port: "$HTTP_PORT"
    endpoint.aether.io/ports: "$HTTP_PORT,$1"
spec:
  serviceAccountName: default
  containers:
    - name: app
      image: ${IMAGE_REGISTRY}/l4echo:latest
EOF
}

# --- a raw-TCP service is reached on the ports it declares ---------------------

# W1 — the two spellings of a raw-TCP service that work: its declared port, and
# the legacy :18082.
leg_W1() {
	local name target
	name="$(mesh_name "$TCP_ONLY")"
	log "W1 a service with no HTTP port, raw TCP on its declared port and on the legacy mesh port: $name:$TCP_PORT, $name:$MESH_TCP_PORT"
	for target in "$name:$TCP_PORT" "$name:$MESH_TCP_PORT"; do
		expect "raw TCP to $target" "$(tcp_marker "$TCP_ONLY")" 90 "$A_PROBES" \
			"NOREPLY means the source has no chain for that port of the service, or no tcp: cluster (is it in the client's upstreams?)" \
			tcp_tokens "$target" line
	done
}

# W2 — every other port of the service's address reaches no application. Until
# chart 3.0.0 a chain with no port sent them all to the declared port
# (onlytcp answered on every one of these). The four ports are the
# cases that differ in the data plane: a port no service declares, two port
# numbers other services declare as HTTP (each has an HTTP chain of its own),
# and port 80.
#
# What the client gets instead is the cluster's own answer for a Service with
# no endpoints on that port, and is recorded, not asserted (decision 10).
leg_W2() {
	local name port
	name="$(mesh_name "$TCP_ONLY")"
	log "W2 captured: raw TCP to a port $TCP_ONLY does not declare ($UNDECLARED_PORT, $HTTP_PORT, $ALT_PORT, $LOW_PORT) reaches no application"
	for port in "$UNDECLARED_PORT" "$HTTP_PORT" "$ALT_PORT" "$LOW_PORT"; do
		expect_no_application "$name:$port"
	done
	note "what the client sees on each of them (recorded, not asserted):"
	for port in "$UNDECLARED_PORT" "$HTTP_PORT" "$ALT_PORT" "$LOW_PORT"; do
		observe_tcp "$name:$port"
	done
}

# W3 — HTTP to a service with no HTTP port is refused by name, at once, on each
# path: a real port number it does not declare, the two legacy HTTP spellings
# (there is no HTTP port for them to mean), and on loopback its raw-TCP port.
leg_W3() {
	local name meaning
	name="$(mesh_name "$TCP_ONLY")"
	log "W3 HTTP to $TCP_ONLY, which has no HTTP port, is refused by name"
	meaning="NOREPLY is the HTTP request carried as raw bytes to the declared port (the any-port chain) or leaving through the passthrough; ':slow' is a refusal that was not immediate"
	captured "HTTP421:no-http-port" "$name:$HTTP_PORT" "" "$meaning"
	captured "HTTP421:no-http-port" "$name" "" "$meaning"
	# The proxy port: Host says :$MESH_HTTP_PORT, a port the service does not
	# declare; with no port in Host, there is no HTTP port to ask for.
	captured "HTTP421:port-not-declared" "$name:$MESH_HTTP_PORT" "" "$meaning"
	loopback "HTTP421:no-http-port" "$name" "$meaning"
	loopback "HTTP421:port-not-http" "$name:$TCP_PORT" \
		"HTTP502/503 is the request forwarded to the raw-TCP port or to a cluster nobody published; ':slow' is a refusal that was not immediate"
}

# --- :18081 is the mesh's HTTP proxy port -------------------------------------

# P1 — the request a client with a proxy sends: connect to the proxy, absolute
# URL in the request line, Host: <name>:<port>. Nothing was dialled that names
# a service port, so Host decides. curl is given --proxy and nothing else: the
# leg measures whether the listener takes the absolute-form request line at
# all, which a hand-written Host header on an origin-form request would not.
leg_P1() {
	log "P1 curl --proxy $PROXY_URL http://<mesh name>:P/, from both clients: Host decides, port included"
	local client name p replies
	name="$(mesh_name mp-multi)"
	for client in "${CLIENTS[@]}"; do
		for p in $(wl_http_ports mp-multi); do
			proxied "$(marker mp-multi "$p")" "$client" "$name:$p" \
				"NOREPLY or HTTP400 is the listener not taking an absolute-form request line; another marker is another port answering; HTTP421 says why in x-aether-error"
		done
	done
	# Port 80 by name. curl drops a default port, so the plain form of this URL
	# is a port-less authority, which is the legacy spelling of A12 and another
	# answer. It is recorded below; the assertion spells ":80" out.
	name="$(mesh_name mp-eighty)"
	for client in "${CLIENTS[@]}"; do
		proxied "$(marker mp-eighty "$LOW_PORT")" "$client" "$name:$LOW_PORT" \
			"'$(marker mp-eighty "$HTTP_PORT")' is ':$LOW_PORT' read as a port-less name (the legacy target)" explicit
	done
	for client in "${CLIENTS[@]}"; do
		replies="$(proxy_tokens "$client" "http://$name:$LOW_PORT/" "" 3)"
		printf 'P1-OBSERVED %s: curl --proxy %s http://%s:%s/ as curl sends it (port-less request line and Host): %s\n' \
			"$client" "$PROXY_URL" "$name" "$LOW_PORT" "$(histogram "$replies")"
	done
}

# P2 — the refusals of the loopback rule hold through the proxy form, and a
# name that is no mesh service is the listener's 404.
leg_P2() {
	log "P2 curl --proxy $PROXY_URL to a port that is not declared, a raw-TCP port, and a name that is no mesh service"
	local client
	for client in "${CLIENTS[@]}"; do
		proxied "HTTP421:port-not-declared" "$client" "$(mesh_name mp-multi):$UNDECLARED_PORT" \
			"HTTP503 (and ':slow') is the on-demand lookup of a cluster that will never exist; a marker is another port answering for an undeclared one"
		proxied "HTTP421:port-not-http" "$client" "$(mesh_name mp-mixed):$TCP_PORT" \
			"HTTP502/503 is the request forwarded to the raw-TCP port; ':slow' is a refusal that was not immediate"
	done
	proxied "HTTP404" client-nocapture "example.invalid:$HTTP_PORT" \
		"a name that is no mesh service is not served by the proxy port: anything but its 404 is the request going somewhere"
}

# P3 — a captured dial to <name>:18081 is a dial to the proxy port, not to a
# port of the service: Host decides there too. (A dial to a real port is A5:
# the dialled port decides, and a Host that names another one is refused.)
leg_P3() {
	log "P3 captured: dial <name>:$MESH_HTTP_PORT, the proxy port, with Host <name>:P"
	local name p
	# Mesh names only, as on the loopback listener: the proxy port has no
	# virtual host for a cluster.local name.
	name="$(mesh_name mp-multi)"
	for p in "$ALT_PORT" "$HTTP_PORT"; do
		captured "$(marker mp-multi "$p")" "$name:$MESH_HTTP_PORT" "$name:$p" \
			"'HTTP421:port-mismatch' is the proxy port read as a real port of the service; another marker is the Host port ignored"
	done
	captured "HTTP421:port-not-declared" "$name:$MESH_HTTP_PORT" "$name:$UNDECLARED_PORT" \
		"'HTTP421:port-mismatch' is the proxy port read as a real port of the service; a marker is another port answering for an undeclared one"
}

# S1 — a pod that is NOT on redirect-all (capture.aether.io/redirect-all:
# "false"; client-nocapture is one). Only the two mesh ports are diverted for
# it, so a dial to a service's own port is not the mesh's. Its ways into the
# mesh are the proxy port's:
#
#   a. the proxy from the environment (http_proxy=$PROXY_URL), two ports;
#   b. an explicit Host "<name>:<port>" to the loopback listener, two ports;
#   c. the same Host to "<name>:$MESH_HTTP_PORT", which IS diverted for it;
#   d. and "curl http://<name>:$MESH_HTTP_PORT/" alone, whose Host says
#      :$MESH_HTTP_PORT, is refused by name: until chart 3.0.0 that was how
#      such a pod reached a service.
#
# What a dial to the service's own port gets from such a pod is recorded, not
# asserted: it is not captured, and the cluster answers for it.
leg_S1() {
	log "S1 a pod that is not on redirect-all reaches the mesh through the proxy port, and <name>:$MESH_HTTP_PORT alone is refused by name"
	local name p hist
	name="$(mesh_name mp-multi)"
	for p in "$HTTP_PORT" "$ALT_PORT"; do
		expect "client-nocapture, http_proxy=$PROXY_URL, http://$name:$p/" "$(marker mp-multi "$p")" "$A_TIMEOUT" "$A_PROBES" \
			"NOREPLY is curl not using the proxy from the environment, or the proxy port refusing an absolute-form request; another marker is another port answering" \
			tokens_via "env:$PROXY_URL" client-nocapture "http://$name:$p/" ""
		loopback "$(marker mp-multi "$p")" "$name:$p" \
			"the loopback listener with Host $name:$p: another marker is another port answering"
		expect "client-nocapture, dial http://$name:$MESH_HTTP_PORT/ with Host: $name:$p" "$(marker mp-multi "$p")" "$A_TIMEOUT" "$A_PROBES" \
			"NOREPLY is the dial to :$MESH_HTTP_PORT not diverted for a pod that is not on redirect-all; 'HTTP421:port-mismatch' is the proxy port read as a port of the service" \
			http_tokens client-nocapture "http://$name:$MESH_HTTP_PORT/" "$name:$p"
	done
	expect "client-nocapture, http://$name:$MESH_HTTP_PORT/ and nothing else" "HTTP421:port-not-declared" "$A_TIMEOUT" "$A_PROBES" \
		"'$(marker mp-multi "$(wl_legacy_port mp-multi)")' is :$MESH_HTTP_PORT read as 'the port the pods declare', which it meant until chart 3.0.0" \
		http_tokens client-nocapture "http://$name:$MESH_HTTP_PORT/" ""
	# The documented limit (runbook, "Prober chart 1.1.0"): a dial to a
	# service's OWN port from a pod that is not on redirect-all is not
	# diverted, and nothing answers it. Asserted, so that the day it is
	# diverted the documentation is found wrong here.
	expect "client-nocapture, a dial to the service's own port, http://$name:$HTTP_PORT/" "NOREPLY" "$A_TIMEOUT" 3 \
		"an answer means scoped capture diverts a service's own port now: the limit the runbook states for the prober's mesh_dns tier, the authz canary and the udsecho client is gone" \
		http_tokens client-nocapture "http://$name:$HTTP_PORT/" ""
}

# P4 — a service's HTTP filters and route rules are applied on every path to
# the service, the proxy port included. Asserted inside A4's window when A4
# runs (p4_in_window); alone, it opens the same window itself.
leg_P4() {
	log "P4 the HTTPRoute of A4 applies through the proxy port, from both clients"
	if [ "$P4_RAN" -eq 1 ]; then
		LEG_CHECKS="$P4_CHECKS"
		LEG_FAILS=(${P4_FAILS[@]+"${P4_FAILS[@]}"})
		note "asserted inside leg A4's window in this run ($P4_CHECKS assertions, ${#LEG_FAILS[@]} failed): see above"
		return 0
	fi
	if ! kc get crd httproutes.gateway.networking.k8s.io >/dev/null 2>&1; then
		fail "the HTTPRoute CRD is not installed, so the agent never watched it" "run '$0 up' first (it installs Gateway API $GWAPI_VERSION)"
		return 0
	fi
	if ! apply_a4_route; then
		fail "the HTTPRoute $A4_ROUTE was not accepted by the API server"
		return 0
	fi
	check_p4_routed
	remove_a4_route
	check_p4_gone
}

# P5 — an https:// URL through a proxy is a CONNECT to <name>:<port>. Nothing
# is decided about it: this prints what the proxy port does with one today.
leg_P5() {
	local name client rc status total seen=""
	name="$(mesh_name mp-multi)"
	log "P5 (recorded, never a failure) curl --proxy $PROXY_URL https://$name:$HTTP_PORT/, a CONNECT"
	for client in "${CLIENTS[@]}"; do
		rc="" status="" total=""
		# SC2016: evaluated by the POD's shell; $? after the substitution is curl's.
		# shellcheck disable=SC2016
		read -r rc status total < <(kc -n "$TEST_NS" exec "deploy/$client" -c curl -- sh -c '
			out=$(curl -s -o /dev/null -w "%{http_connect} %{time_total}" --max-time 5 --proxy "$2" "$1" 2>/dev/null)
			echo "$? ${out:-000 -}"
		' sh "https://$name:$HTTP_PORT/" "$PROXY_URL" 2>/dev/null || true) || true
		if [ -z "$rc" ]; then
			printf 'P5-OBSERVED %s: no observation (the probe did not run)\n' "$client"
			seen+="${seen:+; }$client: no observation"
			continue
		fi
		if [ "$status" = "000" ]; then status="none"; fi
		printf 'P5-OBSERVED %s: CONNECT %s:%s through %s: response status %s, curl exit %s, total %ss\n' \
			"$client" "$name" "$HTTP_PORT" "$PROXY_URL" "$status" "$rc" "$total"
		seen+="${seen:+; }$client: CONNECT status $status, curl exit $rc, ${total}s"
	done
	info "$seen"
}

# --- verify -------------------------------------------------------------------

# installed_images — every image of the installed aether workloads, init
# containers included, one per line.
installed_images() {
	local r
	for r in ds/aether-agent ds/aether-mesh-dns ds/aether-proxy ds/aether-uds-csi deploy/aether-registrar deploy/aether-controller; do
		kc -n "$NS" get "$r" -o jsonpath='{range .spec.template.spec.initContainers[*]}{.image}{"\n"}{end}{range .spec.template.spec.containers[*]}{.image}{"\n"}{end}' 2>/dev/null || true
	done
}

# cell_line VAR IMAGES IMAGE... — one MP-CELL line: the tags the IMAGEs of one
# group are installed at, read from IMAGES (installed_images), under the name
# of the variable that sets them.
cell_line() {
	local var="$1" images="$2" img tag tags="" detail=""
	shift 2
	for img in "$@"; do
		tag="$(printf '%s\n' "$images" |
			awk -v p="$IMAGE_REGISTRY/$img:" 'index($0, p) == 1 { print substr($0, length(p) + 1) }' | sort -u | tr '\n' '+')"
		tag="${tag%+}"
		tag="${tag:-none}"
		detail+="${detail:+ }$img:$tag"
		case ",$tags," in
		*",$tag,"*) ;;
		*) tags+="${tags:+,}$tag" ;;
		esac
	done
	printf 'MP-CELL %s=%s (installed: %s)\n' "$var" "$tags" "$detail"
}

# print_cell — which images this cluster runs, read back from the workloads and
# not from this run's environment, so the log of a `verify` names the skew
# cell it measured.
print_cell() {
	local images
	images="$(installed_images)"
	cell_line MP_TAG_AGENT "$images" "${AGENT_GROUP[@]}"
	cell_line MP_TAG_REGISTRAR "$images" "${REGISTRAR_GROUP[@]}"
}

# await_converged — before any leg: wait until every workload answers on one
# HTTP spelling that works with and without proposal 043 (its HTTP port by
# number; for mp-nine the mesh port, because its port number is the one A10 is
# about). It asserts nothing: a workload that never answers is reported and
# left to the legs, which say what is wrong with it. What it buys is that a
# leg run alone (MP_LEGS) or first does not need a registration-sized timeout.
await_converged() {
	log "waiting for every workload to answer once through the mesh"
	local w name authority want
	for w in "${WORKLOADS[@]}"; do
		name="$(mesh_name "$w")"
		authority="$name:$HTTP_PORT"
		want="$(marker "$w" "$HTTP_PORT")"
		if [ "$w" = "mp-nine" ]; then
			# Its one HTTP port has the number the others use for raw TCP.
			authority="$name:$TCP_PORT"
			want="$(marker "$w" "$TCP_PORT")"
		fi
		await_answer "$w" "http://$authority/" "$want" http_tokens client "http://$authority/" ""
	done
	# The one with no HTTP port: raw TCP on its declared port.
	name="$(mesh_name "$TCP_ONLY")"
	await_answer "$TCP_ONLY" "raw TCP to $name:$TCP_PORT" "$(tcp_marker "$TCP_ONLY")" tcp_tokens "$name:$TCP_PORT" line
}

# await_answer WORKLOAD WHERE WANT FN ARG... — poll FN ARG... <n> (http_tokens,
# tcp_tokens) until one whole batch answers WANT, or say that it never did.
await_answer() {
	local w="$1" where="$2" want="$3" deadline replies
	shift 3
	deadline=$((SECONDS + $(cap_timeout 240)))
	while true; do
		replies="$("$@" "$AWAIT_PROBES")"
		if [ "$(count_token "$replies" "$want")" -eq "$AWAIT_PROBES" ]; then
			ok "$w answers ($want)"
			return 0
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			note "$w did not answer '$want' on $where (last batch: $(histogram "$replies")); going on, the legs will say why"
			return 0
		fi
		sleep 3
	done
}

leg_selected() {
	case " $MP_LEGS " in
	*" $1 "*) return 0 ;;
	*) return 1 ;;
	esac
}

# run_leg ID — run leg_<ID> and record PASS, FAIL, SKIP or INFO (a leg that
# only records) with a detail.
run_leg() {
	local id="$1" i detail=""
	CURRENT_LEG="$id"
	LEG_CHECKS=0
	LEG_FAILS=()
	LEG_SKIP=""
	LEG_INFO=""
	"leg_$id"
	RES_ID+=("$id")
	if [ -n "$LEG_SKIP" ]; then
		RES_STATUS+=(SKIP)
		RES_DETAIL+=("$LEG_SKIP")
	elif [ -n "$LEG_INFO" ]; then
		RES_STATUS+=(INFO)
		RES_DETAIL+=("$LEG_INFO")
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
	for d in "${WORKLOADS[@]}" "$TCP_ONLY" "${CLIENTS[@]}"; do
		kc -n "$TEST_NS" get deploy "$d" >/dev/null 2>&1 ||
			die "'$d' is not deployed in '$CLUSTER': run '$0 up' first"
	done
	BACKEND="$(live_backend)"
	print_cell
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
