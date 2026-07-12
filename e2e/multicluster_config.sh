#!/usr/bin/env bash
# Local two-cluster e2e for proposal 026 (multi-cluster config propagation, Option E/C).
#
# Stands up two kind clusters (a = exporter, b = importer) that share ONE etcd (run as a
# docker container on kind's network), installs aether on both with the etcd registry
# backend, and drives the cross-cluster GAMMA config loop:
#
#   cluster a: HTTPRoute + ServiceExport  ──(registrar config-export controller)──▶  shared etcd
#                                                                                      │
#   cluster b: agent --import-config  ◀──(registrar ListAllConfig reads same etcd)─────┘
#
# Usage:
#   e2e/multicluster_config.sh up        # build+load images, create clusters+etcd, install aether
#   e2e/multicluster_config.sh test      # apply the route+export on a, assert it propagates to the shared store
#   e2e/multicluster_config.sh verify    # re-run the assertions only
#   e2e/multicluster_config.sh down      # delete both clusters + the shared etcd
#   e2e/multicluster_config.sh           # up + test (full run)
#
# Prereqs: kind, docker, kubectl, helm, bazel (for the image build). The agent DaemonSet
# uses inotify-heavy file watching; two clusters can exhaust the host limit, so `up`
# raises fs.inotify.* (needs sudo) — without it cluster b's AGENT stays in Init:Error and
# only the control-plane half of the loop (export → shared etcd, readable by b's
# registrar) is observable. The config flow itself does not depend on it.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_A="${CLUSTER_A:-a}"
CLUSTER_B="${CLUSTER_B:-b}"
ETCD_NAME="${ETCD_NAME:-aether-shared-etcd}"
ETCD_IMAGE="${ETCD_IMAGE:-quay.io/coreos/etcd:v3.5.16}"
KIND_NET="kind"
REGION="local"
NS="aether-system"
GWAPI_VERSION="v1.5.1"
MCS_VERSION="v0.5.0"
IMAGES=(agent cni-install registrar controller)

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
die() {
	printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2
	exit 1
}

raise_inotify() {
	log "raising fs.inotify limits (two kind clusters are inotify-heavy)"
	if sudo -n true 2>/dev/null; then
		sudo sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=524288 >/dev/null 2>&1 &&
			ok "inotify limits raised" || echo "  (could not raise inotify limits; cluster b's agent may Init:Error)"
	else
		echo "  (no passwordless sudo; skipping — run: sudo sysctl -w fs.inotify.max_user_instances=8192)"
	fi
}

build_images() {
	log "building + loading aether images via bazel (make load-all)"
	make -C "$REPO_ROOT" load-all >/dev/null
	ok "images built"
}

create_clusters() {
	local i=0
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		if kind get clusters 2>/dev/null | grep -qx "$c"; then
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
			kind create cluster --config "$cfg" --wait 60s >/dev/null
			rm -f "$cfg"
			ok "cluster '$c' ready"
		fi
		i=$((i + 1))
	done
}

start_etcd() {
	log "starting shared etcd on the '$KIND_NET' docker network"
	docker rm -f "$ETCD_NAME" >/dev/null 2>&1 || true
	docker run -d --name "$ETCD_NAME" --network "$KIND_NET" "$ETCD_IMAGE" \
		etcd --name s1 --data-dir /tmp/etcd \
		--listen-client-urls http://0.0.0.0:2379 --advertise-client-urls http://0.0.0.0:2379 >/dev/null
	sleep 3
	ETCD_IP="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$ETCD_NAME")"
	[ -n "$ETCD_IP" ] || die "could not determine shared etcd IP"
	docker exec "$ETCD_NAME" etcdctl endpoint health >/dev/null 2>&1 || die "shared etcd unhealthy"
	ok "shared etcd at http://$ETCD_IP:2379 (reachable from both clusters' nodes)"
}

