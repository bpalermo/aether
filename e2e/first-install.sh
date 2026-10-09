#!/usr/bin/env bash
# Single-cluster kind e2e for the DOCUMENTED first install (#1404): the commands
# docs/getting-started.md gives, with the chart's DEFAULT values, on an empty
# cluster, then one upgrade. Every other harness here passes
# `--set namespace.create=false` and a handful of feature switches, which is how
# a default that no helm command could install went unnoticed (#1403).
#
# The cluster enforces Pod Security admission at "baseline" for every namespace
# that says nothing else (a kube-apiserver AdmissionConfiguration; several
# distributions ship that default). A plain kind cluster enforces nothing, and
# the namespace labels the docs prescribe would then be untested.
#
# What is NOT a chart default here, and why:
#   - the image coordinates (repository/tag/digest/pullPolicy): the images under
#     test are the ones built from this tree, not the published ones the chart
#     pins by digest;
#   - `clusterName` and `meshDomain`: the documented command sets them.
# Nothing else is set: in particular `namespace.create`, `spire.enabled`,
# `edge.enabled` and `udsCsi.enabled` are whatever values.yaml says.
#
# Assertions (verify):
#   i.   docs — docs/getting-started.md (and the three shorter copies) give the
#        commands this script runs, and no longer the namespace-adoption
#        pre-step of #1398.
#   ii.  enforcement — a hostNetwork pod is refused in an unlabelled namespace,
#        so (iv) cannot pass on a cluster that enforces nothing.
#   iii. the combination no helm command can install (namespace.create=true for
#        the release's own namespace) fails the RENDER, names both ways out and
#        leaves nothing behind: no namespace, no release.
#   iv.  first install — CRDs chart; `kubectl create namespace` + the three
#        pod-security labels; ONE `helm upgrade --install ... --create-namespace`.
#        The release is `deployed`, its manifest holds no Namespace, no pod was
#        refused (zero FailedCreate events) and every workload becomes Ready:
#        agent, proxy, mesh-dns, uds-csi, registrar, controller. SPIRE is
#        installed first because the docs list it as a prerequisite of the
#        default `spire.enabled=true`.
#   v.   upgrade — the same command again: revision 2, `deployed`, the same
#        namespace (UID), still Ready.
#   v-a. rollback (#1471) — `helm rollback aether 1`, then `helm rollback
#        aether 2`: each leaves the release `deployed`, the seeded MeshConfig is
#        the same object with the operator's edit in it, and no revision's
#        manifest holds a MeshConfig (it is a hook). A deleted MeshConfig is
#        seeded again by the next upgrade, and that revision can be rolled back
#        to as well.
#   v-a2. the seed never deletes a MeshConfig (#1471) — the `lookup` that
#        decides to render the seed and the hook that creates it are not
#        atomic. A copy of the chart with a slow `pre-upgrade` hook ahead of
#        the seed holds an upgrade in that window; a MeshConfig created there
#        must be the same object afterwards, with its spec. The upgrade may
#        fail on "already exists" (and then succeeds when run again) or go
#        through (server-side apply); it must not delete or replace.
#   v-b. a release written by an older chart with namespace.create=false (no
#        marker on the agent ServiceAccount): the first upgrade is refused with
#        the `keep` command, goes through after it, and the next one needs
#        nothing.
#   vi.  upgrade safety (#1403) — a release that OWNS its namespace (every
#        install made while namespace.create defaulted to true) is upgraded
#        with the default values and then with an explicit
#        namespace.create=false: the Namespace stays in the manifest and keeps
#        its UID, and now carries helm.sh/resource-policy: keep. Then the
#        ownership annotations are removed, so the Namespace does leave the
#        manifest: `keep` on the live object stops Helm deleting it. In
#        between, the drift case: ownership annotations AND `keep` removed
#        (what a namespace rendered by an older chart looks like when its
#        annotations were lost before the first upgrade); the chart recognises
#        the namespace by its chart labels and keeps rendering it. The prober
#        chart gets the same case in (vii), and one more: a release that
#        rendered one namespace and is upgraded with namespace.name naming
#        another is refused until the old namespace is marked keep.
#   vii. the failed first install — run before (iv), on the empty cluster. A
#        release that was never deployed, whose failed revision lists a
#        Namespace it does not own: upgrading it would make Helm delete the
#        namespace. The upgrade is refused with the fix in the message
#        (`kubectl annotate namespace ... helm.sh/resource-policy=keep`),
#        whether or not the retry still asks for namespace.create=true, the
#        namespace is untouched, and after that command the same upgrade goes
#        through with the namespace kept. For the aether chart and for the
#        prober chart, which carries a copy of the guard.
#
# Usage: e2e/first-install.sh {up|verify|down}   (bare = up + verify)
#
#   FIRST_INSTALL_SOURCE=<checkout>  test that tree's charts/ and docs instead
#                                    of this one's (e.g. a `git worktree` of
#                                    main: the suite must be RED there).
#
# Prereqs: kind, docker, kubectl, helm, jq, bazel (for the image build; CI sets
# FIRST_INSTALL_SKIP_BUILD=1 and pre-loads the images from the nightly build
# artifact).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# The pinned kind binary / node image / kubectl (#1251): one Kubernetes for every
# e2e surface, local and CI alike.
# shellcheck source=e2e/kind-version.sh
. "$REPO_ROOT/e2e/kind-version.sh"
# <registry>/<namespace> every aether image is tagged under, from the single
# setting in bazel/registry/registry.bzl (proposal 040) -- never a literal.
IMAGE_REGISTRY="$("$REPO_ROOT/scripts/image-registry.sh" prefix)"
SOURCE="${FIRST_INSTALL_SOURCE:-$REPO_ROOT}"
CLUSTER="${FIRST_INSTALL_CLUSTER:-first-install}"
CTX="kind-$CLUSTER"
# The names the documented commands use.
NS="aether-system"
RELEASE="aether"
CLUSTER_NAME="my-cluster"
MESH_DOMAIN="aether.internal"
# The three labels the docs prescribe (what the chart used to put on a
# namespace it created).
PSA_LABELS=(
	pod-security.kubernetes.io/enforce=privileged
	pod-security.kubernetes.io/audit=privileged
	pod-security.kubernetes.io/warn=privileged
)
# SPIRE >= 1.15.2 (proposal 036, the SPIFFE Broker API); the same chart pins as
# e2e/l4routes.sh.
SPIRE_CHART_VERSION="${SPIRE_CHART_VERSION:-0.30.2}"
SPIRE_CRDS_VERSION="${SPIRE_CRDS_VERSION:-0.6.1}"
SPIRE_NS="spire-mgmt"
IMAGES=(agent mesh-dns proxy-supervisor cni-install registrar controller uds-csi)
WORKLOADS=(ds/aether-agent ds/aether-proxy ds/aether-mesh-dns ds/aether-uds-csi deploy/aether-registrar deploy/aether-controller)
# Where the API server's admission configuration lives on the host; mounted into
# the node, so it has to outlive `up`.
STATE_DIR="${TMPDIR:-/tmp}/aether-first-install-$CLUSTER"

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
die() {
	printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2
	dump_state
	exit 1
}

