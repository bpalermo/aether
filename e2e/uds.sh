#!/usr/bin/env bash
# Single-cluster kind e2e for proposal 034 (UDS delivery to pods), Phases 1 + 1b,
# on the csi.aether.io carrier (proposal 039 Phase 2: the ONLY carrier; the
# emptyDir one is gone).
#
# Proves that a workload which serves ONLY on a Unix domain socket — no TCP port
# bound at all — joins the mesh and is reachable by name from another mesh pod,
# through both ways of declaring the socket, and that a mis-declared socket
# degrades the one affected service and nothing else.
#
# The workload is e2e/udsecho (built here as <IMAGE_REGISTRY>/udsecho:latest):
# it listens on a socket in its inline `csi: {driver: csi.aether.io}` volume
# (securityContext.fsGroup set, as the carrier requires) and never calls
# listen(2) on a TCP port.
# That is what makes every 200 below load-bearing — a delivery that fell back to
# TCP loopback would find nothing listening, fail the delegated-liveness probe,
# and leave the endpoint unpromoted (proposal 034's failure semantics).
#
# Assertions (verify):
#   a. annotation path  — endpoint.aether.io/uds-socket on the pod              -> 200
#   b. policy path      — an EndpointPolicy on the service, no annotation       -> 200
#   c. precedence       — a policy naming a volume uds-echo does NOT mount does
#                         not disturb it: the pod annotation wins               -> 200
#   d. admission        — the controller webhook rejects a socket file over the
#                         54-byte csi budget, and the CRD rejects a non-Service
#                         target
#   e. drift / fallback — a policy on a TCP-serving service whose pods mount a
#                         csi.aether.io volume but bind no socket in it
#                         unpromotes THAT service and nothing else (no CDS NACK,
#                         no snapshot poisoning), and the service recovers when
#                         the policy is deleted
#   f. CNI telemetry    — the CNI plugin exports nothing itself (#1166); it
#                         reports its capture-divert outcome to the agent, which
#                         exports aether_cni_operations_total. After a fresh
#                         managed-pod ADD the agent's capture_divert success
#                         count has grown, the error series is absent, and the
#                         netconf carries no otlp_endpoint.
#   g. cut-over admission — a pod declaring its socket on an emptyDir (the removed
#                         carrier) is DENIED by the controller's pod webhook, and
#                         the denial names the fix (csi.aether.io + fsGroup)
#   h. resolve failures — the agent counts, per reason, what it could not resolve:
#                         an EndpointPolicy onto pods whose volume is an emptyDir
#                         (admitted: no annotation) -> reason="not_csi", and the
#                         service stays unpromoted; a policy naming a volume the
#                         pods do not declare -> reason="volume_not_declared",
#                         and that TCP service keeps serving. Read from the
#                         agent's Prometheus endpoint and its one log line per
#                         pod per reason.
#
# Usage: e2e/uds.sh {up|test|verify|down}   (bare = up + verify)
#
# Prereqs: kind, docker, kubectl, helm, bazel (for the image build; CI sets
# UDS_SKIP_BUILD=1 and pre-loads the images from the nightly build artifact).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# The pinned kind binary / node image / kubectl (#1251): one Kubernetes for every
# e2e surface, local and CI alike.
# shellcheck source=e2e/kind-version.sh
. "$REPO_ROOT/e2e/kind-version.sh"
# <registry>/<namespace> every aether image is tagged under, from the single
# setting in bazel/registry/registry.bzl (proposal 040) -- never a literal.
IMAGE_REGISTRY="$("$REPO_ROOT/scripts/image-registry.sh" prefix)"
CLUSTER="${UDS_CLUSTER:-uds}"
CTX="kind-$CLUSTER"
NS="aether-system"
TEST_NS="aether-test"
MESH_DOMAIN="aether.internal"
# The pod-local outbound listener port every mesh client dials (post-030).
OUTBOUND_PORT="18081"
# The pinned Gateway API release (#1583): one for every e2e surface.
# shellcheck source=e2e/gateway-api-version.sh
. "$REPO_ROOT/e2e/gateway-api-version.sh"
IMAGES=(agent mesh-dns proxy-supervisor cni-install registrar controller uds-csi udsecho)
# The declared socket, as "<volume>/<file>". The resolved host path is
# /run/aether/uds/<36-byte pod UID>/<file>, so <file> has a 54-byte budget (the
# volume name costs nothing).
SOCKET="s/a.sock"
# The csi.aether.io carrier requires a pod fsGroup (the per-pod tmpfs is
# root:<fsGroup> 2770); 65532 is distroless nonroot, udsecho's user.
FS_GROUP=65532
# The single kind node (kind-cluster.yaml): where containerd execs the plugin.
NODE="${CLUSTER}-control-plane"

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
	printf '\033[1;33m  -- CNI plugin log: capture divert + ADD report (assertion f) --\033[0m\n' >&2
	docker exec "$NODE" sh -c 'grep -hE "divert|ADD outcome|ReportAddResult" /var/log/aether-cni/plugin.log | tail -5' 2>&1 |
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
	# daemon under <IMAGE_REGISTRY>/<img>:latest.
	if [ "${UDS_SKIP_BUILD:-0}" = "1" ]; then
		ok "skipping image build (UDS_SKIP_BUILD=1; images pre-built)"
		return
	fi
	log "building + loading aether images (incl. the udsecho test workload)"
	local t
	for t in //agent/cmd/agent //agent/cmd/mesh-dns //agent/cmd/proxy-supervisor \
		//cni/cmd/cni-install //registrar/cmd/registrar //controller/cmd/controller //agent/cmd/uds-csi \
		//e2e/udsecho; do
		bazel run "$t:image_load" >/dev/null 2>&1 || die "image build failed for $t"
	done
	ok "images built"
}

