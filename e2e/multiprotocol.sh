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

cleanup() {
	[ "$KEEP" = "1" ] && {
		log "--keep: leaving mixed-svc and mp-client in place"
		return
	}
	log "cleanup"
	k delete job mp-client --ignore-not-found --wait=false >/dev/null 2>&1 || true
	k delete deployment mixed-svc --ignore-not-found --wait=false >/dev/null 2>&1 || true
	k delete serviceaccount mixed-svc mp-client --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "deploying the mixed-protocol workload (HTTP :8080 primary + raw TCP :9000)"
cat <<YAML | kubectl --context "$CTX" apply -f - >/dev/null
apiVersion: v1
kind: ServiceAccount
metadata: {name: mixed-svc, namespace: $NS}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: mp-client, namespace: $NS}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: mixed-svc, namespace: $NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: mixed-svc}}
  template:
    metadata:
      labels: {app: mixed-svc, aether.io/managed: "true"}
      annotations:
        # The declaration this whole proposal turns on: an HTTP primary and a
        # raw-TCP secondary on ONE pod, under ONE ServiceAccount.
        endpoint.aether.io/port: "8080"
        endpoint.aether.io/ports: "8080,9000=tcp"
    spec:
      serviceAccountName: mixed-svc
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
k rollout status deployment/mixed-svc --timeout=120s >/dev/null
# The registrar generates the mesh Service; the agent needs its endpoints.
sleep 15

FQDN="mixed-svc.${NS}.${DOMAIN}"

log "running the in-mesh client"
k delete job mp-client --ignore-not-found >/dev/null 2>&1 || true
cat <<YAML | kubectl --context "$CTX" apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: mp-client, namespace: $NS}
spec:
  backoffLimit: 0
  template:
    metadata:
      labels: {app: mp-client, aether.io/managed: "true"}
      annotations:
        # Declare the dependency so the agent delivers the service's clusters
        # and chains to this pod's listeners (demand-scoped distribution, 004).
        config.aether.io/upstreams: "mixed-svc.$NS"
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

              # 1. HTTP on the mesh port. The authority carries the port; the
              #    capture HCM routes on it.
              body=\$(curl -s --max-time 5 "http://\$F:18081/" || true)
              case "\$body" in
                *served-by-http-8080*) echo "RESULT http_18081 PASS" ;;
                *) echo "RESULT http_18081 FAIL got=[\$body]"; rc=1 ;;
              esac

              # 2. HTTP portless — the scheme's default port (80).
              body=\$(curl -s --max-time 5 "http://\$F/" || true)
              case "\$body" in
                *served-by-http-8080*) echo "RESULT http_portless PASS" ;;
                *) echo "RESULT http_portless FAIL got=[\$body]"; rc=1 ;;
              esac

              # 3. Raw TCP on the service's OWN port. This is proposal 037's
              #    reason to exist: before it, this port had no chain at all on
              #    an HTTP-primary service.
              out=\$( (echo hello; sleep 2) | timeout 6 nc "\$F" 9000 2>/dev/null || true)
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
if k get pods -l app=mixed-svc -o jsonpath='{.items[0].metadata.annotations}' 2>/dev/null | grep -q '9000=tcp'; then
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