kc() { kubectl --context "$CTX" "$@"; }
hc() { helm --kube-context "$CTX" "$@"; }

dump_state() {
	kubectl config get-contexts "$CTX" >/dev/null 2>&1 || return 0
	printf '\033[1;33m  -- helm releases --\033[0m\n' >&2
	hc list -A -a 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- namespaces --\033[0m\n' >&2
	kc get ns --show-labels 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- pods --\033[0m\n' >&2
	kc get pods -A -o wide 2>&1 | sed 's/^/    /' >&2 || true
	printf '\033[1;33m  -- %s events --\033[0m\n' "$NS" >&2
	kc -n "$NS" get events --sort-by=.lastTimestamp 2>&1 | tail -30 | cut -c1-400 | sed 's/^/    /' >&2 || true
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
	if [ "${FIRST_INSTALL_SKIP_BUILD:-0}" = "1" ]; then
		ok "skipping image build (FIRST_INSTALL_SKIP_BUILD=1; images pre-built)"
		return
	fi
	log "building + loading aether images"
	local t
	for t in //agent/cmd/agent //agent/cmd/mesh-dns //agent/cmd/proxy-supervisor \
		//cni/cmd/cni-install //registrar/cmd/registrar //controller/cmd/controller //agent/cmd/uds-csi; do
		(cd "$REPO_ROOT" && bazel run "$t:image_load" >/dev/null 2>&1) || die "image build failed for $t"
	done
	ok "images built"
}

# A single node whose API server enforces Pod Security "baseline" wherever a
# namespace carries no label of its own. kube-system and kind's storage
# namespace are exempt: kindnet, kube-proxy and the provisioner are privileged.
create_cluster() {
	# SIGPIPE rule (#1121, e2e/README.md): no pipeline here ends in a reader
	# that exits before its writer is done.
	if kind get clusters 2>/dev/null | grep -cx "$CLUSTER" >/dev/null; then
		ok "kind cluster '$CLUSTER' already exists"
		return
	fi
	log "creating kind cluster '$CLUSTER' (Pod Security: baseline enforced by default)"
	mkdir -p "$STATE_DIR/psa"
	cat >"$STATE_DIR/psa/admission.yaml" <<'YAML'
apiVersion: apiserver.config.k8s.io/v1
kind: AdmissionConfiguration
plugins:
  - name: PodSecurity
    configuration:
      apiVersion: pod-security.admission.config.k8s.io/v1
      kind: PodSecurityConfiguration
      defaults:
        enforce: baseline
        enforce-version: latest
        audit: baseline
        audit-version: latest
        warn: baseline
        warn-version: latest
      exemptions:
        usernames: []
        runtimeClasses: []
        namespaces: [kube-system, local-path-storage]
YAML
	cat >"$STATE_DIR/kind.yaml" <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: $CLUSTER
networking:
  podSubnet: 10.14.0.0/16
  serviceSubnet: 10.114.0.0/16
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: $STATE_DIR/psa
        containerPath: /etc/kubernetes/psa
        readOnly: true
    kubeadmConfigPatches:
      - |
        kind: ClusterConfiguration
        apiServer:
          extraArgs:
            admission-control-config-file: /etc/kubernetes/psa/admission.yaml
          extraVolumes:
            - name: psa
              hostPath: /etc/kubernetes/psa
              mountPath: /etc/kubernetes/psa
              readOnly: true
              pathType: DirectoryOrCreate
YAML
	kind_require_binary || die "kind binary does not match e2e/kind-version.sh (see above)"
	kind create cluster --image "$KIND_NODE_IMAGE" --config "$STATE_DIR/kind.yaml" --wait 120s >/dev/null
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

# The prerequisite the docs name for the default spire.enabled=true
# (docs/getting-started.md, 2a): SPIRE with the Broker Endpoint enabled for the
# aether agent's identity. Its own namespace is labelled privileged for the same
# reason aether's is (the SPIRE agent and CSI driver use hostPath and hostPID).
install_spire() {
	log "installing the documented prerequisite: SPIRE (trust domain $MESH_DOMAIN)"
	hc repo add spiffe https://spiffe.github.io/helm-charts-hardened/ >/dev/null 2>&1 || true
	hc repo update >/dev/null 2>&1 || true
	kc create ns "$SPIRE_NS" >/dev/null 2>&1 || true
	kc label ns "$SPIRE_NS" --overwrite "${PSA_LABELS[@]}" >/dev/null || die "could not label $SPIRE_NS"
	hc upgrade --install spire-crds spiffe/spire-crds \
		-n "$SPIRE_NS" --version "$SPIRE_CRDS_VERSION" --wait --timeout 3m >/dev/null ||
		die "spire-crds install failed"
	hc upgrade --install spire spiffe/spire \
		-n "$SPIRE_NS" --version "$SPIRE_CHART_VERSION" \
		--set global.spire.trustDomain="$MESH_DOMAIN" \
		--set global.spire.clusterName="$CLUSTER_NAME" \
		--set "spiffe-oidc-discovery-provider.enabled=false" \
		--set "spire-agent.sockets.broker.enabled=true" \
		--set "spire-agent.sockets.broker.mountOnHost=true" \
		--set "spire-agent.brokerAPI.brokers.aether-agent.enabled=true" \
		--set "spire-agent.brokerAPI.brokers.aether-agent.idTemplate=spiffe://$MESH_DOMAIN/ns/$NS/sa/aether-agent" \
		--set "spire-agent.brokerAPI.brokers.aether-agent.allowedReferenceTypes[0].typeURL=type.googleapis.com/spiffe.broker.KubernetesObjectReference" \
		--set "spire-agent.brokerAPI.brokers.aether-agent.allowedReferenceTypes[0].allowOverTCP=false" \
		--set "spire-agent.workloadAttestors.k8s.brokerAPI.accessPolicy=permissive" \
		--set "spire-agent.workloadAttestors.k8s.brokerAPI.brokers.aether-agent.enabled=true" \
		--wait --timeout 8m >/dev/null || die "SPIRE install failed"
	ok "SPIRE up"
}

up() {
	raise_inotify
	build_images
	create_cluster
	load_images
	install_spire
	log "environment ready: an empty cluster (no aether namespace, no aether release)"
}

# Render the chart placeholders into a temp copy so the source tree stays clean.
CHARTS=""
charts_dir() {
	[ -n "$CHARTS" ] && return 0
	local out
	out="$(mktemp -d)"
	cp -r "$SOURCE/charts" "$out/"
	sed -i -e 's/{GIT_COMMIT}/e2e/' -e 's/{STABLE_GIT_VERSION}/0.0.0-e2e/' \
		"$out/charts/crds/Chart.yaml" "$out/charts/aether/Chart.yaml" "$out/charts/prober/Chart.yaml"
	CHARTS="$out/charts"
}

# The image coordinates: the only values this suite sets that the documented
# command does not (see the header).
image_args() {
	local img
	img() { printf '%s\n' "--set=$1.image.repository=${IMAGE_REGISTRY}/$2" "--set=$1.image.tag=latest" "--set=$1.image.digest=" "--set=$1.image.pullPolicy=Never"; }
	img agent agent
	img agent.meshDnsDaemon mesh-dns
	img proxy.supervisor proxy-supervisor
	img cniInstall cni-install
	img registrar registrar
	img controller controller
	img udsCsi uds-csi
	printf '%s\n' "--set=proxy.image.pullPolicy=IfNotPresent"
}

# The documented step 3, against the chart in $SOURCE. Extra arguments are
# appended (the upgrade-safety legs pass a namespace value). AETHER_CHART
# replaces the chart directory for one call (leg v-a2's copy with a slow hook).
documented_helm_install() {
	local args
	mapfile -t args < <(image_args)
	hc upgrade --install "$RELEASE" "${AETHER_CHART:-$CHARTS/aether}" \
		--namespace "$NS" --create-namespace \
		--set "clusterName=$CLUSTER_NAME" \
		--set "meshDomain=$MESH_DOMAIN" \
		"${args[@]}" "$@"
}

release_field() { hc list -n "$NS" -a --filter "^${RELEASE}\$" -o json | jq -r ".[0].$1 // empty"; }
ns_uid() { kc get ns "$NS" -o jsonpath='{.metadata.uid}' 2>/dev/null || true; }
ns_phase() { kc get ns "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || true; }
ns_annotation() { kc get ns "$NS" -o json | jq -r --arg k "$1" '.metadata.annotations[$k] // empty'; }
# How many Namespace documents the release's stored manifest holds.
manifest_namespaces() { hc get manifest "$RELEASE" -n "$NS" | grep -c '^kind: Namespace' || true; }
refused_pods() { kc -n "$NS" get events --field-selector reason=FailedCreate -o name 2>/dev/null | grep -c . || true; }

wait_ready() {
	local w
	for w in "${WORKLOADS[@]}"; do
		kc -n "$NS" rollout status "$w" --timeout=420s >/dev/null || die "$w never became Ready ($1)"
	done
	ok "every workload is Ready ($1): ${WORKLOADS[*]}"
}

# Back to an empty cluster, so `verify` can be run again.
reset_aether() {
	hc uninstall "$RELEASE" -n "$NS" >/dev/null 2>&1 || true
	# The proxy pod's termination grace period is 180 s, and the namespace is
	# only gone some time after its last pod.
	kc delete ns "$NS" --wait=true --timeout=480s >/dev/null 2>&1 || true
	[ -z "$(ns_uid)" ] || die "the namespace $NS could not be removed"
}

# i. The docs give the commands this suite runs.
verify_docs() {
	log "i. the docs give these commands"
	local doc f
	doc="$SOURCE/docs/getting-started.md"
	local want
	for want in \
		"kubectl create namespace $NS" \
		"kubectl label namespace $NS" \
		"${PSA_LABELS[@]}" \
		"--namespace $NS --create-namespace"; do
		grep -cF -- "$want" "$doc" >/dev/null || die "docs/getting-started.md does not give '$want': the documented install and this suite have drifted apart"
	done
	# The pre-step #1398 documented (creating the namespace with Helm's
	# ownership metadata so the chart would adopt it) is gone everywhere.
	for f in docs/getting-started.md README.md charts/README.md website/pages/index.md; do
		if grep -cF -- "meta.helm.sh/release-name=" "$SOURCE/$f" >/dev/null; then
			die "$f still documents the namespace-adoption pre-step (meta.helm.sh/release-name=...)"
		fi
		grep -cF -- "--create-namespace" "$SOURCE/$f" >/dev/null || die "$f does not install with --create-namespace"
	done
	ok "docs/getting-started.md, README.md, charts/README.md and website/pages/index.md install with --create-namespace and no adoption pre-step"
}

# ii. The cluster really refuses the agent's kind of pod without the labels.
verify_enforcement() {
	log "ii. Pod Security admission is enforced where a namespace says nothing"
	local out
	out="$(kc -n default run first-install-psa-probe --image=registry.k8s.io/pause:3.10 \
		--overrides='{"spec":{"hostNetwork":true}}' --dry-run=server 2>&1 || true)"
	case "$out" in
	*'violates PodSecurity "baseline'*) ok "a hostNetwork pod is refused in an unlabelled namespace" ;;
	*) die "a hostNetwork pod was NOT refused in an unlabelled namespace, so this cluster cannot show the labels are needed: $out" ;;
	esac
}

# iii. namespace.create=true for the release's own namespace: a render error
# that names both ways out, and nothing left behind.
verify_broken_combination() {
	log "iii. namespace.create=true for the release's own namespace fails the render"
	local out
	if out="$(documented_helm_install --set namespace.create=true 2>&1)"; then
		die "helm installed with namespace.create=true into the release's own namespace; expected a render error"
	fi
	case "$out" in
	*"is the namespace this release is stored in"*) ;;
	*) die "the install failed, but not at the render with the expected reason: $out" ;;
	esac
	case "$out" in
	*"leave namespace.create at its default (false)"*"store the release in another namespace"*) ;;
	*) die "the render error does not name both ways out: $out" ;;
	esac
	[ -z "$(ns_uid)" ] || die "the failed render left the namespace $NS behind"
	[ -z "$(release_field status)" ] || die "the failed render left a release behind"
	ok "render error naming both ways out; no namespace and no release left behind"
}

