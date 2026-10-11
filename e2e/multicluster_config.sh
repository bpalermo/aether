#!/usr/bin/env bash
# Two-cluster kind e2e for proposal 026 (multi-cluster config propagation,
# Option E/C): an HTTPRoute that exists in ONE cluster routes traffic in the
# OTHER.
#
# Two kind clusters (a = exporter, b = importer) share ONE etcd (a docker
# container on kind's network). aether runs on both with the etcd registry
# backend; only b's agents import (agent.importConfig=true).
#
#   cluster a: HTTPRoute + ServiceExport  ──(registrar config-export controller)──▶  shared etcd
#                                                                                      │
#   cluster b: agent --import-config  ◀──(registrar ListAllConfig reads same etcd)─────┘
#
# The HTTPRoute lives in cluster a ONLY: `/v2` -> echo-v2, everything else ->
# echo-v1, parented to the Service `echo`. Cluster a runs no workload at all.
# Cluster b runs echo, echo-v1, echo-v2 and a client, and has NO HTTPRoute, so
# any routing b's node proxy does between them was imported.
#
# What `verify` proves, in order, with the SAME probe from the SAME client:
#
#   C0  control   no ServiceExport in a   -> the store holds no projection for
#                                            demo/echo, and client(b) is answered
#                                            by `echo` itself on both paths
#   C1  export    ServiceExport in a      -> a's export controller writes the
#                                            projection under ITS OWN partition
#                                            of the shared store, naming both
#                                            backends
#   C2  import    (nothing changes in b)  -> client(b): /v2 is answered by
#                                            echo-v2 and / by echo-v1, 20 of 20
#   C3  withdraw  ServiceExport deleted   -> the projection is pruned and
#                                            client(b) is answered by `echo`
#                                            again
#
# C0 and C3 are assertions, not setup: they are the states in which the probe
# gives the other answer, reached from both directions on every run, so a C2
# that passes cannot be a probe that only ever says one thing. `verify` leaves
# the service unexported, so it can be run again.
#
# SPIRE is off (cleartext mesh): the suite is about which route table a proxy
# holds, not about the transport. The data path stays inside cluster b: the
# clusters share a registry but pod IPs do not route between them (that is
# e2e/multicluster_waypoint.sh, proposal 019).
#
# THE RED HALF. C0 and C3 show the probe answering differently on every run. To
# see the GATE fail, bring the importer up without the import and verify:
#
#   CONFIG_IMPORT=off e2e/multicluster_config.sh up && e2e/multicluster_config.sh verify
#
# C0 and C1 pass (the export side is untouched) and C2 fails with every probe
# still answered by 'parent': cluster b's proxy was never given the route.
#
# Usage:
#   e2e/multicluster_config.sh up        # build+load images, create clusters+etcd, install aether, deploy
#   e2e/multicluster_config.sh verify    # the assertions above
#   e2e/multicluster_config.sh test      # the same as verify
#   e2e/multicluster_config.sh down      # delete both clusters + the shared etcd
#   e2e/multicluster_config.sh           # up + verify (full run)
#
# Prereqs: kind, docker, kubectl, helm, bazel (for the image build; CI sets
# CONFIG_SKIP_BUILD=1 and pre-loads the images from the nightly build
# artifact). Two kind clusters are inotify-heavy: `up` raises fs.inotify.* when
# it can (passwordless sudo) and says so when it cannot.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# The pinned kind binary / node image / kubectl (#1251): one Kubernetes for every
# e2e surface, local and CI alike.
# shellcheck source=e2e/kind-version.sh
. "$REPO_ROOT/e2e/kind-version.sh"
# <registry>/<namespace> every aether image is tagged under, from the single
# setting in bazel/registry/registry.bzl (proposal 040) -- never a literal.
IMAGE_REGISTRY="$("$REPO_ROOT/scripts/image-registry.sh" prefix)"
CLUSTER_A="${CLUSTER_A:-a}"
CLUSTER_B="${CLUSTER_B:-b}"
ETCD_NAME="${ETCD_NAME:-aether-shared-etcd}"
# shellcheck source=e2e/etcd-image.sh
. "$REPO_ROOT/e2e/etcd-image.sh"
KIND_NET="kind"
REGION="local"
NS="aether-system"
# The namespace of the exported service, in both clusters: a projection is keyed
# by <namespace>/<service>, so the importer serves it for the same pair.
DEMO_NS="demo"
MESH_DOMAIN="aether.internal"
APP_PORT="8080"
# The pinned Gateway API release (#1583): one for every e2e surface.
# shellcheck source=e2e/gateway-api-version.sh
. "$REPO_ROOT/e2e/gateway-api-version.sh"
MCS_VERSION="v0.5.0"
IMAGES=(agent mesh-dns proxy-supervisor cni-install registrar controller uds-csi)
HTTP_ECHO_IMAGE="hashicorp/http-echo:1.0@sha256:fcb75f691c8b0414d670ae570240cbf95502cc18a9ba57e982ecac589760a186"
CURL_IMAGE="curlimages/curl:8.22.0@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777"
# The red half (see the header): "off" installs cluster b WITHOUT
# agent.importConfig, so C2 must fail. CI never sets it.
CONFIG_IMPORT="${CONFIG_IMPORT:-on}"
# How many probes each measured batch sends.
PROBES=20

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
die() {
	printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2
	dump_state
	exit 1
}

