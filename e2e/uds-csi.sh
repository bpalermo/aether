#!/usr/bin/env bash
# Single-cluster kind e2e for proposal 039 Phase 1: the csi.aether.io CSI node
# plugin (udsCsi.enabled, on by default since Phase 2), proven in isolation:
# registration, publish, refusal, teardown and a plugin roll. Its use as the UDS
# carrier (Phase 2) is e2e/uds.sh's job. The test pods here are NOT
# mesh-managed, so the controller's pod webhook (which would deny the
# no-fsGroup pod at admission) never sees them and (iii) exercises the plugin's
# own FailedMount refusal.
#
# Assertions (verify):
#   i.   registration — the kubelet lists csi.aether.io in the node's CSINode.
#        The plugin serves the kubelet's registration API ITSELF (there is no
#        node-driver-registrar sidecar), so this is the proof that it works.
#   ii.  publish — a pod with securityContext.fsGroup: 65532 and an inline
#        `csi: {driver: csi.aether.io}` volume at /s gets:
#          - a tmpfs at /s, mode 2770, owned by group 65532;
#          - a nonroot app (e2e/udsecho, uid 65532) that binds a socket there,
#            which answers over HTTP;
#          - the per-pod tmpfs at /run/aether/uds/<pod-uid> ON THE HOST (the
#            Bidirectional propagation Phase 2's node proxy depends on),
#            mounted nosuid,nodev,noexec,nosymfollow — and a symlink the app
#            plants there is NOT followed from the host (ELOOP). That is the
#            view the node proxy gets (a HostToContainer clone of the host's
#            mount); the app's own view is a runtime re-bind with the flags
#            the container runtime picks, so the flags are asserted where the
#            confused deputy would act, not in the pod.
#          - the inode cap (#1107): the host mount carries nr_inodes=64
#            (`findmnt -o OPTIONS`, `df -i`), creating files past it fails
#            with ENOSPC, and the socket still answers with the tmpfs full.
#        Deleting the pod unmounts and removes the host directory.
#   iii. refusal — the same pod WITHOUT an fsGroup stays Pending with a
#        FailedMount event whose message names the fix ("fsGroup").
#   iv.  roll — after `kubectl rollout restart` of the plugin DaemonSet the
#        CSINode still lists the driver and a new pod still mounts.
#
# Usage: e2e/uds-csi.sh {up|verify|down}   (bare = up + verify)
#
# Prereqs: kind, docker, kubectl, helm, bazel (for the image build; CI sets
# UDS_CSI_SKIP_BUILD=1 and pre-loads the images from the nightly build artifact).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# <registry>/<namespace> every aether image is tagged under, from the single
# setting in bazel/img/registry.bzl (proposal 040) -- never a literal.
IMAGE_REGISTRY="$("$REPO_ROOT/scripts/image-registry.sh" prefix)"
CLUSTER="${UDS_CSI_CLUSTER:-uds-csi}"
CTX="kind-$CLUSTER"
NS="aether-system"
TEST_NS="aether-test"
GWAPI_VERSION="v1.6.2"
IMAGES=(agent mesh-dns proxy-supervisor cni-install registrar controller uds-csi udsecho)
CURL_IMAGE="curlimages/curl:8.22.0"
DRIVER="csi.aether.io"
UDS_ROOT="/run/aether/uds"
FS_GROUP=65532
# udsCsi.inodes' chart default: the per-pod tmpfs nr_inodes (#1107).
INODES=64
# The single kind node (kind-cluster.yaml).
NODE="${CLUSTER}-control-plane"

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
	printf '\033[1;33m  -- csinode / csidriver --\033[0m\n' >&2
	kc get csinode -o yaml 2>&1 | sed 's/^/    /' >&2 || true
	kc get csidriver "$DRIVER" -o yaml 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- uds-csi log --\033[0m\n' >&2
	kc -n "$NS" logs -l app.kubernetes.io/component=uds-csi --tail=60 --prefix 2>&1 |
		sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- test namespace events --\033[0m\n' >&2
	kc -n "$TEST_NS" get events --sort-by=.lastTimestamp 2>&1 | tail -20 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- host mounts under %s --\033[0m\n' "$UDS_ROOT" >&2
	docker exec "$NODE" grep " $UDS_ROOT" /proc/self/mountinfo 2>&1 | sed 's/^/    /' >&2 || true
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
	if [ "${UDS_CSI_SKIP_BUILD:-0}" = "1" ]; then
		ok "skipping image build (UDS_CSI_SKIP_BUILD=1; images pre-built)"
		return
	fi
	log "building + loading aether images (incl. uds-csi and the udsecho test workload)"
	local t
	for t in //agent/cmd/agent //agent/cmd/mesh-dns //agent/cmd/proxy-supervisor \
		//cni/cmd/cni-install //registrar/cmd/registrar //controller/cmd/controller //agent/cmd/uds-csi \
		//e2e/udsecho; do
		(cd "$REPO_ROOT" && bazel run "$t:image_load" >/dev/null 2>&1) || die "image build failed for $t"
	done
	ok "images built"
}

create_cluster() {
	# SIGPIPE rule (#1121, e2e/README.md): this script runs under pipefail, so
	# no pipeline may end in a reader that exits before its writer is done
	# (`head`, `grep -q`/`-m`, `awk '...; exit'`): the writer dies of SIGPIPE
	# and the pipeline fails with 141 at a random point. Read to EOF instead
	# (`grep -c ... >/dev/null`).
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
	kind create cluster --config "$cfg" --wait 60s >/dev/null
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
	log "installing the aether CRDs"
	helm --kube-context "$CTX" upgrade --install aether-crds "$charts/crds" \
		-n "$NS" --create-namespace --wait --timeout 2m >/dev/null || die "crds chart install failed"

	# SPIRE off: the driver has nothing to do with identity and must work
	# without it. Everything else is chart default, plus udsCsi.enabled.
	log "installing aether with udsCsi.enabled=true (SPIRE off)"
	# shellcheck disable=SC2046
	helm --kube-context "$CTX" upgrade --install aether "$charts/aether" \
		-n "$NS" --create-namespace \
		--set namespace.create=false \
		--set spire.enabled=false \
		--set edge.enabled=false \
		--set udsCsi.enabled=true \
		--set udsCsi.debug=true \
		$(img agent agent) $(img agent.meshDnsDaemon mesh-dns) \
		$(img proxy.supervisor proxy-supervisor) $(img cniInstall cni-install) \
		$(img registrar registrar) $(img controller controller) $(img udsCsi uds-csi) \
		--set proxy.image.pullPolicy=IfNotPresent \
		--timeout 5m >/dev/null || die "aether install failed"
	rm -rf "$(dirname "$charts")"

	kc -n "$NS" rollout status ds/aether-uds-csi --timeout=180s >/dev/null || die "the uds-csi DaemonSet never became Ready"
	kc -n "$NS" rollout status ds/aether-agent --timeout=240s >/dev/null || die "the agent DaemonSet never became Ready"
	ok "aether up (uds-csi Ready)"
}

up() {
	raise_inotify
	build_images
	create_cluster
	load_images
	install_crds
	install_aether
	kc create ns "$TEST_NS" >/dev/null 2>&1 || true
}

# --- workloads ---------------------------------------------------------------

# socket_pod <name> <with-fsgroup: yes|no>: the udsecho app (distroless nonroot,
# uid 65532) binding /s/a.sock on the CSI volume, plus a curl container to look
# at it from inside the pod.
socket_pod() {
	local name="$1" sc=""
	[ "$2" = yes ] && sc="securityContext: {fsGroup: ${FS_GROUP}}"
	kc apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $name, namespace: $TEST_NS, labels: {app: uds-csi-e2e}}
spec:
  $sc
  terminationGracePeriodSeconds: 1
  containers:
    - name: app
      image: ${IMAGE_REGISTRY}/udsecho:latest
      imagePullPolicy: Never
      args: ["--socket=/s/a.sock", "--text=served-by-${name}"]
      volumeMounts: [{name: s, mountPath: /s}]
    - name: probe
      image: $CURL_IMAGE
      command: ["sleep", "infinity"]
      volumeMounts: [{name: s, mountPath: /s}]
  volumes:
    - name: s
      csi: {driver: $DRIVER}
YAML
}

pod_uid() { kc -n "$TEST_NS" get pod "$1" -o jsonpath='{.metadata.uid}'; }
in_pod() { kc -n "$TEST_NS" exec "$1" -c probe -- sh -c "$2"; }

# --- assertions --------------------------------------------------------------

csinode_drivers() { kc get csinode "$NODE" -o jsonpath='{.spec.drivers[*].name}' 2>/dev/null || true; }

wait_registered() {
	local deadline=$((SECONDS + 90)) drivers=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		drivers="$(csinode_drivers)"
		case " $drivers " in *" $DRIVER "*) return 0 ;; esac
		sleep 2
	done
	die "CSINode $NODE lists [${drivers}] — the kubelet never registered $DRIVER"
}

