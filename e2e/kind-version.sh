# shellcheck shell=bash
# Sourced, not run: the ONE Kubernetes every kind e2e surface runs (#1251).
#
# Every kind cluster this repository creates — the nightly conformance jobs, the
# per-PR //test/e2e, the script-driven nightly suites and a local run of any
# e2e/*.sh — uses the node image below, created by the kind binary below, driven
# by the kubectl below. Bump Kubernetes HERE and nowhere else:
#
#   1. KIND_VERSION      the kind release (https://github.com/kubernetes-sigs/kind/releases)
#   2. KIND_NODE_IMAGE   a kindest/node image FROM THAT RELEASE's notes, with its
#                        @sha256 digest: the digest is what selects the build made
#                        for that kind release (node images are not portable across
#                        kind releases)
#   3. KUBECTL_VERSION   the same Kubernetes patch as the node image
#
# then copy the node image into test/e2e/testdata/kind-config.yaml (the e2e
# framework reads a kind config, not this file). //e2e:kind_pin_test fails the
# build until every copy agrees. See docs/runbook.md, "Bumping the e2e
# Kubernetes version".
#
# Keep the three values as plain single-line assignments: //e2e:kind_pin_test
# and .github/actions/setup-kind read them by sourcing this file.
KIND_VERSION="v0.33.0"
# shellcheck disable=SC2034 # read by the consumers that source this file
KUBECTL_VERSION="v1.35.8"
# Override per run with KIND_NODE_IMAGE=<image> (e.g. to try the next minor
# locally); CI never sets it.
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0}"

# kind_require_binary: call before `kind create cluster --image "$KIND_NODE_IMAGE"`.
#
# A node image is built by, and for, one kind release. An OLDER kind binary than
# KIND_VERSION is refused: it can fail to boot a newer node image in ways that
# surface minutes later as an unrelated-looking cluster failure (#1251). A NEWER
# kind is only warned about — kind keeps running the previous release's images,
# which is how CI already runs them on a runner whose preinstalled kind is newer.
# KIND_ALLOW_SKEW=1 turns the refusal into a warning, for a deliberate experiment.
kind_require_binary() {
	local have
	have="$(kind version 2>/dev/null | awk '{print $2}')"
	if [ -z "$have" ]; then
		echo "kind not found on PATH; install $KIND_VERSION: go install sigs.k8s.io/kind@$KIND_VERSION" >&2
		return 1
	fi
	[ "$have" = "$KIND_VERSION" ] && return 0
	local older
	older="$(printf '%s\n%s\n' "$have" "$KIND_VERSION" | sort -V | head -n1)"
	if [ "$older" = "$have" ]; then
		if [ "${KIND_ALLOW_SKEW:-0}" = "1" ]; then
			echo "WARNING: kind $have is older than the pinned $KIND_VERSION that built $KIND_NODE_IMAGE (KIND_ALLOW_SKEW=1; continuing)" >&2
			return 0
		fi
		echo "kind $have is older than the pinned $KIND_VERSION (e2e/kind-version.sh), which built $KIND_NODE_IMAGE." >&2
		echo "  install it:  go install sigs.k8s.io/kind@$KIND_VERSION" >&2
		echo "  or, deliberately, run against the skew:  KIND_ALLOW_SKEW=1 $0 ..." >&2
		return 1
	fi
	echo "WARNING: kind $have is newer than the pinned $KIND_VERSION (e2e/kind-version.sh); creating $KIND_NODE_IMAGE with it anyway" >&2
}
