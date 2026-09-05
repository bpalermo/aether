#!/usr/bin/env bash
# Age-matched aether-proxy RSS sampler (#628).
#
# #628 -- "Envoy proxy heap growth under load" -- has never had two AGE-MATCHED
# samples. Every reading so far was taken at whatever age the pods happened to be,
# and since the churn driver recycles the proxy every ~30-90 minutes, a "higher"
# reading on one node is usually just an older incarnation. Without a fixed age the
# ramp-then-plateau question (born-hot vs leak) cannot be answered.
#
# This samples the proxy container's working set together with the pod's age, so
# rows are comparable across nodes, generations and runs.
#
# Usage:
#   e2e/soak/sample-proxy-rss.sh                 # sample right now, whatever the ages
#   e2e/soak/sample-proxy-rss.sh --at-age 1800   # wait until the YOUNGEST proxy pod
#                                                # is 1800s old, then sample once
#
# --at-age polls every 15s for at most 20 minutes. If the deadline passes it samples
# anyway and warns on stderr -- a row with an honest age_seconds beats no row at all.
#
# Rows are appended to /tmp/soak-proxy-rss.tsv (header written once) and echoed to
# stdout:
#
#   timestamp             node            pod                age_seconds  working_set_mi
#   2026-09-05T01:35:04Z  main-worker-01  aether-proxy-abcde  1803         241
#
# Requires metrics-server (`kubectl top`), which talos-main runs.
set -uo pipefail

NS="${SOAK_PROXY_NS:-aether-system}"
SELECTOR="${SOAK_PROXY_SELECTOR:-app.kubernetes.io/name=aether-proxy}"
CONTAINER="${SOAK_PROXY_CONTAINER:-proxy}"
OUT="${SOAK_PROXY_RSS_TSV:-/tmp/soak-proxy-rss.tsv}"
POLL_SECONDS="${SOAK_PROXY_RSS_POLL:-15}"
MAX_WAIT_SECONDS="${SOAK_PROXY_RSS_MAX_WAIT:-1200}"

AT_AGE=""

usage() {
	sed -n '2,30p' "$0"
	exit "${1:-0}"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--at-age)
		AT_AGE="${2:-}"
		if ! [[ "$AT_AGE" =~ ^[0-9]+$ ]]; then
			echo "sample-proxy-rss: --at-age needs a whole number of seconds" >&2
			exit 2
		fi
		shift 2
		;;
	-h | --help) usage 0 ;;
	*)
		echo "sample-proxy-rss: unknown argument '$1'" >&2
		usage 2
		;;
	esac
done

# "<pod> <node> <startTime>" per Running proxy pod. startTime is the incarnation's
# birth, which is what makes a reading age-matched rather than node-matched.
pod_table() {
	kubectl get pods -n "$NS" -l "$SELECTOR" \
		--field-selector=status.phase=Running \
		-o go-template='{{range .items}}{{.metadata.name}} {{.spec.nodeName}} {{.status.startTime}}{{"\n"}}{{end}}' \
		2>/dev/null | grep -v '<no value>'
}

# Age in seconds of the youngest Running proxy pod; empty if there are none.
youngest_age() {
	local now pod node start age min=""
	now=$(date -u +%s)
	while read -r pod node start; do
		[ -n "$start" ] || continue
		age=$(date -u -d "$start" +%s 2>/dev/null) || continue
		age=$((now - age))
		if [ -z "$min" ] || [ "$age" -lt "$min" ]; then min="$age"; fi
	done < <(pod_table)
	printf '%s' "$min"
}

# Working set of the proxy container, in Mi, keyed by pod name. `kubectl top
# --containers` prints POD NAME CPU MEMORY; the proxy pod also runs an `authz`
# container, so filtering on the container name is mandatory.
mem_table() {
	kubectl top pods -n "$NS" -l "$SELECTOR" --containers --no-headers 2>/dev/null |
		awk -v c="$CONTAINER" '$2 == c { print $1, $4 }'
}

# Normalise a kubectl quantity (Ki/Mi/Gi, or bare bytes) to whole Mi.
to_mi() {
	local v="$1"
	case "$v" in
	*Ki) echo $((${v%Ki} / 1024)) ;;
	*Mi) echo "${v%Mi}" ;;
	*Gi) echo $((${v%Gi} * 1024)) ;;
	*[!0-9]*) echo "NA" ;;
	*) echo $((v / 1024 / 1024)) ;;
	esac
}

sample_once() {
	local now ts pod node start age mem mi
	now=$(date -u +%s)
	ts=$(date -u +%FT%TZ)

	local mems
	mems=$(mem_table)
	if [ -z "$mems" ]; then
		echo "sample-proxy-rss: no '$CONTAINER' rows from 'kubectl top pods -n $NS -l $SELECTOR --containers'" >&2
	fi

	if [ ! -s "$OUT" ]; then
		printf 'timestamp\tnode\tpod\tage_seconds\tworking_set_mi\n' >>"$OUT"
	fi

	while read -r pod node start; do
		[ -n "$start" ] || continue
		age=$(date -u -d "$start" +%s 2>/dev/null) || continue
		age=$((now - age))
		mem=$(printf '%s\n' "$mems" | awk -v p="$pod" '$1 == p { print $2; exit }')
		if [ -z "$mem" ]; then
			mi="NA"
		else
			mi=$(to_mi "$mem")
		fi
		printf '%s\t%s\t%s\t%s\t%s\n' "$ts" "$node" "$pod" "$age" "$mi" | tee -a "$OUT"
	done < <(pod_table | sort -k2,2)
}

if [ -n "$AT_AGE" ]; then
	waited=0
	while :; do
		age=$(youngest_age)
		if [ -z "$age" ]; then
			echo "sample-proxy-rss: no Running proxy pods yet (waited ${waited}s)" >&2
		elif [ "$age" -ge "$AT_AGE" ]; then
			break
		fi
		if [ "$waited" -ge "$MAX_WAIT_SECONDS" ]; then
			echo "sample-proxy-rss: youngest proxy pod still ${age:-?}s < ${AT_AGE}s after ${waited}s; sampling anyway" >&2
			break
		fi
		sleep "$POLL_SECONDS"
		waited=$((waited + POLL_SECONDS))
	done
fi

sample_once