verify_registration() {
	log "(i) the kubelet registered $DRIVER through the plugin's own registration socket"
	wait_registered
	ok "CSINode $NODE lists: $(csinode_drivers)"
	local policy
	policy="$(kc get csidriver "$DRIVER" -o jsonpath='{.spec.fsGroupPolicy} {.spec.podInfoOnMount} {.spec.volumeLifecycleModes[*]}')"
	[ "$policy" = "File true Ephemeral" ] || die "CSIDriver $DRIVER spec is '$policy', want 'File true Ephemeral'"
	ok "CSIDriver $DRIVER: fsGroupPolicy=File podInfoOnMount=true volumeLifecycleModes=[Ephemeral]"
	if kc -n "$NS" get ds aether-uds-csi -o jsonpath='{.spec.template.spec.containers[*].name}' | grep -c registrar >/dev/null; then
		die "the uds-csi DaemonSet has a registrar sidecar; the plugin must register itself"
	fi
	ok "no node-driver-registrar sidecar (containers: $(kc -n "$NS" get ds aether-uds-csi -o jsonpath='{.spec.template.spec.containers[*].name}'))"
}

# assert_published <pod>: every property of a published volume, from inside the
# pod and from the host.
assert_published() {
	local pod="$1" uid got mnt body
	kc -n "$TEST_NS" wait --for=condition=Ready "pod/$pod" --timeout=120s >/dev/null ||
		die "pod $pod never became Ready"
	uid="$(pod_uid "$pod")"

	got="$(in_pod "$pod" "stat -c '%a %g' /s" || true)"
	[ "$got" = "2770 $FS_GROUP" ] || die "/s in $pod is '$got' (mode gid), want '2770 $FS_GROUP'"
	ok "$pod: /s is mode 2770, group $FS_GROUP (stat: $got)"

	mnt="$(in_pod "$pod" "grep ' /s ' /proc/mounts" || true)"
	case "$mnt" in tmpfs\ /s\ tmpfs\ *) ;; *) die "/s in $pod is not a tmpfs mount: '$mnt'" ;; esac
	case "$mnt" in *size=1024k*nr_inodes=${INODES}*mode=2770*gid=${FS_GROUP}*) ;; *) die "/s in $pod lacks the plugin's size/nr_inodes/mode/gid: '$mnt'" ;; esac
	ok "$pod: /s is the plugin's tmpfs ($mnt)"

	got="$(in_pod "$pod" "stat -c '%F %g' /s/a.sock" || true)"
	[ "$got" = "socket $FS_GROUP" ] || die "/s/a.sock in $pod is '$got', want 'socket $FS_GROUP' (the nonroot app could not bind, or setgid did not apply)"
	body="$(in_pod "$pod" "curl -sS --max-time 5 --unix-socket /s/a.sock http://uds/" || true)"
	case "$body" in *"served-by-$pod"*) ;; *) die "the socket in $pod answered '$body'" ;; esac
	ok "$pod: the nonroot app bound /s/a.sock (group $FS_GROUP) and it answers: $body"

	got="$(docker exec "$NODE" stat -c '%a %g' "$UDS_ROOT/$uid" || true)"
	[ "$got" = "2770 $FS_GROUP" ] || die "host $UDS_ROOT/$uid is '$got', want '2770 $FS_GROUP'"
	docker exec "$NODE" test -S "$UDS_ROOT/$uid/a.sock" ||
		die "host $UDS_ROOT/$uid/a.sock is not a socket — the host does not see the pod's tmpfs"
	local hostmnt opt
	hostmnt="$(docker exec "$NODE" grep " $UDS_ROOT/$uid " /proc/self/mountinfo || true)"
	case "$hostmnt" in *" - tmpfs "*) ;; *) die "host has no tmpfs at $UDS_ROOT/$uid — the plugin's mount did not propagate: '$hostmnt'" ;; esac
	for opt in nosuid nodev noexec nosymfollow; do
		case ",$(awk '{print $6}' <<<"$hostmnt")," in *",$opt,"*) ;; *) die "host mount $UDS_ROOT/$uid lacks $opt: '$hostmnt'" ;; esac
	done
	ok "host: $UDS_ROOT/$uid is the pod's tmpfs (2770, gid $FS_GROUP, holds a.sock), mounted $(awk '{print $6}' <<<"$hostmnt")"

	# nosymfollow where it matters: the app plants a symlink to a file ROOT on
	# the host can read, and root on the host (standing in for the node proxy,
	# whose /run/aether view clones this mount) must NOT follow it. The link
	# must be created (the fsGroup can write /s) and the target must be
	# readable directly, or the refusal would be vacuous.
	in_pod "$pod" "ln -sf /etc/hostname /s/link" || die "$pod: could not create a symlink on /s"
	docker exec "$NODE" cat /etc/hostname >/dev/null || die "host /etc/hostname is not readable (control)"
	local follow
	if follow="$(docker exec "$NODE" cat "$UDS_ROOT/$uid/link" 2>&1)"; then
		die "host followed a symlink the app planted on its tmpfs — nosymfollow is not in effect (read: $follow)"
	fi
	ok "host: a symlink the app planted is not followed (${follow##*: })"

	assert_inode_cap "$pod" "$uid"
}

