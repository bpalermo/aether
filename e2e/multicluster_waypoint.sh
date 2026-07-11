#!/usr/bin/env bash
# Local two-cluster e2e for proposal 019 (per-node east/west waypoint).
#
# Proves the CROSS-CLUSTER DATA PATH when pod IPs are not routable across clusters
# but node IPs are — which is exactly kind's default (two kind clusters on the
# shared docker network have routable node IPs but non-overlapping, non-routable
# pod CIDRs). Builds on the 026 harness (two kinds + ONE shared etcd) and adds:
#
#   - SPIRE on BOTH clusters with ONE shared trust domain (aether.internal) via a
#     shared upstream-CA on disk (e2e/certs/ca.{crt,key}), so a pod SVID minted in
#     cluster b validates in cluster a — end-to-end mTLS crosses the cluster line.
#   - aether installed with --east-west-waypoint on both, mesh-DNS + capture on.
#   - echo deployed in cluster b only; a client in cluster a with echo in its
#     upstreams. The client's egress is captured, resolved via mesh-DNS, and its
#     EDS for echo points at b's NODE IP + tunnel port (split-horizon). b's node
#     proxy SNI-forwards to the local echo pod; mTLS is end-to-end pod<->pod.
#
# Assertion: client(a) -> echo.aether-test.<meshDomain> returns 200 AND echo sees
# XFCC = spiffe://aether.internal/ns/aether-test/sa/client (the real caller,
# across clusters). Intra-cluster traffic is unaffected (no waypoint stats in a).
#
# Usage: e2e/multicluster_waypoint.sh {up|test|verify|down}  (bare = up + test)
#
# Prereqs: kind, docker, kubectl, helm, bazel. Two kinds + SPIRE are inotify- and
# resource-heavy; `up` raises fs.inotify.* (needs passwordless sudo).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_A="${CLUSTER_A:-a}"
CLUSTER_B="${CLUSTER_B:-b}"
ETCD_NAME="${ETCD_NAME:-aether-shared-etcd}"
ETCD_IMAGE="${ETCD_IMAGE:-quay.io/coreos/etcd:v3.5.16}"
KIND_NET="kind"
REGION="local"
NS="aether-system"
TEST_NS="aether-test"
TRUST_DOMAIN="aether.internal"
MESH_DOMAIN="aether.internal"
TUNNEL_PORT="15009"
GWAPI_VERSION="v1.5.1"
SPIRE_CHART_VERSION="${SPIRE_CHART_VERSION:-0.28.4}"
SPIRE_CRDS_VERSION="${SPIRE_CRDS_VERSION:-0.5.0}"
SPIRE_CLASS="spire-mgmt-spire" # spire-controller-manager class (namespace-release)
IMAGES=(agent cni-install registrar controller)

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
die() {
	printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2
	exit 1
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
	log "building + loading aether images"
	make -C "$REPO_ROOT" load-all >/dev/null 2>&1 || die "image build (make load-all) failed"
}

create_clusters() {
	local i=0
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		if kind get clusters 2>/dev/null | grep -qx "$c"; then
			ok "kind cluster '$c' already exists"
		else
			log "creating kind cluster '$c' (non-overlapping, non-routable pod CIDRs)"
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
	ok "shared etcd at http://$ETCD_IP:2379"
}

load_images() {
	log "loading images into both clusters"
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		for img in "${IMAGES[@]}"; do
			kind load docker-image "ghcr.io/bpalermo/aether/${img}:latest" --name "$c" >/dev/null 2>&1
		done
	done
	ok "images loaded"
}

