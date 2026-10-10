#!/usr/bin/env bash
# Hermetic test of scripts/docker-hub-mirror.sh and of what keeps the CI jobs
# off Docker Hub's anonymous limit (#1602). No Docker, no network: a fake
# `docker`, `curl`, `sudo`, `systemctl` and `sleep` on PATH.
#
#   1. the daemon: the mirror is added in front of whatever daemon.json holds,
#      every other key survives, the daemon is RELOADED (never restarted), and
#      a daemon that already lists the mirror is left alone. A file that cannot
#      be parsed, a reload that fails or one that never takes effect is a
#      warning and the pull still happens.
#   2. the pull: by the unchanged, digest-pinned reference; a reference with no
#      digest is refused before anything is touched; a failed pull is retried
#      and fails the script only after the last attempt.
#   3. the mirror probe: asked for the DIGEST under the Docker Hub repository
#      path (`library/` for an official image, no registry host); anything but
#      a 200 for that digest is a warning that names the fall-back to Docker
#      Hub; an image of another registry is not probed.
#   4. the tree: `--config=ci` turns testcontainers' reaper off (the one
#      Docker Hub pull of the race and integration jobs), the workflow steps
#      that run the impacted test lists use that config, and no workflow or
#      action sets the variable itself.
#
# Run: bazel test //scripts:docker_hub_mirror_test (jq is the Bazel-pinned
#      one), or bash scripts/docker_hub_mirror_test.sh with jq on PATH.
# shellcheck disable=SC2016,SC2034 # a condition handed to `expect` is single-
# quoted on purpose: `expect` evaluates it, and that is where its variables
# (some of them set only for it) are read.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/docker-hub-mirror.sh"
ROOT="$HERE/.."

# Under Bazel, JQ_RLOCATIONPATH names the pinned jq in the runfiles.
if [ -n "${JQ_RLOCATIONPATH:-}" ]; then
	JQ="${TEST_SRCDIR:-${RUNFILES_DIR:-$PWD/..}}/${JQ_RLOCATIONPATH}"
fi
JQ="${JQ:-$(command -v jq)}"
[ -x "$JQ" ] || {
	echo "FAIL: no jq (JQ=${JQ})"
	exit 1
}
export JQ

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}

D1="sha256:$(printf 'a%.0s' {1..64})"
D2="sha256:$(printf 'b%.0s' {1..64})"
NODE="kindest/node:v1.2.3@$D1"
MIRROR_URL="https://mirror.gcr.io"

# --- the fakes -----------------------------------------------------------------
BIN="$TMP/bin"
mkdir -p "$BIN"
cat >"$BIN/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker $*" >>"$STATE/calls"
case "$1" in
info)
	if [ -e "$STATE/listed" ]; then echo '["https://mirror.gcr.io/"]'; else echo '[]'; fi
	;;
pull)
	n=$(cat "$STATE/pulls" 2>/dev/null || echo 0)
	n=$((n + 1))
	echo "$n" >"$STATE/pulls"
	[ "$n" -gt "$(cat "$STATE/pull_failures" 2>/dev/null || echo 0)" ] || {
		echo "Error response from daemon: Get \"https://auth.docker.io/token\": net/http: request canceled (Client.Timeout exceeded)" >&2
		exit 1
	}
	;;
image) echo "[\"${3:-}\"]" ;;
esac
EOF
cat >"$BIN/sudo" <<'EOF'
#!/usr/bin/env bash
echo "sudo $*" >>"$STATE/calls"
exec "$@"
EOF
cat >"$BIN/systemctl" <<'EOF'
#!/usr/bin/env bash
echo "systemctl $*" >>"$STATE/calls"
[ ! -e "$STATE/reload_fails" ] || exit 1
# The daemon applies what the file says at the time of the signal.
if [ -e "$STATE/reload_applies" ] && grep -q '"https://mirror.gcr.io"' "$DOCKER_DAEMON_JSON"; then
	touch "$STATE/listed"
