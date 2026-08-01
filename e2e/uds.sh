#!/usr/bin/env bash
# Single-cluster kind e2e for proposal 034 (UDS delivery to pods), Phases 1 + 1b.
#
# Proves that a workload which serves ONLY on a Unix domain socket — no TCP port
# bound at all — joins the mesh and is reachable by name from another mesh pod,
# through both ways of declaring the socket, and that a mis-declared socket
# degrades the one affected service and nothing else.
#
# The workload is e2e/udsecho (built here as ghcr.io/bpalermo/aether/udsecho:latest):
# it listens on a socket in an emptyDir and never calls listen(2) on a TCP port.
# That is what makes every 200 below load-bearing — a delivery that fell back to
# TCP loopback would find nothing listening, fail the delegated-liveness probe,
# and leave the endpoint unpromoted (proposal 034's failure semantics).
#
# Assertions (verify):
#   a. annotation path  — endpoint.aether.io/uds-socket on the pod              -> 200
#   b. policy path      — an EndpointPolicy on the service, no annotation       -> 200
#   c. precedence       — a policy naming a volume uds-echo does NOT mount does
#                         not disturb it: the pod annotation wins               -> 200
#   d. admission        — the controller webhook rejects a socket over the
#                         AF_UNIX budget, and the CRD rejects a non-Service target
#   e. drift / fallback — a policy on a TCP-only service (whose pods have no
#                         socket volume) unpromotes THAT service and nothing
#                         else (no CDS NACK, no snapshot poisoning), and the
#                         service recovers when the policy is deleted
#
# Usage: e2e/uds.sh {up|test|verify|down}   (bare = up + verify)
#
# Prereqs: kind, docker, kubectl, helm, bazel (for the image build; CI sets
# UDS_SKIP_BUILD=1 and pre-loads the images from the nightly build artifact).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER="${UDS_CLUSTER:-uds}"
CTX="kind-$CLUSTER"
NS="aether-system"
TEST_NS="aether-test"
MESH_DOMAIN="aether.internal"
# The pod-local outbound listener port every mesh client dials (post-030).
OUTBOUND_PORT="18081"
GWAPI_VERSION="v1.6.1"
IMAGES=(agent mesh-dns cni-install registrar controller udsecho)
# The declared socket, as "<volume>/<file>". SHORT by necessity: the resolved
# host path is <kubelet-pods-dir>/<36-byte UID>/volumes/kubernetes.io~empty-dir/
# + this value, and the whole thing must fit an AF_UNIX sun_path (107 bytes),
# which leaves ~15 characters. "s/a.sock" is 8.
SOCKET="s/a.sock"

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
die() {
	printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2
	dump_state
	exit 1
}

kc() { kubectl --context "$CTX" "$@"; }

