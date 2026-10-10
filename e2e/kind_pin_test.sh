#!/usr/bin/env bash
# //e2e:kind_pin_test (#1251): every kind e2e surface runs the Kubernetes that
# e2e/kind-version.sh pins, and nothing else carries a copy that could drift.
#
# Hermetic: reads only its data files (the pin file, the e2e harnesses, the
# //test/e2e kind config, the workflows and the composite actions). Runs from
# the runfiles root of the main repository, where those sit at their source
# paths.
#
# Checks:
#   1. the pin is well formed: kind and kubectl are vX.Y.Z, the node image is
#      kindest/node:<kubectl version>@sha256:<64 hex> (a tag-only image would let
#      the digest — the build made for the pinned kind release — float)
#   2. test/e2e/testdata/kind-config.yaml names exactly that node image on every
#      node
#   3. every `kindest/node:<ref>` anywhere in a workflow, a composite action, an
#      e2e harness or an e2e kind config is exactly the pinned image
#   4. every helm/kind-action step pins `version:` and `kubectl_version:` to the
#      pin, and a step that creates a cluster (no `install_only: true`) pins
#      `node_image:` too. A `${{ ... }}` value is accepted only inside
#      .github/actions/setup-kind/, the action that reads the pin file itself.
#   5. every e2e harness that runs `kind create cluster` sources the pin file
#      and passes --image "$KIND_NODE_IMAGE"
#   6. helm/kind-action is used ONLY by .github/actions/setup-kind, and that
#      action reads e2e/kind-version.sh: a workflow cannot install kind any other
#      way and so cannot pin a different one
#   7. every workflow job that drives kind (a `kind ` command or an e2e/*.sh
#      harness) installs it through setup-kind (directly or via
#      .github/actions/run-e2e-script) — not the runner's preinstalled kind,
#      whose default node image moves with the runner image. The same holds
#      for every composite action that drives kind (run-e2e-script,
#      run-conformance), since a job that calls one has no kind command of its
#      own
#   8. setup-kind pulls the pinned node image through the Docker Hub mirror
#      (scripts/docker-hub-mirror.sh, #1602) BEFORE helm/kind-action, and hands
#      that script the pin's own output: every kind job gets the image the same
#      way, and none is left to pull it from Docker Hub anonymously when its
#      cluster is created
set -euo pipefail

fail=0
err() {
	echo "FAIL: $*" >&2
	fail=1
}

PIN=e2e/kind-version.sh
[ -f "$PIN" ] || {
	echo "FAIL: $PIN not in runfiles" >&2
	exit 1
}
unset KIND_NODE_IMAGE
# The pin file is a data file here (sourced from runfiles); //e2e:e2e_shell lints it.
# shellcheck disable=SC1091
. e2e/kind-version.sh

# --- 1. the pin itself.
semver='^v[0-9]+\.[0-9]+\.[0-9]+$'
[[ "$KIND_VERSION" =~ $semver ]] || err "$PIN: KIND_VERSION '$KIND_VERSION' is not vX.Y.Z"
[[ "$KUBECTL_VERSION" =~ $semver ]] || err "$PIN: KUBECTL_VERSION '$KUBECTL_VERSION' is not vX.Y.Z"
want_image_re="^kindest/node:${KUBECTL_VERSION//./\\.}@sha256:[0-9a-f]{64}$"
[[ "$KIND_NODE_IMAGE" =~ $want_image_re ]] ||
	err "$PIN: KIND_NODE_IMAGE '$KIND_NODE_IMAGE' is not kindest/node:$KUBECTL_VERSION@sha256:<digest> (the node image and kubectl must be the same Kubernetes, and the digest is required)"
echo "pin: kind $KIND_VERSION, kubectl $KUBECTL_VERSION, node $KIND_NODE_IMAGE"