# iv. The documented first install.
verify_first_install() {
	log "iv. the documented first install, default values, empty cluster"
	# 1) the CRDs chart, as documented: no namespace flag.
	hc upgrade --install aether-crds "$CHARTS/crds" --wait --timeout 2m >/dev/null || die "the crds chart did not install"
	# 2) the namespace, labelled for Pod Security admission.
	kc create namespace "$NS" >/dev/null || die "kubectl create namespace $NS failed"
	kc label namespace "$NS" "${PSA_LABELS[@]}" >/dev/null || die "kubectl label namespace $NS failed"
	# 3) the system: one helm command.
	local out
	out="$(documented_helm_install 2>&1)" || die "the documented install command failed: $out"
	[ "$(release_field status)" = "deployed" ] || die "the release is '$(release_field status)', want deployed"
	[ "$(release_field revision)" = "1" ] || die "the release is at revision $(release_field revision), want 1"
	[ "$(manifest_namespaces)" = "0" ] || die "the release's manifest holds a Namespace; with the default values the chart must not render one"
	ok "release deployed at revision 1; its manifest holds no Namespace"
	wait_ready "first install"
	[ "$(refused_pods)" = "0" ] || die "$(refused_pods) pod creation(s) were refused in $NS (FailedCreate)"
	ok "no pod was refused by Pod Security admission"
}

