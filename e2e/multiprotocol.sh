#!/usr/bin/env bash
# Multi-protocol per service (proposal 037) end-to-end check.
#
# Deploys ONE workload that serves an HTTP primary on :8080 and a raw-TCP
# secondary on :9000 — the shape the whole proposal exists to enable, and the
# one no existing suite covers — then drives it from a mesh client and asserts
# both protocols reach the right port.
#
# Why a Job rather than `kubectl exec`: exec into talos-main is not available to
# this harness, so the checks have to run INSIDE the mesh as a managed workload
# and report through their own logs. That is also closer to what a real caller
# experiences: the client pod is captured, so its dials traverse the capture
# listener exactly as an application's would.
#
# Usage:
#   e2e/multiprotocol.sh [--keep]
#
# --keep leaves the workloads in place for inspection.
set -euo pipefail

NS="${NS:-aether-test}"
CTX="${CTX:-talos-main}"
DOMAIN="${DOMAIN:-aether.internal}"
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

k() { kubectl --context "$CTX" -n "$NS" "$@"; }

log() { printf '\n==> %s\n' "$*"; }
ok() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad() {
	printf '  \033[31mFAIL\033[0m %s\n' "$*"
	FAILED=1
}
FAILED=0

# Everything this script creates carries OWNER_LABEL, and cleanup deletes BY
# THAT LABEL rather than by name.
#
# Deleting by name is how this script destroyed the soak harness's long-lived
# workload (#918). Both harnesses had called theirs `mixed-svc` in this same
# namespace, so a correct, successful e2e run tore down the workload driving
# proposal 037's Phase 4 evidence -- and printed PASS while doing it, because
# the run itself had genuinely passed. Renaming to mp-e2e-svc makes that one
# collision impossible; deleting by label makes the CLASS impossible, because a
# selector cannot match an object this script did not create.
OWNER_LABEL="app.kubernetes.io/created-by=multiprotocol-e2e"

cleanup() {
	[ "$KEEP" = "1" ] && {
		log "--keep: leaving mp-e2e-svc and mp-client in place"
		return
	}
	log "cleanup"
	k delete job,deployment,serviceaccount -l "$OWNER_LABEL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "deploying the mixed-protocol workload (HTTP :8080 primary + raw TCP :9000)"
cat <<YAML | kubectl --context "$CTX" apply -f - >/dev/null
apiVersion: v1
kind: ServiceAccount
metadata: {name: mp-e2e-svc, namespace: $NS, labels: {app.kubernetes.io/created-by: multiprotocol-e2e}}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: mp-client, namespace: $NS, labels: {app.kubernetes.io/created-by: multiprotocol-e2e}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: mp-e2e-svc, namespace: $NS, labels: {app.kubernetes.io/created-by: multiprotocol-e2e}}
spec:
  replicas: 1
  selector: {matchLabels: {app: mp-e2e-svc}}
  template:
    metadata:
      labels: {app: mp-e2e-svc, aether.io/managed: "true", app.kubernetes.io/created-by: multiprotocol-e2e}
      annotations:
        # The declaration this whole proposal turns on: an HTTP primary and a
        # raw-TCP secondary on ONE pod, under ONE ServiceAccount.
        endpoint.aether.io/port: "8080"
        endpoint.aether.io/ports: "8080,9000=tcp"
    spec:
      serviceAccountName: mp-e2e-svc
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: http
          image: hashicorp/http-echo:1.0
          args: ["-text=served-by-http-8080", "-listen=:8080"]
          ports: [{containerPort: 8080}]
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: ["ALL"]}
        - name: tcp
          image: istio/tcp-echo-server:1.3
          args: ["9000", "served-by-tcp-9000"]
          ports: [{containerPort: 9000}]
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: ["ALL"]}
YAML

log "waiting for the workload to register"
k rollout status deployment/mp-e2e-svc --timeout=120s >/dev/null

# Wait on the OBSERVABLE, not on a clock (#919).
#
# This step waits for a chain of independent async events -- registrar observes
# the pods, generates the mesh Service, the agent observes it, builds the
# clusters, EDS propagates -- and none of them has a bounded duration. A fixed
# `sleep 15` failed 2 of 3 consecutive runs here, and only became visible once
# #918 stopped the script from reusing the soak harness's permanently-warm
# `mixed-svc`. Two bugs had been cancelling out: the e2e passed BECAUSE it was
# destroying somebody else's workload.
#
# Raising the sleep would trade a flake for a slower flake. Poll instead, and
# say which stage failed to converge so a real regression stays distinguishable
# from slowness.
await_mesh_service() {
	local deadline=$((SECONDS + 120))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if k get svc mp-e2e-svc >/dev/null 2>&1; then
			return 0
		fi
		sleep 3
	done
	bad "registrar never generated the mesh Service for mp-e2e-svc within 120s"
	bad "  -> the failure is registration, NOT the 037 data path; do not read it as a routing regression"
	return 1
}