create_cluster() {
	# SIGPIPE rule (#1121, e2e/README.md): this script runs under pipefail, so
	# no pipeline may end in a reader that exits before its writer is done
	# (`head`, `grep -q`/`-m`, `awk '...; exit'`): the writer dies of SIGPIPE
	# and the pipeline fails with 141 at a random point. Read to EOF instead
	# (`grep -c ... >/dev/null`, `sed -n '1p'`).
	if kind get clusters 2>/dev/null | grep -cx "$CLUSTER" >/dev/null; then
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
	kind_require_binary || die "kind binary does not match e2e/kind-version.sh (see above)"
	kind create cluster --image "$KIND_NODE_IMAGE" --config "$cfg" --wait 60s >/dev/null
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
	img() { echo "--set $1.image.repository=${IMAGE_REGISTRY}/$2 --set $1.image.tag=latest --set $1.image.digest= --set $1.image.pullPolicy=Never"; }
	# The crds chart FIRST: the agent's EndpointPolicy reconciler is CRD-presence
	# gated at manager setup, so a CRD installed later would need an agent restart.
	log "installing the aether CRDs (MeshConfig, HTTPFilter, EdgeConfig, EndpointPolicy)"
	helm --kube-context "$CTX" upgrade --install aether-crds "$charts/crds" \
		-n "$NS" --create-namespace --wait --timeout 2m >/dev/null || die "crds chart install failed"
	kc wait --for=condition=Established crd/endpointpolicies.config.aether.io --timeout=60s >/dev/null ||
		die "the EndpointPolicy CRD never became Established"

	# SPIRE off (cleartext mesh inbound, #421) — this suite tests DELIVERY to the
	# app, which is orthogonal to the inbound transport. otel.enabled with NO
	# endpoint: every component keeps its Prometheus exporter and pushes nothing
	# (assertion h reads the agent's resolve-failure counter there). Everything
	# else is chart default: capture/redirect-all, mesh DNS, the kubernetes
	# registry backend, and udsCsi.enabled (the csi.aether.io plugin, the agent's
	# --uds-csi-root, the proxy's read-only mount of the CSI root).
	log "installing aether (SPIRE off, UDS delivery on the csi.aether.io carrier by default)"
	# shellcheck disable=SC2046
	helm --kube-context "$CTX" upgrade --install aether "$charts/aether" \
		-n "$NS" --create-namespace \
		--set namespace.create=false \
		--set "meshDomain=$MESH_DOMAIN" \
		--set spire.enabled=false \
		--set edge.enabled=false \
		--set otel.enabled=true \
		$(img agent agent) $(img agent.meshDnsDaemon mesh-dns) \
		$(img proxy.supervisor proxy-supervisor) $(img cniInstall cni-install) \
		$(img registrar registrar) $(img controller controller) $(img udsCsi uds-csi) \
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
	kc -n "$NS" rollout status ds/aether-uds-csi --timeout=180s >/dev/null || die "the csi.aether.io plugin DaemonSet never became Ready"
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
      # Required by csi.aether.io: the per-pod tmpfs is root:<fsGroup> 2770,
      # which is how the distroless nonroot user creates the socket in it.
      securityContext: {fsGroup: $FS_GROUP}
      containers:
        - name: app
          image: ${IMAGE_REGISTRY}/udsecho:latest
          imagePullPolicy: Never
          args: ["--socket=/s/a.sock", "--text=served-by-uds-echo"]
          volumeMounts: [{name: s, mountPath: /s}]
      volumes:
        - name: s
          csi: {driver: csi.aether.io}
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
      securityContext: {fsGroup: $FS_GROUP}
      containers:
        - name: app
          image: ${IMAGE_REGISTRY}/udsecho:latest
          imagePullPolicy: Never
          args: ["--socket=/s/a.sock", "--text=served-by-uds-cr-echo"]
          volumeMounts: [{name: s, mountPath: /s}]
      volumes:
        - name: s
          csi: {driver: csi.aether.io}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: tcp-echo, namespace: $TEST_NS}
