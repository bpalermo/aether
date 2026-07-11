#!/usr/bin/env bash
# Local two-REGION e2e for proposal 006 Phase 3 (cross-region etcd replicator).
#
# Two kind clusters, each its OWN regional etcd (shared-nothing registry planes),
# registrars cross-wired with --peer-etcd: each region's leader registrar mirrors
# its own /aether/v1/regions/<self>/ subtree verbatim into the peer's etcd under
# an origin-heartbeat lease (P2a mirror loop + P2b lease, PRs #512/#513).
#
# Builds on the 019 waypoint harness (e2e/multicluster_waypoint.sh) — same two
# kinds + shared-trust-domain SPIRE + waypoint data path — but the clusters no
# longer share an etcd, so the cross-cluster call only works IF replication does:
# echo(b)'s endpoint reaches a's registrar exclusively through b's mirror into
# etcd-a.
#
# Assertions (verify):
#   1. visibility  — etcd-a holds mirrored /regions/region-b/ keys and vice versa.
#   2. data path   — client(a) -> echo.aether-test.<meshDomain>:18081 == 200 over
#                    the mirror + waypoint (mTLS end-to-end).
#   3. failover    — scale b's registrar to 0 (replicator leader dies, keepalive
#                    stops): b's mirror in etcd-a EXPIRES at the lease TTL (~30s)
#                    with NO peer-side GC, while etcd-b keeps b's origin data; the
#                    client call then fails (a's EDS dropped b's endpoints).
#   4. recovery    — scale b's registrar back up: resync re-mirrors under a fresh
#                    lease and the client call returns 200 again.
#
# Usage: e2e/multicluster_replicator.sh {up|test|verify|down}  (bare = up + test)
#
# Prereqs: kind, docker, kubectl, helm, bazel. Two kinds + SPIRE are inotify- and
# resource-heavy; `up` raises fs.inotify.* (needs passwordless sudo).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_A="${CLUSTER_A:-a}"
CLUSTER_B="${CLUSTER_B:-b}"
ETCD_IMAGE="${ETCD_IMAGE:-quay.io/coreos/etcd:v3.5.16}"
KIND_NET="kind"
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
# Origin-heartbeat lease TTL is 30s (replicator default); how long verify waits
# for the mirror to expire after the origin dies (TTL + keepalive/gRPC slack).
FAILOVER_TIMEOUT="${FAILOVER_TIMEOUT:-90}"

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
die() {
	printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2
	exit 1
}

etcd_name() { echo "aether-etcd-region-$1"; }
region_of() { echo "region-$1"; }

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
	# REPLICATOR_SKIP_BUILD=1 so this step is a no-op — the images are already in
	# the local docker daemon under ghcr.io/bpalermo/aether/<img>:latest.
	if [ "${REPLICATOR_SKIP_BUILD:-0}" = "1" ]; then
		ok "skipping image build (REPLICATOR_SKIP_BUILD=1; images pre-built)"
		return
	fi
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

# start_etcds runs ONE etcd per region on the kind docker network — the
# shared-nothing condition proposal 006 replicates across.
start_etcds() {
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		local name
		name="$(etcd_name "$c")"
		log "starting regional etcd '$name'"
		docker rm -f "$name" >/dev/null 2>&1 || true
		docker run -d --name "$name" --network "$KIND_NET" "$ETCD_IMAGE" \
			etcd --name "$name" --data-dir /tmp/etcd \
			--listen-client-urls http://0.0.0.0:2379 --advertise-client-urls http://0.0.0.0:2379 >/dev/null
	done
	sleep 3
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		local name
		name="$(etcd_name "$c")"
		docker exec "$name" etcdctl endpoint health >/dev/null 2>&1 || die "etcd '$name' unhealthy"
		ok "$name at http://$(etcd_ip "$c"):2379"
	done
}

