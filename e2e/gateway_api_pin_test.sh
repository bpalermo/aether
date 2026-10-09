#!/usr/bin/env bash
# //e2e:gateway_api_pin_test (#1583): every e2e surface installs the Gateway
# API release e2e/gateway-api-version.sh pins, and nothing else carries a copy
# that a bump could miss.
#
# Hermetic: reads only its data files (the pin file, go.mod, the e2e harnesses,
# the workflows and the composite actions), from the runfiles root of the main
# repository where they sit at their source paths.
#
# Checks:
#   1. the pin is a full vX.Y.Z, and it is go.mod's `sigs.k8s.io/gateway-api`
#      version: the CRDs a suite installs are the release the code under test
#      is built against
#   2. no e2e harness assigns GWAPI_VERSION itself; one that reads it sources
#      the pin file, before its first use
#   3. no e2e harness, workflow or composite action spells a Gateway API
#      release in a download URL: the version there is always a variable
#   4. a workflow cannot source a shell file, so .github/workflows/e2e.yaml
#      carries ONE copy, `GATEWAY_API_VERSION`, equal to the pin; no other
#      workflow or action assigns one
#   5. every `gateway-api-version:` handed to a composite action is that
#      variable, never a literal
set -euo pipefail

fail=0
err() {
	echo "FAIL: $*" >&2
	fail=1
}

PIN=e2e/gateway-api-version.sh
for f in "$PIN" go.mod; do
	[ -f "$f" ] || {
		echo "FAIL: $f not in runfiles" >&2
		exit 1
	}
done
unset GWAPI_VERSION
# The pin file is a data file here (sourced from runfiles); //e2e:e2e_shell lints it.
# shellcheck disable=SC1091
. e2e/gateway-api-version.sh

# --- 1. the pin itself, against go.mod.
semver='^v[0-9]+\.[0-9]+\.[0-9]+$'
[[ "$GWAPI_VERSION" =~ $semver ]] || err "$PIN: GWAPI_VERSION '$GWAPI_VERSION' is not vX.Y.Z"
gomod="$(sed -nE 's#^[[:space:]]*(require[[:space:]]+)?sigs\.k8s\.io/gateway-api[[:space:]]+([^[:space:]]+).*$#\2#p' go.mod | head -n1)"
echo "pin: Gateway API $GWAPI_VERSION; go.mod sigs.k8s.io/gateway-api ${gomod:-<none>}"
[ "$gomod" = "$GWAPI_VERSION" ] ||
	err "go.mod: sigs.k8s.io/gateway-api '${gomod:-<none>}' != $PIN '$GWAPI_VERSION' (the e2e suites install the CRDs of the release the code is built against; bump both together)"

mapfile -t ci_files < <(find -L .github/workflows .github/actions -type f \( -name '*.yaml' -o -name '*.yml' \) 2>/dev/null | sort)
[ "${#ci_files[@]}" -gt 0 ] || err "no workflow files in runfiles"
mapfile -t e2e_files < <(find -L e2e -maxdepth 1 -type f -name '*.sh' | sort)

# --- 2. the harnesses read the pin file.
readers=0
for f in "${e2e_files[@]}"; do
	case "$f" in "$PIN" | e2e/gateway_api_pin_test.sh) continue ;; esac
	while IFS= read -r hit; do
		err "$f:${hit%%:*}: assigns GWAPI_VERSION itself; source $PIN instead (a bump must be one file)"
	done < <(grep -nE '(^|[[:space:];])(export[[:space:]]+|local[[:space:]]+|readonly[[:space:]]+)?GWAPI_VERSION=' "$f" || true)
	use_at="$(grep -nE '\$\{?GWAPI_VERSION' "$f" | grep -vE '^[0-9]+:[[:space:]]*#' | awk -F: 'NR == 1 { print $1 }' || true)"
	[ -n "$use_at" ] || continue
	readers=$((readers + 1))
	# shellcheck disable=SC2016 # matching the harness's literal source line
	src_at="$(grep -nE '^\. "\$(REPO_ROOT/e2e|HERE)/gateway-api-version\.sh"' "$f" | awk -F: 'NR == 1 { print $1 }' || true)"
	if [ -z "$src_at" ]; then
		err "$f:$use_at: reads GWAPI_VERSION but does not source $PIN"
	elif [ "$src_at" -gt "$use_at" ]; then
		err "$f:$use_at: reads GWAPI_VERSION before it sources $PIN (line $src_at)"
	fi
