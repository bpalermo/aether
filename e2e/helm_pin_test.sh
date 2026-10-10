#!/usr/bin/env bash
# //e2e:helm_pin_test (#1580, #1581, #1543): every workflow job that runs Helm
# runs a release e2e/helm-version.sh pins, never the runner image's preinstalled
# Helm (which moves, majors included, whenever GitHub updates the image) and
# never azure/setup-helm's default (`latest`).
#
# Hermetic: reads its data files (the pin file, the e2e harnesses, the
# workflows and the composite actions) from the runfiles root of the main
# repository, where they sit at their source paths, and runs the rules_helm
# toolchain's helm binary (argument 1) for `version` and `list --help` only.
#
# Checks:
#   1. the pin is well formed: a v3.Y.Z, a v4.Y.Z and a default major that is
#      one of the two; helm_pin_version maps a major (or nothing) to its
#      release and refuses everything else, a version string included
#   2. HELM_V4_VERSION is the Helm the rules_helm toolchain downloads: the one
#      every chart template test renders with
#   3. helm_list_all_flags gives `-A -a` under Helm 3 and `-A` under Helm 4,
#      and the real Helm 4 binary indeed has no `-a` on `helm list`
#   4. azure/setup-helm is used ONLY by .github/actions/setup-helm, SHA-pinned,
#      with `version:` taken from the step that reads the pin file
#   5. a caller of .github/actions/setup-helm passes at most `major:`, as 3, 4
#      or an expression (the action refuses anything but 3 and 4 at run time)
#   6. every workflow job, and every composite action, that runs a `helm`
#      command or an e2e/*.sh harness installs Helm through setup-helm before
#      its first such line (a job may get it from run-e2e-script or
#      run-conformance, which are held to the same rule as actions)
#   7. no workflow, composite action or e2e harness spells `helm list` with
#      -a/--all: that is Helm 3 only, and the diagnostics that used it masked
#      the error (#1581). helm_list_all_flags is the one place that decides
#   8. the nightly first-install job runs e2e/first-install.sh under BOTH
#      majors: a matrix over 3 and 4, handed to setup-helm through
#      run-e2e-script's `helm-major` (#1543)
set -euo pipefail

fail=0
err() {
	echo "FAIL: $*" >&2
	fail=1
}