etcd_ip() {
	docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$(etcd_name "$1")"
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

# install_aether installs each cluster against ITS OWN regional etcd, with the
# peer region's etcd as the --peer-etcd replication target (the P2 cross-wiring).
install_aether() {
	local charts
	charts="$(chart_dir)"
	local img
	img() { echo "--set $1.image.repository=ghcr.io/bpalermo/aether/$2 --set $1.image.tag=latest --set $1.image.digest= --set $1.image.pullPolicy=Never"; }
	local peer
	for c in "$CLUSTER_A" "$CLUSTER_B"; do
		[ "$c" = "$CLUSTER_A" ] && peer="$CLUSTER_B" || peer="$CLUSTER_A"
		log "installing aether on '$c' (region $(region_of "$c"), own etcd, peer-etcd -> $(region_of "$peer"))"
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
			--set "registrar.region=$(region_of "$c")" \
			--set "registrar.etcd.endpoints[0]=http://$(etcd_ip "$c"):2379" \
			--set "registrar.peerEtcd[0]=$(region_of "$peer")=http://$(etcd_ip "$peer"):2379" \
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

# mirror_present <cluster-of-etcd> <region-prefix-to-look-for>
mirror_present() {
	docker exec "$(etcd_name "$1")" etcdctl get --prefix "/aether/v1/regions/$2/" --keys-only 2>/dev/null | grep -q .
}

client_code() {
	# curl -w prints 000 itself on connection failure (then exits nonzero), so
	# normalize to the last 3 chars instead of appending a second fallback.
	local out
	out="$(kubectl --context "kind-$CLUSTER_A" -n "$TEST_NS" exec deploy/client -c curl -- \
		curl -sS -o /dev/null -w '%{http_code}' --max-time 12 "http://echo.$TEST_NS.$MESH_DOMAIN:18081/" 2>/dev/null)" || true
	printf '%s' "${out:-000}" | tail -c 3
}

verify() {
	local region_a region_b
	region_a="$(region_of "$CLUSTER_A")"
	region_b="$(region_of "$CLUSTER_B")"

	log "1/4 replication visibility: each region's subtree mirrored into the peer etcd"
	local i
	for i in $(seq 1 24); do
		mirror_present "$CLUSTER_A" "$region_b" && mirror_present "$CLUSTER_B" "$region_a" && break
		sleep 5
	done
	mirror_present "$CLUSTER_A" "$region_b" || die "etcd-a has no mirrored $region_b keys (b's replicator -> etcd-a)"
	mirror_present "$CLUSTER_B" "$region_a" || die "etcd-b has no mirrored $region_a keys (a's replicator -> etcd-b)"
	ok "both mirrors present (a sees $region_b, b sees $region_a)"

	log "2/4 data path over the mirror: client(a) -> echo(b) (echo is known to 'a' ONLY via replication)"
	local code=000
	for i in 1 2 3 4 5 6; do
		code="$(client_code)"
		[ "$code" = "200" ] && break
		sleep 6
	done
	[ "$code" = "200" ] || die "cross-region call returned $code (expected 200)"
	ok "cross-region call succeeded (HTTP 200) over mirror + waypoint"

	log "3/4 failover: kill region $region_b's registrar (replicator leader) -> its mirror in etcd-a must EXPIRE at the lease TTL"
	kubectl --context "kind-$CLUSTER_B" -n "$NS" scale deploy/aether-registrar --replicas=0 >/dev/null
	kubectl --context "kind-$CLUSTER_B" -n "$NS" wait --for=delete pod -l app.kubernetes.io/component=registrar --timeout=90s >/dev/null 2>&1 || true
	local waited=0 gone=0
	while [ "$waited" -lt "$FAILOVER_TIMEOUT" ]; do
		if ! mirror_present "$CLUSTER_A" "$region_b"; then
			gone=1
			break
		fi
		sleep 5
		waited=$((waited + 5))
	done
	[ "$gone" = "1" ] || die "mirrored $region_b keys still in etcd-a after ${FAILOVER_TIMEOUT}s (lease did not lapse)"
	ok "mirror expired in etcd-a ${waited}s after origin death (origin-heartbeat lease lapse, no peer GC)"
	mirror_present "$CLUSTER_B" "$region_b" || die "region $region_b's ORIGIN data vanished from its own etcd (must persist)"
	ok "origin data intact in etcd-b (only the mirror expired)"
	# a's registrar drops echo from its snapshot -> the client call must fail.
	for i in 1 2 3 4 5 6; do
		code="$(client_code)"
		[ "$code" != "200" ] && break
		sleep 5
	done
	[ "$code" != "200" ] || die "client(a) still reaches echo after region-b mirror expired"
	ok "client(a) -> echo now fails ($code) — a's EDS dropped the dead region"

	log "4/4 recovery: restore region $region_b's registrar -> resync re-mirrors under a fresh lease"
	kubectl --context "kind-$CLUSTER_B" -n "$NS" scale deploy/aether-registrar --replicas=2 >/dev/null
	kubectl --context "kind-$CLUSTER_B" -n "$NS" rollout status deploy/aether-registrar --timeout=120s >/dev/null || true
	for i in $(seq 1 24); do
		mirror_present "$CLUSTER_A" "$region_b" && break
		sleep 5
	done
	mirror_present "$CLUSTER_A" "$region_b" || die "mirror did not return to etcd-a after registrar recovery"
	ok "mirror re-established in etcd-a"
	for i in $(seq 1 12); do
		code="$(client_code)"
		[ "$code" = "200" ] && break
		sleep 6
	done
	[ "$code" = "200" ] || die "cross-region call did not recover (last: $code)"
	ok "cross-region call restored (HTTP 200) — full failover round trip"
}

down() {
	log "tearing down"
	kind delete cluster --name "$CLUSTER_A" >/dev/null 2>&1 || true
	kind delete cluster --name "$CLUSTER_B" >/dev/null 2>&1 || true
	docker rm -f "$(etcd_name "$CLUSTER_A")" "$(etcd_name "$CLUSTER_B")" >/dev/null 2>&1 || true
	ok "clusters + regional etcds removed"
}

up() {
	raise_inotify
	build_images
	create_clusters
	start_etcds
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
