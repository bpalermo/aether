#!/bin/sh
# The workload of churn.sh's new-ServiceAccount step (#1008/#1014). NOT run on
# the workstation: churn.sh ships it to the cluster as a per-step ConfigMap and
# runs it with busybox sh inside curlimages/curl (already resident on every
# node for the udp-dialer, so the pod starts without a pull).
#
# The pod runs under a ServiceAccount that did not exist a second earlier, so its
# first request is the node proxy's first request from that identity. It drives
# HTTP at every destination in NEWSA_TARGETS for NEWSA_SECONDS, starting the
# instant the container starts, with the user agent aether-soak-newsa/<step> so
# the source proxies' access logs can be filtered to exactly these requests.
#
# Output (stdout, so churn.sh reads it with `kubectl logs`):
#   AETHER_NEWSA <ts> step=… node=… dst=… t=Ns ok=… non2xx=… connerr=…     every ~10s, per destination
#   AETHER_NEWSA_FINAL step=… node=… <dst>:ok=…,non2xx=…,connerr=…,codes=… …   once
#   AETHER_METRIC newsa_tally={…}                                           once, the same numbers as JSON
#
#   ok       2xx responses
#   non2xx   any other HTTP status (a 503/NC from the source proxy lands here)
#   connerr  no HTTP status at all (curl http_code 000: DNS, connect, timeout);
#            `codes` names the curl exit code (6 = DNS, 7 = connect, 28 = timeout)
#
# Env (all set by churn.sh): NEWSA_STEP, NEWSA_NODE, NEWSA_SECONDS, NEWSA_RPS,
# NEWSA_TARGETS ("name=url name=url", url ends in a path).
set -u

STEP="${NEWSA_STEP:?}"
NODE="${NEWSA_NODE:-unknown}"
SECONDS_TOTAL="${NEWSA_SECONDS:-120}"
RPS="${NEWSA_RPS:-20}"
TARGETS="${NEWSA_TARGETS:?}"
UA="aether-soak-newsa/$STEP"
OUT=/tmp/newsa

mkdir -p "$OUT"

ndst=0
for t in $TARGETS; do ndst=$((ndst + 1)); done
# The step's rate is shared across its destinations (20 rps -> 10 rps each).
per=$((RPS / ndst))
if [ "$per" -lt 1 ]; then per=1; fi
batch=$((per * 10))

# One destination: 10-second batches of `batch` requests, paced by curl's own
# --rate, until SECONDS_TOTAL has elapsed. One curl process per batch keeps the
# fork cost at one per 10s instead of one per request.
drive() { # $1 name, $2 url
	name=$1
	url=$2
	ok=0
	bad=0
	conn=0
	start=$(date +%s)
	: >"$OUT/$name.codes"
	while [ $(($(date +%s) - start)) -lt "$SECONDS_TOTAL" ]; do
		curl -s --out-null --rate "${per}/s" --connect-timeout 2 -m 5 \
			-A "$UA" -w '%{http_code} %{exitcode}\n' \
			"${url}?newsa=[1-${batch}]" >"$OUT/$name.batch" 2>/dev/null
		while read -r code rc; do
			case "$code" in
			2??) ok=$((ok + 1)) ;;
			000)
				conn=$((conn + 1))
				echo "err$rc" >>"$OUT/$name.codes"
				;;
			*)
				bad=$((bad + 1))
				echo "$code" >>"$OUT/$name.codes"
				;;
			esac
		done <"$OUT/$name.batch"
		echo "AETHER_NEWSA $(date -u +%FT%TZ) step=$STEP node=$NODE dst=$name t=$(($(date +%s) - start))s ok=$ok non2xx=$bad connerr=$conn"
	done
	# "503x150+err6x3", or "-" when every request was a 2xx.
	codes=$(sort "$OUT/$name.codes" | uniq -c | awk '{printf "%s%sx%s", (NR > 1 ? "+" : ""), $2, $1}')
	echo "$name $ok $bad $conn ${codes:--}" >"$OUT/$name.final"
}

for t in $TARGETS; do
	drive "${t%%=*}" "${t#*=}" &
done
wait

final="AETHER_NEWSA_FINAL step=$STEP node=$NODE"
json=""
for t in $TARGETS; do
	read -r name ok bad conn codes <"$OUT/${t%%=*}.final"
	final="$final $name:ok=$ok,non2xx=$bad,connerr=$conn,codes=$codes"
	json="$json${json:+,}\"$name\":{\"ok\":$ok,\"non2xx\":$bad,\"connerr\":$conn,\"codes\":\"$codes\"}"
done
echo "$final"
echo "AETHER_METRIC newsa_tally={\"step\":\"$STEP\",\"node\":\"$NODE\",\"seconds\":$SECONDS_TOTAL,\"dst\":{$json}}"

# Stay up until churn.sh deletes the Deployment: exiting would restart the
# container and start a SECOND burst under an identity that is no longer new.
exec sleep 86400
