# Proposal: Edge Proxy (North-South Ingress Gateway)

**Status:** Draft — design review
**Author:** Bruno Palermo
**Date:** 2026-06-11

## Problem Statement

Aether today is east-west only: traffic enters the mesh exclusively from
mesh-managed pods through their netns-bound outbound listeners. There is no
way for external (north-south) traffic to reach mesh services.

This proposal adds an **edge proxy**: an Envoy ingress gateway running as a
normal Deployment behind a Service (LoadBalancer/NodePort), which terminates
external traffic and forwards it **directly to destination pods' mesh inbound
listeners** (`<pod_ip>:15008`, mTLS) — exactly the path every mesh client
already uses. Edge nodes (or any node hosting only gateways) do **not** run
the agent/proxy DaemonSet.

## Design Principle: the Edge Is Just Another Mesh Client

The mesh's east-west contract is already everything an ingress needs:

| Contract element | Where it lives today |
|---|---|
| Endpoint discovery | registrar `WatchEndpoints` stream (versioned snapshot + deltas) |
| Delivery address | `pod_ip:15008` netns-bound inbound mTLS listener |
| Endpoint readiness | active HTTP/2 HC on `/-/-/ready` (`MeshReadyPath`) over the cluster mTLS; EDS health (DRAINING on deletion-requested) for eds-mode endpoints |
| Client identity | any SVID in the trust domain; the callee records it in XFCC |
| Per-service routing | one cluster + vhost per service (`NewServiceCluster`, `NewServiceVirtualHost`) |

Consequence: **zero changes to the node data plane.** Inbound listeners
already accept any trust-domain client certificate; the edge presents its own
gateway identity (`spiffe://<td>/ns/<ns>/sa/<edge-sa>`) on every upstream
connection, and destination pods see that identity in
`x-forwarded-client-cert` — correct, since the external caller has no mesh
identity. The mTLS model is untouched: the only cleartext hop is external
client → edge listener (until TLS termination) — pod-to-pod stays mTLS.

This is also why the per-source machinery is *not* needed at the edge: the
node proxy needs `transport_socket_matcher` + per-downstream pools because it
multiplexes many workload identities; the edge has exactly one identity. Its
clusters use the plain `UpstreamTransportSocket(edgeSpiffeID, …)` — the same
code path the node proxy already takes when it has zero local workloads — and
**no `connection_pool_per_downstream_connection`**, so the edge gets fully
multiplexed upstream connection reuse (no per-conn pools, none of the #136
leak class).

## Architecture

```
                       ┌─ edge pod (Deployment ×N, unprivileged) ─┐
 internet ──TLS──────► │ envoy ◄──UDS xDS── agent --mode=edge     │
   (LB / NodePort)     │   │        (emptyDir /run/aether)        │
                       │   └─ SDS: edge SVID + trust bundle       │
                       └───────│──────────────────────────────────┘
                               │ mTLS (edge identity)
                               ▼
                    dest pod_ip:15008  (existing inbound listener,
                    any node, reached via pod network — no node-proxy
                    hop on the edge side, no DS on edge nodes)
```

### Control plane: `agent --mode=edge` (sidecar), not a new server

The edge Envoy needs xDS; three options were considered:

1. **Node agent** — excluded by requirement (no DS on edge nodes).
2. **Registrar grows an edge xDS server** — centralizes config, but SDS
   (SVID delivery) must be pod-local anyway (Workload API socket), splitting
   secrets from config across two control planes; also adds a new
   fan-out/HA surface to the registrar.
3. **Agent sidecar in edge mode** *(chosen)* — the agent already contains
   everything: registrar watch client (`registry/internal/registrar`),
   snapshot cache + delta-xDS server + ACK tracking, service
   cluster/CLA/vhost generators, SPIRE integration. Edge mode is a
   *subtraction*: no CNI server, no local-workload storage, no netns
   listeners, no delegated-identity API (the edge needs only its **own**
   SVID + trust bundle, served straight from the Workload API
   `csi.spiffe.io` socket — the same `commonspire.Source` the agent already
   uses for registrar mTLS, feeding SDS via the existing node-SVID path of
   the SPIRE bridge).

The xDS socket moves from a hostPath to a pod-local `emptyDir`
(`/run/aether/xds.sock`) — nothing else on the node needs it.

### What edge mode generates

- **Service clusters + EDS**: identical generators (`NewServiceCluster` /
  `ServiceLocalityLbEndpointFromRegistryEndpoint`), restricted to the
  *exposed* service set, with two deltas:
  - plain `UpstreamTransportSocket` (edge SVID, trust-domain validation
    context) instead of the matcher;
  - `connection_pool_per_downstream_connection` **off**.
- **One edge listener** (`0.0.0.0:8443`): HCM with vhosts per exposed
  service. v1 routing is the mesh convention — `Host`/`:authority` selects
  the service — plus optional per-service external domain aliases. The
  existing readiness `health_check` filter (`/aether/readyz`) rides the
  chain and backs the pod's readinessProbe, so a rolling edge deploy behind
  the Service is gated on Envoy actually serving (ACK-tracking included).