# assert_inode_cap <pod> <uid>: the per-pod tmpfs is capped at $INODES inodes
# (#1107). The host's mount carries the option and reports the cap; the app
# filling it gets ENOSPC well before anything `size` bounds, and the socket it
# already bound keeps answering with the tmpfs full.
assert_inode_cap() {
	local pod="$1" uid="$2" opts total free out made err body
	opts="$(docker exec "$NODE" findmnt -n -o OPTIONS -M "$UDS_ROOT/$uid")" ||
		die "findmnt found no mount at host $UDS_ROOT/$uid"
	case ",$opts," in *",nr_inodes=${INODES},"*) ;; *) die "host mount $UDS_ROOT/$uid lacks nr_inodes=${INODES}: '$opts'" ;; esac
	out="$(docker exec "$NODE" df --output=itotal,iavail "$UDS_ROOT/$uid")" || die "host df on $UDS_ROOT/$uid failed: $out"
	read -r total free <<<"$(tail -1 <<<"$out")"
	[ "$total" = "$INODES" ] || die "host df -i $UDS_ROOT/$uid reports $total inodes, want $INODES"
	ok "host: $UDS_ROOT/$uid is capped at $total inodes ($free free; options $opts)"

	# Fill it. A cap that does not bite would let all 200 through.
	# shellcheck disable=SC2016  # evaluated by the POD's shell
	out="$(in_pod "$pod" 'i=0; while [ $i -lt 200 ]; do
		if ! err=$(touch /s/fill.$i 2>&1); then echo "$i $err"; exit 0; fi
		i=$((i + 1)); done; echo "$i none"')"
	made="${out%% *}" err="${out#* }"
	[ "$made" -lt 200 ] || die "$pod created 200 files on a tmpfs capped at $INODES inodes — the cap is not in effect"
	[ "$made" -gt 0 ] || die "$pod could not create even one file on /s (control): $err"
	case "$err" in *"No space left on device"*) ;; *) die "$pod: file $made on /s failed with '$err', want ENOSPC" ;; esac
	free="$(docker exec "$NODE" df --output=iavail "$UDS_ROOT/$uid" | tail -1 | tr -d ' ' || true)"
	[ "$free" = 0 ] || die "host df -i $UDS_ROOT/$uid reports $free inodes free after ENOSPC, want 0"
	body="$(in_pod "$pod" "curl -sS --max-time 5 --unix-socket /s/a.sock http://uds/" || true)"
	case "$body" in *"served-by-$pod"*) ;; *) die "with the tmpfs full, the socket in $pod answered '$body'" ;; esac
	ok "$pod: file $((made + 1)) on /s failed with ENOSPC (${err##*: }); 0 inodes free and the socket still answers"
	in_pod "$pod" 'rm -f /s/fill.*' || die "$pod: could not clean up the fill files"
}