PIN=e2e/helm-version.sh
[ -f "$PIN" ] || {
	echo "FAIL: $PIN not in runfiles" >&2
	exit 1
}
# The rules_helm toolchain's helm, as $(rootpath @helm//:helm): an external
# repository's file sits beside the main repository in the runfiles tree.
HELM_BIN="${1:-}"
case "$HELM_BIN" in
external/*) HELM_BIN="$PWD/../${HELM_BIN#external/}" ;;
"") ;;
/*) ;;
*) HELM_BIN="$PWD/$HELM_BIN" ;;
esac
[ -x "$HELM_BIN" ] || {
	echo "FAIL: the rules_helm helm binary '${1:-}' is not in runfiles (argument 1)" >&2
	exit 1
}

# The pin file is a data file here (sourced from runfiles); //e2e:e2e_shell lints it.
# shellcheck disable=SC1091
. e2e/helm-version.sh

# --- 1. the pin itself.
[[ "$HELM_V3_VERSION" =~ ^v3\.[0-9]+\.[0-9]+$ ]] || err "$PIN: HELM_V3_VERSION '$HELM_V3_VERSION' is not v3.Y.Z"
[[ "$HELM_V4_VERSION" =~ ^v4\.[0-9]+\.[0-9]+$ ]] || err "$PIN: HELM_V4_VERSION '$HELM_V4_VERSION' is not v4.Y.Z"
case "$HELM_DEFAULT_MAJOR" in
3 | 4) ;;
*) err "$PIN: HELM_DEFAULT_MAJOR '$HELM_DEFAULT_MAJOR' is neither 3 nor 4" ;;
esac
echo "pin: helm 3 = $HELM_V3_VERSION, helm 4 = $HELM_V4_VERSION, default major $HELM_DEFAULT_MAJOR"

expect_version() {
	# expect_version <argument> <want>
	local got
	got="$(helm_pin_version "$1" 2>/dev/null)" || got="<refused>"
	[ "$got" = "$2" ] || err "helm_pin_version '$1' = '$got', want '$2'"
}
expect_version 3 "$HELM_V3_VERSION"
expect_version 4 "$HELM_V4_VERSION"
default_var="HELM_V${HELM_DEFAULT_MAJOR}_VERSION"
expect_version "" "${!default_var:-<no such pin>}"
for bad in 2 5 latest v3 "$HELM_V3_VERSION" "$HELM_V4_VERSION" "3 " "34"; do
	expect_version "$bad" "<refused>"
done

# --- 2. Helm 4 is the chart template tests' Helm.
# helm reads its config directories from HOME, which a Bazel test may not have.
export HOME="$TEST_TMPDIR"
toolchain="$("$HELM_BIN" version --short 2>/dev/null || true)"
toolchain="${toolchain%%+*}"
[ "$toolchain" = "$HELM_V4_VERSION" ] ||
	err "$PIN: HELM_V4_VERSION '$HELM_V4_VERSION' != the rules_helm toolchain's helm '$toolchain' (MODULE.bazel): the Helm 4 the e2e jobs install must be the one the chart template tests render with; bump them together"

# --- 3. the `helm list` flags, per major.
# A stand-in `helm` that answers `version --short` only.
fake_dir="$TEST_TMPDIR/fake-helm"
flags_under() {
	# flags_under <version --short output, or "" for a helm that fails>
	rm -rf "$fake_dir"
	mkdir -p "$fake_dir"
	if [ -n "$1" ]; then
		# shellcheck disable=SC2016 # the stand-in's own $1 and $2
		printf '#!/bin/sh\n[ "$1 $2" = "version --short" ] && echo "%s"\n' "$1" >"$fake_dir/helm"
	else
		printf '#!/bin/sh\nexit 1\n' >"$fake_dir/helm"
	fi
	chmod +x "$fake_dir/helm"
	PATH="$fake_dir:$PATH" helm_list_all_flags
}
expect_flags() {
	local got
	got="$(flags_under "$1")"
	[ "$got" = "$2" ] || err "helm_list_all_flags under helm '$1' = '$got', want '$2'"
}
expect_flags "${HELM_V3_VERSION}+g0123abc" "-A -a"
expect_flags "v3.18.4+gd80839c" "-A -a"
expect_flags "${HELM_V4_VERSION}+g0123abc" "-A"
expect_flags "v5.0.0" "-A"
expect_flags "" "-A"
# The reason the Helm 4 answer is `-A`: the real binary has no -a/--all there.
list_help="$("$HELM_BIN" list --help 2>&1 || true)"
grep -qE '^[[:space:]]+-A, --all-namespaces' <<<"$list_help" ||
	err "the rules_helm helm's 'list --help' does not offer -A/--all-namespaces; the check is stale"
if grep -qE '^[[:space:]]+(-a,[[:space:]]*)?--all([[:space:]]|$)' <<<"$list_help"; then
	err "helm $toolchain has 'helm list --all' again: helm_list_all_flags ($PIN) assumes Helm 4 dropped it"
fi

# --- the files every remaining check reads.
mapfile -t ci_files < <(find -L .github/workflows .github/actions -type f \( -name '*.yaml' -o -name '*.yml' \) 2>/dev/null | sort)
[ "${#ci_files[@]}" -gt 0 ] || err "no workflow files in runfiles"
mapfile -t e2e_files < <(find -L e2e -maxdepth 1 -type f -name '*.sh' | sort)
[ "${#e2e_files[@]}" -gt 0 ] || err "no e2e harness in runfiles"

SETUP_HELM=.github/actions/setup-helm/action.yml
# Literal text this test matches, not shell to expand.
# shellcheck disable=SC2016
GHA_EXPR='${{'
# shellcheck disable=SC2016
PIN_OUTPUT='${{ steps.pin.outputs.version }}'

# The patterns handed to awk -v spell a literal dot as [.]: awk would eat the
# backslash of \. in such a string.
#
# One record per step that uses the action matching <pattern>:
# "<line>|<ref>|<version>|<major>|<other with: keys>". "|", not a tab: a
# whitespace IFS merges the empty fields of unset keys.
helm_steps() {
	awk -v pat="$2" '
	function indent(s) { match(s, /[^ ]/); return RSTART - 1 }
	function flush() {
		if (start) printf "%d|%s|%s|%s|%s\n", start, ref, v["version"], v["major"], other
		start = 0; in_with = 0; withind = 0; other = ""; delete v
	}
	{
		line = $0
		if (start && line !~ /^[[:space:]]*(#|$)/ && indent(line) < keyind) flush()
		if (line ~ pat) {
			flush()
			start = NR
			keyind = indent(line)
			if (substr(line, keyind + 1, 2) == "- ") keyind += 2
			ref = line; sub(/.*@/, "", ref); sub(/[[:space:]].*$/, "", ref)
			if (line !~ /@/) ref = ""
			next
		}
		if (!start || line ~ /^[[:space:]]*(#|$)/) next
		if (indent(line) == keyind) { in_with = (line ~ /^[[:space:]]+with:[[:space:]]*$/); withind = 0; next }
		if (in_with && !withind) withind = indent(line)
		# Only the keys of `with:` itself, not text inside a block scalar under one.
		if (in_with && indent(line) == withind && match(line, /^[[:space:]]+[A-Za-z0-9_-]+:/)) {
			k = line; sub(/^[[:space:]]+/, "", k); sub(/:.*/, "", k)
			val = line; sub(/^[^:]*:[[:space:]]*/, "", val); sub(/[[:space:]]+#.*$/, "", val)
			gsub(/^["\x27]|["\x27]$/, "", val)
			if (k == "version" || k == "major") v[k] = (val == "" ? "<empty>" : val)
			else other = other (other == "" ? "" : ",") k
		}
	}
	END { flush() }' "$1"
}