---
# (e) A plain TCP-serving service: the drift target. It mounts a csi.aether.io
# volume "s" it never binds a socket in, so a policy naming s/a.sock RESOLVES
# (to a path nothing listens on) — the drift that reaches the data plane. A
# policy naming a volume it does not declare at all is (h)'s
# volume_not_declared: it never resolves, and the service keeps TCP.
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
      securityContext: {fsGroup: $FS_GROUP}
      containers:
        - name: app
          image: hashicorp/http-echo:1.0@sha256:fcb75f691c8b0414d670ae570240cbf95502cc18a9ba57e982ecac589760a186
          args: ["-text=served-by-tcp-echo", "-listen=:8080"]
          ports: [{containerPort: 8080}]
          volumeMounts: [{name: s, mountPath: /s}]
      volumes:
        - name: s
          csi: {driver: csi.aether.io}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: legacy-echo, namespace: $TEST_NS}
---
# (h) A workload still on the REMOVED carrier: its socket in an emptyDir, and
# declared by an EndpointPolicy (below), not an annotation — so the pod webhook
# has nothing to refuse and it is admitted, exactly like a pre-039 workload
# that was running when the chart was upgraded. The agent must refuse to
# deliver to it (reason="not_csi") and it must stay unpromoted.
apiVersion: apps/v1
kind: Deployment
metadata: {name: legacy-echo, namespace: $TEST_NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: legacy-echo}}
  template:
    metadata:
      labels: {app: legacy-echo, aether.io/managed: "true"}
      annotations: {endpoint.aether.io/port: "8080"}
    spec:
      serviceAccountName: legacy-echo
      securityContext: {fsGroup: $FS_GROUP}
      containers:
        - name: app
          image: ${IMAGE_REGISTRY}/udsecho:latest
          imagePullPolicy: Never
          args: ["--socket=/s/a.sock", "--text=served-by-legacy-echo"]
          volumeMounts: [{name: s, mountPath: /s}]
      volumes:
        - name: s
          emptyDir: {}
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
      annotations: {config.aether.io/upstreams: "uds-echo.$TEST_NS,uds-cr-echo.$TEST_NS,tcp-echo.$TEST_NS,legacy-echo.$TEST_NS"}
    spec:
      serviceAccountName: client
      containers:
        - name: curl
          image: curlimages/curl:8.22.0@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777
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
---
# (h) The same service-scoped declaration for the emptyDir workload.
apiVersion: config.aether.io/v1
kind: EndpointPolicy
metadata: {name: legacy, namespace: $TEST_NS}
spec:
  targetRef: {kind: Service, name: legacy-echo}
  udsSocket: $SOCKET
