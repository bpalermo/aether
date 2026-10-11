#!/usr/bin/env bash
# //e2e:suite_wiring_test: every kind harness under e2e/ is run by the nightly
# workflow, every job of that workflow is wired the way its siblings are, and
# every install of the aether chart names its cluster.
#
# Two harnesses sat in e2e/ for months with no workflow running them: one had
# stopped building what it loaded, the other could only run against a cluster
# of the maintainer's. Nothing failed, because nothing looked. And a job added
# to .github/workflows/e2e.yaml by hand has four places to be named in; missing
# the last two is silent (a suite that fails at night and is not reported).
#
# Hermetic: reads only its data files (the e2e harnesses, the workflows and the
# composite actions), from the runfiles root of the main repository where they
# sit at their source paths.
#
# Checks:
#   1. every e2e/*.sh that creates a kind cluster is the `script:` of a job of
#      .github/workflows/e2e.yaml, and every `script:` there is such a file
#   2. every job of e2e.yaml other than `build` and `report-failure` (a suite
#      job): `needs: build`; the `if:` every sibling has, with its OWN name
#      after `only ==`; a `timeout-minutes`; a `permissions` block or the
#      workflow's read-only default, never a write; and its body is
#      .github/actions/run-e2e-script or run-conformance, the two actions
#      //e2e:kind_pin_test and //e2e:helm_pin_test hold to the pinned kind and
#      Helm, so a job cannot bring its cluster up by hand
#   3. a job that runs a harness passes a `skip-build-var` the harness reads:
#      otherwise `up` would build the images on the runner, with a Bazel the
#      job never set up
#   4. the `only` choices are `all` plus exactly the suite jobs
#   5. `report-failure` needs `build` plus exactly the suite jobs, and its
#      RESULTS names each of them with that job's own result
#   6. every helm command that installs, upgrades or renders the aether chart,
#      in a harness, a workflow or a composite action, passes `clusterName`:
#      the chart has no cluster name to fall back on that is true of the
#      cluster it is installed in
set -euo pipefail

fail=0
err() {
	echo "FAIL: $*" >&2
	fail=1
}

E2E=.github/workflows/e2e.yaml
[ -f "$E2E" ] || {
	echo "FAIL: $E2E not in runfiles" >&2
	exit 1
}
mapfile -t ci_files < <(find -L .github/workflows .github/actions -type f \( -name '*.yaml' -o -name '*.yml' \) 2>/dev/null | sort)
mapfile -t harnesses < <(find -L e2e -maxdepth 1 -type f -name '*.sh' | sort)
[ "${#harnesses[@]}" -gt 0 ] || err "no e2e harness in runfiles"

