# Proposal: Edge Proxy (North-South Ingress Gateway)

**Status:** Implemented — PRs #236 (control plane), #237 (EdgeRoute CRD), #238
(chart), #239 (downstream TLS)
**Author:** Bruno Palermo
**Date:** 2026-06-19

## Problem statement

Aether is east-west only: traffic enters the mesh exclusively from mesh-managed
pods through their netns-bound outbound listeners. There is no sanctioned way for
external (north-south) traffic to reach mesh services, and we explicitly do not
want to route north-south traffic through the node proxy / DaemonSet.

This adds an **edge proxy**: an unprivileged Envoy Deployment behind a Service
that terminates external traffic and forwards it **directly to destination pods'
mesh inbound listeners** (`<pod_ip>:15008`, mTLS) — exactly the path every mesh
client already uses. Edge pods do **not** run the agent/proxy DaemonSet.

## Design principle: the edge is a single-identity mesh client

The transport migration (#98–#101) already made node-proxy egress a plain
per-source mTLS HTTP/2 client dialing `pod_ip:15008`, where the destination pod's
own netns-bound inbound listener terminates mTLS and records the caller in
`x-forwarded-client-cert`. The edge is **the same path with one identity instead
of many**:

| Contract element | Where it lives |
|---|---|
| Endpoint discovery | registrar `WatchEndpoints` stream (scoped to the exposed services) |
| Delivery address | `pod_ip:15008` netns-bound inbound mTLS listener |
| Endpoint readiness | EDS health (delegated liveness) + outlier detection |
| Client identity | the edge's single SVID; the callee records it in XFCC |
| Per-service routing | `NewServiceCluster` / `BuildOutboundClusterVirtualHost` |

Consequence: **zero node data-plane changes.** Inbound listeners already accept
any trust-domain client certificate; the edge presents its own gateway identity
(`spiffe://<td>/ns/<ns>/sa/<edge-sa>`) and destination pods see it in XFCC —
correct, since the external caller has no mesh identity. Because the edge has a
single identity it does **not** need the node proxy's per-source
`transport_socket_matcher` or `connection_pool_per_downstream_connection`: it uses
a single transport socket and gets full upstream connection multiplexing.

## Control plane: `agent edge` (subtractive sidecar)

The edge runs the agent binary in a new `edge` subcommand — a subtraction of the
node agent. It keeps the controller-runtime manager, the registrar watch client,
the snapshot cache, the delta-xDS server + ACK tracking, and the cluster/CLA/vhost
generators. It drops the CNI server, local pod storage, per-pod netns listeners,
and — crucially — the SPIRE bridge:

- **Envoy fetches its SVID from SPIRE directly.** The edge has one identity, so
  the bridge's reason for being (delegated fan-out of many workload SVIDs over the
  admin socket) does not apply. The edge Envoy bootstrap declares a static
  `spire_agent` SDS gRPC cluster pointing at the SPIRE Agent's Workload API socket
  (which serves the Envoy SDS API natively), and the service clusters' transport
  sockets fetch the edge SVID + trust bundle over it. SAN pinning (#165/#166) is
  unchanged — `match_typed_subject_alt_names` is inline; only the trust bundle
  comes over SDS.
- **xDS over a pod-local socket.** The agent serves xDS on an `emptyDir`
  (`/run/aether/xds.sock`); the co-located Envoy dials it. One agent ↔ one Envoy
  per pod, keyed on `--proxy-id=$(POD_NAME)` = Envoy `--service-node`.

The edge runs no leader election (each replica is its own control plane), so a
sick sidecar fails exactly one replica behind the Service.

## Exposure: the EdgeRoute CRD

Exposure is an `EdgeRoute` custom resource (`config.aether.io/v1`, **namespaced**):

```yaml
apiVersion: config.aether.io/v1
kind: EdgeRoute
metadata:
  name: api
  namespace: aether-edge
spec:
  hosts: [api.example.com]   # external Host/SNI; empty = the service mesh FQDN
  service: svc-1             # mesh service = ServiceAccount = registry key
  port: 8080                 # optional; omit = default port
```

The edge agent watches EdgeRoutes (level-based: re-lists on every change), projects
them into the listener's route table (external host → service cluster; routes to
the same cluster merge into one vhost), and scopes its registrar watch to exactly
the referenced services. A custom CRD was chosen over Gateway API for v1: it names
the mesh service directly (no `Service`-object indirection), is a tiny watch (vs a
GatewayClass-conformance controller), and reuses the `config.aether.io` machinery
from proposal 015. Gateway API is the v2 once the L7 vocabulary (path/header
matching, weighted backends) is wanted — the EdgeRoute spec is shaped to migrate.

