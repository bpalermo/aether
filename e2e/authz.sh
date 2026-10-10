#!/usr/bin/env bash
# Single-cluster kind e2e for the node-local ext_authz sidecar (proposal 027)
# and its pod ordering (#1275).
#
# The aether-proxy pod runs the authz sidecar (here the chart's OPA preset) next
# to Envoy, reached over a per-pod Unix socket. Until chart 2.4.9 it was a second
# REGULAR container: the kubelet started it after `proxy` without waiting, so
# when its image had to be pulled the new Envoy took the node's listeners with
# nothing behind the authz socket, and every check in that window was an
# ext_authz error — a 403 under failureMode DENY (talos-main, 2026-10-05: 7–13 s
# per node on the first roll onto OPA 1.21.1). Since 2.4.9 it is a NATIVE sidecar
# (init container, restartPolicy: Always, startupProbe on the socket): `proxy`
# starts only once authz accepts, and authz is stopped only after `proxy` exits.
#
# Assertions (verify):
#   a. decisions      — a mesh request to authz-echo WITH the allow header -> 200,
#                       without it -> 403 (OPA denies), and the ext_authz ok and
#                       denied counters both moved: the filter is really on the
#                       path and OPA is really deciding
#   b. pod ordering   — on the live proxy pod `authz` is a started native sidecar
#                       (initContainerStatuses, started=true) and `proxy` started
#                       after it. Skipped with AUTHZ_EXPECT=red (the old layout
#                       has no such sidecar)
#   c. the roll       — the OPA image is evicted from the node first, so the new
#                       pod's sidecar must be PULLED again (the #1275 trigger);
#                       then `kubectl rollout restart ds/aether-proxy` under
#                       continuous allowed requests. Gates: ZERO 403s on the
#                       allowed path, ZERO new ext_authz errors (Envoy's
#                       http.*.ext_authz.error, read on the node's admin, which
#                       hot restart carries across epochs), and the node's Envoy
#                       is at a NEWER hot-restart epoch afterwards (the roll
#                       really handed over). Then (a) again.
#   d. policy reload  — (#1383) the policy is changed the way an operator
#                       changes it, `helm upgrade` with another
#                       proxy.authzSidecar.opa.policy. The sidecar runs
#                       `opa run --watch`, so the gates are: the OLD allow
#                       header turns 403 and the NEW one 200 with the SAME
#                       proxy pod (UID), no container restart and the same
#                       Envoy epoch; then a policy that does not parse reaches
#                       the node and changes NOTHING (the last good policy
#                       keeps deciding, the sidecar keeps running and logs the
#                       error); then the first policy again, still the same
#                       pod. ZERO ext_authz errors throughout. Skipped with
#                       AUTHZ_EXPECT=red (an older chart reads the policy once)
#
# Red/green: AUTHZ_CHARTS=<dir holding an older charts/ tree> installs that chart
# instead (e.g. main before #1275, extracted with `git archive`), and
# AUTHZ_EXPECT=red inverts gate (c): the run passes only if the roll DID produce
# 403s or ext_authz errors — evidence that the harness discriminates.
#
# Usage: e2e/authz.sh {up|test|verify|down}   (bare = up + verify)
# Env: AUTHZ_CLUSTER (authz), AUTHZ_SKIP_BUILD=1 (CI: images pre-built),
#      AUTHZ_CHARTS (default: this repo's charts/), AUTHZ_EXPECT (green|red),
#      AUTHZ_EVICT_IMAGE (1: re-pull the OPA image on the roll; 0: keep it
#      cached), AUTHZ_LOOPS (3 parallel request loops), AUTHZ_RELOAD_TIMEOUT
#      (240: seconds a changed policy may take to decide; the kubelet syncs a
#      ConfigMap volume periodically; 27-64 s observed on one kind node, not a bound)
#
# Prereqs: kind, docker, kubectl, helm, bazel (for the image build; CI sets
# AUTHZ_SKIP_BUILD=1 and pre-loads the images from the nightly build artifact).
# The proxy and OPA images are pulled from their registries.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# The pinned kind binary / node image / kubectl (#1251).
# shellcheck source=e2e/kind-version.sh
. "$REPO_ROOT/e2e/kind-version.sh"
IMAGE_REGISTRY="$("$REPO_ROOT/scripts/image-registry.sh" prefix)"
CLUSTER="${AUTHZ_CLUSTER:-authz}"
CTX="kind-$CLUSTER"
NS="aether-system"
TEST_NS="aether-test"
MESH_DOMAIN="aether.internal"
OUTBOUND_PORT="18081"
# The pinned Gateway API release (#1583): one for every e2e surface.
# shellcheck source=e2e/gateway-api-version.sh
. "$REPO_ROOT/e2e/gateway-api-version.sh"
IMAGES=(agent mesh-dns proxy-supervisor cni-install registrar controller uds-csi)
CHARTS_SRC="${AUTHZ_CHARTS:-$REPO_ROOT/charts}"
EXPECT="${AUTHZ_EXPECT:-green}"
EVICT_IMAGE="${AUTHZ_EVICT_IMAGE:-1}"
LOOPS="${AUTHZ_LOOPS:-3}"
NODE="${CLUSTER}-control-plane"
ALLOW_HEADER="x-authz: letmein"
ALLOW_HEADER_V2="x-authz: second"
RELOAD_TIMEOUT="${AUTHZ_RELOAD_TIMEOUT:-240}"
# The chart's OPA preset image; read from the chart being installed so a red run
# against an older chart evicts the image THAT chart runs.
OPA_IMAGE="$(awk '/^ *opa:/ { o = 1 } o && /^ *image:/ { print $2; exit }' "$CHARTS_SRC/aether/values.yaml")"

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
die() {
	printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2
	dump_state
	exit 1
}