ka() { kubectl --context "kind-$CLUSTER_A" "$@"; }
kb() { kubectl --context "kind-$CLUSTER_B" "$@"; }

# The shared store's key for one cluster's projection of demo/echo:
# <own prefix>/config/<base64url(<ns>/<svc>), unpadded>.
config_prefix() { printf '/aether/v1/regions/%s/clusters/cluster-%s/config/' "$REGION" "$1"; }
config_key() { printf '%s' "$DEMO_NS/echo" | basenc --base64url -w0 | tr -d '='; }

# A red run is one of: the export never reached the store, the importer never
# read it, or the proxy holds the routes and sends the request elsewhere. Print
# what separates them rather than leave it to a second run (the #590 principle).
dump_state() {
	local c
	printf '\033[1;33m  -- shared store: config + export keys --\033[0m\n' >&2
	docker exec "$ETCD_NAME" etcdctl get --prefix /aether/v1/regions --keys-only 2>/dev/null |
		grep -E '/(config|exports)/' | sed 's/^/    /' >&2 || true
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		kubectl config get-contexts "kind-$c" >/dev/null 2>&1 || continue
		printf '\033[1;33m  -- cluster %s: pods --\033[0m\n' "$c" >&2
		kubectl --context "kind-$c" get pods -A -o wide 2>&1 | sed 's/^/    /' >&2 || true
		printf '\033[1;33m  -- cluster %s: httproutes / serviceexports --\033[0m\n' "$c" >&2
		kubectl --context "kind-$c" get httproutes,serviceexports -A 2>&1 | sed 's/^/    /' >&2 || true
		printf '\033[1;33m  -- cluster %s: registrar log (config export) --\033[0m\n' "$c" >&2
		kubectl --context "kind-$c" -n "$NS" logs -l app.kubernetes.io/component=registrar --tail=40 --prefix 2>&1 |
			sed 's/^/    /' >&2 || true
		printf '\033[1;33m  -- cluster %s: agent log (config import) --\033[0m\n' "$c" >&2
		kubectl --context "kind-$c" -n "$NS" logs -l app.kubernetes.io/component=agent --all-containers --tail=400 --prefix 2>&1 |
			grep -i 'import' | sed -n '1,40s/^/    /p' >&2 || true
	done
}

raise_inotify() {
	log "raising fs.inotify limits (two kind clusters are inotify-heavy)"
	if sudo -n true 2>/dev/null; then
		sudo sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=524288 >/dev/null 2>&1 &&
			ok "inotify limits raised" || echo "  (could not raise inotify limits)"
	else
		echo "  (no passwordless sudo; run: sudo sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=524288)"
	fi
}

build_images() {
	# CI builds the images itself (bazel image_load via RBE) and sets
	# CONFIG_SKIP_BUILD=1 so this step is a no-op: they are already in the local
	# docker daemon under <IMAGE_REGISTRY>/<img>:latest.
	if [ "${CONFIG_SKIP_BUILD:-0}" = "1" ]; then
		ok "skipping image build (CONFIG_SKIP_BUILD=1; images pre-built)"
		return
	fi
	# One target per image this script loads. `make load-all` is not that list:
	# it has never built the controller, so a machine that had not built it by
	# hand stopped at the image load below, with no message.
	log "building + loading aether images"
	local t
	for t in //agent/cmd/agent //agent/cmd/mesh-dns //agent/cmd/proxy-supervisor \
		//cni/cmd/cni-install //registrar/cmd/registrar //controller/cmd/controller //agent/cmd/uds-csi; do
		(cd "$REPO_ROOT" && bazel run "$t:image_load" >/dev/null 2>&1) || die "image build failed for $t"
	done
	ok "images built"
}