# Every failure here is one of "the name did not resolve", "the endpoint was
# never promoted", or "the policy did not reach the agent" — dump all three
# rather than leave the next red run to a bisect (the #590 principle).
dump_state() {
	kubectl config get-contexts "$CTX" >/dev/null 2>&1 || return 0
	printf '\033[1;33m  -- pods --\033[0m\n' >&2
	kc get pods -A -o wide 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- endpointpolicies --\033[0m\n' >&2
	kc get endpointpolicies -A -o yaml 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- mesh services (mesh-DNS answers their ClusterIPs) --\033[0m\n' >&2
	kc -n "$TEST_NS" get svc 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- agent log (uds resolution + policy projection) --\033[0m\n' >&2
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
	# CI builds the images itself (bazel image_load via RBE) and sets
	# UDS_SKIP_BUILD=1 so this is a no-op — they are already in the local docker
	# daemon under ghcr.io/bpalermo/aether/<img>:latest.
	if [ "${UDS_SKIP_BUILD:-0}" = "1" ]; then
		ok "skipping image build (UDS_SKIP_BUILD=1; images pre-built)"
		return
	fi
	log "building + loading aether images (incl. the udsecho test workload)"
	local t
	for t in //agent/cmd/agent //agent/cmd/mesh-dns //cni/cmd/cni-install \
		//registrar/cmd/registrar //controller/cmd/controller //e2e/udsecho; do
		bazel run "$t:image_load" >/dev/null 2>&1 || die "image build failed for $t"
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
		-e "s#POD_SUBNET#10.10.0.0/16#g" \
		-e "s#SVC_SUBNET#10.110.0.0/16#g" \
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

install_crds() {
	log "installing Gateway API CRDs"
	kc apply --server-side -f \
		"https://github.com/kubernetes-sigs/gateway-api/releases/download/${GWAPI_VERSION}/standard-install.yaml" >/dev/null
	ok "Gateway API CRDs installed"
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
	# The crds chart FIRST: the agent's EndpointPolicy reconciler is CRD-presence
	# gated at manager setup, so a CRD installed later would need an agent restart.
	log "installing the aether CRDs (MeshConfig, HTTPFilter, EdgeConfig, EndpointPolicy)"
	helm --kube-context "$CTX" upgrade --install aether-crds "$charts/crds" \
		-n "$NS" --create-namespace --wait --timeout 2m >/dev/null || die "crds chart install failed"
	kc wait --for=condition=Established crd/endpointpolicies.config.aether.io --timeout=60s >/dev/null ||
		die "the EndpointPolicy CRD never became Established"

	# SPIRE off (cleartext mesh inbound, #421) — this suite tests DELIVERY to the
	# app, which is orthogonal to the inbound transport. Everything else is chart
	# default: capture/redirect-all, mesh DNS, the kubernetes registry backend,
	# and proxy.udsWorkloads.enabled (the kubelet pod-volumes mount on the proxy
	# + the agent's default --kubelet-pods-dir).
	log "installing aether (SPIRE off, UDS delivery on by default)"
	# shellcheck disable=SC2046
	helm --kube-context "$CTX" upgrade --install aether "$charts/aether" \
		-n "$NS" --create-namespace \
		--set namespace.create=false \
		--set "meshDomain=$MESH_DOMAIN" \
		--set spire.enabled=false \
		--set edge.enabled=false \
		$(img agent agent) $(img agent.meshDnsDaemon mesh-dns) $(img cniInstall cni-install) \
		$(img registrar registrar) $(img controller controller) \
		--set proxy.image.pullPolicy=IfNotPresent \
		--timeout 5m >/dev/null || die "aether install failed"
	rm -rf "$(dirname "$charts")"

	kc -n "$NS" rollout status ds/aether-agent --timeout=240s >/dev/null || die "the agent DaemonSet never became Ready"
	kc -n "$NS" rollout status ds/aether-mesh-dns --timeout=180s >/dev/null || die "the mesh-DNS DaemonSet never became Ready"
	kc -n "$NS" rollout status deploy/aether-registrar --timeout=180s >/dev/null || die "the registrar never became Ready"
	# The admission assertions need the webhook actually serving: its
	# failurePolicy is Ignore, so a controller that is not up yet ADMITS the
	# invalid policies this suite expects it to reject.
	kc -n "$NS" rollout status deploy/aether-controller --timeout=180s >/dev/null || die "the controller never became Ready"
	ok "aether up"
}

# The three servers + the client. No Kubernetes Service objects are created on
# purpose: the registrar generates the selectorless mesh VIP Service (the name
# mesh-DNS answers, and the object an EndpointPolicy targetRef names) for every
# registered service, and it deliberately SKIPS a name a non-aether Service
# already owns — hand-writing one here would suppress the VIP and break
# resolution. The registry service name is the pod's ServiceAccount.
deploy_workloads() {
	log "deploying the UDS + TCP workloads and the client"
	kc create ns "$TEST_NS" >/dev/null 2>&1 || true
	kc apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: uds-echo, namespace: $TEST_NS}
---
# (a) Annotation path: the socket is declared in the pod template.
apiVersion: apps/v1
kind: Deployment
metadata: {name: uds-echo, namespace: $TEST_NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: uds-echo}}
  template:
    metadata:
      labels: {app: uds-echo, aether.io/managed: "true"}
      annotations:
        endpoint.aether.io/port: "8080"
        endpoint.aether.io/uds-socket: "$SOCKET"
    spec:
      serviceAccountName: uds-echo
      # fsGroup makes the emptyDir group-writable for the distroless nonroot
      # user, so the app can create the socket in it.
      securityContext: {fsGroup: 65532}
      containers:
        - name: app
          image: ghcr.io/bpalermo/aether/udsecho:latest
          imagePullPolicy: Never
          args: ["--socket=/s/a.sock", "--text=served-by-uds-echo"]
          volumeMounts: [{name: s, mountPath: /s}]
      volumes:
        - name: s
          emptyDir: {}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: uds-cr-echo, namespace: $TEST_NS}
---
# (b) Policy path: identical pod shape, NO uds-socket annotation. Delivery comes
# from the EndpointPolicy below.
apiVersion: apps/v1
kind: Deployment
metadata: {name: uds-cr-echo, namespace: $TEST_NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: uds-cr-echo}}
  template:
    metadata:
      labels: {app: uds-cr-echo, aether.io/managed: "true"}
      annotations:
        endpoint.aether.io/port: "8080"
    spec:
      serviceAccountName: uds-cr-echo
      securityContext: {fsGroup: 65532}
      containers:
        - name: app
          image: ghcr.io/bpalermo/aether/udsecho:latest
          imagePullPolicy: Never
          args: ["--socket=/s/a.sock", "--text=served-by-uds-cr-echo"]
          volumeMounts: [{name: s, mountPath: /s}]
      volumes:
        - name: s
          emptyDir: {}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: tcp-echo, namespace: $TEST_NS}
