# Edge proxy e2e (talos-main)

Validates the north-south edge gateway (proposal 003): external traffic →
edge Service → edge Envoy → destination pod's mesh inbound (`pod_ip:15008`,
mTLS), with **no node proxy / DaemonSet in the path**.

This is a manual runbook against a live cluster (talos-main); it is not a CI
target. It assumes aether is already deployed (agent + registrar + controller)
and SPIRE is installed with a `spire-controller-manager` class.

## 1. Enable the edge

Add to the `helm upgrade aether ...` values (no `--reuse-values`):

```yaml
edge:
  enabled: true
  service:
    type: LoadBalancer            # or NodePort for a quick test
  spire:
    clusterSpiffeID:
      className: spire-mgmt-spire  # the talos-main SPIRE class
  # Downstream TLS is optional; omit for a plain-HTTP first pass.
  # tls:
  #   enabled: true
  #   secretName: edge-cert       # a kubernetes.io/tls Secret in the edge namespace
```

The edge Deployment, Service, bootstrap ConfigMap, RBAC and ClusterSPIFFEID
render only when `edge.enabled=true`.

## 2. Expose a service

Apply `httproute.yaml` (a Gateway + HTTPRoute, proposal 018). The edge exposes a
service ONLY at the external hostnames its HTTPRoutes list:

```bash
kubectl apply -f test/e2e/edge/httproute.yaml
kubectl get gateways,httproutes -n aether-ingress
```

## 3. Drive traffic

```bash
# External entrypoint (LoadBalancer IP or NodePort).
EDGE=$(kubectl get svc -n aether-edge aether-edge -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

# Host-routed (matches HTTPRoute.spec.hostnames):
curl -sS -H 'Host: api.example.com' "http://${EDGE}/" -o /dev/null -w '%{http_code}\n'   # 200

# The internal mesh FQDN is NOT routable from the edge -> 404:
curl -sS -H 'Host: svc-1.aether.internal' "http://${EDGE}/" -o /dev/null -w '%{http_code}\n'  # 404

# Unmatched authority -> 404 (the edge serves only its exposed set):
curl -sS -H 'Host: nope.example.com' "http://${EDGE}/" -o /dev/null -w '%{http_code}\n'   # 404
```

With `edge.tls.enabled`, use `https://` and the cert's SNI.

## 4. Assertions

- **Direct-to-pod, edge identity in XFCC.** The destination app should observe
  `x-forwarded-client-cert` with `URI=spiffe://<trust-domain>/ns/<ns>/sa/<edge-sa>`
  (the edge SA, not a mesh workload). With the echo app:
  `curl ... | jq '.headers["x-forwarded-client-cert"]'`.
- **No node proxy in the path.** Schedule the edge on a node and confirm there is
  no agent/proxy DaemonSet pod required for the request to succeed (the edge dials
  `pod_ip:15008` directly).
- **Hitless rollout.** `kubectl rollout restart deploy/aether-edge -n aether-edge`
  under a load generator → no failed requests (Service + `/aether/readyz` readiness
  gate).
- **Live route changes.** `kubectl apply`/`delete` an HTTPRoute → the route
  appears/disappears within a reconcile, no edge redeploy.

## Rollback

`helm upgrade aether ... --set edge.enabled=false` removes all edge resources.
The Gateway API CRDs are installed out-of-band (standard channel); aether does not
own them.