create_clusters() {
	local i=0
	# SIGPIPE rule (#1121, e2e/README.md): this script runs under pipefail, so
	# no pipeline may end in a reader that exits before its writer is done
	# (`head`, `grep -q`/`-m`, `awk '...; exit'`): the writer dies of SIGPIPE
	# and the pipeline fails with 141 at a random point. Read to EOF instead
	# (`grep -c ... >/dev/null`). The pods' own `sh` scripts run without
	# pipefail.
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		if kind get clusters 2>/dev/null | grep -cx "$c" >/dev/null; then
			ok "kind cluster '$c' already exists"
		else
			log "creating kind cluster '$c' from e2e/kind-cluster.yaml"
			# Render the base config per cluster: its name + distinct, non-overlapping
			# pod/service CIDRs (a=10.10/10.110, b=10.20/10.120) so cross-cluster
			# endpoints in the shared registry never collide on IP.
			local cfg
			cfg="$(mktemp)"
			sed -e "s/CLUSTER_NAME/$c/g" \
				-e "s#POD_SUBNET#10.$((10 + i * 10)).0.0/16#g" \
				-e "s#SVC_SUBNET#10.$((110 + i * 10)).0.0/16#g" \
				"$REPO_ROOT/e2e/kind-cluster.yaml" >"$cfg"
			kind_require_binary || die "kind binary does not match e2e/kind-version.sh (see above)"
			kind create cluster --image "$KIND_NODE_IMAGE" --config "$cfg" --wait 60s >/dev/null
			rm -f "$cfg"
			ok "cluster '$c' ready"
		fi
		i=$((i + 1))
	done
}