---
# (e) A plain TCP-serving service with NO socket volume at all: the drift target.
apiVersion: apps/v1
kind: Deployment
metadata: {name: tcp-echo, namespace: $TEST_NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: tcp-echo}}
  template:
    metadata:
      labels: {app: tcp-echo, aether.io/managed: "true"}
      annotations: {endpoint.aether.io/port: "8080"}
    spec:
      serviceAccountName: tcp-echo
      containers:
        - name: app
          image: hashicorp/http-echo:1.0
          args: ["-text=served-by-tcp-echo", "-listen=:8080"]
          ports: [{containerPort: 8080}]
---
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
      # Declare the upstreams so all three clusters are warm on this node
      # (demand-scoped distribution, proposal 004) instead of paying an ODCDS
      # round trip inside each assertion's poll.
      annotations: {config.aether.io/upstreams: "uds-echo.$TEST_NS,uds-cr-echo.$TEST_NS,tcp-echo.$TEST_NS"}
    spec:
      serviceAccountName: client
      containers:
        - name: curl
          image: curlimages/curl:8.11.1
          command: ["sleep", "infinity"]
YAML
	# (b) The service-scoped declaration for uds-cr-echo. targetRef names the mesh
	# Service the registrar generates for the service (same name as its
	# ServiceAccount, same namespace).
	kc apply -f - >/dev/null <<YAML
apiVersion: config.aether.io/v1
kind: EndpointPolicy
metadata: {name: uds-cr, namespace: $TEST_NS}
spec:
  targetRef: {kind: Service, name: uds-cr-echo}
  udsSocket: $SOCKET
YAML
	local d
	for d in uds-echo uds-cr-echo tcp-echo client; do
		kc -n "$TEST_NS" rollout status "deploy/$d" --timeout=180s >/dev/null ||
			die "workload '$d' never became Ready"
	done
	ok "workloads deployed (uds-echo, uds-cr-echo, tcp-echo, client)"
}

# --- data-path probes -------------------------------------------------------

mesh_code() {
	kc -n "$TEST_NS" exec deploy/client -c curl -- \
		curl -sS -o /dev/null -w '%{http_code}' --max-time 10 \
		"http://$1.$TEST_NS.$MESH_DOMAIN:$OUTBOUND_PORT/" 2>/dev/null || echo 000
}

mesh_body() {
	kc -n "$TEST_NS" exec deploy/client -c curl -- \
		curl -sS --max-time 10 "http://$1.$TEST_NS.$MESH_DOMAIN:$OUTBOUND_PORT/" 2>/dev/null || true
}