fi
EOF
cat >"$BIN/curl" <<'EOF'
#!/usr/bin/env bash
echo "curl ${*: -1}" >>"$STATE/calls"
cat "$STATE/curl_out" 2>/dev/null
exit "$(cat "$STATE/curl_rc" 2>/dev/null || echo 0)"
EOF
cat >"$BIN/sleep" <<'EOF'
#!/usr/bin/env bash
echo "sleep $*" >>"$STATE/calls"
EOF
chmod +x "$BIN"/*

# new_case: a fresh state directory. By default the reload takes effect and the
# mirror answers 200 with digest D1.
new_case() {
	STATE="$TMP/state.$1"
	rm -rf "$STATE"
	mkdir -p "$STATE/etc"
	: >"$STATE/calls"
	touch "$STATE/reload_applies"
	mirror_answers 200 "$D1"
	export STATE DOCKER_DAEMON_JSON="$STATE/etc/daemon.json"
}
mirror_answers() {
	printf 'HTTP/2 %s \r\ncontent-type: application/vnd.oci.image.index.v1+json\r\nDocker-Content-Digest: %s\r\n\r\n' "$1" "$2" >"$STATE/curl_out"
}
# run <args>: status in RC, output in OUT.
run() {
	OUT="$(PATH="$BIN:$PATH" HUB_MIRROR_RELOAD_WAIT=2 bash "$SCRIPT" "$@" 2>&1)"
	RC=$?
}
calls() { grep -cE -- "$1" "$STATE/calls" || true; }
expect() {
	# expect <name> <condition as a shell test, evaluated here>
	if eval "$2"; then pass "$1"; else
		fail "$1 [$2]"
		printf '  rc=%s\n  out:\n%s\n  calls:\n%s\n' "$RC" "$OUT" "$(cat "$STATE/calls")" | sed 's/^/    /'
	fi
}

# --- 1. the daemon -------------------------------------------------------------
new_case merge
echo '{"exec-opts":["native.cgroupdriver=cgroupfs"],"registry-mirrors":["https://other.example"]}' >"$DOCKER_DAEMON_JSON"
run "$NODE"
expect "merge: succeeds" '[ "$RC" -eq 0 ]'
expect "merge: the mirror goes first and the existing mirror is kept" \
	'[ "$("$JQ" -c ".[\"registry-mirrors\"]" "$DOCKER_DAEMON_JSON")" = "[\"$MIRROR_URL\",\"https://other.example\"]" ]'
expect "merge: every other key of daemon.json survives" \
	'[ "$("$JQ" -c ".[\"exec-opts\"]" "$DOCKER_DAEMON_JSON")" = "[\"native.cgroupdriver=cgroupfs\"]" ]'
expect "merge: the daemon is reloaded once, as root" '[ "$(calls "^sudo systemctl reload docker$")" -eq 1 ]'
expect "merge: the daemon is never restarted" '[ "$(calls "systemctl (restart|stop|start)")" -eq 0 ]'
expect "merge: the image is pulled by its unchanged reference, after the reload" \
	'[ "$(grep -nE "^(systemctl reload|docker pull)" "$STATE/calls" | cut -d: -f2- | tr "\n" "|")" = "systemctl reload docker|docker pull $NODE|" ]'
expect "merge: no warning" '! grep -q "::warning::" <<<"$OUT"'

new_case no_file
run "$NODE"
expect "no daemon.json: one is written with the mirror" \
	'[ "$("$JQ" -c . "$DOCKER_DAEMON_JSON")" = "{\"registry-mirrors\":[\"$MIRROR_URL\"]}" ]'
expect "no daemon.json: reloaded and pulled" '[ "$RC" -eq 0 ] && [ "$(calls "^systemctl reload")" -eq 1 ] && [ "$(calls "^docker pull")" -eq 1 ]'

new_case already
touch "$STATE/listed"
echo '{"registry-mirrors":["https://mirror.gcr.io"]}' >"$DOCKER_DAEMON_JSON"
before="$(cat "$DOCKER_DAEMON_JSON")"
run "$NODE"
expect "already listed: the file and the daemon are left alone" \
	'[ "$RC" -eq 0 ] && [ "$(cat "$DOCKER_DAEMON_JSON")" = "$before" ] && [ "$(calls "^systemctl")" -eq 0 ]'
expect "already listed: still pulled, no warning" '[ "$(calls "^docker pull")" -eq 1 ] && ! grep -q "::warning::" <<<"$OUT"'

new_case same_twice
echo '{"registry-mirrors":["https://mirror.gcr.io"]}' >"$DOCKER_DAEMON_JSON"
run "$NODE"
expect "named in the file but not by the daemon: reloaded, and the mirror is not listed twice" \
	'[ "$(calls "^systemctl reload")" -eq 1 ] && [ "$("$JQ" -c ".[\"registry-mirrors\"]" "$DOCKER_DAEMON_JSON")" = "[\"$MIRROR_URL\"]" ]'

new_case bad_json
echo 'not json' >"$DOCKER_DAEMON_JSON"
run "$NODE"
expect "unparsable daemon.json: left untouched, not reloaded" \
	'[ "$(cat "$DOCKER_DAEMON_JSON")" = "not json" ] && [ "$(calls "^systemctl")" -eq 0 ]'
expect "unparsable daemon.json: a warning that names Docker Hub, and the pull still happens" \
	'[ "$RC" -eq 0 ] && grep -q "::warning::.*goes to Docker Hub" <<<"$OUT" && [ "$(calls "^docker pull")" -eq 1 ]'

new_case unwritable
: >"$STATE/blocker"
export DOCKER_DAEMON_JSON="$STATE/blocker/daemon.json"
run "$NODE"
expect "daemon.json cannot be written: not reloaded, a warning that names Docker Hub, and the pull still happens" \
	'[ "$RC" -eq 0 ] && [ "$(calls "^systemctl")" -eq 0 ] && grep -q "::warning::.*cannot write" <<<"$OUT" && grep -q "::warning::.*goes to Docker Hub" <<<"$OUT" && [ "$(calls "^docker pull")" -eq 1 ]'

new_case reload_fails
touch "$STATE/reload_fails"
run "$NODE"
expect "reload fails: a warning that names Docker Hub, and the pull still happens" \
	'[ "$RC" -eq 0 ] && grep -q "::warning::.*reload docker. failed" <<<"$OUT" && grep -q "::warning::.*goes to Docker Hub" <<<"$OUT" && [ "$(calls "^docker pull")" -eq 1 ]'

new_case reload_ignored
rm "$STATE/reload_applies"
run "$NODE"
expect "reload has no effect: waited for, then a warning that names Docker Hub; the pull still happens" \
	'[ "$RC" -eq 0 ] && [ "$(calls "^sleep 1$")" -eq 2 ] && grep -q "::warning::.*does not list" <<<"$OUT" && grep -q "::warning::.*goes to Docker Hub" <<<"$OUT" && [ "$(calls "^docker pull")" -eq 1 ]'

new_case no_images
run
expect "no image named: the daemon is configured and nothing is pulled" \
	'[ "$RC" -eq 0 ] && [ "$(calls "^systemctl reload")" -eq 1 ] && [ "$(calls "^docker pull")" -eq 0 ] && [ "$(calls "^curl")" -eq 0 ]'

# --- 2. the pull ---------------------------------------------------------------
for ref in "kindest/node:v1.2.3" "kindest/node" "kindest/node:v1.2.3@sha256:abc" "kindest/node@$D1 "; do
	new_case unpinned
	run "$NODE" "$ref"
	expect "unpinned '$ref': refused with exit 2" '[ "$RC" -eq 2 ] && grep -q "::error::.*not pinned by digest" <<<"$OUT"'
	expect "unpinned '$ref': nothing was touched (no reload, no pull, not even the pinned image)" \
		'[ ! -s "$STATE/calls" ] && [ ! -e "$DOCKER_DAEMON_JSON" ]'
done

new_case retry
echo 2 >"$STATE/pull_failures"
run "$NODE"
expect "pull fails twice: retried, third attempt succeeds" '[ "$RC" -eq 0 ] && [ "$(calls "^docker pull $NODE$")" -eq 3 ]'
expect "pull fails twice: backs off 10 s then 20 s, with a warning each time" \
	'[ "$(grep -E "^sleep" "$STATE/calls" | tr "\n" "|")" = "sleep 10|sleep 20|" ] && [ "$(grep -c "::warning::.*attempt [12]/3" <<<"$OUT")" -eq 2 ]'

new_case exhausted
echo 99 >"$STATE/pull_failures"
run "$NODE" "curlimages/curl@$D2"
expect "pull never succeeds: exit 1 after 3 attempts, with an error that says re-run" \
	'[ "$RC" -eq 1 ] && [ "$(calls "^docker pull")" -eq 3 ] && grep -q "::error::.*could not pull $NODE in 3 attempts.*re-run" <<<"$OUT"'

new_case two
run "$NODE" "docker.io/library/alpine:3@$D1"
expect "two images: both pulled, each by its own reference, one reload" \
	'[ "$RC" -eq 0 ] && [ "$(calls "^docker pull $NODE$")" -eq 1 ] && [ "$(calls "^docker pull docker.io/library/alpine:3@$D1$")" -eq 1 ] && [ "$(calls "^systemctl reload")" -eq 1 ]'

# --- 3. the mirror probe -------------------------------------------------------
probe_url() { grep -E '^curl ' "$STATE/calls" | sed 's/^curl //' | tr '\n' '|'; }
check_repo() {
	# check_repo <reference> <repository path the mirror is asked for>
	new_case repo
	WANT_REPO="$2"
	run "$1"
	expect "probe: '$1' is asked for as $2 by digest" \
		'[ "$RC" -eq 0 ] && [ "$(probe_url)" = "$MIRROR_URL/v2/$WANT_REPO/manifests/$D1|" ]'
}
check_repo "$NODE" kindest/node
check_repo "kindest/node@$D1" kindest/node
check_repo "alpine:3.20@$D1" library/alpine
check_repo "alpine@$D1" library/alpine
check_repo "docker.io/kindest/node:v1@$D1" kindest/node
check_repo "index.docker.io/library/alpine:3@$D1" library/alpine
check_repo "org/team/image:1@$D1" org/team/image

for ref in "gcr.io/etcd-development/etcd:v3@$D1" "registry.k8s.io/pause:3.10@$D1" "localhost:5000/x:1@$D1" "localhost/x@$D1" "quay.io/coreos/etcd@$D1"; do
	new_case foreign
	run "$ref"
	expect "probe: '$ref' is not a Docker Hub image: not probed, pulled as it is, no warning" \
		'[ "$RC" -eq 0 ] && [ "$(calls "^curl")" -eq 0 ] && [ "$(calls "^docker pull $ref$")" -eq 1 ] && ! grep -q "::warning::" <<<"$OUT"'
done

new_case served
run "$NODE"
expect "mirror serves the digest: said so, no warning" \
	'grep -q "$MIRROR_URL serves kindest/node@$D1; the daemon asks it first" <<<"$OUT" && ! grep -q "::warning::" <<<"$OUT"'

new_case absent
mirror_answers 404 ""
run "$NODE"
expect "mirror answers 404: a warning that names the fall-back to Docker Hub, and the pull still happens" \
	'[ "$RC" -eq 0 ] && grep -q "::warning::.*does not serve kindest/node@$D1 (HTTP 404).*falls back to Docker Hub" <<<"$OUT" && [ "$(calls "^docker pull")" -eq 1 ]'

new_case other_digest
mirror_answers 200 "$D2"
run "$NODE"
expect "mirror answers with another digest: a warning, not 'serves'" \
	'[ "$RC" -eq 0 ] && grep -q "::warning::.*does not serve kindest/node@$D1 (HTTP 200 with digest .$D2.)" <<<"$OUT" && ! grep -q "the daemon asks it first" <<<"$OUT"'

new_case unreachable
: >"$STATE/curl_out"
echo 28 >"$STATE/curl_rc"
run "$NODE"
expect "mirror does not answer: a warning, and the pull still happens" \
	'[ "$RC" -eq 0 ] && grep -q "::warning::.*does not serve kindest/node@$D1 (no answer)" <<<"$OUT" && [ "$(calls "^docker pull")" -eq 1 ]'

new_case served_unconfigured
touch "$STATE/reload_fails"
run "$NODE"
expect "mirror serves it but the daemon was not given the mirror: a warning, never 'the daemon asks it first'" \
	'grep -q "::warning::.*serves kindest/node@$D1, but the daemon was not given the mirror" <<<"$OUT" && ! grep -q "the daemon asks it first" <<<"$OUT"'

# --- 4. the tree ---------------------------------------------------------------
BAZELRC="$ROOT/.bazelrc"
RYUK_LINE='test:ci --test_env=TESTCONTAINERS_RYUK_DISABLED=true'
if grep -qxF -- "$RYUK_LINE" "$BAZELRC"; then
	pass ".bazelrc: --config=ci turns the testcontainers reaper off"
else
	fail ".bazelrc has no '$RYUK_LINE': the race and integration jobs pull testcontainers/ryuk from Docker Hub again"
fi

# A step that runs the impacted lists (the only way a workflow reaches the
# testcontainers tests) names no target on its `bazel test` line.
listed=0
for wf in ci.yaml main.yaml; do
	f="$ROOT/.github/workflows/$wf"
	[ -f "$f" ] || {
		fail "$f not found"
		continue
	}
	while IFS= read -r line; do
		case "$line" in *//*) continue ;; esac
		listed=$((listed + 1))
		case "$line" in
		*--config=ci*) ;;
		*) fail "$wf: '$line' runs a test list without --config=ci, so with the testcontainers reaper and its Docker Hub pull" ;;
		esac
	done < <(grep -E '^[[:space:]]*bazel test( |$)' "$f" | sed -E 's/^[[:space:]]+//')
done
if [ "$listed" -ge 4 ]; then
	pass "workflows: $listed 'bazel test' step(s) over a test list, all with --config=ci"
else
	fail "workflows: only $listed 'bazel test' step(s) over a test list found in ci.yaml and main.yaml; the check is stale"
fi

# The switch has one home. A workflow that sets it could set it to false.
hits="$(grep -rnE 'TESTCONTAINERS_(RYUK|HUB_IMAGE)' "$ROOT/.github" || true)"
if [ -z "$hits" ]; then
	pass "workflows and actions: none sets a testcontainers variable of its own"
else
	fail "a workflow or action sets a testcontainers variable; .bazelrc (test:ci) is the one place: $hits"
fi

echo
if [ "$FAILS" -ne 0 ]; then
	echo "docker_hub_mirror_test: $FAILS failure(s)"
	exit 1
fi
echo "docker_hub_mirror_test: all passed"