etcd_ip() { docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$ETCD_NAME"; }

start_etcd() {
	log "starting shared etcd on the '$KIND_NET' docker network"
	docker rm -f "$ETCD_NAME" >/dev/null 2>&1 || true
	docker run -d --name "$ETCD_NAME" --network "$KIND_NET" "$ETCD_IMAGE" \
		etcd --name s1 --data-dir /tmp/etcd \
		--listen-client-urls http://0.0.0.0:2379 --advertise-client-urls http://0.0.0.0:2379 >/dev/null ||
		die "could not start the shared etcd"
	# Wait on the observable, not on a clock.
	local deadline=$((SECONDS + 60))
	until docker exec "$ETCD_NAME" etcdctl endpoint health >/dev/null 2>&1; do
		[ "$SECONDS" -lt "$deadline" ] || die "the shared etcd never became healthy"
		sleep 2
	done
	local ip
	ip="$(etcd_ip)"
	[ -n "$ip" ] || die "could not determine the shared etcd IP"
	ok "shared etcd at http://$ip:2379 (reachable from both clusters' nodes)"
}

load_images() {
	log "loading images into both clusters"
	local c img
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		for img in "${IMAGES[@]}"; do
			kind load docker-image "${IMAGE_REGISTRY}/${img}:latest" --name "$c" >/dev/null 2>&1 ||
				die "could not load ${IMAGE_REGISTRY}/${img}:latest into '$c' (was it built?)"
		done
	done
	ok "images loaded into '$CLUSTER_A' and '$CLUSTER_B'"
}

# The CRDs go in BEFORE aether: the registrar's config-export controller and
# the agent's GAMMA reconciler look their kinds up when the manager is set up.
install_crds() {
	local gwapi="https://github.com/kubernetes-sigs/gateway-api/releases/download/${GWAPI_VERSION}/experimental-install.yaml"
	local se="https://raw.githubusercontent.com/kubernetes-sigs/mcs-api/${MCS_VERSION}/config/crd/multicluster.x-k8s.io_serviceexports.yaml"
	local si="https://raw.githubusercontent.com/kubernetes-sigs/mcs-api/${MCS_VERSION}/config/crd/multicluster.x-k8s.io_serviceimports.yaml"
	local c
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		log "installing Gateway API + MCS CRDs on '$c'"
		# server-side: the CRDs exceed the client annotation limit
		kubectl --context "kind-$c" apply --server-side -f "$gwapi" >/dev/null || die "Gateway API CRD install failed on '$c'"
		kubectl --context "kind-$c" apply -f "$se" >/dev/null || die "ServiceExport CRD install failed on '$c'"
		kubectl --context "kind-$c" apply -f "$si" >/dev/null || die "ServiceImport CRD install failed on '$c'"
		ok "CRDs installed on '$c'"
	done
}

# render the chart placeholders to a temp copy so the source tree stays clean.
chart_dir() {
	local out
	out="$(mktemp -d)"
	cp -r "$REPO_ROOT/charts" "$out/"
	sed -i -e 's/{GIT_COMMIT}/e2e/' -e 's/{STABLE_GIT_VERSION}/0.0.0-e2e/' "$out/charts/crds/Chart.yaml" "$out/charts/aether/Chart.yaml"
	echo "$out/charts"
}

install_aether() {
	# Declared and assigned separately (SC2155): `local x="$(cmd)"` is a single
	# `local` builtin whose exit status is the builtin's, not the substitution's,
	# so `set -e` cannot see etcd_ip fail and the install would proceed with
	# "http://:2379" and blame the chart.
	local etcd charts
	etcd="http://$(etcd_ip):2379"
	charts="$(chart_dir)"
	img() { echo "--set $1.image.repository=${IMAGE_REGISTRY}/$2 --set $1.image.tag=latest --set $1.image.digest= --set $1.image.pullPolicy=Never"; }
	# cluster a EXPORTS (registrar config-export controller); cluster b IMPORTS (agent --import-config).
	local c
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		local import="false"
		if [ "$c" = "$CLUSTER_B" ] && [ "$CONFIG_IMPORT" != "off" ]; then import="true"; fi
		log "installing aether on '$c' (cluster-$c, etcd backend → shared, import-config=$import)"
		helm --kube-context "kind-$c" upgrade --install aether-crds "$charts/crds" -n "$NS" --create-namespace --wait --timeout 2m >/dev/null ||
			die "crds chart install failed on '$c'"
		# clusterName is each cluster's partition of the shared store: two
		# clusters under one name would overwrite each other's projections.
		# shellcheck disable=SC2046
		helm --kube-context "kind-$c" upgrade --install aether "$charts/aether" -n "$NS" --create-namespace \
			--set namespace.create=false \
			--set "clusterName=cluster-$c" \
			--set "meshDomain=$MESH_DOMAIN" \
			--set spire.enabled=false \
			--set edge.enabled=false \
			--set "agent.importConfig=$import" \
			--set registrar.registryBackend=etcd \
			--set registrar.enableMCS=true \
			--set "registrar.region=$REGION" \
			--set "registrar.etcd.region=$REGION" \
			--set "registrar.etcd.endpoints[0]=$etcd" \
			$(img agent agent) $(img agent.meshDnsDaemon mesh-dns) $(img proxy.supervisor proxy-supervisor) $(img cniInstall cni-install) $(img registrar registrar) $(img controller controller) $(img udsCsi uds-csi) \
			--set proxy.image.pullPolicy=IfNotPresent \
			--timeout 5m >/dev/null || die "aether install failed on '$c'"
		# Every one of these is on the loop: a's registrar exports, b's registrar
		# serves the projections, b's agent imports them, b's mesh DNS resolves the
		# name the probe dials. An agent that is not Running is a failed bring-up
		# here, not a footnote (it was one until this suite asserted the import).
		kubectl --context "kind-$c" -n "$NS" rollout status deploy/aether-registrar --timeout=180s >/dev/null ||
			die "the registrar never became Ready on '$c'"
		kubectl --context "kind-$c" -n "$NS" rollout status deploy/aether-controller --timeout=180s >/dev/null ||
			die "the controller never became Ready on '$c'"
		kubectl --context "kind-$c" -n "$NS" rollout status ds/aether-agent --timeout=300s >/dev/null ||
			die "the agent DaemonSet never became Ready on '$c'"
		kubectl --context "kind-$c" -n "$NS" rollout status ds/aether-mesh-dns --timeout=180s >/dev/null ||
			die "the mesh-DNS DaemonSet never became Ready on '$c'"
		ok "aether up on '$c'"
	done
	rm -rf "$(dirname "$charts")"
}

# One HTTP workload in cluster b. Its ServiceAccount is its mesh service name;
# the body it answers with is how a probe tells the three apart.
deploy_echo() {
	local name="$1" text="$2"
	kb apply -f - >/dev/null <<YAML || die "workload $name apply failed"
apiVersion: v1
kind: ServiceAccount
metadata: {name: $name, namespace: $DEMO_NS}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: $name, namespace: $DEMO_NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: $name}}
  template:
    metadata:
      labels: {app: $name, aether.io/managed: "true"}
      annotations: {endpoint.aether.io/port: "$APP_PORT"}
    spec:
      serviceAccountName: $name
      containers:
        - name: echo
          image: $HTTP_ECHO_IMAGE
          args: ["-text=$text", "-listen=:$APP_PORT"]
          ports: [{containerPort: $APP_PORT}]
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: ["ALL"]}
YAML
}