# await_code SERVICE WANT TIMEOUT — poll the service until it answers WANT
# ("!200" = anything but 200) and echo the last code seen. Everything here is
# eventually-consistent: a cold cluster costs one ODCDS round trip, and a health
# state change costs a probe cycle (5s interval, 2 failures to demote).
await_code() {
	local svc="$1" want="$2" timeout="$3" code=000 deadline
	deadline=$((SECONDS + timeout))
	while true; do
		code="$(mesh_code "$svc")"
		if [ "$want" = "!200" ]; then
			if [ "$code" != "200" ]; then
				printf '%s' "$code"
				return 0
			fi
		elif [ "$code" = "$want" ]; then
			printf '%s' "$code"
			return 0
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			printf '%s' "$code"
			return 1
		fi
		sleep 5
	done
}

# expect_rejected YAML — apply and require a NON-ZERO exit, echoing the error.
# The ValidatingWebhookConfiguration is failurePolicy: Ignore (a down webhook
# must never wedge writes), so an apply that lands before the controller's
# endpoint is reachable is ADMITTED. Retry, undoing any object that got through,
# so this asserts "the webhook rejects it" and not "the webhook was up".
expect_rejected() {
	local yaml="$1" out rc
	for _ in 1 2 3 4 5 6; do
		rc=0
		out="$(printf '%s\n' "$yaml" | kc apply -f - 2>&1)" || rc=$?
		if [ "$rc" -ne 0 ]; then
			printf '%s' "$out"
			return 0
		fi
		printf '%s\n' "$yaml" | kc delete -f - >/dev/null 2>&1 || true
		sleep 5
	done
	printf '%s' "$out"
	return 1
}

# --- assertions -------------------------------------------------------------

# a + b: both ways of declaring the socket deliver real traffic. uds-echo and
# uds-cr-echo bind NO TCP port, so a 200 can only have come over the pipe.
verify_delivery() {
	log "a. annotation path: uds-echo (endpoint.aether.io/uds-socket: $SOCKET)"
	local code body
	code="$(await_code uds-echo 200 180)" ||
		die "uds-echo answered $code, expected 200 — the pod serves ONLY on $SOCKET, so this means delivery never reached the socket (or the endpoint was never promoted)"
	body="$(mesh_body uds-echo)"
	case "$body" in
	*served-by-uds-echo*) ok "uds-echo served over its Unix socket (HTTP 200, body from the UDS app; it binds no TCP port)" ;;
	*) die "uds-echo answered 200 but the body was not the UDS app's: $body" ;;
	esac

	log "b. EndpointPolicy path: uds-cr-echo (no annotation; policy 'uds-cr' declares $SOCKET)"
	code="$(await_code uds-cr-echo 200 180)" ||
		die "uds-cr-echo answered $code, expected 200 — the service-scoped EndpointPolicy did not switch delivery to the socket"
	body="$(mesh_body uds-cr-echo)"
	case "$body" in
	*served-by-uds-cr-echo*) ok "uds-cr-echo served over the socket declared by its EndpointPolicy (HTTP 200)" ;;
	*) die "uds-cr-echo answered 200 but the body was not the UDS app's: $body" ;;
	esac
}

# c: the pod annotation wins over a policy. The policy names a volume uds-echo's
# pods do not mount, so if the policy won, delivery would break.
verify_precedence() {
	log "c. precedence: an EndpointPolicy for uds-echo naming a volume its pods do NOT mount"
	kc apply -f - >/dev/null <<YAML
apiVersion: config.aether.io/v1
kind: EndpointPolicy
metadata: {name: uds-echo-bogus, namespace: $TEST_NS}
spec:
  targetRef: {kind: Service, name: uds-echo}
  udsSocket: bogus/x.sock
YAML
	# Give the agent a beat to project the policy and regenerate delivery clusters
	# (it pushes immediately on change), then require sustained success.
	sleep 15
	local code check
	for check in 1 2 3; do
		code="$(await_code uds-echo 200 60)" ||
			die "uds-echo answered $code on check $check after an EndpointPolicy pointed at 'bogus/x.sock' — the pod annotation must win over the policy"
	done
	ok "uds-echo keeps serving (3/3 checks): the pod annotation wins over the service-scoped policy"
	kc -n "$TEST_NS" delete endpointpolicy uds-echo-bogus >/dev/null
	ok "bogus precedence policy removed"
}