kc() { kubectl --context "$CTX" "$@"; }

dump_state() {
	kubectl config get-contexts "$CTX" >/dev/null 2>&1 || return 0
	printf '\033[1;33m  -- pods --\033[0m\n' >&2
	kc get pods -A -o wide 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- proxy pod container timeline --\033[0m\n' >&2
	container_timeline 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- authz sidecar log (decision log, tail) --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=proxy -c authz --tail=20 --prefix 2>&1 |
		cut -c1-300 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- proxy log (tail) --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=proxy -c proxy --tail=30 --prefix 2>&1 |
		cut -c1-300 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- envoy ext_authz stats --\033[0m\n' >&2
	authz_stats 2>&1 | sed 's/^/    /' >&2 || true
}

# container_timeline — every proxy pod's init/regular container start times.
container_timeline() {
	# shellcheck disable=SC2016 # a jsonpath template, not shell
	kc -n "$NS" get pods -l app.kubernetes.io/component=proxy -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{range .status.initContainerStatuses[*]}{"  init "}{.name}{" started="}{.started}{" running="}{.state.running.startedAt}{" terminated="}{.state.terminated.finishedAt}{"\n"}{end}{range .status.containerStatuses[*]}{"  ctr  "}{.name}{" started="}{.started}{" running="}{.state.running.startedAt}{"\n"}{end}{end}'
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
	if [ "${AUTHZ_SKIP_BUILD:-0}" = "1" ]; then
		ok "skipping image build (AUTHZ_SKIP_BUILD=1; images pre-built)"
		return
	fi
	log "building + loading aether images"
	local t
	for t in //agent/cmd/agent //agent/cmd/mesh-dns //agent/cmd/proxy-supervisor \
		//cni/cmd/cni-install //registrar/cmd/registrar //controller/cmd/controller //agent/cmd/uds-csi; do
		bazel run "$t:image_load" >/dev/null 2>&1 || die "image build failed for $t"
	done
	ok "images built"
}

create_cluster() {
	# SIGPIPE rule (#1121, e2e/README.md): read to EOF, never `grep -q` a pipe.
	if kind get clusters 2>/dev/null | grep -cx "$CLUSTER" >/dev/null; then
		ok "kind cluster '$CLUSTER' already exists"
		return
	fi
	log "creating kind cluster '$CLUSTER'"
	local cfg
	cfg="$(mktemp)"
	sed -e "s/CLUSTER_NAME/$CLUSTER/g" \
		-e "s#POD_SUBNET#10.12.0.0/16#g" \
		-e "s#SVC_SUBNET#10.112.0.0/16#g" \
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

chart_dir() {
	local out
	out="$(mktemp -d)"
	cp -r "$CHARTS_SRC" "$out/charts"
	sed -i -e 's/{GIT_COMMIT}/e2e/' -e 's/{STABLE_GIT_VERSION}/0.0.0-e2e/' \
		"$out/charts/crds/Chart.yaml" "$out/charts/aether/Chart.yaml"
	echo "$out/charts"
}

# The canary policy (charts/prober/values.yaml, authzCanary): allow iff the
# request carries the allow header, deny everything else.
POLICY='package envoy.authz

default allow := false

allow if input.attributes.request.http.headers["x-authz"] == "letmein"'

# The same policy with another allow header (d): what was allowed is denied and
# the reverse, so a reload is visible from the client.
POLICY_V2="${POLICY/letmein/second}"

# A policy that does not parse (d).
POLICY_BAD='package envoy.authz

default allow := false

allow if {
  input.attributes.request.http.headers["x-authz"] =='

# write_values POLICY FILE — a values file, not --set: the policy is multi-line.
write_values() {
	{
		echo "proxy:"
		echo "  authzSidecar:"
		echo "    enabled: true"
		echo "    failureMode: DENY"
		echo "    opa:"
		echo "      enabled: true"
		echo "      policy: |"
		printf '%s\n' "$1" | sed 's/^/        /'
	} >"$2"
}

# helm_aether CHARTS VALUES — install or upgrade the release. Every value is
# passed every time (never --reuse-values), so an upgrade that changes only
# the policy changes only the policy ConfigMap.
helm_aether() {
	img() { echo "--set $1.image.repository=${IMAGE_REGISTRY}/$2 --set $1.image.tag=latest --set $1.image.digest= --set $1.image.pullPolicy=Never"; }
	# SPIRE off (cleartext mesh, #421): ext_authz runs on the source proxy's
	# outbound chain, orthogonal to the inbound transport. otel.enabled with no
	# endpoint, as in e2e/uds.sh.
	# shellcheck disable=SC2046
	helm --kube-context "$CTX" upgrade --install aether "$1/aether" \
		-n "$NS" --create-namespace \
		--set namespace.create=false \
		--set "meshDomain=$MESH_DOMAIN" \
		--set spire.enabled=false \
		--set edge.enabled=false \
		--set otel.enabled=true \
		-f "$2" \
		$(img agent agent) $(img agent.meshDnsDaemon mesh-dns) \
		$(img proxy.supervisor proxy-supervisor) $(img cniInstall cni-install) \
		$(img registrar registrar) $(img controller controller) $(img udsCsi uds-csi) \
		--set proxy.image.pullPolicy=IfNotPresent \
		--timeout 5m >/dev/null
}

install_aether() {
	local charts values
	charts="$(chart_dir)"
	values="$(mktemp)"
	write_values "$POLICY" "$values"
	log "installing the aether CRDs (from $CHARTS_SRC)"
	helm --kube-context "$CTX" upgrade --install aether-crds "$charts/crds" \
		-n "$NS" --create-namespace --wait --timeout 2m >/dev/null || die "crds chart install failed"
	kc wait --for=condition=Established crd/httpfilters.config.aether.io --timeout=60s >/dev/null ||
		die "the HTTPFilter CRD never became Established"

	log "installing aether with the OPA authz sidecar (failureMode DENY)"
	helm_aether "$charts" "$values" || die "aether install failed"
	rm -rf "$(dirname "$charts")" "$values"

	kc -n "$NS" rollout status ds/aether-agent --timeout=240s >/dev/null || die "the agent DaemonSet never became Ready"
	kc -n "$NS" rollout status ds/aether-proxy --timeout=300s >/dev/null || die "the proxy DaemonSet never became Ready"
	kc -n "$NS" rollout status ds/aether-mesh-dns --timeout=180s >/dev/null || die "the mesh-DNS DaemonSet never became Ready"
	kc -n "$NS" rollout status deploy/aether-registrar --timeout=180s >/dev/null || die "the registrar never became Ready"
	kc -n "$NS" rollout status deploy/aether-controller --timeout=180s >/dev/null || die "the controller never became Ready"
	ok "aether up"
}

# The echo target, the extAuthz HTTPFilter on it, and the client — the prober's
# authzCanary shape. No Service objects: the registrar generates the mesh VIP
# Service from the ServiceAccount (see e2e/uds.sh).
deploy_workloads() {
	log "deploying authz-echo, its extAuthz HTTPFilter, and the client"
	kc create ns "$TEST_NS" >/dev/null 2>&1 || true
	kc apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: authz-echo, namespace: $TEST_NS}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: authz-echo, namespace: $TEST_NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: authz-echo}}
  template:
    metadata:
      labels: {app: authz-echo, aether.io/managed: "true"}
      annotations: {endpoint.aether.io/port: "8080"}
    spec:
      serviceAccountName: authz-echo
      containers:
        - name: app
          image: hashicorp/http-echo:1.0@sha256:fcb75f691c8b0414d670ae570240cbf95502cc18a9ba57e982ecac589760a186
          args: ["-text=served-by-authz-echo", "-listen=:8080"]
          ports: [{containerPort: 8080}]
---
apiVersion: config.aether.io/v1
kind: HTTPFilter
metadata: {name: authz-e2e, namespace: $TEST_NS}
spec:
  scope: SCOPE_CHAIN
  targetRefs:
    - kind: Service
      name: authz-echo
  extAuthz:
    contextExtensions:
      e2e: "true"
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
      annotations: {config.aether.io/upstreams: "authz-echo.$TEST_NS"}
    spec:
      serviceAccountName: client
      containers:
        - name: curl
          image: curlimages/curl:8.22.0@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777
          command: ["sleep", "infinity"]
YAML
	local d
	for d in authz-echo client; do
		kc -n "$TEST_NS" rollout status "deploy/$d" --timeout=180s >/dev/null ||
			die "workload '$d' never became Ready"
	done
	ok "workloads deployed"
}

URL() { echo "http://authz-echo.$TEST_NS.$MESH_DOMAIN:$OUTBOUND_PORT/"; }

# mesh_code [HEADER] — one request from the client; prints the HTTP code.
mesh_code() {
	local args=(curl -sS -o /dev/null -w '%{http_code}' --max-time 10)
	[ $# -gt 0 ] && args+=(-H "$1")
	kc -n "$TEST_NS" exec deploy/client -c curl -- "${args[@]}" "$(URL)" 2>/dev/null || echo 000
}

# await_code WANT TIMEOUT [HEADER]
await_code() {
	local want="$1" deadline=$((SECONDS + $2)) code
	shift 2
	while true; do
		code="$(mesh_code "$@")"
		if [ "$code" = "$want" ]; then
			printf '%s' "$code"
			return 0
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			printf '%s' "$code"
			return 1
		fi
		sleep 3
	done
}

# authz_stats — the node Envoy's ext_authz counters (plain /stats), read on the
# node-shared admin (hostNetwork; whichever epoch currently holds it).
authz_stats() {
	docker exec "$NODE" curl -s --max-time 3 'http://127.0.0.1:9901/stats?filter=ext_authz\.' 2>/dev/null || true
}

# authz_sum KIND — sum of http.*.ext_authz.KIND over every HCM.
authz_sum() {
	awk -v k=".ext_authz.$1:" 'index($1, k) { s += $2 } END { printf "%d", s }' <<<"$(authz_stats)"
}

# envoy_epoch — the node Envoy's hot-restart epoch.
envoy_epoch() {
	docker exec "$NODE" curl -s --max-time 3 http://127.0.0.1:9901/server_info 2>/dev/null |
		tr -d ' \n' | { grep -o '"restart_epoch":[0-9]*' || true; } | cut -d: -f2
}

verify_decisions() {
	log "a. decisions: allow header -> 200, none -> 403"
	local code ok0 den0 ok1 den1
	ok0="$(authz_sum ok)"
	den0="$(authz_sum denied)"
	code="$(await_code 200 180 "$ALLOW_HEADER")" ||
		die "the allowed request answered $code, expected 200 — the sidecar is not allowing (or the mesh path is down)"
	ok "allowed request -> 200"
	code="$(await_code 403 60)" ||
		die "the request without the allow header answered $code, expected 403 — the extAuthz filter is not on the path"
	ok "request without the header -> 403"
	ok1="$(authz_sum ok)"
	den1="$(authz_sum denied)"
	[ "$ok1" -gt "$ok0" ] || die "ext_authz.ok did not move ($ok0 -> $ok1): the 200 was not decided by the sidecar"
	[ "$den1" -gt "$den0" ] || die "ext_authz.denied did not move ($den0 -> $den1): the 403 was not the sidecar's decision"
	ok "the sidecar decided both (ext_authz ok $ok0 -> $ok1, denied $den0 -> $den1)"
}

# b. The live pod's container order (#1275).
verify_ordering() {
	if [ "$EXPECT" = "red" ]; then
		log "b. pod ordering: skipped (AUTHZ_EXPECT=red: the old chart has no native sidecar)"
		return
	fi
	log "b. pod ordering: authz is a started native sidecar, and proxy started after it"
	local pod policy started a_at p_at
	pod="$(kc -n "$NS" get pods -l app.kubernetes.io/component=proxy -o jsonpath='{.items[0].metadata.name}')"
	policy="$(kc -n "$NS" get pod "$pod" -o jsonpath='{.spec.initContainers[?(@.name=="authz")].restartPolicy}')"
	[ "$policy" = "Always" ] || die "$pod: no init container 'authz' with restartPolicy Always (got '$policy')"
	started="$(kc -n "$NS" get pod "$pod" -o jsonpath='{.status.initContainerStatuses[?(@.name=="authz")].started}')"
	[ "$started" = "true" ] || die "$pod: the authz sidecar is not started (started='$started')"
	a_at="$(kc -n "$NS" get pod "$pod" -o jsonpath='{.status.initContainerStatuses[?(@.name=="authz")].state.running.startedAt}')"
	p_at="$(kc -n "$NS" get pod "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="proxy")].state.running.startedAt}')"
	[ -n "$a_at" ] && [ -n "$p_at" ] || die "$pod: missing start times (authz='$a_at' proxy='$p_at')"
	# RFC 3339 UTC at second resolution compares as a string.
	[[ ! "$p_at" < "$a_at" ]] || die "$pod: proxy started at $p_at, BEFORE its authz sidecar ($a_at)"
	ok "$pod: authz (native sidecar) running since $a_at, proxy since $p_at"
}