YAML
	local d
	for d in uds-echo uds-cr-echo tcp-echo legacy-echo client; do
		kc -n "$TEST_NS" rollout status "deploy/$d" --timeout=180s >/dev/null ||
			die "workload '$d' never became Ready"
	done
	ok "workloads deployed (uds-echo, uds-cr-echo, tcp-echo, legacy-echo, client)"
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
	log "d. admission: a socket file over the 54-byte budget must be REJECTED at apply time"
	local out
	# 55 bytes of file name: one over /run/aether/uds/<36-byte uid>/ + 54 = 107.
	if out="$(expect_rejected "apiVersion: config.aether.io/v1
kind: EndpointPolicy
metadata: {name: uds-too-long, namespace: $TEST_NS}
spec:
  targetRef: {kind: Service, name: uds-echo}
  udsSocket: s/$(printf 'f%.0s' $(seq 1 55))")"; then
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

	log "e. drift: an EndpointPolicy for tcp-echo, whose csi.aether.io volume holds no socket"
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

# cni_divert_count RESULT — aether_cni_operations_total{aether_cni_operation=
# "capture_divert",aether_cni_result=RESULT} summed over every agent pod's
# Prometheus exporter. Empty when no agent exports that series.
cni_divert_count() {
	local result="$1" pod metrics total="" v
	for pod in $(kc -n "$NS" get pods -l app.kubernetes.io/component=agent -o jsonpath='{.items[*].metadata.name}'); do
		metrics="$(kc get --raw "/api/v1/namespaces/$NS/pods/$pod:8080/proxy/metrics" 2>/dev/null)" || continue
		v="$(awk -v r="aether_cni_result=\"$result\"" '$1 ~ /^aether_cni_operations_total\{/ && index($1, "aether_cni_operation=\"capture_divert\"") && index($1, r) { s += $2; n++ } END { if (n) printf "%d", s }' <<<"$metrics")"
		[ -n "$v" ] && total=$((${total:-0} + v))
	done
	printf '%s' "$total"
}

# (f) #1166: the CNI plugin links no OTel SDK and exports nothing; what only it
# can see (the capture divert it installs AFTER AddPod answers) reaches
# Prometheus through the agent, via ReportAddResult. Three checks:
#   1. the chained netconf has no otlp_endpoint (cni-install stopped writing it);
#   2. a fresh managed-pod ADD grows the agent's
#      aether_cni_operations_total{capture_divert,success} — the soak gate's
#      existence proof, now exported by the agent;
#   3. the error series is absent: no pod in this suite started UNCAPTURED.
verify_cni_telemetry() {
	log "(f) the CNI plugin's capture-divert outcome reaches Prometheus through the agent (#1166)"
	local conflists
	conflists="$(docker exec "$NODE" sh -c 'cat /etc/cni/net.d/*.conflist')" || die "could not read the node's CNI conflists"
	grep -q '"type": *"aether-cni"' <<<"$conflists" || die "no chained aether-cni entry in the node's conflists — the check below would pass vacuously"
	# A here-string, not `docker exec | grep -q`: under pipefail grep -q's early
	# exit SIGPIPEs the producer and a MATCH reads as a miss.
	if grep -q '"otlp_endpoint"' <<<"$conflists"; then
		die "the chained aether netconf still carries otlp_endpoint — cni-install must not write it any more (#1166)"
	fi
	ok "netconf carries no otlp_endpoint"

	local before after deadline
	before="$(cni_divert_count success)"
	# A fresh managed ADD (and its DEL) now, so the count under test moved after
	# the read above and cannot be an artefact of an earlier install.
	kc -n "$TEST_NS" run cni-telemetry-probe --image=registry.k8s.io/pause:3.10@sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a \
		--restart=Never --labels='app=cni-telemetry-probe,aether.io/managed=true' >/dev/null
	kc -n "$TEST_NS" wait --for=condition=Ready pod/cni-telemetry-probe --timeout=120s >/dev/null ||
		die "the probe pod never became Ready"
	kc -n "$TEST_NS" delete pod cni-telemetry-probe --wait=true --timeout=60s >/dev/null || true

	deadline=$((SECONDS + 60))
	while true; do
		after="$(cni_divert_count success)"
		[ -n "$after" ] && [ "$after" -gt "${before:-0}" ] && break
		[ "$SECONDS" -lt "$deadline" ] ||
			die "aether_cni_operations_total{capture_divert,success} did not grow within 60s of a managed pod ADD (before='${before:-<no series>}' after='${after:-<no series>}') — the plugin's ReportAddResult did not reach the agent"
		sleep 3
	done
	ok "the agent counted the probe pod's capture divert (success: ${before:-<no series>} -> $after)"

	local errors
	errors="$(cni_divert_count error)"
	[ -z "$errors" ] || die "aether_cni_operations_total{capture_divert,error} = $errors — a pod in this suite started UNCAPTURED"
	ok "no capture_divert error series"
}