strip() {
	# A YAML scalar: drop a trailing comment and surrounding quotes/space.
	local v="$1"
	v="${v%%#*}"
	v="${v#"${v%%[![:space:]]*}"}"
	v="${v%"${v##*[![:space:]]}"}"
	v="${v#\"}"
	v="${v%\"}"
	v="${v#\'}"
	v="${v%\'}"
	printf '%s' "$v"
}

# --- 2. the //test/e2e kind config.
CFG=test/e2e/testdata/kind-config.yaml
[ -f "$CFG" ] || err "$CFG not in runfiles"
nodes=$(grep -cE '^[[:space:]]*-[[:space:]]+role:' "$CFG" || true)
images=0
while IFS= read -r line; do
	images=$((images + 1))
	got="$(strip "${line#*image:}")"
	[ "$got" = "$KIND_NODE_IMAGE" ] || err "$CFG: image '$got' != pinned '$KIND_NODE_IMAGE'"
done < <(grep -E '^[[:space:]]+image:' "$CFG" || true)
[ "$nodes" -gt 0 ] || err "$CFG: no nodes found"
[ "$images" -eq "$nodes" ] || err "$CFG: $nodes node(s) but $images image: line(s); every node must name the pinned image"

# --- the files every remaining check reads.
mapfile -t ci_files < <(find -L .github/workflows .github/actions -type f \( -name '*.yaml' -o -name '*.yml' \) 2>/dev/null | sort)
[ "${#ci_files[@]}" -gt 0 ] || err "no workflow files in runfiles"
mapfile -t e2e_files < <(find -L e2e -maxdepth 1 -type f \( -name '*.sh' -o -name '*.yaml' \) | sort)

# --- 3. every literal node image.
# (The pin file is the source, and this test names image shapes, not images.)
for f in "${ci_files[@]}" "${e2e_files[@]}" "$CFG"; do
	case "$f" in "$PIN" | e2e/kind_pin_test.sh) continue ;; esac
	while IFS= read -r ref; do
		[ "$ref" = "$KIND_NODE_IMAGE" ] || err "$f: '$ref' != pinned '$KIND_NODE_IMAGE'"
	done < <(grep -oE 'kindest/node:[^[:space:]"'\'',)}]+' "$f" || true)
done