# load_loop SECONDS OUT — allowed requests back to back from the client pod:
# "<unix s> <code>" per request.
load_loop() {
	# shellcheck disable=SC2016  # evaluated by the POD's shell
	kc -n "$TEST_NS" exec deploy/client -c curl -- sh -c '
		url="$1"; end=$(( $(date +%s) + $2 ))
		while [ "$(date +%s)" -lt "$end" ]; do
			c=$(curl -s -o /dev/null --max-time 5 -H "x-authz: letmein" -w "%{http_code}" "$url" 2>/dev/null)
			echo "$(date +%s) ${c:-000}"
		done
	' sh "$(URL)" "$1" >"$2" 2>/dev/null || true
}

# evict_opa_image — remove the sidecar image from the node, so the replacement
# pod has to pull it again: the condition #1275 was seen under (an image pull
# held the old layout's sidecar back 7–13 s).
evict_opa_image() {
	if [ "$EVICT_IMAGE" != "1" ]; then
		echo "  (AUTHZ_EVICT_IMAGE=$EVICT_IMAGE: the OPA image stays cached)"
		return
	fi
	docker exec "$NODE" crictl rmi "$OPA_IMAGE" >/dev/null 2>&1 ||
		die "could not evict $OPA_IMAGE from $NODE (crictl rmi)"
	if docker exec "$NODE" crictl inspecti "$OPA_IMAGE" >/dev/null 2>&1; then
		die "$OPA_IMAGE is still on $NODE after crictl rmi"
	fi
	ok "evicted $OPA_IMAGE from $NODE: the new pod's sidecar must pull it"
}