# d: admission. The budget check belongs to the controller webhook (it runs the
# agent's own resolver with a worst-case pod UID); the target-kind check is in
# the CRD schema itself.
verify_admission() {
	log "d. admission: an over-budget socket must be REJECTED at apply time"
	local out
	if out="$(expect_rejected "apiVersion: config.aether.io/v1
kind: EndpointPolicy
metadata: {name: uds-too-long, namespace: $TEST_NS}
spec:
  targetRef: {kind: Service, name: uds-echo}
  udsSocket: waytoolongvolumenameforanafunixpath/socket.sock")"; then
		case "$out" in
		*AF_UNIX*) ok "over-budget EndpointPolicy rejected, and the message names the limit: $(printf '%s' "$out" | tr '\n' ' ')" ;;
		*) die "the over-budget EndpointPolicy was rejected but the error never mentions the AF_UNIX limit: $out" ;;
		esac
	else
		die "an EndpointPolicy whose socket path overflows the AF_UNIX budget was ADMITTED (webhook not in the path?): $out"
	fi

	log "d. admission: a non-Service targetRef must be REJECTED"
	if out="$(expect_rejected "apiVersion: config.aether.io/v1
kind: EndpointPolicy
metadata: {name: uds-bad-target, namespace: $TEST_NS}
spec:
  targetRef: {kind: ConfigMap, name: uds-echo}
  udsSocket: $SOCKET")"; then
		ok "EndpointPolicy with targetRef.kind=ConfigMap rejected: $(printf '%s' "$out" | tr '\n' ' ')"
	else
		die "an EndpointPolicy targeting a ConfigMap was ADMITTED: $out"
	fi
}

# e: drift. A policy on a service whose pods carry no socket volume degrades that
# service only — the endpoint stays unpromoted (safe-degraded, never a blackhole)
# and every other service keeps serving, which is what "an unusable socket is not
# a CDS NACK" means in practice.
verify_drift() {
	log "e. drift: baseline — tcp-echo serves over TCP loopback"
	local code
	code="$(await_code tcp-echo 200 180)" ||
		die "tcp-echo answered $code before any policy was applied, expected 200"
	ok "tcp-echo serves (HTTP 200) with plain TCP delivery"

	log "e. drift: an EndpointPolicy for tcp-echo, whose pods mount no socket volume"
	kc apply -f - >/dev/null <<YAML
apiVersion: config.aether.io/v1
kind: EndpointPolicy
metadata: {name: tcp-echo-drift, namespace: $TEST_NS}
spec:
  targetRef: {kind: Service, name: tcp-echo}
  udsSocket: $SOCKET
YAML
	code="$(await_code tcp-echo '!200' 150)" ||
		die "tcp-echo still answers 200 after its delivery was pointed at a socket that does not exist — delivery should fail the liveness probe and the endpoint should stay unpromoted"
	ok "tcp-echo stopped serving ($code): the endpoint is unpromoted, exactly like an app that never bound its port — not blackholed"

	# The point of the assertion: the unusable pipe address poisoned nothing.
	log "e. drift: the other services must be unaffected (no CDS NACK, no snapshot poisoning)"
	code="$(await_code uds-echo 200 90)" ||
		die "uds-echo answered $code while a BAD policy existed for tcp-echo — one unusable socket must not affect another service's clusters"
	code="$(await_code uds-cr-echo 200 90)" ||
		die "uds-cr-echo answered $code while a BAD policy existed for tcp-echo — one unusable socket must not affect another service's clusters"
	ok "uds-echo and uds-cr-echo keep serving: the bad policy affected only its own service"

	log "e. drift: deleting the policy must restore tcp-echo"
	kc -n "$TEST_NS" delete endpointpolicy tcp-echo-drift >/dev/null
	code="$(await_code tcp-echo 200 150)" ||
		die "tcp-echo answered $code after the bad policy was deleted, expected 200 — delivery should revert to the TCP port"
	ok "tcp-echo recovered (HTTP 200) once the policy was removed"
}

verify() {
	verify_delivery
	verify_precedence
	verify_admission
	verify_drift
	log "all proposal 034 assertions passed (annotation, EndpointPolicy, precedence, admission, drift+recovery)"
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
	install_crds
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