- **Downstream TLS**: terminate with a certificate from a standard
  Kubernetes TLS Secret (mounted file SDS with reload). v1 single cert;
  SNI-per-domain later. An `--insecure-http` toggle serves plain HTTP for
  in-cluster/e2e use.

### Exposure model (v1: explicit allowlist)

Nothing is exposed by default. The edge values list what is routable:

```yaml
edge:
  exposes:
    - service: svc-1            # registry service name (= ServiceAccount)
      hosts: [api.example.com]  # external vhost domains (Host/SNI match)
    - service: svc-2            # no hosts: reachable as Host: svc-2 only
```

This is deliberately not a CRD: the mesh has no CRDs today and the registry
is the source of truth for *endpoints*; exposure is edge-local routing
config. Gateway API (`Gateway`/`HTTPRoute`) is the obvious v2 once the
shape settles — the v1 internals (per-service vhosts) map 1:1 onto it.

### Deployment shape (chart: `charts/edge` or `edge.enabled` in agent chart)

- Deployment (replicas ≥ 2), **unprivileged**, no hostNetwork, no
  hostPaths: csi.spiffe.io socket + emptyDir xDS socket + TLS Secret only.
  (Notably: a non-privileged pod gets a private cgroupns — the
  `cgroup_memory` overload monitor that is fatal in the privileged node DS
  would work here. Edge bootstrap gets the same overload ladder, tuned to
  its own limits.)
- Standard `RollingUpdate` with `maxSurge`; **no hot-restart supervisor in
  v1** — gateways behind a Service drain via the LB + readiness gate, the
  problem hot restart solves (long-lived node-local capture) doesn't apply.
  Plain Envoy entrypoint, static bootstrap ConfigMap pointing at the
  sidecar's UDS.
- SPIRE: a registration entry for the edge ServiceAccount
  (spire-controller-manager picks it up like any workload; no
  `aether.io/managed` label — the edge is not a mesh-captured pod).
- Node agent DS untouched; optionally document `nodeSelector`/taints to
  keep the DS off dedicated edge nodes.

### Health & rollouts

- Edge→endpoint health: same composition as any client — active
  `MeshReadyPath` HC for active-mode endpoints, registry-fed EDS health
  (incl. DRAINING on deletion-requested) for eds-mode. The edge benefits
  from the drain work (P2) automatically as it lands.
- Edge pod readiness: the `/aether/readyz` health_check filter (drain-aware:
  503s on Envoy drain) + ACK-tracked config delivery — reused from #128.
- Overload manager: same ladder; `bypass_overload_manager` is irrelevant
  here (no health gateway — no local workloads).

## Security Considerations

- **Exposure is opt-in and edge-local**; the mesh's east-west surface is
  unchanged.
- Destination pods authenticate the edge like any peer and see
  `URI=spiffe://…/sa/<edge-sa>` in XFCC. **Service-level authorization
  (e.g. "only the edge identity may call svc-1") is out of scope** — the
  mesh has no peer-authz policy yet anywhere; when it grows one, the edge
  identity is just another principal in it.
- External client identity does not enter the mesh as a certificate;
  propagate it as headers (`x-forwarded-for`, optionally JWT validation at
  the edge later).
- The edge pod is unprivileged and holds only its own SVID — compromise
  yields one client identity, not the node-proxy's multi-workload cert set.

## Implementation Plan (PRs)

| PR | Scope |
|----|-------|
| 1 | `agent edge` subcommand: registrar watch + snapshot cache wired without CNI/storage/netns; SDS from Workload API source (own SVID + bundle); exposed-service filtering; plain-transport service clusters (pooling flag off); edge listener (HTTP only) + vhosts + readiness filter |
| 2 | Edge chart: Deployment/Service/ConfigMap/values (`edge.exposes`), csi.spiffe.io + emptyDir UDS wiring; overload ladder in edge bootstrap |
| 3 | Downstream TLS termination from a k8s TLS Secret (file-based SDS w/ rotation) + per-service host aliases |
| 4 | talos e2e: expose svc-1, external traffic through NodePort, rolling edge restart hitless behind the Service, XFCC shows edge SA at the destination |
| 5 (later) | Gateway API translation; JWT/external authn at the edge; peer authz when the mesh grows it |

## Open Questions

- **Cross-cluster routing**: endpoints carry `cluster` subset metadata; an
  edge could route to remote-cluster endpoints (pod IP reachability across
  clusters permitting). Defer until the multi-cluster registrar work
  ([[multicluster-registry]]) settles.
- **Listener-per-port vs single 8443**: v1 single HTTPS port + optional
  HTTP; TCP/SNI passthrough is a later mode.
- **Rate limiting / WAF**: out of scope; standard Envoy filters can be
  layered into the edge HCM later without touching the mesh.