verify_roll() {
	log "c. rolling the proxy DaemonSet under continuous allowed requests (expect: $EXPECT)"
	local work err0 err1 epoch0 epoch1 i secs=120 t0 total f403 other
	local pids=()
	work="$(mktemp -d)"
	err0="$(authz_sum error)"
	epoch0="$(envoy_epoch)"
	[ -n "$epoch0" ] || die "could not read the node Envoy's restart_epoch before the roll"
	evict_opa_image
	for i in $(seq 1 "$LOOPS"); do
		load_loop "$secs" "$work/load.$i.out" &
		pids+=("$!")
	done
	sleep 5
	t0="$(date +%s)"
	kc -n "$NS" rollout restart ds/aether-proxy >/dev/null
	kc -n "$NS" rollout status ds/aether-proxy --timeout=300s >/dev/null || die "the proxy roll never completed"
	# rollout status returns once the new pod is Ready; the old one is deleted
	# about a second later and then terminates.
	for i in $(seq 1 90); do
		[ "$(kc -n "$NS" get pods -l app.kubernetes.io/component=proxy --no-headers 2>/dev/null | wc -l)" = 1 ] && break
		sleep 2
	done
	ok "roll done after $(($(date +%s) - t0))s; letting the load finish"
	wait "${pids[@]}" || true
	container_timeline | sed 's/^/    /'

	err1="$(authz_sum error)"
	epoch1="$(envoy_epoch)"
	total="$(cat "$work"/load.*.out | wc -l)"
	f403="$(cat "$work"/load.*.out | awk '$2 == "403"' | wc -l)"
	other="$(cat "$work"/load.*.out | awk '$2 != "200" && $2 != "403"' | wc -l)"
	echo "  allowed requests: $total, codes: $(cat "$work"/load.*.out | awk '{ print $2 }' | sort | uniq -c | tr '\n' ' ')"
	echo "  non-200 per second (relative to the rollout restart):"
	cat "$work"/load.*.out | awk -v t0="$t0" '$2 != "200" { n[$1 - t0 " " $2]++ } END { for (k in n) print k, n[k] }' |
		sort -n | awk '{ printf "    t=%+ds code=%s x%d\n", $1, $2, $3 }'
	echo "  ext_authz.error: $err0 -> $err1; envoy restart_epoch: $epoch0 -> ${epoch1:-?}"
	echo "AUTHZ_ROLL EXPECT=$EXPECT REQUESTS=$total F403=$f403 OTHER_NON200=$other EXT_AUTHZ_ERRORS=$((err1 - err0)) EPOCH=$epoch0->${epoch1:-?}"
	rm -rf "$work"

	[ "$total" -gt 0 ] || die "the load loops recorded no requests — the roll was not measured"
	[ -n "$epoch1" ] && [ "$epoch1" -gt "$epoch0" ] ||
		die "the node Envoy is at restart_epoch '${epoch1:-?}', not newer than $epoch0 — no hot-restart handoff happened, so this roll proves nothing"
	case "$EXPECT" in
	green)
		[ "$f403" = 0 ] || die "$f403 allowed requests got 403 during the proxy roll — the new Envoy served before its authz sidecar was up (#1275)"
		[ "$err1" = "$err0" ] || die "ext_authz.error grew by $((err1 - err0)) during the proxy roll — an Envoy had no authz behind its socket (#1275)"
		ok "zero 403s on the allowed path and zero ext_authz errors across the roll ($total requests; other non-200: $other)"
		;;
	red)
		[ "$f403" -gt 0 ] || [ "$err1" -gt "$err0" ] ||
			die "AUTHZ_EXPECT=red but the roll produced no 403 and no ext_authz error — the harness did not reproduce the gap"
		ok "RED as expected: $f403 x 403 and $((err1 - err0)) ext_authz errors across the roll"
		;;
	*) die "AUTHZ_EXPECT must be green or red" ;;
	esac
}