# --- 4. one installer.
installers=0
for f in "${ci_files[@]}"; do
	while IFS="|" read -r n ref ver _ other; do
		installers=$((installers + 1))
		if [ "$f" != "$SETUP_HELM" ]; then
			err "$f:$n: uses azure/setup-helm directly; use ./.github/actions/setup-helm (it installs a release $PIN pins)"
			continue
		fi
		[[ "$ref" =~ ^[0-9a-f]{40}$ ]] || err "$f:$n: azure/setup-helm@$ref is not pinned to a full commit SHA"
		[ "$ver" = "$PIN_OUTPUT" ] ||
			err "$f:$n: azure/setup-helm 'version: ${ver:-<unset>}' must be $PIN_OUTPUT, the release read from $PIN (unset, the action installs the latest Helm)"
		[ -z "$other" ] || err "$f:$n: azure/setup-helm sets '$other'; only 'version:' is expected (a download URL of its own would bypass the pin)"
	done < <(helm_steps "$f" 'uses:[[:space:]]*azure/setup-helm@')
done
if [ -f "$SETUP_HELM" ]; then
	[ "$installers" -eq 1 ] || err "$SETUP_HELM: expected exactly one azure/setup-helm step in the tree, found $installers"
	# shellcheck disable=SC2016 # the action's literal source line
	grep -qF '. "$GITHUB_ACTION_PATH/../../../e2e/helm-version.sh"' "$SETUP_HELM" || err "$SETUP_HELM does not source e2e/helm-version.sh"
	grep -qE 'helm_pin_version[[:space:]]' "$SETUP_HELM" || err "$SETUP_HELM does not resolve the release with helm_pin_version"
else
	err "$SETUP_HELM is missing: it is the one way a workflow installs Helm"