deploy_workloads() {
	log "exporter ('$CLUSTER_A'): the Service and the HTTPRoute — and no ServiceExport yet"
	# The parent Service and the route are the whole of cluster a's side. The
	# ServiceExport is what `verify` applies and deletes: it is the switch under
	# test.
	ka apply -f - >/dev/null <<YAML || die "exporter fixtures apply failed"
apiVersion: v1
kind: Namespace
metadata: {name: $DEMO_NS}
---
apiVersion: v1
kind: Service
metadata: {name: echo, namespace: $DEMO_NS}
spec: {selector: {app: echo}, ports: [{port: $APP_PORT}]}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: echo-routes, namespace: $DEMO_NS}
spec:
  parentRefs: [{group: "", kind: Service, name: echo, port: $APP_PORT}]
  rules:
    - matches: [{path: {type: PathPrefix, value: /v2}}]
      backendRefs: [{name: echo-v2, port: $APP_PORT}]
    - matches: [{path: {type: PathPrefix, value: /}}]
      backendRefs: [{name: echo-v1, port: $APP_PORT}]
YAML
	ok "Service echo + HTTPRoute echo-routes applied on '$CLUSTER_A'"

	log "importer ('$CLUSTER_B'): echo, echo-v1, echo-v2 and the client — and no HTTPRoute"
	kb create ns "$DEMO_NS" >/dev/null 2>&1 || true
	deploy_echo echo parent
	deploy_echo echo-v1 v1
	deploy_echo echo-v2 v2
	# The upstreams annotation puts all three services in the client node's
	# dependency set (demand-scoped distribution, proposal 004): a route whose
	# backend cluster the node was never sent looks exactly like a routing bug.
	kb apply -f - >/dev/null <<YAML || die "client apply failed"
apiVersion: v1
kind: ServiceAccount
metadata: {name: client, namespace: $DEMO_NS}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: client, namespace: $DEMO_NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: client}}
  template:
    metadata:
      labels: {app: client, aether.io/managed: "true"}
      annotations:
        config.aether.io/upstreams: "echo.$DEMO_NS,echo-v1.$DEMO_NS,echo-v2.$DEMO_NS"
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
	for d in echo echo-v1 echo-v2 client; do
		kb -n "$DEMO_NS" rollout status "deploy/$d" --timeout=180s >/dev/null ||
			die "workload '$d' never became Ready on '$CLUSTER_B'"
	done
	ok "workloads deployed on '$CLUSTER_B' (echo answers 'parent', echo-v1 'v1', echo-v2 'v2')"
}

# --- probes -------------------------------------------------------------------

# probe_batch PATH N — N requests from the client in cluster b to the mesh name
# of `echo`, path PATH, in ONE exec. Prints N lines: the body's first token
# ("parent" / "v1" / "v2"), or the HTTP status when the body is not one of them,
# or NOREPLY.
probe_batch() {
	local path="$1" n="$2"
	# SC2016: the single quotes are the point — the body is evaluated by the
	# POD's shell; $url/$n/$i/$body are its variables.
	# shellcheck disable=SC2016
	kb -n "$DEMO_NS" exec deploy/client -c curl -- sh -c '
		url="$1"; n="$2"; i=1
		while [ "$i" -le "$n" ]; do
			body=$(curl -s --max-time 5 -w "\nHTTP%{http_code}\n" "$url" 2>/dev/null | tr -d "\r")
			first=$(printf "%s\n" "$body" | sed -n "1p")
			case "$first" in
			parent | v1 | v2) printf "%s\n" "$first" ;;
			*) printf "%s\n" "$(printf "%s\n" "$body" | grep "^HTTP" | sed -n "1p" | grep . || echo NOREPLY)" ;;
			esac
			i=$((i + 1))
		done
	' sh "http://echo.$DEMO_NS.$MESH_DOMAIN:18081$path" "$n" 2>/dev/null || true
}