assert_unpublished() {
	local pod="$1" uid="$2" deadline=$((SECONDS + 60))
	kc -n "$TEST_NS" delete pod "$pod" --wait=true --timeout=90s >/dev/null ||
		die "pod $pod did not finish terminating (NodeUnpublishVolume stuck?)"
	while docker exec "$NODE" test -e "$UDS_ROOT/$uid"; do
		[ "$SECONDS" -lt "$deadline" ] || die "host $UDS_ROOT/$uid still exists after $pod was deleted"
		sleep 2
	done
	if docker exec "$NODE" grep -q " $UDS_ROOT/$uid " /proc/self/mountinfo; then
		die "host still has a mount at $UDS_ROOT/$uid after $pod was deleted"
	fi
	ok "$pod deleted: the host tmpfs $UDS_ROOT/$uid is unmounted and removed"
}

verify_publish() {
	log "(ii) a pod with fsGroup $FS_GROUP gets a mesh-owned per-pod tmpfs"
	kc -n "$TEST_NS" delete pod -l app=uds-csi-e2e --wait=true --timeout=60s >/dev/null 2>&1 || true
	socket_pod with-fsgroup yes
	assert_published with-fsgroup
	assert_unpublished with-fsgroup "$(pod_uid with-fsgroup)"
}