# apply_policy POLICY — the operator's way: a helm upgrade that differs from the
# installed release only in proxy.authzSidecar.opa.policy.
apply_policy() {
	local charts values
	charts="$(chart_dir)"
	values="$(mktemp)"
	write_values "$1" "$values"
	helm_aether "$charts" "$values" || die "the helm upgrade that changes the policy failed"
	rm -rf "$(dirname "$charts")" "$values"
}

# proxy_identity — what must NOT change when only the policy does: the proxy
# pod (name, UID), its containers' restart counts, the DaemonSet's generation
# and the node Envoy's hot-restart epoch.
proxy_identity() {
	# shellcheck disable=SC2016 # a jsonpath template, not shell
	kc -n "$NS" get pods -l app.kubernetes.io/component=proxy -o jsonpath='{range .items[*]}{.metadata.name}{" uid="}{.metadata.uid}{" authz-restarts="}{.status.initContainerStatuses[?(@.name=="authz")].restartCount}{" proxy-restarts="}{.status.containerStatuses[?(@.name=="proxy")].restartCount}{" "}{end}'
	printf 'ds-generation=%s envoy-epoch=%s' \
		"$(kc -n "$NS" get ds aether-proxy -o jsonpath='{.metadata.generation}')" "$(envoy_epoch)"
}