# v. One upgrade with the same command.
verify_upgrade() {
	log "v. the same command again (an upgrade)"
	local uid out
	uid="$(ns_uid)"
	out="$(documented_helm_install 2>&1)" || die "the upgrade failed: $out"
	[ "$(release_field status)" = "deployed" ] || die "after the upgrade the release is '$(release_field status)', want deployed"
	[ "$(release_field revision)" = "2" ] || die "after the upgrade the release is at revision $(release_field revision), want 2"
	[ "$(ns_uid)" = "$uid" ] && [ "$(ns_phase)" = "Active" ] || die "the namespace changed across the upgrade (uid $uid -> $(ns_uid), phase $(ns_phase))"
	ok "release deployed at revision 2; namespace unchanged (uid $uid)"
	wait_ready "after the upgrade"
}

# One field of the seeded MeshConfig (a jq path), or nothing.
meshconfig_field() { kc -n "$NS" get meshconfig default -o json 2>/dev/null | jq -r "$1 // empty"; }
# How many MeshConfig documents revision $1's stored manifest holds.
manifest_meshconfigs() { hc get manifest "$RELEASE" -n "$NS" --revision "$1" | grep -c '^kind: MeshConfig' || true; }

# v-a. Back to the first revision, and forward again (#1471). The chart seeds
# the `default` MeshConfig once and the operator owns it afterwards. While the
# seed was a release object it was in revision 1's manifest and in no later
# one, and still live: `helm rollback <release> 1` failed with `no MeshConfig
# with the name "default" found` and left the release `failed`.
verify_rollback_to_first() {
	log "v-a. helm rollback to the release's first revision, and forward again (#1471)"
	local uid out rev
	uid="$(meshconfig_field .metadata.uid)"
	[ -n "$uid" ] || die "the install seeded no MeshConfig 'default' in $NS"
	# The operator's edit: it must be there after every step below.
	kc -n "$NS" patch meshconfig default --type merge -p '{"spec":{"proxy":{"accessLogsEnabled":true}}}' >/dev/null ||
		die "could not edit the seeded MeshConfig"
	out="$(hc rollback "$RELEASE" 1 -n "$NS" 2>&1)" || die "helm rollback $RELEASE 1 failed: $out"
	[ "$(release_field status)" = "deployed" ] || die "after the rollback to revision 1 the release is '$(release_field status)', want deployed"
	[ "$(release_field revision)" = "3" ] || die "after the rollback to revision 1 the release is at revision $(release_field revision), want 3"
	out="$(hc rollback "$RELEASE" 2 -n "$NS" 2>&1)" || die "helm rollback $RELEASE 2 failed: $out"
	[ "$(release_field status)" = "deployed" ] || die "after the rollback to revision 2 the release is '$(release_field status)', want deployed"
	[ "$(release_field revision)" = "4" ] || die "after the rollback to revision 2 the release is at revision $(release_field revision), want 4"
	ok "rolled back to revision 1 (revision 3, deployed) and to revision 2 (revision 4, deployed)"
	[ "$(meshconfig_field .metadata.uid)" = "$uid" ] ||
		die "the MeshConfig was deleted or replaced by a rollback (uid $uid -> '$(meshconfig_field .metadata.uid)')"
	[ "$(meshconfig_field .spec.proxy.accessLogsEnabled)" = "true" ] ||
		die "a rollback reverted the operator's edit of the MeshConfig (spec: $(meshconfig_field '.spec | tojson'))"
	ok "the MeshConfig is the same object (uid $uid) and kept the operator's edit"
	# Why it works: the seed is in no revision's manifest.
	for rev in 1 2 3 4; do
		[ "$(manifest_meshconfigs "$rev")" = "0" ] ||
			die "revision $rev's manifest holds a MeshConfig: a rollback to it fails once a later revision exists"
	done
	ok "no revision's manifest holds a MeshConfig"
	wait_ready "after the rollbacks"

	# An absent MeshConfig is seeded again by the next upgrade, outside the
	# manifest as on the install.
	kc -n "$NS" delete meshconfig default --wait=true >/dev/null || die "could not delete the MeshConfig"
	out="$(documented_helm_install 2>&1)" || die "the upgrade over a deleted MeshConfig failed: $out"
	[ "$(release_field revision)" = "5" ] || die "after that upgrade the release is at revision $(release_field revision), want 5"
	uid="$(meshconfig_field .metadata.uid)"
	[ -n "$uid" ] || die "the upgrade did not seed the MeshConfig again"
	[ "$(manifest_meshconfigs 5)" = "0" ] || die "the upgrade seeded the MeshConfig as a release object (revision 5's manifest holds it)"
	out="$(hc rollback "$RELEASE" 4 -n "$NS" 2>&1)" || die "helm rollback $RELEASE 4 failed: $out"
	out="$(hc rollback "$RELEASE" 5 -n "$NS" 2>&1)" || die "helm rollback $RELEASE 5 (the revision that seeded the MeshConfig again) failed: $out"
	[ "$(release_field status)" = "deployed" ] || die "after those rollbacks the release is '$(release_field status)', want deployed"
	# Revision 5 stores the seed as a hook; a rollback to it must not run it
	# (Helm would delete the live MeshConfig first).
	[ "$(meshconfig_field .metadata.uid)" = "$uid" ] ||
		die "a rollback to the revision that seeded the MeshConfig replaced it (uid $uid -> '$(meshconfig_field .metadata.uid)')"
	ok "a deleted MeshConfig is seeded again by the next upgrade (uid $uid); that revision can be rolled back to, and the rollback leaves the object alone"
	wait_ready "after the MeshConfig was seeded again"
}