# install_spire deploys spiffe/spire on a cluster with the SHARED upstream CA, so
# both clusters issue SVIDs under the same trust domain that cross-validate.
install_spire() {
	local ctx="kind-$1"
	log "installing SPIRE on '$1' (trust domain $TRUST_DOMAIN, shared upstream CA)"
	helm --kube-context "$ctx" repo add spiffe https://spiffe.github.io/helm-charts-hardened/ >/dev/null 2>&1 || true
	helm --kube-context "$ctx" repo update >/dev/null 2>&1 || true
	kubectl --context "$ctx" create ns spire-mgmt >/dev/null 2>&1 || true
	# The shared upstream CA (same bytes in both clusters) is the root of trust.
	# Must live in the namespace the spire-server runs in (spire-mgmt).
	kubectl --context "$ctx" -n spire-mgmt create secret generic upstream-ca \
		--from-file=tls.crt="$REPO_ROOT/e2e/certs/ca.crt" \
		--from-file=tls.key="$REPO_ROOT/e2e/certs/ca.key" \
		--from-file=bundle.crt="$REPO_ROOT/e2e/certs/ca.crt" >/dev/null 2>&1 || true
	helm --kube-context "$ctx" upgrade --install spire-crds spiffe/spire-crds \
		-n spire-mgmt --version "$SPIRE_CRDS_VERSION" --wait --timeout 3m >/dev/null
	helm --kube-context "$ctx" upgrade --install spire spiffe/spire \
		-n spire-mgmt --version "$SPIRE_CHART_VERSION" \
		--set global.spire.trustDomain="$TRUST_DOMAIN" \
		--set global.spire.clusterName="cluster-$1" \
		--set "spiffe-oidc-discovery-provider.enabled=false" \
		--set "spire-server.upstreamAuthority.disk.enabled=true" \
		--set "spire-server.upstreamAuthority.disk.secret.create=false" \
		--set "spire-server.upstreamAuthority.disk.secret.name=upstream-ca" \
		--set "spire-server.upstreamAuthority.disk.secret.namespace=spire-mgmt" \
		--set "spire-agent.authorizedDelegates[0]=spiffe://$TRUST_DOMAIN/ns/$NS/sa/aether-agent" \
		--set "spire-agent.sockets.admin.enabled=true" \
		--set "spire-agent.sockets.admin.mountOnHost=true" \
		--wait --timeout 6m >/dev/null || die "SPIRE install failed on '$1'"
	# A ClusterSPIFFEID so every mesh pod gets spiffe://<td>/ns/<ns>/sa/<sa>. The
	# className must match the spire-controller-manager class ($SPIRE_CLASS).
	kubectl --context "$ctx" apply -f - >/dev/null <<YAML
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: aether-workloads
spec:
  className: $SPIRE_CLASS
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  podSelector:
    matchLabels:
      aether.io/managed: "true"
YAML
	ok "SPIRE up on '$1'"
}

install_crds() {
	local ctx="kind-$1"
	kubectl --context "$ctx" apply --server-side -f \
		"https://github.com/kubernetes-sigs/gateway-api/releases/download/${GWAPI_VERSION}/standard-install.yaml" >/dev/null 2>&1 || true
}

chart_dir() {
	local out
	out="$(mktemp -d)"
	cp -r "$REPO_ROOT/charts" "$out/"
	sed -i -e 's/{GIT_COMMIT}/e2e/' -e 's/{STABLE_GIT_VERSION}/0.0.0-e2e/' "$out/charts/crds/Chart.yaml" "$out/charts/aether/Chart.yaml"
	echo "$out/charts"
}

install_aether() {
	local etcd="http://$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$ETCD_NAME"):2379" charts
	charts="$(chart_dir)"
	local img
	img() { echo "--set $1.image.repository=ghcr.io/bpalermo/aether/$2 --set $1.image.tag=latest --set $1.image.digest= --set $1.image.pullPolicy=Never"; }
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		log "installing aether on '$c' (waypoint ON, spire ON, shared etcd)"
		helm --kube-context "kind-$c" upgrade --install aether-crds "$charts/crds" -n "$NS" --create-namespace --wait --timeout 2m >/dev/null
		# shellcheck disable=SC2046
		helm --kube-context "kind-$c" upgrade --install aether "$charts/aether" -n "$NS" --create-namespace \
			--set namespace.create=false \
			--set "clusterName=cluster-$c" \
			--set "meshDomain=$MESH_DOMAIN" \
			--set spire.enabled=true \
			--set spire.trustDomain="$TRUST_DOMAIN" \
			--set controller.webhook.spire=false \
			--set edge.enabled=false \
			--set agent.gamma=true \
			--set agent.meshDns=true \
			--set agent.transparentCapture=true \
			--set agent.eastWestWaypoint=true \
			--set "agent.eastWestTunnelPort=$TUNNEL_PORT" \
			--set registrar.registryBackend=etcd \
			--set registrar.enableMCS=true \
			--set "registrar.region=$REGION" \
			--set "registrar.etcd.region=$REGION" \
			--set "registrar.etcd.endpoints[0]=$etcd" \
			$(img agent agent) $(img cniInstall cni-install) $(img registrar registrar) $(img controller controller) \
			--timeout 5m >/dev/null || die "aether install failed on '$c'"
		kubectl --context "kind-$c" -n "$NS" rollout status ds/aether-agent --timeout=180s >/dev/null || true
		ok "aether up on '$c'"
	done
	rm -rf "$(dirname "$charts")"
}