count_token() { printf '%s\n' "$1" | awk -v want="$2" '$1 == want { n++ } END { print n + 0 }'; }
histogram() { printf '%s\n' "$1" | awk 'NF { c[$1]++ } END { for (k in c) printf "%s=%d ", k, c[k] }'; }

# await_answer PATH WANT TIMEOUT — poll small batches until every probe of one
# batch answers WANT. The convergence gate before each measured batch: the
# importer polls (10 s), then the route table is pushed, so measuring across
# the change would mix two configurations into one exact count. On a timeout it
# prints what it last saw.
await_answer() {
	local path="$1" want="$2" timeout="$3" replies deadline probes=5
	deadline=$((SECONDS + timeout))
	while true; do
		replies="$(probe_batch "$path" "$probes")"
		if [ "$(count_token "$replies" "$want")" -eq "$probes" ]; then
			return 0
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			printf '%s' "$(histogram "$replies")"
			return 1
		fi
		sleep 3
	done
}

# expect_all PATH WANT WHAT — one measured batch, all of it WANT.
expect_all() {
	local path="$1" want="$2" what="$3" replies got
	replies="$(probe_batch "$path" "$PROBES")"
	got="$(count_token "$replies" "$want")"
	[ "$got" -eq "$PROBES" ] ||
		die "$what: expected $PROBES/$PROBES '$want' on $path, got $(histogram "$replies")"
}

# projection_keys CLUSTER — the config keys CLUSTER holds for demo/echo in the
# shared store (one line, or nothing). Ends in `|| true`: no match is an answer
# here, not an error (e2e/README.md, "A captured gate value never aborts").
projection_keys() {
	docker exec "$ETCD_NAME" etcdctl get --prefix "$(config_prefix "$1")" --keys-only 2>/dev/null |
		grep -F "$(config_key)" || true
}

# await_projection CLUSTER present|absent TIMEOUT
await_projection() {
	local c="$1" want="$2" timeout="$3" keys deadline
	deadline=$((SECONDS + timeout))
	while true; do
		keys="$(projection_keys "$c")"
		if [ "$want" = "present" ] && [ -n "$keys" ]; then return 0; fi
		if [ "$want" = "absent" ] && [ -z "$keys" ]; then return 0; fi
		[ "$SECONDS" -lt "$deadline" ] || return 1
		sleep 2
	done
}

# --- assertions ---------------------------------------------------------------

# C0 — the control, and it runs first. Without it C2 could not be told from a
# client that is answered 'v2' for a reason that has nothing to do with 026.
verify_control() {
	log "C0 control: no ServiceExport in '$CLUSTER_A' — no projection in the store, and '$CLUSTER_B' routes echo to echo"
	ka -n "$DEMO_NS" get httproute echo-routes >/dev/null 2>&1 ||
		die "HTTPRoute echo-routes is not in '$CLUSTER_A': run '$0 up' first (it applies the fixtures)"
	if kb get httproutes -A -o name 2>/dev/null | grep -c . >/dev/null; then
		die "cluster '$CLUSTER_B' has an HTTPRoute of its own: the suite could not tell an imported route from a local one"
	fi
	# A previous verify leaves no export; delete one anyway, so a run that was
	# interrupted between C1 and C3 does not make this control lie.
	ka -n "$DEMO_NS" delete serviceexport echo --ignore-not-found >/dev/null || die "could not delete the ServiceExport"
	await_projection "$CLUSTER_A" absent 60 ||
		die "C0: the store still holds a projection for $DEMO_NS/echo under cluster-$CLUSTER_A with no ServiceExport: $(projection_keys "$CLUSTER_A")"
	ok "the shared store holds no projection for $DEMO_NS/echo"
	await_answer /v2 parent 240 >/dev/null ||
		die "C0: client('$CLUSTER_B') GET /v2 was never answered by 'parent' with nothing exported: $(await_answer /v2 parent 1) — the plain mesh path to echo is not carrying traffic, so nothing below could mean anything"
	expect_all /v2 parent "C0"
	expect_all / parent "C0"
	ok "$PROBES/$PROBES of /v2 and of / answered by echo itself: nothing routes on the path yet"
}