# (g) The cut-over's front door: a pod that still declares its socket on an
# emptyDir is refused at admission, with a message that names the fix — not
# admitted to run Ready-but-undelivered. A bare Pod, not a Deployment: a
# Deployment's apply succeeds and only its ReplicaSet's pod creations fail.
verify_cutover_admission() {
	log "g. cut-over admission: a pod with an emptyDir socket carrier must be DENIED"
	local out
	if out="$(expect_rejected "apiVersion: v1
kind: Pod
metadata:
  name: legacy-carrier
  namespace: $TEST_NS
  labels: {aether.io/managed: \"true\"}
  annotations:
    endpoint.aether.io/port: \"8080\"
    endpoint.aether.io/uds-socket: \"$SOCKET\"
spec:
  securityContext: {fsGroup: $FS_GROUP}
  containers:
    - name: app
      image: ${IMAGE_REGISTRY}/udsecho:latest
      imagePullPolicy: Never
      args: [\"--socket=/s/a.sock\"]
      volumeMounts: [{name: s, mountPath: /s}]
  volumes:
    - name: s
      emptyDir: {}")"; then
		case "$out" in
		*not_csi*"csi: {driver: csi.aether.io}"*fsGroup*)
			ok "emptyDir-carrier pod denied, naming the fix: $(printf '%s' "$out" | tr '\n' ' ')"
			;;
		*) die "the emptyDir-carrier pod was denied, but the message does not name the fix (not_csi, csi: {driver: csi.aether.io}, fsGroup): $out" ;;
		esac
	else
		die "a pod declaring its UDS socket on an emptyDir was ADMITTED (pod webhook not in the path, or not checking the carrier): $out"
	fi
}

# resolve_failures REASON — the agent's aether_agent_uds_resolve_failures_total
# for REASON, summed over every agent pod, read through the API server's pod
# proxy (the agent's Prometheus exporter on :8080). Empty when no agent exports
# the series at all.
resolve_failures() {
	local reason="$1" pod metrics total="" v
	for pod in $(kc -n "$NS" get pods -l app.kubernetes.io/component=agent -o jsonpath='{.items[*].metadata.name}'); do
		metrics="$(kc get --raw "/api/v1/namespaces/$NS/pods/$pod:8080/proxy/metrics" 2>/dev/null)" || continue
		v="$(awk -v r="reason=\"$reason\"" '$1 ~ /^aether_agent_uds_resolve_failures_total\{/ && index($1, r) { s += $2; n++ } END { if (n) printf "%d", s }' <<<"$metrics")"
		[ -n "$v" ] && total=$((${total:-0} + v))
	done
	printf '%s' "$total"
}