fi

# --- 5. the callers of setup-helm.
callers=0
for f in "${ci_files[@]}"; do
	while IFS="|" read -r n _ ver major other; do
		callers=$((callers + 1))
		[ -z "$ver" ] || err "$f:$n: setup-helm takes no 'version:' (got '$ver'); a job names a major, the release comes from $PIN"
		[ -z "$other" ] || err "$f:$n: setup-helm takes only 'major:' (got '$other')"
		case "$major" in
		"" | 3 | 4) ;;
		"$GHA_EXPR"*) ;;
		*) err "$f:$n: setup-helm 'major: $major' is neither 3, 4 nor an expression" ;;
		esac
	done < <(helm_steps "$f" 'uses:[[:space:]]*[.]/[.]github/actions/setup-helm[[:space:]]*(#.*)?$')
done
[ "$callers" -gt 0 ] || err "no workflow or composite action uses .github/actions/setup-helm: the check is stale"
echo "checked $installers azure/setup-helm step(s) and $callers setup-helm caller(s)"

# --- 6. every unit that runs Helm installs the pinned one first.
# A helm invocation in a run: script (or in a `diagnostics:` block handed to
# run-e2e-script), or an e2e harness, every one of which drives helm.
HELM_USE='(^|[[:space:];&|(`"])helm[[:space:]]+(--kube-context|completion|create|dependency|env|get|history|install|lint|list|package|plugin|pull|push|registry|repo|rollback|search|show|status|template|test|uninstall|upgrade|verify|version)([[:space:]]|$)|(^|[^.A-Za-z0-9_])[.]/e2e/[A-Za-z0-9_.$-]+'
PROVIDERS='uses:[[:space:]]*[.]/[.]github/actions/(setup-helm|run-e2e-script|run-conformance)([[:space:]]|$)'
# Workflows: per job under `jobs:`. Emits "<job>\t<first helm line>\t<first provider line>".
job_helm_use() {
	awk -v re="$HELM_USE" -v prov="$PROVIDERS" '
	function done_job() {
		if (job != "" && use_at) printf "%s\t%d\t%d\n", job, use_at, setup_at
		job = ""; use_at = 0; setup_at = 0
	}
	/^jobs:[[:space:]]*$/ { in_jobs = 1; next }
	in_jobs && /^[^[:space:]#]/ { done_job(); in_jobs = 0 }
	in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { done_job(); job = $1; sub(/:$/, "", job); next }
	job == "" || /^[[:space:]]*#/ { next }
	$0 ~ prov { if (!setup_at) setup_at = NR; next }
	$0 ~ re { if (!use_at) use_at = NR }
	END { done_job() }' "$1"
}
# Composite actions: the whole `runs:` block is one unit (not `inputs:`, whose
# descriptions may well mention `helm install`), and only setup-helm itself
# provides Helm there.
action_helm_use() {
	awk -v re="$HELM_USE" '
	/^runs:[[:space:]]*$/ { in_runs = 1; next }
	in_runs && /^[^[:space:]#]/ { in_runs = 0 }
	!in_runs || /^[[:space:]]*#/ { next }
	/uses:[[:space:]]*\.\/\.github\/actions\/setup-helm([[:space:]]|$)/ { if (!setup_at) setup_at = NR; next }
	$0 ~ re { if (!use_at) use_at = NR }
	END { if (use_at) printf "(action)\t%d\t%d\n", use_at, setup_at }' "$1"
}
units=0
for f in "${ci_files[@]}"; do
	case "$f" in
	"$SETUP_HELM") continue ;;
	.github/workflows/*) records="$(job_helm_use "$f")" ;;
	*) records="$(action_helm_use "$f")" ;;
	esac
	[ -n "$records" ] || continue
	while IFS=$'\t' read -r unit use_at setup_at; do
		units=$((units + 1))
		if [ "$setup_at" -eq 0 ]; then
			err "$f:$use_at: $unit runs helm with whatever the runner image ships; add ./.github/actions/setup-helm before it"
		elif [ "$setup_at" -gt "$use_at" ]; then
			err "$f:$use_at: $unit runs helm before it installs the pinned one (line $setup_at)"
		fi
	done <<<"$records"
done
[ "$units" -gt 0 ] || err "no workflow job or composite action runs helm: the check is stale"
echo "checked $units job(s)/action(s) that run helm"

# --- 7. `helm list` never spells the Helm 3 only -a/--all.
LIST_ALL='[[:space:]]list[[:space:]]([^|;&]*[[:space:]])?(-[A-Zb-z]*a[A-Za-z]*|--all)([[:space:]"]|$)'
for f in "${ci_files[@]}" "${e2e_files[@]}"; do
	case "$f" in "$PIN" | e2e/helm_pin_test.sh) continue ;; esac
	while IFS= read -r hit; do
		err "$f:${hit%%:*}: 'helm list' with -a/--all fails under Helm 4; use \$(helm_list_all_flags) from $PIN"
	done < <(grep -nE '(^|[^A-Za-z0-9_-])(helm|hc)[[:space:]]' "$f" | grep -vE '^[0-9]+:[[:space:]]*#' | grep -E "$LIST_ALL" || true)
done

# --- 8. first-install runs under both majors.
E2E=.github/workflows/e2e.yaml
if [ -f "$E2E" ]; then
	job="$(awk '
		/^jobs:[[:space:]]*$/ { in_jobs = 1; next }
		in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { cur = $1; sub(/:$/, "", cur); next }
		in_jobs && cur == "first-install" && !/^[[:space:]]*#/ { print }' "$E2E")"
	[ -n "$job" ] || err "$E2E: no first-install job"
	matrix="$(grep -E '^[[:space:]]+helm:[[:space:]]*\[' <<<"$job" || true)"
	majors="$({ grep -oE '[0-9]+' <<<"${matrix#*\[}" || true; } | sort -u | tr '\n' ' ')"
	[ "$majors" = "3 4 " ] ||
		err "$E2E: the first-install job's matrix must be 'helm: [\"3\", \"4\"]' (got '${matrix:-<none>}'): e2e/first-install.sh is the suite that runs under both Helm majors (#1543)"
	# shellcheck disable=SC2016 # literal workflow text
	grep -qE '^[[:space:]]+helm-major:[[:space:]]*\$\{\{ matrix\.helm \}\}[[:space:]]*$' <<<"$job" ||
		err "$E2E: the first-install job does not pass 'helm-major: \${{ matrix.helm }}' to run-e2e-script, so both legs would run the default Helm"
	grep -qE '^[[:space:]]+script:[[:space:]]*first-install\.sh[[:space:]]*$' <<<"$job" ||
		err "$E2E: the first-install job no longer runs first-install.sh"
else
	err "$E2E not in runfiles"
fi
RUN_E2E=.github/actions/run-e2e-script/action.yml
if [ -f "$RUN_E2E" ]; then
	found=0
	while IFS="|" read -r _ _ _ major _; do
		# shellcheck disable=SC2016 # literal action text
		[ "$major" = '${{ inputs.helm-major }}' ] && found=1
	done < <(helm_steps "$RUN_E2E" 'uses:[[:space:]]*[.]/[.]github/actions/setup-helm[[:space:]]*(#.*)?$')
	[ "$found" -eq 1 ] || err "$RUN_E2E: its setup-helm step does not pass 'major: \${{ inputs.helm-major }}', so a job cannot choose the Helm major"
else
	err "$RUN_E2E not in runfiles"
fi

if [ "$fail" -ne 0 ]; then
	echo "Helm pin drift: a workflow gets Helm from .github/actions/setup-helm, at a release e2e/helm-version.sh pins (docs/runbook.md, \"Bumping Helm\")" >&2
	exit 1
fi
echo "PASS: every CI helm invocation runs a pinned Helm (default $(helm_pin_version ""))"
