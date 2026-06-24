# Gateway API — supported features (aether)

Aether implements Gateway API natively on its own SPIRE-mTLS + registry-EDS data plane
(proposal 018), both north-south (edge `Gateway`/`HTTPRoute`) and east-west (GAMMA,
`HTTPRoute`/`GRPCRoute` with `parentRef: Service`). This is the honest
supported-features list — "ship support first, report it honestly, chase the badge
later". `Supported` = implemented and e2e-validated on talos; `Partial` = implemented
with the noted limitation; `Planned` = on the 018 roadmap, not yet shipped.

## Route types

| Feature | Status | Notes |
|---|---|---|
| `HTTPRoute` (edge, parentRef=Gateway) | Supported | path + host matches, wildcard TLS; `backendRef` requires `port` (use the service default) |
| `HTTPRoute` (GAMMA, parentRef=Service) | Supported | path/header matches, weighted backendRefs, per-rule request timeout; producer routes |
| `GRPCRoute` (GAMMA) | Supported | method match → `/<service>/<method>`; service-only → prefix; header matches; weighted backends |
| `TCPRoute` / `TLSRoute` / `UDPRoute` | Planned | Phase 3b; layers on the TCP-over-mTLS floor |

## Routing vocabulary (HTTP/gRPC)

| Feature | Status | Notes |
|---|---|---|
| Path match (Exact, PathPrefix) | Supported | |
| Header match (Exact) | Supported | |
| Method match (gRPC) | Supported | |
| Weighted backends (canary/split) | Supported | `WeightedClusters` |
| Request timeout | Supported | HTTPRoute `timeouts.request` |
| Request mirroring | Planned | |
| Request/response header & redirect filters | Planned | |
| RegularExpression matches | Partial | gRPC method RegularExpression type is not translated (Exact only) |

## Mesh & multi-cluster

| Feature | Status | Notes |
|---|---|---|
| GAMMA Mesh profile (parentRef=Service) | Supported | producer routes; consumer (per-namespace) overrides Planned |
| GAMMA rules on the transparent-capture path | Supported | applies to clients dialing `<svc>.<meshDomain>` (the default path) |
| `ReferenceGrant` (cross-namespace backendRefs) | N/A → Planned | aether's data-plane cluster name is namespace-free (`<svc>.<meshDomain>`), so cross-namespace backend references are not part of the routing model today; grant enforcement is a conformance-only follow-up |
| MCS `ServiceExport`/`ServiceImport`, `clusterset.local` | Planned | registry-backed (proposal 006), DNS strictly local |

## Transport / security

| Feature | Status | Notes |
|---|---|---|
| Listener TLS termination (edge) | Supported | SDS; wildcard certs |
| Upstream mTLS (SPIRE) on every hop | Supported | per-endpoint SPIFFE SAN from registry EDS |
| TCP-over-mTLS passthrough (non-HTTP) | Planned | Phase 3a floor |

## Status reporting

GatewayClass/Gateway/Route status-condition reporting (Accepted/Programmed/ResolvedRefs)
and a machine-readable supported-features report are a Planned conformance follow-up.