# job_names: the keys under `jobs:`.
job_names() {
	awk '
	/^jobs:[[:space:]]*$/ { in_jobs = 1; next }
	in_jobs && /^[^[:space:]#]/ { in_jobs = 0 }
	in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { j = $1; sub(/:$/, "", j); print j }' "$E2E"
}
# job_body NAME: the job's lines, comments dropped.
job_body() {
	awk -v want="$1" '
	/^jobs:[[:space:]]*$/ { in_jobs = 1; next }
	in_jobs && /^[^[:space:]#]/ { in_jobs = 0 }
	in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { cur = $1; sub(/:$/, "", cur); next }
	in_jobs && cur == want && !/^[[:space:]]*#/ { print }' "$E2E"
}
# scalar BODY KEY INDENT: the value of the first `KEY:` at exactly INDENT spaces.
scalar() {
	awk -v key="$2" -v ind="$3" '
	{
		match($0, /[^ ]/)
		if (RSTART - 1 == ind && index($0, key ":") == RSTART && !done) {
			v = substr($0, RSTART + length(key) + 1)
			sub(/^[[:space:]]+/, "", v); sub(/[[:space:]]+$/, "", v)
			print v; done = 1
		}
	}' <<<"$1"
}
# as_set "a, b,c": one name per line, sorted.
as_set() { tr ',' '\n' <<<"$1" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' | grep -v '^$' | sort; }

mapfile -t jobs < <(job_names)
suites=()
for j in "${jobs[@]}"; do
	case "$j" in build | report-failure) ;; *) suites+=("$j") ;; esac
done
[ "${#suites[@]}" -gt 0 ] || err "$E2E: no suite job found: the check is stale"
want_suites="$(printf '%s\n' "${suites[@]}" | sort)"

# --- 2 + 3. every suite job.
# Literal workflow text, not shell to expand.
# shellcheck disable=SC2016
IF_HEAD="github.event_name == 'schedule' || github.event.inputs.only == 'all' || github.event.inputs.only == "
scripts_run=()
for j in "${suites[@]}"; do
	body="$(job_body "$j")"
	got="$(scalar "$body" needs 4)"
	[ "$got" = "build" ] || err "$E2E: job '$j' has 'needs: ${got:-<none>}', want 'needs: build' (the images every suite runs are the build job's artifact)"
	got="$(scalar "$body" if 4)"
	[ "$got" = "${IF_HEAD}'$j'" ] ||
		err "$E2E: job '$j' has 'if: ${got:-<none>}'; want \"${IF_HEAD}'$j'\" — it must run on the schedule, on only=all, and on only=$j"
	[ -n "$(scalar "$body" timeout-minutes 4)" ] || err "$E2E: job '$j' has no timeout-minutes"
	if grep -qE '^[[:space:]]+[a-z-]+:[[:space:]]*write[[:space:]]*$' <<<"$body"; then
		err "$E2E: job '$j' asks for a write permission; a suite job checks out and runs a cluster on the runner, nothing more"
	fi
	uses="$(grep -oE 'uses:[[:space:]]*\./\.github/actions/[a-z0-9-]+' <<<"$body" | sed 's#.*/##' | sort -u | tr '\n' ' ' || true)"
	case "$uses" in
	"run-e2e-script " | "run-conformance ") ;;
	*) err "$E2E: job '$j' uses '${uses:-no composite action}'; a suite job's body is ./.github/actions/run-e2e-script or run-conformance (they install the pinned kind and Helm and load the build job's images)" ;;
	esac
	if grep -qE '^[[:space:]]+(- )?run:' <<<"$body"; then
		err "$E2E: job '$j' has a 'run:' step of its own; what a suite does goes in its harness or in the composite action's inputs"
	fi
	if [ "$uses" = "run-e2e-script " ]; then
		script="$(scalar "$body" script 10)"
		var="$(scalar "$body" skip-build-var 10)"
		if [ -z "$script" ] || [ ! -f "e2e/$script" ]; then
			err "$E2E: job '$j' runs 'script: ${script:-<none>}', which is not a file under e2e/"
		else
			scripts_run+=("e2e/$script")
			# shellcheck disable=SC2016 # the harness's literal text
			if [ -z "$var" ] || ! grep -qF '${'"$var"':-0}' "e2e/$script"; then
				err "$E2E: job '$j' passes 'skip-build-var: ${var:-<none>}', which e2e/$script does not read (\"\${${var:-VAR}:-0}\"): its 'up' would try to build the images on the runner"
			fi
		fi
	fi
done
echo "checked ${#suites[@]} suite job(s)"

# --- 1. every kind harness is run.
creators=0
for f in "${harnesses[@]}"; do
	grep -qE '^[[:space:]]*kind create cluster' "$f" || continue
	creators=$((creators + 1))
	found=0
	for s in "${scripts_run[@]+"${scripts_run[@]}"}"; do
		[ "$s" = "$f" ] && found=1
	done
	[ "$found" -eq 1 ] ||
		err "$f creates a kind cluster and no job of $E2E runs it: a harness nothing runs stops working unnoticed. Add a job (and see checks 2 to 5), or remove the harness"
done
[ "$creators" -gt 0 ] || err "no e2e harness creates a kind cluster: the e2e glob in runfiles is empty or the check is stale"
echo "checked $creators kind harness(es)"