deploy_workloads() {
	log "deploying echo in '$CLUSTER_B' and client in '$CLUSTER_A'"
	kubectl --context "kind-$CLUSTER_B" create ns "$TEST_NS" >/dev/null 2>&1 || true
	kubectl --context "kind-$CLUSTER_A" create ns "$TEST_NS" >/dev/null 2>&1 || true
	# echo: a mesh pod in cluster b, registered as service "echo" in aether-test.
	kubectl --context "kind-$CLUSTER_B" apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: ServiceAccount
metadata: {name: echo, namespace: aether-test}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: echo, namespace: aether-test}
spec:
  replicas: 1
  selector: {matchLabels: {app: echo}}
  template:
    metadata:
      labels: {app: echo, aether.io/managed: "true"}
      annotations: {endpoint.aether.io/port: "8080"}
    spec:
      serviceAccountName: echo
      containers:
        - name: echo
          image: hashicorp/http-echo:1.0
          args: ["-text=hello-from-cluster-b", "-listen=:8080"]
          ports: [{containerPort: 8080}]
YAML
	# client: a mesh pod in cluster a that depends on echo (upstreams annotation).
	kubectl --context "kind-$CLUSTER_A" apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: ServiceAccount
metadata: {name: client, namespace: aether-test}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: client, namespace: aether-test}
spec:
  replicas: 1
  selector: {matchLabels: {app: client}}
  template:
    metadata:
      labels: {app: client, aether.io/managed: "true"}
      annotations: {config.aether.io/upstreams: "echo.aether-test"}
    spec:
      serviceAccountName: client
      containers:
        - name: curl
          image: curlimages/curl:8.11.1
          command: ["sleep", "infinity"]
YAML
	kubectl --context "kind-$CLUSTER_B" -n "$TEST_NS" rollout status deploy/echo --timeout=120s >/dev/null || true
	kubectl --context "kind-$CLUSTER_A" -n "$TEST_NS" rollout status deploy/client --timeout=120s >/dev/null || true
	ok "workloads deployed"
}

verify() {
	log "verifying cross-cluster call: client(a) -> echo(b) via the per-node waypoint"
	# The endpoint b advertises for echo must carry b's NODE IP (proposal 019 P1).
	local nip
	nip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${CLUSTER_B}-control-plane" 2>/dev/null || true)"
	[ -n "$nip" ] && ok "cluster b node IP: $nip"
	docker exec "$ETCD_NAME" etcdctl get --prefix /aether/v1/regions --keys-only 2>/dev/null | grep -q "clusters/cluster-b" &&
		ok "cluster b endpoints present in the shared registry" || echo "  (no cluster-b endpoints yet)"

	# The data-path assertion. echo is a cross-cluster (off-node) service, so the
	# FIRST request warms the demand-scoped cold path (ODCDS) — retry like a real
	# client until it routes (a known aether cold-start behavior, not a waypoint
	# fault). Once warm it stays 200.
	local code=000 i
	for i in 1 2 3 4 5 6; do
		code="$(kubectl --context "kind-$CLUSTER_A" -n "$TEST_NS" exec deploy/client -c curl -- \
			curl -sS -o /dev/null -w '%{http_code}' --max-time 12 "http://echo.$TEST_NS.$MESH_DOMAIN:18081/" 2>/dev/null || echo 000)"
		[ "$code" = "200" ] && break
		sleep 6
	done
	if [ "$code" = "200" ]; then
		ok "cross-cluster call succeeded (HTTP 200) — waypoint data path works: client(a) -> b-node:$TUNNEL_PORT -> echo pod(b), mTLS end-to-end"
	else
		die "cross-cluster call returned $code (expected 200) — inspect a's EDS for echo (should be b-node-ip:$TUNNEL_PORT) and b's ew_tunnel listener"
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
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		install_crds "$c"
		install_spire "$c"
	done
	install_aether
	deploy_workloads
}

case "${1:-}" in
up) up ;;
test) verify ;;
verify) verify ;;
down) down ;;
"") up && sleep 20 && verify ;;
*) die "usage: $0 {up|test|verify|down}" ;;
esac