verify_no_fsgroup() {
	log "(iii) a pod WITHOUT an fsGroup is refused with a FailedMount naming the fix"
	socket_pod no-fsgroup no
	# Events are matched on THIS pod's UID, never its name: a FailedMount left by
	# an earlier run's same-named pod lives for an hour and would make this
	# assertion pass against a plugin that mounts it (seen on kind).
	local uid deadline=$((SECONDS + 120)) msg="" phase started
	uid="$(pod_uid no-fsgroup)"
	while [ "$SECONDS" -lt "$deadline" ]; do
		msg="$(kc -n "$TEST_NS" get events \
			--field-selector "involvedObject.uid=${uid},reason=FailedMount" \
			-o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null || true)"
		[ -n "$msg" ] && break
		sleep 3
	done
	[ -n "$msg" ] || die "no FailedMount event on pod no-fsgroup (uid $uid) within 120s"
	grep -q 'fsGroup' <<<"$msg" || die "the FailedMount event does not name the fix: $msg"
	# Still refused a beat later: Pending, and no container ever started.
	sleep 10
	phase="$(kc -n "$TEST_NS" get pod no-fsgroup -o jsonpath='{.status.phase}')"
	started="$(kc -n "$TEST_NS" get pod no-fsgroup -o jsonpath='{.status.containerStatuses[*].state.running.startedAt}')"
	[ "$phase" = Pending ] && [ -z "$started" ] ||
		die "pod no-fsgroup is $phase (running since: '${started}'), want Pending with no container started"
	ok "pod no-fsgroup (uid $uid) is Pending, no container started; FailedMount: $(head -1 <<<"$msg" | sed 's/.*desc = //')"
	kc -n "$TEST_NS" delete pod no-fsgroup --wait=true --timeout=60s >/dev/null || true
}

verify_roll() {
	log "(iv) a rollout restart of the plugin re-registers and still publishes"
	kc -n "$NS" rollout restart ds/aether-uds-csi >/dev/null
	kc -n "$NS" rollout status ds/aether-uds-csi --timeout=180s >/dev/null ||
		die "the uds-csi DaemonSet did not roll"
	wait_registered
	ok "after the roll CSINode $NODE still lists: $(csinode_drivers)"
	socket_pod after-roll yes
	assert_published after-roll
	assert_unpublished after-roll "$(pod_uid after-roll)"
}

verify() {
	verify_registration
	verify_publish
	verify_no_fsgroup
	verify_roll
	log "all assertions passed (proposal 039 Phase 1: registration, publish + nosymfollow + host propagation + inode cap, unpublish, fsGroup refusal, plugin roll)"
}

down() {
	log "tearing down"
	kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	ok "cluster '$CLUSTER' removed"
}

case "${1:-}" in
up) up ;;
test | verify) verify ;;
down) down ;;
"") up && verify ;;
*) die "usage: $0 {up|verify|down}" ;;
esac