# await_failures REASON TIMEOUT — wait until the counter for REASON is >= 1.
await_failures() {
	local reason="$1" deadline=$((SECONDS + $2)) v
	while true; do
		v="$(resolve_failures "$reason")"
		if [ -n "$v" ] && [ "$v" -ge 1 ]; then
			printf '%s' "$v"
			return 0
		fi
		[ "$SECONDS" -lt "$deadline" ] || {
			printf '%s' "${v:-<no series>}"
			return 1
		}
		sleep 3
	done
}

# agent_log_has REGEX — grep EVERY agent pod's (JSON) log, not `logs ds/`, which
# reads one pod.
agent_log_has() {
	local pod
	for pod in $(kc -n "$NS" get pods -l app.kubernetes.io/component=agent -o jsonpath='{.items[*].metadata.name}'); do
		if grep -Eq -- "$1" <<<"$(kc -n "$NS" logs "$pod" -c agent 2>/dev/null)"; then
			return 0
		fi
	done
	return 1
}

# (h) What admission cannot see — a socket declared by an EndpointPolicy — the
# agent refuses at resolution, counted per reason, never silently.
verify_resolve_failures() {
	log "h. resolve failures: the counter is exported, seeded at zero for every reason"
	local v code
	v="$(resolve_failures volume_not_declared)"
	[ -n "$v" ] || die "no agent exports aether_agent_uds_resolve_failures_total{reason=\"volume_not_declared\"} — the counter must be seeded (a zero, not an absent series)"
	ok "aether_agent_uds_resolve_failures_total{reason=\"volume_not_declared\"} is exported (= $v before the drift below)"

	log "h. not_csi: legacy-echo (emptyDir socket, declared by EndpointPolicy 'legacy') is refused and stays unpromoted"
	v="$(await_failures not_csi 90)" ||
		die "aether_agent_uds_resolve_failures_total{reason=\"not_csi\"} is $v after 90s — the agent did not count the emptyDir-carrier workload"
	ok "resolve_failures{reason=\"not_csi\"} = $v"
	agent_log_has '"reason":"not_csi".*"pod":"legacy-echo' ||
		die "no agent log line carries reason not_csi for a legacy-echo pod"
	ok "the agent logged the refusal once for the pod (reason not_csi, pod legacy-echo-*)"
	code="$(mesh_code legacy-echo)"
	[ "$code" != "200" ] ||
		die "legacy-echo answered 200 — a workload on the removed emptyDir carrier must NOT be delivered"
	ok "legacy-echo is not delivered ($code): the endpoint stays unpromoted, the 034 failure semantics"

	log "h. volume_not_declared: a policy on tcp-echo naming a volume its pods do not declare"
	kc apply -f - >/dev/null <<YAML
apiVersion: config.aether.io/v1
kind: EndpointPolicy
metadata: {name: tcp-echo-undeclared, namespace: $TEST_NS}
spec:
  targetRef: {kind: Service, name: tcp-echo}
  udsSocket: nope/a.sock
YAML
	v="$(await_failures volume_not_declared 90)" ||
		die "aether_agent_uds_resolve_failures_total{reason=\"volume_not_declared\"} is $v after 90s"
	ok "resolve_failures{reason=\"volume_not_declared\"} = $v"
	# The policy never resolved, so delivery never left TCP: sustained 200s.
	local check
	for check in 1 2 3; do
		code="$(await_code tcp-echo 200 60)" ||
			die "tcp-echo answered $code on check $check — a policy naming an undeclared volume must leave TCP delivery in place"
	done
	ok "tcp-echo keeps serving over TCP (3/3): an unresolvable policy never reaches the data plane"
	kc -n "$TEST_NS" delete endpointpolicy tcp-echo-undeclared >/dev/null
}

verify() {
	verify_delivery
	verify_precedence
	verify_admission
	verify_drift
	verify_cni_telemetry
	verify_cutover_admission
	verify_resolve_failures
	log "all assertions passed (proposal 034 on the csi.aether.io carrier: annotation, EndpointPolicy, precedence, admission, drift+recovery; #1166: CNI capture divert via the agent; 039 Phase 2: emptyDir carrier denied, resolve failures counted)"
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
