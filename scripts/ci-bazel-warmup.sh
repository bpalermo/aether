#!/usr/bin/env bash
# CI warm-up: fetch + analyse, never build, with a bounded retry on FETCH
# failures only (#1001).
#
#   scripts/ci-bazel-warmup.sh [bazel build flags...] <targets...>
#
# Runs `bazel build --nobuild <args>` — loading and analysis, which is where
# every external repository a job needs gets fetched — before the job's first
# real Bazel step. A repository fetch that fails on a transient network error
# (proxy.golang.org `TLS handshake timeout`, a github.com release 504) is retried
# up to WARMUP_ATTEMPTS times with a doubling backoff, so a cold or partly-cold
# repository cache no longer turns a flaky download into a red job. Anything
# else (a broken BUILD file, a bad flag) is a real failure and is NOT retried:
# retrying it would only delay the red by the whole backoff.
#
# The warm-up never talks to BuildBuddy: the remote executor, remote cache and
# BES backend are cleared AFTER the caller's flags, so `--config=ci` can be
# passed as-is (its platforms and toolchains decide which repositories the real
# step will need) without a BuildBuddy key. Remote settings of the real steps are
# untouched.
#
# Environment:
#   BAZEL                   bazel binary (default: bazel)
#   WARMUP_ATTEMPTS         total attempts (default: 3)
#   WARMUP_BACKOFF_SECONDS  wait before the 2nd attempt, doubled after (default: 20)
#
# scripts/check-ci-bazel-warmup.sh gates the retry/no-retry behaviour with a
# stub bazel (the `shell` job in ci.yaml runs it).
set -uo pipefail

bazel="${BAZEL:-bazel}"
attempts="${WARMUP_ATTEMPTS:-3}"
delay="${WARMUP_BACKOFF_SECONDS:-20}"

if [ "$#" -eq 0 ]; then
	echo "usage: $0 [bazel build flags...] <targets...>" >&2
	exit 2
fi

# What a transient fetch failure looks like in Bazel's output. The first line is
# Bazel's own wrapper for ANY repository rule failure (it names the repository);
# the rest cover downloads that fail outside a repository rule (bazelisk fetching
# Bazel itself, the module registry).
fetch_failure='An error occurred during the fetch of repository|fetch_repo: |Error downloading|Error accessing registry|could not download Bazel|TLS handshake timeout|connection reset by peer|i/o timeout'

log="$(mktemp)"
trap 'rm -f "$log"' EXIT

n=1
while :; do
	echo ">> bazel warm-up attempt ${n}/${attempts}: build --nobuild (fetch + analysis only)"
	"$bazel" build --nobuild "$@" --remote_executor= --remote_cache= --bes_backend= 2>&1 | tee "$log"
	rc="${PIPESTATUS[0]}"
	if [ "$rc" -eq 0 ]; then
		echo ">> bazel warm-up succeeded on attempt ${n}/${attempts}"
		exit 0
	fi
	if ! grep -Eq "$fetch_failure" "$log"; then
		echo "::error::bazel warm-up failed (exit ${rc}) with no repository-fetch error in its output — a real failure, not retried"
		exit "$rc"
	fi
	if [ "$n" -ge "$attempts" ]; then
		echo "::error::bazel warm-up: repository fetch failed on all ${attempts} attempts (last exit ${rc}). If the log names proxy.golang.org or github.com, the upstream is down for longer than the backoff: re-run the job later (docs/runbook.md, 'CI: external repository fetches')."
		exit "$rc"
	fi
	echo "::warning::bazel warm-up attempt ${n}/${attempts} hit a repository-fetch error (exit ${rc}); retrying in ${delay}s"
	sleep "$delay"
	delay=$((delay * 2))
	n=$((n + 1))
done