# node_has_policy POLICY — the kubelet's copy of the ConfigMap volume on the
# node (what the sidecar has mounted at /policy) holds exactly that text. The
# OPA image has no shell, so this reads the volume from the node.
node_has_policy() {
	local uid
	uid="$(kc -n "$NS" get pods -l app.kubernetes.io/component=proxy -o jsonpath='{.items[0].metadata.uid}')"
	[ "$(docker exec "$NODE" cat "/var/lib/kubelet/pods/$uid/volumes/kubernetes.io~configmap/opa-policy/policy.rego" 2>/dev/null)" = "$1" ]
}

# d. The policy changes; the pod does not (#1383).
verify_reload() {
	if [ "$EXPECT" = "red" ]; then
		log "d. policy reload: skipped (AUTHZ_EXPECT=red: the old chart's sidecar reads its policy once)"
		return
	fi
	log "d. policy reload: a changed policy reaches the running sidecar; the proxy pod is not replaced"
	local id0 id code err0 err1 t0 deadline
	[ "$(kc -n "$NS" get pods -l app.kubernetes.io/component=proxy --no-headers 2>/dev/null | wc -l)" = 1 ] ||
		die "expected exactly one proxy pod before the policy change"
	id0="$(proxy_identity)"
	err0="$(authz_sum error)"

	t0="$SECONDS"
	apply_policy "$POLICY_V2"
	code="$(await_code 403 "$RELOAD_TIMEOUT" "$ALLOW_HEADER")" ||
		die "the old allow header still answers $code ${RELOAD_TIMEOUT}s after the policy changed — the sidecar did not reload it"
	code="$(await_code 200 30 "$ALLOW_HEADER_V2")" ||
		die "the new allow header answers $code, expected 200 — the reloaded policy is not the new one"
	ok "the new policy decides $((SECONDS - t0))s after the upgrade began (old header -> 403, new header -> 200)"
	id="$(proxy_identity)"
	[ "$id" = "$id0" ] || die "the policy change replaced or restarted the proxy pod: before [$id0], after [$id]"
	ok "same pod, no restart, same epoch: $id"

	# A policy that does not parse: wait until the node really has it, give
	# the watcher time to act on it, then require that nothing changed.
	apply_policy "$POLICY_BAD"
	deadline=$((SECONDS + RELOAD_TIMEOUT))
	until node_has_policy "$POLICY_BAD"; do
		[ "$SECONDS" -lt "$deadline" ] || die "the bad policy never reached the node's ConfigMap volume in ${RELOAD_TIMEOUT}s"
		sleep 2
	done
	sleep 10
	code="$(mesh_code "$ALLOW_HEADER_V2")"
	[ "$code" = 200 ] || die "with a policy that does not parse on the node the allowed request answers $code, expected 200 — the last good policy was dropped"
	code="$(mesh_code "$ALLOW_HEADER")"
	[ "$code" = 403 ] || die "with a policy that does not parse on the node the denied request answers $code, expected 403"
	id="$(proxy_identity)"
	[ "$id" = "$id0" ] || die "the bad policy restarted or replaced something: before [$id0], after [$id]"
	# Read to EOF, never `grep -q` on a pipe (#1121).
	[ "$(kc -n "$NS" logs -l app.kubernetes.io/component=proxy -c authz --tail=2000 2>/dev/null | grep -c 'rego_parse_error')" -gt 0 ] ||
		die "the sidecar logged no rego_parse_error for the bad policy — the runbook's way to see a failed reload is gone"
	ok "a policy that does not parse changes nothing: the last good policy decides, the sidecar runs on and logs the error"

	apply_policy "$POLICY"
	code="$(await_code 200 "$RELOAD_TIMEOUT" "$ALLOW_HEADER")" ||
		die "the first policy did not come back (allow header answers $code) — the sidecar did not recover from the bad policy"
	code="$(await_code 403 30 "$ALLOW_HEADER_V2")" ||
		die "the second policy's header still answers $code after the first policy was restored"
	id="$(proxy_identity)"
	[ "$id" = "$id0" ] || die "recovering from the bad policy restarted or replaced something: before [$id0], after [$id]"
	err1="$(authz_sum error)"
	[ "$err1" = "$err0" ] || die "ext_authz.error grew by $((err1 - err0)) across the policy changes — a reload left Envoy without an answer"
	ok "a valid policy recovers it, still the same pod; zero ext_authz errors across all three changes"
}

verify() {
	[ -n "$OPA_IMAGE" ] || die "could not read the OPA preset image from $CHARTS_SRC/aether/values.yaml"
	verify_decisions
	verify_ordering
	verify_roll
	verify_decisions
	verify_ordering
	verify_reload
	log "all assertions passed (027 ext_authz: allow/deny decided by the sidecar; #1275: the sidecar is up before the proxy and the roll is error-free; #1383: a policy change is reloaded in place; expect=$EXPECT)"
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