# --- 4. every helm/kind-action step.
# Emits one record per step: "<line>\t<key>=<value>..." for the keys we police.
kind_steps() {
	awk '
	function indent(s) { match(s, /[^ ]/); return RSTART - 1 }
	function flush() {
		if (start) printf "%d\tversion=%s\tkubectl_version=%s\tnode_image=%s\tinstall_only=%s\n", start, v["version"], v["kubectl_version"], v["node_image"], v["install_only"]
		start = 0; delete v
	}
	{
		line = $0
		if (start && line !~ /^[[:space:]]*(#|$)/ && indent(line) < keyind) flush()
		if (line ~ /uses:[[:space:]]*helm\/kind-action@/) {
			flush()
			start = NR
			keyind = indent(line)
			if (substr(line, keyind + 1, 2) == "- ") keyind += 2
			next
		}
		if (start && match(line, /^[[:space:]]+(version|kubectl_version|node_image|install_only):/)) {
			k = line; sub(/^[[:space:]]+/, "", k); sub(/:.*/, "", k)
			val = line; sub(/^[^:]*:[[:space:]]*/, "", val); sub(/[[:space:]]+#.*$/, "", val)
			gsub(/^["\x27]|["\x27]$/, "", val)
			v[k] = (val == "" ? "<empty>" : val)
		}
	}
	END { flush() }' "$1"
}

# Literal text this test matches, not shell to expand.
# shellcheck disable=SC2016
GHA_EXPR='${{'
# shellcheck disable=SC2016
IMAGE_ARG='--image "$KIND_NODE_IMAGE"'

check_value() {
	# check_value <file> <line> <key> <got> <want>
	local f=$1 n=$2 key=$3 got=$4 want=$5
	if [ -z "$got" ]; then
		err "$f:$n: helm/kind-action step does not pin '$key' (the action's default moves with its releases; pin it to $want)"
	elif [[ "$got" == "$GHA_EXPR"* ]]; then
		case "$f" in
		.github/actions/setup-kind/*) ;;
		*) err "$f:$n: '$key: $got' is an expression; only .github/actions/setup-kind (which reads $PIN) may pin by expression" ;;
		esac
	elif [ "$got" != "$want" ]; then
		err "$f:$n: '$key: $got' != pinned '$want' ($PIN)"
	fi
}

steps=0
for f in "${ci_files[@]}"; do
	while IFS=$'\t' read -r n ver kv ni io; do
		steps=$((steps + 1))
		ver=${ver#version=} kv=${kv#kubectl_version=} ni=${ni#node_image=} io=${io#install_only=}
		check_value "$f" "$n" version "$ver" "$KIND_VERSION"
		check_value "$f" "$n" kubectl_version "$kv" "$KUBECTL_VERSION"
		if [ "$io" != "true" ] || [ -n "$ni" ]; then
			check_value "$f" "$n" node_image "$ni" "$KIND_NODE_IMAGE"
		fi
	done < <(kind_steps "$f")
done
echo "checked $steps helm/kind-action step(s)"

# --- 5. every harness that creates a kind cluster.
creators=0
for f in "${e2e_files[@]}"; do
	case "$f" in *.sh) ;; *) continue ;; esac
	[ "$f" = "$PIN" ] && continue
	grep -qE '^[[:space:]]*kind create cluster' "$f" || continue
	creators=$((creators + 1))
	# shellcheck disable=SC2016 # matching the harness's literal source line
	grep -qE '^\. "\$(REPO_ROOT/e2e|HERE)/kind-version\.sh"' "$f" || err "$f: creates a kind cluster but does not source e2e/kind-version.sh"
	while IFS= read -r line; do
		case "$line" in
		*"$IMAGE_ARG"*) ;;
		*) err "$f: '$(strip "$line")' does not pass --image \"\$KIND_NODE_IMAGE\"" ;;
		esac
	done < <(grep -E '^[[:space:]]*kind create cluster' "$f")
done
[ "$creators" -gt 0 ] || err "no e2e harness creates a kind cluster: the e2e glob in runfiles is empty or the check is stale"
echo "checked $creators e2e harness(es) that create kind clusters"

# --- 6. one installer.
SETUP_KIND=.github/actions/setup-kind/action.yml
for f in "${ci_files[@]}"; do
	[ "$f" = "$SETUP_KIND" ] && continue
	if grep -qE 'uses:[[:space:]]*helm/kind-action@' "$f"; then
		err "$f: uses helm/kind-action directly; use ./.github/actions/setup-kind (it installs the pinned kind/kubectl/node image)"
	fi
done
if [ -f "$SETUP_KIND" ]; then
	grep -q 'e2e/kind-version.sh' "$SETUP_KIND" || err "$SETUP_KIND does not read e2e/kind-version.sh"
else
	err "$SETUP_KIND not in runfiles"
fi

# --- 7. every job that drives kind installs the pinned one.
# Emits "<job>" for each job under `jobs:` that runs kind or an e2e harness but
# never references setup-kind / run-e2e-script.
unpinned_kind_jobs() {
	awk '
	function done_job() {
		if (job != "" && drives && !pinned) print job
		job = ""; drives = 0; pinned = 0
	}
	/^jobs:[[:space:]]*$/ { in_jobs = 1; next }
	in_jobs && /^[^[:space:]#]/ { done_job(); in_jobs = 0 }
	in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { done_job(); job = $1; sub(/:$/, "", job); next }
	job == "" || /^[[:space:]]*#/ { next }
	/(^|[^A-Za-z0-9_-])kind (create|load|export|get|delete)[[:space:]]/ { drives = 1 }
	/\.\/e2e\/[A-Za-z0-9_.-]+\.sh/ { drives = 1 }
	/\.github\/actions\/(setup-kind|run-e2e-script)/ { pinned = 1 }
	END { done_job() }' "$1"
}
jobs_checked=0
for f in "${ci_files[@]}"; do
	case "$f" in .github/workflows/*) ;; *) continue ;; esac
	jobs_checked=$((jobs_checked + 1))
	while IFS= read -r job; do
		err "$f: job '$job' drives kind without .github/actions/setup-kind (it would run the runner's preinstalled kind and its default node image)"
	done < <(unpinned_kind_jobs "$f")
done
echo "checked the kind-driving jobs of $jobs_checked workflow(s)"

# A composite action that drives kind is held to the same rule as a job (a job
# that reaches kind only through such an action, e.g. run-conformance, has no
# kind command of its own for the loop above to see). setup-kind is exempt: it
# is the installer.
actions_checked=0
for f in "${ci_files[@]}"; do
	case "$f" in
	"$SETUP_KIND") continue ;;
	.github/actions/*) ;;
	*) continue ;;
	esac
	actions_checked=$((actions_checked + 1))
	code=$(grep -vE '^[[:space:]]*#' "$f" || true)
	# A harness is run as ./e2e/<script>; an action that only sources a pin file
	# by its own path (<action path>/../../../e2e/helm-version.sh) drives nothing.
	if grep -qE '(^|[^A-Za-z0-9_-])kind (create|load|export|get|delete)[[:space:]]|(^|[^.A-Za-z0-9_])\./e2e/[A-Za-z0-9_.$-]+' <<<"$code" &&
		! grep -qE 'uses:[[:space:]]*\./\.github/actions/(setup-kind|run-e2e-script)' <<<"$code"; then
		err "$f: drives kind without .github/actions/setup-kind (it would run the runner's preinstalled kind and its default node image)"
	fi
done
echo "checked $actions_checked composite action(s)"

# --- 8. the node image comes through the Docker Hub mirror.
if [ -f "$SETUP_KIND" ]; then
	code=$(grep -nvE '^[[:space:]]*#' "$SETUP_KIND" || true)
	# shellcheck disable=SC2016 # the action's literal text
	mirror_line=$(grep -E '^[0-9]+:[[:space:]]+"\$GITHUB_ACTION_PATH/\.\./\.\./\.\./scripts/docker-hub-mirror\.sh" "\$NODE_IMAGE"[[:space:]]*$' <<<"$code" | head -n1 | cut -d: -f1)
	kind_line=$(grep -E 'uses:[[:space:]]*helm/kind-action@' <<<"$code" | head -n1 | cut -d: -f1)
	if [ -z "$mirror_line" ]; then
		err "$SETUP_KIND: no step runs scripts/docker-hub-mirror.sh \"\$NODE_IMAGE\": the node image would be pulled from Docker Hub anonymously (#1602)"
	elif [ -n "$kind_line" ] && [ "$mirror_line" -gt "$kind_line" ]; then
		err "$SETUP_KIND:$mirror_line: scripts/docker-hub-mirror.sh runs after helm/kind-action (line $kind_line), which has already pulled the node image from Docker Hub"
	fi
	# shellcheck disable=SC2016 # the action's literal text
	grep -qE '^[0-9]+:[[:space:]]+NODE_IMAGE:[[:space:]]*\$\{\{ steps\.pin\.outputs\.node_image \}\}[[:space:]]*$' <<<"$code" ||
		err "$SETUP_KIND: NODE_IMAGE is not \${{ steps.pin.outputs.node_image }}: the image pulled through the mirror must be the pin's"
	echo "checked the Docker Hub mirror step of $SETUP_KIND"
fi

if [ "$fail" -ne 0 ]; then
	echo "kind pin drift: bump e2e/kind-version.sh and every copy together (docs/runbook.md, \"Bumping the e2e Kubernetes version\")" >&2
	exit 1
fi
echo "PASS: every kind e2e surface runs $KIND_NODE_IMAGE"