# --- 4. the `only` choices.
options="$(grep -E '^[[:space:]]+options:[[:space:]]*\[' "$E2E" | sed -n '1p' | sed -e 's/^[^[]*\[//' -e 's/\].*$//')"
got="$(as_set "$options" | grep -vx all || true)"
[ "$got" = "$want_suites" ] ||
	err "$E2E: the 'only' choices are not 'all' plus the suite jobs. Only in the choices: [$(comm -23 <(echo "$got") <(echo "$want_suites") | tr '\n' ' ')] only a job: [$(comm -13 <(echo "$got") <(echo "$want_suites") | tr '\n' ' ')]"
as_set "$options" | grep -qx all || err "$E2E: the 'only' choices do not offer 'all'"

# --- 5. report-failure.
body="$(job_body report-failure)"
[ -n "$body" ] || err "$E2E: no report-failure job"
needs="$(scalar "$body" needs 4)"
needs="${needs#[}"
needs="${needs%]}"
got="$(as_set "$needs" | grep -vx build || true)"
[ "$got" = "$want_suites" ] ||
	err "$E2E: report-failure does not need 'build' plus the suite jobs, so a failure of the missing one is never reported. Only in needs: [$(comm -23 <(echo "$got") <(echo "$want_suites") | tr '\n' ' ')] only a job: [$(comm -13 <(echo "$got") <(echo "$want_suites") | tr '\n' ' ')]"
as_set "$needs" | grep -qx build || err "$E2E: report-failure does not need 'build'"
for j in build "${suites[@]}"; do
	# shellcheck disable=SC2016 # literal workflow text
	grep -qE "^[[:space:]]+$j="'\$\{\{ needs\.'"$j"'\.result \}\}[[:space:]]*$' <<<"$body" ||
		err "$E2E: report-failure's RESULTS has no '$j=\${{ needs.$j.result }}' line, so the issue it files could not name that job"
done
results="$(grep -cE '^[[:space:]]+[a-z0-9-]+=\$\{\{ needs\.' <<<"$body" || true)"
[ "$results" -eq $((${#suites[@]} + 1)) ] ||
	err "$E2E: report-failure's RESULTS has $results line(s), want $((${#suites[@]} + 1)) (build and each suite job, once)"

# --- 6. every aether chart install names its cluster.
# One record per shell command: backslash continuations joined, comment lines
# dropped, prefixed with the line the command starts on.
commands() {
	awk '
	function emit(n, cmd) {
		# Only a command that names the aether chart directory as an argument
		# (…/aether, quoted or in a ${VAR:-…} default) is of interest below.
		if (cmd ~ /\/aether[}"]*([[:space:]]|$)/) print n "\t" cmd
	}
	/^[[:space:]]*#/ { next }
	{
		if (!open) { start = NR; buf = "" }
		line = $0
		if (sub(/\\[[:space:]]*$/, " ", line)) { buf = buf line; open = 1; next }
		emit(start, buf line)
		open = 0
	}
	END { if (open) emit(start, buf) }' "$1"
}
installs=0
for f in "${harnesses[@]}" "${ci_files[@]}"; do
	case "$f" in e2e/*_test.sh) continue ;; esac
	while IFS=$'\t' read -r n cmd; do
		# helm (or a harness's wrapper of it), with a verb that renders the chart.
		grep -qE '(^|[^A-Za-z0-9_-])(install|upgrade|template)([[:space:]]|$)' <<<"$cmd" || continue
		grep -qE '(^|[[:space:]"/-])(helm|hc)[[:space:]]' <<<"$cmd" || continue
		installs=$((installs + 1))
		grep -qE -- '--set[[:space:]]+"?clusterName=' <<<"$cmd" ||
			err "$f:$n: installs or renders the aether chart without --set clusterName=<name>: name the cluster (in a multi-cluster suite, each one differently)"
	done < <(commands "$f")
done
# 10 harnesses, the conformance action and ci.yaml's render at the time of
# writing; fewer than the harnesses means the pattern no longer sees them.
[ "$installs" -ge "$creators" ] ||
	err "found only $installs helm command(s) on the aether chart for $creators kind harness(es): the check is stale"
echo "checked $installs helm command(s) on the aether chart"

if [ "$fail" -ne 0 ]; then
	echo "see e2e/README.md, \"Adding a suite\"" >&2
	exit 1
fi
echo "PASS: ${#suites[@]} suite jobs, $creators kind harnesses, $installs aether chart installs"