# v-a2. The seed hook never deletes a MeshConfig (#1471). The chart renders the
# seed when a `lookup` finds no MeshConfig, and Helm creates it later, as a
# pre-upgrade hook: an object created in between is not the chart's to touch.
# Left to Helm's default hook-delete-policy (before-hook-creation) the hook
# deleted that object and put the seed in its place, and the upgrade reported
# success. The window is held open here by a second pre-upgrade hook of a lower
# weight, a Pod that sleeps, in a copy of the chart.
verify_seed_race() {
	log "v-a2. a MeshConfig created between the render and the seed hook is not deleted (#1471)"
	local image slow out logf pid rc phase i uid after
	# A sleeper that is on every kind node without a pull: the image kind's
	# storage provisioner runs its helper pods with.
	image="$(kc -n local-path-storage get cm local-path-config -o jsonpath='{.data.helperPod\.yaml}' | awk '$1 == "image:" { print $2; exit }')"
	[ -n "$image" ] || die "could not read the helper image from local-path-storage/local-path-config"
	slow="$(mktemp -d)"
	logf="$slow/upgrade.log"
	cp -r "$CHARTS/aether" "$slow/aether"
	cat >"$slow/aether/templates/zz-e2e-slow-hook.yaml" <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: e2e-slow-pre-upgrade
  namespace: {{ .Release.Namespace }}
  annotations:
    helm.sh/hook: pre-upgrade
    helm.sh/hook-weight: "-5"
    helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 0
  containers:
    - name: sleep
      image: $image
      imagePullPolicy: IfNotPresent
      command: ["sleep", "15"]
YAML
	kc -n "$NS" delete meshconfig default --wait=true >/dev/null || die "could not delete the MeshConfig"
	AETHER_CHART="$slow/aether" documented_helm_install >"$logf" 2>&1 &
	pid=$!
	# Once the slow hook is running, the chart is rendered (no MeshConfig
	# was live, so the seed is in it) and the seed hook has not run yet.
	phase=""
	for i in $(seq 1 120); do
		phase="$(kc -n "$NS" get pod e2e-slow-pre-upgrade -o jsonpath='{.status.phase}' 2>/dev/null || true)"
		[ "$phase" = "Running" ] && break
		sleep 0.5
	done
	if [ "$phase" != "Running" ]; then
		wait "$pid" || true
		die "the slow pre-upgrade hook never ran (pod phase '$phase'): $(cat "$logf")"
	fi
	[ -z "$(meshconfig_field .metadata.uid)" ] || die "a MeshConfig exists before the test created one: the window was not held open"
	# The operator's object, with a spec the seed does not have.
	kc create -f - >/dev/null <<YAML || die "could not create the MeshConfig"
apiVersion: config.aether.io/v1
kind: MeshConfig
metadata:
  name: default
  namespace: $NS
spec:
  proxy:
    accessLogsEnabled: true
YAML
	uid="$(meshconfig_field .metadata.uid)"
	rc=0
	wait "$pid" || rc=$?
	out="$(cat "$logf")"
	after="$(meshconfig_field .metadata.uid)"
	[ -n "$after" ] || die "the upgrade deleted the MeshConfig created during it (uid $uid) and left none (helm exit $rc): $out"
	[ "$after" = "$uid" ] ||
		die "the upgrade deleted the MeshConfig created during it and put the seed in its place (uid $uid -> $after, helm exit $rc)"
	[ "$(meshconfig_field .spec.proxy.accessLogsEnabled)" = "true" ] ||
		die "the upgrade overwrote the spec of the MeshConfig created during it (spec: $(meshconfig_field '.spec | tojson'))"
	if [ "$rc" -ne 0 ]; then
		# Client-side creation: the seed hook fails on the object that is
		# there, and the same command again succeeds (the object is live, so
		# the seed is not rendered).
		case "$out" in
		*"already exists"*) ;;
		*) die "the upgrade failed, and not on the MeshConfig that already exists: $out" ;;
		esac
		[ "$(release_field status)" = "failed" ] || die "after the failed upgrade the release is '$(release_field status)', want failed"
		out="$(documented_helm_install 2>&1)" || die "the upgrade run again after 'already exists' failed: $out"
		ok "the upgrade failed on 'already exists' and left the MeshConfig alone; run again, it succeeded"
	else
		ok "the upgrade went through and left the MeshConfig in place"
	fi
	[ "$(release_field status)" = "deployed" ] || die "after leg v-a2 the release is '$(release_field status)', want deployed"
	[ "$(meshconfig_field .metadata.uid)" = "$uid" ] || die "the upgrade run again replaced the MeshConfig (uid $uid -> '$(meshconfig_field .metadata.uid)')"
	[ "$(meshconfig_field .spec.proxy.accessLogsEnabled)" = "true" ] || die "the upgrade run again overwrote the MeshConfig's spec"
	ok "the MeshConfig created between the render and the seed hook is the same object (uid $uid), with its spec"
	kc -n "$NS" delete pod e2e-slow-pre-upgrade --ignore-not-found >/dev/null 2>&1 || true
	rm -rf "$slow"
	wait_ready "after the seed race"
}