# C1 — the export half, which was the whole of this suite before it ran nightly.
verify_export() {
	log "C1 export: a ServiceExport for echo in '$CLUSTER_A' — the projection must appear under cluster-$CLUSTER_A's partition"
	ka apply -f - >/dev/null <<YAML || die "ServiceExport apply failed"
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ServiceExport
metadata: {name: echo, namespace: $DEMO_NS}
YAML
	await_projection "$CLUSTER_A" present 60 ||
		die "C1: the projection for $DEMO_NS/echo never appeared under $(config_prefix "$CLUSTER_A") — cluster '$CLUSTER_A''s config-export controller did not write it"
	ok "written to the shared store: $(projection_keys "$CLUSTER_A")"
	# The importer never exports: a key under ITS partition would mean a cluster
	# wrote config it does not own.
	[ -z "$(projection_keys "$CLUSTER_B")" ] ||
		die "C1: cluster-$CLUSTER_B's partition holds a projection for $DEMO_NS/echo, which only '$CLUSTER_A' exports: $(projection_keys "$CLUSTER_B")"
	# The value is a protobuf; the backend cluster names in it are plain strings.
	local value b
	value="$(docker exec "$ETCD_NAME" etcdctl get --prefix "$(config_prefix "$CLUSTER_A")" --print-value-only 2>/dev/null | tr -c '[:print:]' '\n' || true)"
	for b in echo-v1 echo-v2; do
		grep -cF "$b.$DEMO_NS.$MESH_DOMAIN" <<<"$value" >/dev/null ||
			die "C1: the projection does not name the backend $b.$DEMO_NS.$MESH_DOMAIN"
	done
	ok "the projection names both backends of the route (echo-v1, echo-v2)"
}

# C2 — the import half and the data path: nothing was applied to cluster b.
verify_import() {
	log "C2 import: client('$CLUSTER_B') must now be routed by the HTTPRoute that exists only in '$CLUSTER_A'"
	await_answer /v2 v2 180 >/dev/null ||
		die "C2: client('$CLUSTER_B') GET /v2 never reached echo-v2: $(await_answer /v2 v2 1) — 'parent' means cluster '$CLUSTER_B''s proxy holds no imported route (agent import, or its registrar's ListAllConfig); a 503 means the route arrived and its backend cluster did not"
	await_answer / v1 60 >/dev/null ||
		die "C2: client('$CLUSTER_B') GET / never reached echo-v1: $(await_answer / v1 1)"
	expect_all /v2 v2 "C2"
	expect_all / v1 "C2"
	ok "$PROBES/$PROBES of /v2 answered by echo-v2 and $PROBES/$PROBES of / by echo-v1: cluster '$CLUSTER_B' routes with cluster '$CLUSTER_A''s HTTPRoute"
}

# C3 — withdrawal, and the control again from the other side: the EXPORT, not
# the fixture, was routing.
verify_withdraw() {
	log "C3 withdraw: delete the ServiceExport — the projection is pruned and '$CLUSTER_B' routes echo to echo again"
	ka -n "$DEMO_NS" delete serviceexport echo >/dev/null || die "could not delete the ServiceExport"
	await_projection "$CLUSTER_A" absent 60 ||
		die "C3: the projection outlived its ServiceExport: $(projection_keys "$CLUSTER_A")"
	ok "the projection is gone from the shared store"
	await_answer /v2 parent 180 >/dev/null ||
		die "C3: client('$CLUSTER_B') GET /v2 is still not answered by echo after the export was withdrawn: $(await_answer /v2 parent 1) — an imported route outlived its projection"
	await_answer / parent 60 >/dev/null ||
		die "C3: client('$CLUSTER_B') GET / is still not answered by echo after the export was withdrawn: $(await_answer / parent 1)"
	expect_all /v2 parent "C3"
	expect_all / parent "C3"
	ok "$PROBES/$PROBES of /v2 and of / answered by echo itself again"
}

verify() {
	verify_control
	verify_export
	verify_import
	verify_withdraw
	log "all cross-cluster config assertions passed (control, export, import on the data path, withdrawal)"
}

down() {
	log "tearing down"
	kind delete cluster --name "$CLUSTER_A" >/dev/null 2>&1 || true
	kind delete cluster --name "$CLUSTER_B" >/dev/null 2>&1 || true
	docker rm -f "$ETCD_NAME" >/dev/null 2>&1 || true
	ok "clusters + shared etcd removed"
}

up() {
	raise_inotify
	build_images
	create_clusters
	start_etcd
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
*)
	echo "usage: $0 {up|test|verify|down}" >&2
	exit 2
	;;
esac