await_mesh_service || exit 1

# NO wait on endpoints here, deliberately. The generated mesh Service is
# SELECTORLESS and its endpoints live in the aether registry, not in Kubernetes
# -- `kubectl get endpointslice -l kubernetes.io/service-name=<svc>` returns
# nothing for it, ever. A first version of this polled exactly that and so
# timed out on every run, which is the same defect this file keeps finding
# elsewhere: waiting on an observable nobody checked exists.
#
# What we actually need to know is whether the path is REACHABLE, and the only
# way to observe that is to try it. So the retry lives inside the probe below,
# where a slow convergence costs a few extra attempts and a genuinely broken
# path still fails on the deadline.

FQDN="mp-e2e-svc.${NS}.${DOMAIN}"

log "running the in-mesh client"
k delete job mp-client --ignore-not-found >/dev/null 2>&1 || true
cat <<YAML | kubectl --context "$CTX" apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: mp-client, namespace: $NS, labels: {app.kubernetes.io/created-by: multiprotocol-e2e}}
spec:
  backoffLimit: 0
  template:
    metadata:
      labels: {app: mp-client, aether.io/managed: "true", app.kubernetes.io/created-by: multiprotocol-e2e}
      annotations:
        # Declare the dependency so the agent delivers the service's clusters
        # and chains to this pod's listeners (demand-scoped distribution, 004).
        config.aether.io/upstreams: "mp-e2e-svc.$NS"
    spec:
      serviceAccountName: mp-client
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
              F="$FQDN"
              rc=0

              # Retry each check until it succeeds or the deadline passes
              # (#919). The mesh Service, its clusters and the capture chains
              # converge asynchronously after the workload registers, and a
              # fixed sleep outside this pod failed 2 runs in 3. A slow
              # convergence costs extra attempts here; a genuinely broken path
              # still fails, because the deadline is bounded and the last
              # response is what gets reported.
              try_until() { # seconds marker command...
                deadline=\$(( \$(date +%s) + \$1 )); shift
                marker="\$1"; shift
                while :; do
                  out=\$("\$@" 2>/dev/null || true)
                  case "\$out" in (*"\$marker"*) printf '%s' "\$out"; return 0 ;; esac
                  [ "\$(date +%s)" -ge "\$deadline" ] && { printf '%s' "\$out"; return 1; }
                  sleep 2
                done
              }

              # 1. HTTP on the mesh port. The authority carries the port; the
              #    capture HCM routes on it.
              body=\$(try_until 90 served-by-http-8080 curl -s --max-time 5 "http://\$F:18081/")
              case "\$body" in
                *served-by-http-8080*) echo "RESULT http_18081 PASS" ;;
                *) echo "RESULT http_18081 FAIL got=[\$body]"; rc=1 ;;
              esac

              # 2. HTTP portless — the scheme's default port (80).
              body=\$(try_until 60 served-by-http-8080 curl -s --max-time 5 "http://\$F/")
              case "\$body" in
                *served-by-http-8080*) echo "RESULT http_portless PASS" ;;
                *) echo "RESULT http_portless FAIL got=[\$body]"; rc=1 ;;
              esac

              # 3. Raw TCP on the service's OWN port. This is proposal 037's
              #    reason to exist: before it, this port had no chain at all on
              #    an HTTP-primary service.
              tcp_dial() { (echo hello; sleep 2) | timeout 6 nc "\$F" 9000 2>/dev/null; }
              out=\$(try_until 90 served-by-tcp-9000 tcp_dial)
              case "\$out" in
                *served-by-tcp-9000*) echo "RESULT tcp_9000 PASS" ;;
                *) echo "RESULT tcp_9000 FAIL got=[\$out]" ; rc=1 ;;
              esac

              echo "RESULT exit \$rc"
              exit 0
YAML

k wait --for=condition=complete job/mp-client --timeout=180s >/dev/null 2>&1 || true

log "results"
OUT="$(k logs job/mp-client 2>/dev/null || true)"
echo "$OUT" | grep '^RESULT' || {
	bad "client produced no results"
	echo "$OUT" | tail -20
}

for check in http_18081 http_portless tcp_9000; do
	if echo "$OUT" | grep -q "^RESULT $check PASS"; then
		ok "$check"
	else
		bad "$check — $(echo "$OUT" | grep "^RESULT $check" || echo 'no result line')"
	fi
done

log "registry: the pod must appear under BOTH protocol keys (037 dual registration)"
if k get pods -l app=mp-e2e-svc -o jsonpath='{.items[0].metadata.annotations}' 2>/dev/null | grep -q '9000=tcp'; then
	ok "pod carries ports=8080,9000=tcp"
else
	bad "pod annotation missing the =tcp suffix"
fi

if [ "$FAILED" = "0" ]; then
	printf '\n\033[32mmulti-protocol e2e: PASS\033[0m\n'
else
	printf '\n\033[31mmulti-protocol e2e: FAIL\033[0m\n'
	exit 1
fi