`--expose <svc>` is a static seed (routed at the service FQDN), merged with the
CRs; handy for bring-up before any EdgeRoute exists.

## Data plane

- **One listener** (`0.0.0.0:<edge.httpPort>`): an HCM with the `/aether/readyz`
  readiness `health_check` filter ahead of the router (backs the pod readiness
  probe, drain-aware) and RDS pointing at the EdgeRoute-derived route table. No
  on-demand/ODCDS catch-all — the edge serves only its explicit exposed set; an
  unmatched authority gets an immediate 404.
- **Service clusters**: the existing EDS generators, with a single SPIRE-SDS
  transport socket and `connection_pool_per_downstream_connection` off.
- **Downstream TLS** (optional, #239): the listener terminates external TLS from a
  mounted Kubernetes TLS Secret (`edge.tls`). No client certificate is required
  (external callers have no mesh identity); the edge→pod hop stays mTLS. The cert
  is mounted as files — rotating the Secret takes effect on the next edge pod roll.

## Deployment

The edge is part of `charts/aether`, gated on `edge.enabled` (default off): an
unprivileged Deployment (Envoy + the `agent edge` sidecar), a Service
(LoadBalancer/NodePort), an Envoy bootstrap ConfigMap (the `agent_xds` and
`spire_agent` clusters), a namespaced Role/RoleBinding for the EdgeRoute watch, and
an optional ClusterSPIFFEID so SPIRE issues the edge pod its SVID. Being
unprivileged, the edge pod gets a private cgroup namespace, so its overload monitor
works (unlike the privileged node DaemonSet).

## Security

- Exposure is opt-in and edge-local; the mesh's east-west surface is unchanged.
- Destination pods authenticate the edge like any peer and see `URI=.../sa/<edge-sa>`
  in XFCC. Service-level peer authorization is out of scope (the mesh has no peer
  authz anywhere yet); when it grows one, the edge identity is just another principal.
- The external client's identity does not enter the mesh as a certificate; propagate
  it as headers (`x-forwarded-for`, optional JWT validation at the edge later).
- The edge pod is unprivileged and holds only its own SVID.

## Deferred

- Gateway API translation; JWT / external authn at the edge; peer authz when the
  mesh grows one; cross-cluster edge routing (until the multi-cluster registry).

## End-to-end validation (talos-main)

See `test/e2e/edge/README.md` for the runbook. Summary:

1. `helm upgrade aether` with `edge.enabled=true`, `edge.spire.clusterSpiffeID.className`
   set, and (optionally) `edge.tls.enabled=true` + a TLS Secret.
2. Apply an `EdgeRoute` exposing `svc-1` (and a multi-port one for the `:3001` h2c
   case from proposal 005).
3. `curl` the edge Service from outside the mesh → 200; confirm the destination
   pod's XFCC shows `URI=spiffe://…/sa/<edge-sa>`.
4. Roll the edge Deployment under load → hitless behind the Service/readiness gate.
5. Add/remove an EdgeRoute → the route appears/disappears with no redeploy.
6. Confirm the edge pod runs on a node with **no** agent DaemonSet.