# One upgrade of the upgrade-safety leg: the namespace must be the same, Active
# object afterwards, and in or out of the manifest as stated.
safe_upgrade() {
	local what="$1" uid="$2" want_in_manifest="$3" out
	shift 3
	out="$(documented_helm_install "$@" 2>&1)" || die "$what: the upgrade failed: $out"
	# Helm deletes in the background: give a wrongly issued delete time to show.
	sleep 5
	[ "$(ns_uid)" = "$uid" ] && [ "$(ns_phase)" = "Active" ] ||
		die "$what: THE NAMESPACE WAS DELETED OR REPLACED (uid $uid -> '$(ns_uid)', phase '$(ns_phase)')"
	[ "$(manifest_namespaces)" = "$want_in_manifest" ] ||
		die "$what: the release's manifest holds $(manifest_namespaces) Namespace(s), want $want_in_manifest"
	ok "$what: namespace kept (uid $uid, Active); Namespace documents in the manifest: $want_in_manifest"
}

# v-b. A release written by an OLDER chart with namespace.create=false. Its
# manifest never held the Namespace, so nothing could be deleted, but the chart
# cannot tell it from an older release whose Namespace lost every sign of
# ownership: neither carries the marker this chart stamps on the agent
# ServiceAccount. The first upgrade is therefore refused once, with the `keep`
# command; after it the upgrade goes through, and the one after that needs
# nothing (the marker is there), even with `keep` removed again.
verify_legacy_unmarked() {
	log "v-b. a release of an older chart with namespace.create=false: asked for keep once, then never again"
	local uid out
	uid="$(ns_uid)"
	# What an older chart's ServiceAccount looks like: no marker.
	kc -n "$NS" annotate serviceaccount aether-agent aether.io/release-namespace-rendered- >/dev/null
	if out="$(documented_helm_install 2>&1)"; then
		die "the upgrade of an unmarked release whose namespace is not protected was not refused"
	fi
	case "$out" in
	*"Helm could DELETE it"*"kubectl annotate namespace $NS helm.sh/resource-policy=keep"*) ;;
	*) die "the upgrade failed, but not with the command that makes it safe: $out" ;;
	esac
	[ "$(ns_uid)" = "$uid" ] && [ "$(ns_phase)" = "Active" ] || die "THE NAMESPACE WAS DELETED by a refused upgrade"
	ok "refused once, naming kubectl annotate namespace $NS helm.sh/resource-policy=keep"
	kc annotate namespace "$NS" helm.sh/resource-policy=keep >/dev/null
	safe_upgrade "after keep" "$uid" 0
	kc annotate namespace "$NS" helm.sh/resource-policy- >/dev/null
	safe_upgrade "the next upgrade, keep removed again (the marker is stamped)" "$uid" 0
}

# vi. A release that owns its namespace must never lose it.
verify_upgrade_safety() {
	log "vi. upgrade safety: a release that owns its namespace (the old default)"
	# Put the running release into the state every install made while
	# namespace.create defaulted to true is in: the Namespace carries Helm's
	# ownership metadata for this release and is part of its manifest. (Done on
	# the live install rather than a second one: removing the first takes the
	# proxy pod's whole termination grace period.)
	local uid out
	uid="$(ns_uid)"
	kc label namespace "$NS" app.kubernetes.io/managed-by=Helm >/dev/null
	kc annotate namespace "$NS" "meta.helm.sh/release-name=$RELEASE" "meta.helm.sh/release-namespace=$NS" >/dev/null
	out="$(documented_helm_install 2>&1)" || die "upgrading over a namespace the release owns failed: $out"
	[ "$(manifest_namespaces)" = "1" ] || die "a namespace the release owns is not in its manifest (the chart must keep rendering it)"
	[ "$(ns_uid)" = "$uid" ] || die "the namespace was replaced while the release took ownership of it"
	ok "the release owns its namespace: the Namespace is in the manifest (uid $uid)"

	safe_upgrade "default values" "$uid" 1
	safe_upgrade "explicit namespace.create=false" "$uid" 1 --set namespace.create=false
	[ "$(ns_annotation helm.sh/resource-policy)" = "keep" ] || die "the owned namespace does not carry helm.sh/resource-policy: keep"
	ok "the namespace carries helm.sh/resource-policy: keep"

	# Drift before the first upgrade to this chart: the Namespace is in the
	# stored manifest, as an older chart left it, but the live object has lost
	# Helm's ownership annotations and never had `keep`. The chart still
	# recognises it by the labels every chart version put on it, keeps
	# rendering it, and Helm stamps ownership and `keep` back.
	kc annotate namespace "$NS" meta.helm.sh/release-name- meta.helm.sh/release-namespace- helm.sh/resource-policy- >/dev/null
	safe_upgrade "ownership annotations lost before keep was ever stamped" "$uid" 1
	[ "$(ns_annotation helm.sh/resource-policy)" = "keep" ] && [ "$(ns_annotation meta.helm.sh/release-name)" = "$RELEASE" ] ||
		die "the upgrade did not put keep and the ownership annotations back on the namespace"
	ok "keep and the ownership annotations are back on the namespace"

	# The second lock: once the release no longer owns the namespace, the
	# Namespace leaves the manifest, which is exactly when Helm deletes an
	# object. `keep` on the live object is what stops it.
	kc annotate namespace "$NS" meta.helm.sh/release-name- meta.helm.sh/release-namespace- >/dev/null
	safe_upgrade "ownership annotations removed (the Namespace leaves the manifest)" "$uid" 0
	wait_ready "after the upgrade-safety upgrades"
}