done
[ "$readers" -gt 0 ] || err "no e2e harness reads GWAPI_VERSION: the e2e glob in runfiles is empty or the check is stale"
echo "checked $readers e2e harness(es) that install Gateway API CRDs"

# --- 3. no release spelled in a download URL.
for f in "${ci_files[@]}" "${e2e_files[@]}"; do
	[ "$f" = e2e/gateway_api_pin_test.sh ] && continue
	while IFS= read -r hit; do
		err "$f:${hit%%:*}: a Gateway API download names a literal release; use the pinned variable ($PIN)"
	done < <(grep -nE 'kubernetes-sigs/gateway-api/(releases/download|raw|archive|blob|tree)/[^$[:space:]]' "$f" | grep -vE '^[0-9]+:[[:space:]]*#' || true)
done

# --- 4. the one workflow copy.
E2E=.github/workflows/e2e.yaml
copies=0
for f in "${ci_files[@]}"; do
	while IFS= read -r hit; do
		n="${hit%%:*}"
		val="${hit#*GATEWAY_API_VERSION:}"
		val="${val%%#*}"
		val="${val//[[:space:]\"\']/}"
		# An `env:` entry of a step that forwards an input or the workflow's value.
		# shellcheck disable=SC2016 # literal workflow text
		case "$val" in '${{'*) continue ;; esac
		copies=$((copies + 1))
		[ "$f" = "$E2E" ] || err "$f:$n: assigns GATEWAY_API_VERSION '$val'; only $E2E carries the copy of $PIN"
		[ "$val" = "$GWAPI_VERSION" ] || err "$f:$n: GATEWAY_API_VERSION '$val' != pinned '$GWAPI_VERSION' ($PIN)"
	done < <(grep -nE '^[[:space:]]*GATEWAY_API_VERSION:' "$f" || true)
done
[ "$copies" -eq 1 ] || err "$E2E: expected exactly one literal GATEWAY_API_VERSION in the workflows, found $copies"

# --- 5. what the composite actions are handed.
handed=0
for f in "${ci_files[@]}"; do
	case "$f" in .github/workflows/*) ;; *) continue ;; esac
	while IFS= read -r hit; do
		handed=$((handed + 1))
		n="${hit%%:*}"
		val="${hit#*gateway-api-version:}"
		val="${val%%#*}"
		val="${val#"${val%%[![:space:]]*}"}"
		val="${val%"${val##*[![:space:]]}"}"
		# shellcheck disable=SC2016 # literal workflow text
		[ "$val" = '${{ env.GATEWAY_API_VERSION }}' ] ||
			err "$f:$n: 'gateway-api-version: $val' must be \${{ env.GATEWAY_API_VERSION }}, the workflow's one copy of $PIN"
	done < <(grep -nE '^[[:space:]]+gateway-api-version:' "$f" || true)
done
[ "$handed" -gt 0 ] || err "no workflow hands gateway-api-version to a composite action: the check is stale"
echo "checked $copies workflow copy and $handed gateway-api-version input(s)"

if [ "$fail" -ne 0 ]; then
	echo "Gateway API pin drift: bump e2e/gateway-api-version.sh, go.mod and e2e.yaml's GATEWAY_API_VERSION together (docs/runbook.md, \"Bumping Gateway API\")" >&2
	exit 1
fi
echo "PASS: every e2e surface installs Gateway API $GWAPI_VERSION"
