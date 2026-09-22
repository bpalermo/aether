#!/usr/bin/env bash
# Prove the proposal-037 any-port shim's counter can fire, before trusting a zero.
#
# Proposal 037 Phase 4 deletes the portless TCP floor chain once
# `cap_tcp_anyport_<svc>` reads zero across a full release. That gate is only
# worth anything if the counter would have MOVED had a legacy client existed --
# otherwise zero means "nobody looked", and deleting the chain on the strength of
# it is a guess wearing the costume of evidence (aether#853; see
# feedback_gate_periodic_events_shorter_than_validation).
#
# This script drives the one spelling nothing else drives: a raw-TCP dial at a
# TCP-primary service on a port that is neither the TCP mesh port (18082) nor the
# service's primary. Envoy ranks destination_port above prefix_ranges, so the two
# qualified chains claim their own ports and everything else falls through to the
# portless shim -- which is precisely the legacy client Phase 4 is asking about.
#
# Run it ONCE before the soak, read the counter either side, and record both
# numbers. Then the soak's own zero is a measurement.
#
# Usage:
#   e2e/soak/anyport-probe.sh [DIALS]
#
# The counter is NOT read here on purpose: the Prometheus LB (192.168.100.51) is
# not reachable from the workstation, so the read goes through Grafana. The
# script prints the exact query for both sides.
set -euo pipefail

NS="${NS:-aether-test}"
CTX="${CTX:-talos-main}"
DOMAIN="${DOMAIN:-aether.internal}"
# A TCP-PRIMARY service: the portless floor chain exists only for those.
SVC="${SVC:-tcp-echo}"
# Neither 18082 nor tcp-echo's primary 9000, so nothing more specific claims it.
PORT="${PORT:-19999}"
DIALS="${1:-20}"

k() { kubectl --context "$CTX" -n "$NS" "$@"; }

FQDN="${SVC}.${NS}.${DOMAIN}"
# Match the counter by PATTERN, never by a name assembled here.
#
# The stat prefix is "cap_tcp_anyport_" + the ENVOY CLUSTER name, and that
# carries the scheme: `tcp:tcp-echo.aether-test.aether.internal`, which the OTLP
# ingest flattens to `tcp_tcp_echo_aether_test_aether_internal` -- a doubled
# `tcp_` that a name derived from the service FQDN silently omits. A query built
# that way returns "no data", which reads exactly like a legitimate zero and
# would retire the shim on the strength of a typo. The pattern cannot be wrong,
# and it covers every service rather than just this one -- which is what the
# Phase 4 gate actually asks.
METRIC='{__name__=~"envoy_tcp_cap_tcp_anyport_.*_downstream_cx_total"}'

cat <<TXT

==> any-port shim probe (proposal 037 Phase 4 gate)
    target : ${FQDN}:${PORT}  (${DIALS} dials)
    counter: ${METRIC}

    READ THE COUNTER NOW, BEFORE CONTINUING -- in Grafana, datasource
    "prometheus":

      sum(${METRIC})

    A counter Envoy has never incremented has NO series at all, so "no data" is
    the expected before-reading, and it is a legitimate zero.

TXT

k delete job anyport-probe --ignore-not-found >/dev/null 2>&1 || true
cat <<YAML | kubectl --context "$CTX" apply -f - >/dev/null
apiVersion: v1
kind: ServiceAccount
metadata: {name: anyport-probe, namespace: $NS}
---
apiVersion: batch/v1
kind: Job
metadata: {name: anyport-probe, namespace: $NS}
spec:
  backoffLimit: 0
  template:
    metadata:
      labels: {app: anyport-probe, aether.io/managed: "true"}
      annotations:
        config.aether.io/upstreams: "$SVC"
    spec:
      serviceAccountName: anyport-probe
      restartPolicy: Never
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: probe
          image: curlimages/curl:8.22.0
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: ["ALL"]}
          command: ["sh", "-c"]
          args:
            - |
              set -u
              ok=0; fail=0
              i=0
              while [ \$i -lt $DIALS ]; do
                i=\$((i+1))
                out=\$( (echo "anyport-\$i"; sleep 1) | timeout 5 nc "$FQDN" $PORT 2>/dev/null || true)
                case "\$out" in
                  *hello*) ok=\$((ok+1)) ;;
                  *) fail=\$((fail+1)) ;;
                esac
              done
              # Both outcomes are informative. A dial that SUCCEEDS proves the
              # shim still forwards to the primary port, which is the behaviour
              # Phase 4 would remove. A dial that FAILS still increments
              # downstream_cx_total, because the counter counts ACCEPTED
              # connections, not successful ones.
              echo "RESULT anyport dials=$DIALS ok=\$ok fail=\$fail"
YAML

k wait --for=condition=complete job/anyport-probe --timeout=180s >/dev/null 2>&1 || true
echo "==> probe output"
k logs job/anyport-probe 2>/dev/null || echo "  (no logs)"

cat <<TXT

==> READ THE COUNTER AGAIN (allow ~60s for the OTLP -> Prometheus hop):

      sum(${METRIC})

    Expect it to have risen by ${DIALS}. If it did NOT move, the gate is vacuous
    and Phase 4 has no evidence path -- that is a finding, not a passing run.

    Then clean up so the soak starts with the shim quiet:
      kubectl --context $CTX -n $NS delete job anyport-probe
      kubectl --context $CTX -n $NS delete serviceaccount anyport-probe

TXT