# vii. The one upgrade that could still delete a namespace is refused.
#
# The state: a release that has NEVER been deployed, whose failed revision lists
# a Namespace the release does not own and nothing protects. That is what a
# first install of a chart older than this one left behind when it was run with
# --create-namespace ("already exists"). Helm upgrades from the failed revision,
# and an object that left the manifest is deleted. Rebuilt here with this
# chart: a first install whose manifest holds the Namespace and which fails
# because the API server rejects one workload (an imagePullPolicy it does not
# know), after which the ownership annotations and `keep` are taken off the
# namespace.
#
# guard_leg <label> <install function> <arguments of the failing first install...>
# with RELEASE and NS set by the caller.
guard_leg() {
	local label="$1" install="$2" uid out
	shift 2
	if out="$("$install" "$@" 2>&1)"; then
		die "$label: the first install with $* succeeded; this leg needs it to fail"
	fi
	[ "$(release_field status)" = "failed" ] || die "$label: the release is '$(release_field status)' after the rejected first install, want failed: $out"
	[ "$(manifest_namespaces)" = "1" ] || die "$label: the failed revision's manifest does not list the Namespace"
	# What that namespace looked like: made by `helm --create-namespace`, so
	# with no ownership annotations, no `keep` and none of the chart's labels.
	kc annotate namespace "$NS" meta.helm.sh/release-name- meta.helm.sh/release-namespace- helm.sh/resource-policy- >/dev/null
	kc label namespace "$NS" app.kubernetes.io/managed-by- app.kubernetes.io/instance- >/dev/null
	uid="$(ns_uid)"
	ok "$label: a failed first install whose manifest lists a Namespace the release no longer owns (uid $uid)"

	# Two retries, both refused by the SAME message (the one with the fix in
	# it): the command that failed, unchanged, which still asks for
	# namespace.create=true (#1405: the operator must not have to get past a
	# "cannot create" error first to learn about `keep`), and the command with
	# the default.
	local how
	for how in "--set namespace.create=true" ""; do
		# shellcheck disable=SC2086 # $how is zero or two words on purpose
		if out="$("$install" $how 2>&1)"; then
			sleep 5
			die "$label: the upgrade over the failed first install ($how) was NOT refused (namespace now: uid '$(ns_uid)', phase '$(ns_phase)')"
		fi
		case "$out" in
		*"Helm could DELETE it"*"helm.sh/resource-policy=keep"*) ;;
		*) die "$label: the upgrade ($how) failed, but not with the guard's message: $out" ;;
		esac
		sleep 5
		[ "$(ns_uid)" = "$uid" ] && [ "$(ns_phase)" = "Active" ] || die "$label: THE NAMESPACE WAS DELETED by a refused upgrade (uid $uid -> '$(ns_uid)', phase '$(ns_phase)')"
	done
	ok "$label: the upgrade is refused and names the fix, with and without namespace.create=true; namespace untouched"

	# What the message says to do.
	kc annotate namespace "$NS" helm.sh/resource-policy=keep >/dev/null
	out="$("$install" 2>&1)" || die "$label: the upgrade still fails after the namespace was marked keep: $out"
	sleep 5
	[ "$(ns_uid)" = "$uid" ] && [ "$(ns_phase)" = "Active" ] || die "$label: THE NAMESPACE WAS DELETED after it was marked keep (uid $uid -> '$(ns_uid)', phase '$(ns_phase)')"
	[ "$(release_field status)" = "deployed" ] || die "$label: the release is '$(release_field status)' after the upgrade, want deployed"
	[ "$(manifest_namespaces)" = "0" ] || die "$label: the Namespace is still in the manifest of a release that does not own it"
	ok "$label: marked keep, the same command upgrades; namespace kept (uid $uid), no longer in the manifest"
}

# Both charts: the release is stored in the namespace itself and asks for
# namespace.create=true, as the failed installs of the older charts did. The
# Namespace gets into the first manifest because the release already owns it
# (the charts read that from the cluster when namespace.create is set).
preown_namespace() {
	kc create namespace "$NS" >/dev/null
	kc label namespace "$NS" app.kubernetes.io/managed-by=Helm >/dev/null
	kc annotate namespace "$NS" "meta.helm.sh/release-name=$RELEASE" "meta.helm.sh/release-namespace=$NS" >/dev/null
}

aether_guard_leg() {
	preown_namespace
	guard_leg "aether" documented_helm_install --set namespace.create=true --set-string agent.image.pullPolicy=Bogus
}