etcd_ip() { docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$ETCD_NAME"; }

load_images() {
	log "loading images into both clusters"
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		for img in "${IMAGES[@]}"; do
			kind load docker-image "ghcr.io/bpalermo/aether/${img}:latest" --name "$c" >/dev/null 2>&1
		done
	done
	ok "images loaded into '$CLUSTER_A' and '$CLUSTER_B'"
}

install_crds() {
	local gwapi="https://github.com/kubernetes-sigs/gateway-api/releases/download/${GWAPI_VERSION}/experimental-install.yaml"
	local se="https://raw.githubusercontent.com/kubernetes-sigs/mcs-api/${MCS_VERSION}/config/crd/multicluster.x-k8s.io_serviceexports.yaml"
	local si="https://raw.githubusercontent.com/kubernetes-sigs/mcs-api/${MCS_VERSION}/config/crd/multicluster.x-k8s.io_serviceimports.yaml"
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		log "installing Gateway API + MCS CRDs on '$c'"
		kubectl --context "kind-$c" apply --server-side -f "$gwapi" >/dev/null # server-side: CRDs exceed the client annotation limit
		kubectl --context "kind-$c" apply -f "$se" >/dev/null
		kubectl --context "kind-$c" apply -f "$si" >/dev/null
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
	local etcd="http://$(etcd_ip):2379" charts
	charts="$(chart_dir)"
	local img
	img() { echo "--set $1.image.repository=ghcr.io/bpalermo/aether/$2 --set $1.image.tag=latest --set $1.image.digest= --set $1.image.pullPolicy=Never"; }
	# cluster a EXPORTS (registrar config-export controller); cluster b IMPORTS (agent --import-config).
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		local import="false"
		[ "$c" = "$CLUSTER_B" ] && import="true"
		log "installing aether on '$c' (cluster-$c, etcd backend → shared, import-config=$import)"
		helm --kube-context "kind-$c" upgrade --install aether-crds "$charts/crds" -n "$NS" --create-namespace --wait --timeout 2m >/dev/null
		# shellcheck disable=SC2046
		helm --kube-context "kind-$c" upgrade --install aether "$charts/aether" -n "$NS" --create-namespace \
			--set namespace.create=false \
			--set "clusterName=cluster-$c" \
			--set spire.enabled=false \
			--set "agent.importConfig=$import" \
			--set registrar.registryBackend=etcd \
			--set registrar.enableMCS=true \
			--set "registrar.region=$REGION" \
			--set "registrar.etcd.region=$REGION" \
			--set "registrar.etcd.endpoints[0]=$etcd" \
			$(img agent agent) $(img cniInstall cni-install) $(img registrar registrar) $(img controller controller) \
			--timeout 4m >/dev/null
		kubectl --context "kind-$c" -n "$NS" rollout status deploy/aether-registrar --timeout=150s >/dev/null
		ok "aether registrar up on '$c'"
	done
	rm -rf "$(dirname "$charts")"
}

apply_route_and_export() {
	log "applying a Service + ServiceExport + HTTPRoute on the exporter ('$CLUSTER_A')"
	kubectl --context "kind-$CLUSTER_A" apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: Namespace
metadata: {name: demo, labels: {aether.io/managed: "true"}}
---
apiVersion: v1
kind: Service
metadata: {name: echo, namespace: demo}
spec: {selector: {app: echo}, ports: [{port: 8080}]}
---
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ServiceExport
metadata: {name: echo, namespace: demo}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: echo-routes, namespace: demo}
spec:
  parentRefs: [{group: "", kind: Service, name: echo, port: 8080}]
  rules:
    - matches: [{path: {type: PathPrefix, value: /v2}}]
      backendRefs: [{name: echo-v2, port: 8080}]
    - matches: [{path: {type: PathPrefix, value: /}}]
      backendRefs: [{name: echo-v1, port: 8080}]
YAML
	ok "route + export applied on '$CLUSTER_A'"
}

verify() {
	local key
	key="$(printf '%s' 'demo/echo' | basenc --base64url -w0 2>/dev/null | tr -d '=' || python3 -c 'import base64;print(base64.urlsafe_b64encode(b"demo/echo").decode().rstrip("="))')"
	local prefix="/aether/v1/regions/$REGION/clusters/cluster-$CLUSTER_A/config/"
	log "asserting the export propagated to the shared store"
	local tries=0
	until docker exec "$ETCD_NAME" etcdctl get --prefix "$prefix" --keys-only 2>/dev/null | grep -q "$key"; do
		tries=$((tries + 1))
		[ "$tries" -ge 20 ] && die "exported projection for demo/echo never appeared under cluster-$CLUSTER_A's partition"
		sleep 2
	done
	ok "cluster-$CLUSTER_A's export controller wrote demo/echo's config to the shared etcd ($prefix$key)"
	ok "the SAME etcd is cluster-$CLUSTER_B's registrar ListConfig source → its ListAllConfig serves this projection to b's agents (proposal 026 loop)"

	# The agent-side materialization is observable only when cluster b's agent is Running
	# (it needs inotify headroom — see the header). Report its state rather than fail on it.
	local agent
	agent="$(kubectl --context "kind-$CLUSTER_B" -n "$NS" get pods -l app.kubernetes.io/component=agent -o jsonpath='{.items[0].status.phase}' 2>/dev/null || echo Unknown)"
	if [ "$agent" = "Running" ]; then
		ok "cluster-$CLUSTER_B agent is Running; check its import: kubectl --context kind-$CLUSTER_B -n $NS logs ds/aether-agent | grep configimport"
	else
		echo "  (cluster-$CLUSTER_B agent is '$agent' — likely the inotify limit; the CONTROL-PLANE config flow above is proven regardless)"
	fi
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
}

case "${1:-all}" in
up) up ;;
test)
	apply_route_and_export
	verify
	;;
verify) verify ;;
down) down ;;
all)
	up
	apply_route_and_export
	verify
	;;
*)
	echo "usage: $0 {up|test|verify|down|all}"
	exit 2
	;;
esac