# The prober chart carries a copy of the guard (#1405). Its image reference is
# a package-time placeholder in the source tree, so its pod never starts;
# nothing here needs it to. The pull policy is always passed, so that no retry
# reuses the first revision's, which is the one the API rejects.
PROBER_NS="first-install-prober"
prober_install() {
	hc upgrade --install prober "$CHARTS/prober" --namespace "$PROBER_NS" --set image.pullPolicy=IfNotPresent "$@"
}
prober_reset() {
	hc uninstall prober -n "$PROBER_NS" >/dev/null 2>&1 || true
	kc delete ns "$PROBER_NS" --wait=true --timeout=120s >/dev/null 2>&1 || true
}
prober_guard_leg() {
	local RELEASE=prober NS="$PROBER_NS"
	prober_reset
	preown_namespace
	guard_leg "prober" prober_install --set namespace.create=true --set-string image.pullPolicy=Bogus
	prober_reset
}

# The prober chart's copy of the drift case in (vi): a DEPLOYED release whose
# manifest lists the Namespace (namespace.create=true, release stored in
# another namespace), the live object without ownership annotations and
# without `keep`, upgraded with namespace.create=false.
PROBER_RELEASE_NS="first-install-prober-release"
prober_elsewhere_install() {
	hc upgrade --install prober "$CHARTS/prober" --namespace "$PROBER_RELEASE_NS" --create-namespace \
		--set "namespace.name=$PROBER_NS" --set image.pullPolicy=IfNotPresent "$@"
}
prober_drift_leg() {
	local NS="$PROBER_NS" uid out
	out="$(prober_elsewhere_install --set namespace.create=true 2>&1)" || die "prober: installing with namespace.create=true from another namespace failed: $out"
	uid="$(ns_uid)"
	kc annotate namespace "$NS" meta.helm.sh/release-name- meta.helm.sh/release-namespace- helm.sh/resource-policy- >/dev/null
	out="$(prober_elsewhere_install --set namespace.create=false 2>&1)" || die "prober: the upgrade over the drifted namespace failed: $out"
	sleep 5
	[ "$(ns_uid)" = "$uid" ] && [ "$(ns_phase)" = "Active" ] ||
		die "prober: THE NAMESPACE WAS DELETED after its ownership annotations drifted (uid $uid -> '$(ns_uid)', phase '$(ns_phase)')"
	[ "$(ns_annotation helm.sh/resource-policy)" = "keep" ] || die "prober: the upgrade did not put keep back on the namespace"
	ok "prober: ownership annotations lost before keep was ever stamped: namespace kept (uid $uid), keep is back"
	hc uninstall prober -n "$PROBER_RELEASE_NS" >/dev/null 2>&1 || true
	kc delete ns "$PROBER_NS" "$PROBER_RELEASE_NS" --wait=true --timeout=120s >/dev/null 2>&1 || true
}

# A release that rendered namespace A is upgraded with namespace.name=B: A
# leaves the manifest, and Helm would delete it. Refused, naming the `keep`
# command for A; after it the upgrade goes through and A is still there.
prober_migration_leg() {
	local old="$PROBER_NS" new="$PROBER_NS-moved" uid out
	local NS="$old"
	out="$(prober_elsewhere_install --set namespace.create=true 2>&1)" || die "prober: installing with namespace.create=true from another namespace failed: $out"
	kc annotate namespace "$old" helm.sh/resource-policy- >/dev/null
	uid="$(ns_uid)"
	if out="$(prober_elsewhere_install --set namespace.create=true --set "namespace.name=$new" 2>&1)"; then
		sleep 5
		die "prober: moving the release to another namespace was NOT refused (the old one now: uid '$(ns_uid)', phase '$(ns_phase)')"
	fi
	case "$out" in
	*"owns the namespace \"$old\""*"kubectl annotate namespace $old helm.sh/resource-policy=keep"*) ;;
	*) die "prober: the move failed, but not with the command that protects the old namespace: $out" ;;
	esac
	kc annotate namespace "$old" helm.sh/resource-policy=keep >/dev/null
	out="$(prober_elsewhere_install --set namespace.create=true --set "namespace.name=$new" 2>&1)" || die "prober: the move still fails after the old namespace was marked keep: $out"
	sleep 5
	[ "$(ns_uid)" = "$uid" ] && [ "$(ns_phase)" = "Active" ] || die "prober: THE OLD NAMESPACE WAS DELETED by the move (uid $uid -> '$(ns_uid)', phase '$(ns_phase)')"
	[ "$(kc get ns "$new" -o jsonpath='{.status.phase}' 2>/dev/null || true)" = "Active" ] || die "prober: the new namespace $new was not created"
	ok "prober: a move to another namespace is refused until the old one is marked keep; then both exist (old uid $uid)"
	hc uninstall prober -n "$PROBER_RELEASE_NS" >/dev/null 2>&1 || true
	kc delete ns "$old" "$new" "$PROBER_RELEASE_NS" --wait=true --timeout=120s >/dev/null 2>&1 || true
}

verify_failed_install_guard() {
	log "vii. a failed first install that lists an unowned Namespace cannot be upgraded into deleting it"
	# The aether chart does not render without its CRDs (documented step 1; (iv)
	# runs the same command again).
	hc upgrade --install aether-crds "$CHARTS/crds" --wait --timeout 2m >/dev/null || die "the crds chart did not install"
	aether_guard_leg
	prober_guard_leg
	prober_drift_leg
	prober_migration_leg
}

verify() {
	charts_dir
	reset_aether
	verify_docs
	verify_enforcement
	verify_broken_combination
	# Before the documented install, because it needs a release that was never
	# deployed; it leaves the cluster empty again.
	verify_failed_install_guard
	reset_aether
	verify_first_install
	verify_upgrade
	verify_rollback_to_first
	verify_seed_race
	verify_legacy_unmarked
	verify_upgrade_safety
	rm -rf "$(dirname "$CHARTS")"
	log "all assertions passed (#1403/#1404: the documented first install works with default values, upgrades, and no upgrade deletes the namespace)"
}

down() {
	log "tearing down"
	kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	rm -rf "$STATE_DIR"
	ok "cluster '$CLUSTER' removed"
}

case "${1:-}" in
up) up ;;
test | verify) verify ;;
down) down ;;
"") up && verify ;;
*) die "usage: $0 {up|verify|down}" ;;
esac
