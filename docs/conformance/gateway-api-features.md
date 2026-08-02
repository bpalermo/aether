# Gateway API — supported features (aether)

Aether implements Gateway API natively on its own SPIRE-mTLS + registry-EDS data plane
(proposal 018), both north-south (edge `Gateway`/`HTTPRoute`) and east-west (GAMMA,
`HTTPRoute`/`GRPCRoute` with `parentRef: Service`). This is the honest
supported-features list — "ship support first, report it honestly, chase the badge
later". `Supported` = implemented and e2e-validated on talos; `Partial` = implemented
with the noted limitation; `Planned` = on the 018 roadmap, not yet shipped.

The L4 routes (TCPRoute/TLSRoute/UDPRoute) and the TCP-over-mTLS floor below were
e2e-validated on talos (aether 0.41.0).

## Route types

| Feature | Status | Notes |
|---|---|---|
| `HTTPRoute` (edge, parentRef=Gateway) | Supported | path + host matches, wildcard TLS; `backendRef` requires `port` (use the service default) |
| `HTTPRoute` (GAMMA, parentRef=Service) | Supported | path/header matches, weighted backendRefs, per-rule request timeout; producer routes |
| `GRPCRoute` (GAMMA) | Supported | method match → `/<service>/<method>`; service-only → prefix; header matches; weighted backends |
| `TCPRoute` (edge, parentRef=Gateway) | Supported | raw TCP through the edge LB → backend over the TCP floor; e2e-validated |
| `TLSRoute` (edge) | Supported | TLS **passthrough** — `tls_inspector` reads SNI for routing, the edge does NOT terminate TLS (validated: client sees the backend's cert); per-SNI `server_names` filter chains |
| `UDPRoute` (east-west, parentRef=Service) | Supported | `udp_proxy` floor to the backend's app UDP port. NOTE: UDP is plaintext — mesh mTLS does not cover the UDP floor (DTLS not implemented) |

## Routing vocabulary (HTTP/gRPC)

| Feature | Status | Notes |
|---|---|---|
| Path match (Exact, PathPrefix) | Supported | |
| Header match (Exact) | Supported | |
| Method match (gRPC) | Supported | |
| Weighted backends (canary/split) | Supported | `WeightedClusters` |
| Request timeout | Supported | HTTPRoute `timeouts.request` |
| Request mirroring | Planned | |
| `RequestRedirect` filter | Supported | `scheme` → `SchemeRedirect`; `hostname` → `HostRedirect`; `port` → `PortRedirect`; `statusCode` 301/302 → `MOVED_PERMANENTLY`/`FOUND`; `path.ReplaceFullPath` → `PathRedirect`; `path.ReplacePrefixMatch` → `PrefixRewrite`; yields `Route_Redirect` (no backend); applied on HTTPRoute (edge + GAMMA) |
| `URLRewrite` filter | Supported | `hostname` → `HostRewriteLiteral`; `path.ReplacePrefixMatch` → `PrefixRewrite`; `path.ReplaceFullPath` → `RegexRewrite` (`.*` → value); applied on HTTPRoute (edge + GAMMA) |
| `RequestHeaderModifier` filter | Supported | `set` → `OVERWRITE_IF_EXISTS_OR_ADD`; `add` → `APPEND_IF_EXISTS_OR_ADD`; `remove` → `request_headers_to_remove`; applied at route level on HTTPRoute (edge + GAMMA) and GRPCRoute |
| `ResponseHeaderModifier` filter | Supported | same shape on `response_headers_to_add` / `response_headers_to_remove`; applied at route level on HTTPRoute (edge + GAMMA) and GRPCRoute |
| RegularExpression matches | Supported | gRPC method `RegularExpression` type → Envoy `safe_regex` path (`/<serviceRegex>/<methodRegex>`); unset component uses `[^/]+` |

## Mesh & multi-cluster

| Feature | Status | Notes |
|---|---|---|
| GAMMA Mesh profile (parentRef=Service) | Supported | producer routes; consumer (per-namespace) overrides Planned |
| GAMMA rules on the transparent-capture path | Supported | applies to clients dialing `<svc>.<ns>.<meshDomain>` (the default path) |
| East-west L4 on the transparent-capture path | Supported | TCPRoute/TLSRoute/UDPRoute (parentRef=Service) project onto the capture floor; unconditional since proposal 031 (the `--l4-routes` flag was retired), gated only on the route CRDs being installed |
| `ReferenceGrant` (cross-namespace backendRefs) | Supported (admission + status + per-namespace resolution) | A cross-namespace backendRef (namespace set and != the route's) is admitted only when a `ReferenceGrant` in the backend's namespace has a `from` matching `{group: gateway.networking.k8s.io, kind: <route kind>, namespace: <route ns>}` and a `to` matching `{group: "", kind: Service}` (optionally `name`). Without a grant the route's `ResolvedRefs` is `False`/`RefNotPermitted` and the backend is DROPPED from the data plane (rest of the route still applies). Enforced on edge HTTPRoute/TCPRoute/TLSRoute and east-west GAMMA HTTPRoute/GRPCRoute + L4 TCPRoute/TLSRoute/UDPRoute. The data-plane cluster name is namespace-qualified (`<svc>.<ns>.<meshDomain>`) since the proposal 020 cutover, so a *granted* cross-ns ref resolves to the backend's own namespace |
| MCS `ServiceExport`/`ServiceImport`, `clusterset.local` | Supported (phase 1) | registry-backed (proposals 018 + 006), DNS strictly local. `registrar.enableMCS=true` exports local `ServiceExport`s to the registry and materializes peer-exported services as `ServiceImport`s + clusterset VIPs. Requires the etcd backend + the MCS-API CRDs |

## Transport / security

| Feature | Status | Notes |
|---|---|---|
| Listener TLS termination (edge) | Supported | SDS; wildcard certs |
| Upstream mTLS (SPIRE) on every hop | Supported | per-endpoint SPIFFE SAN from registry EDS |
| TCP-over-mTLS passthrough (non-HTTP) | Supported | Phase 3a floor: per-source SPIRE mTLS, the inbound default TCP floor chain, and protocol-aware **TCP-connect liveness** for non-HTTP apps |

## Status reporting

GatewayClass/Gateway/Route status-condition reporting (Accepted/Programmed/ResolvedRefs)
is **Supported**: the edge controller is namespace-agnostic — it reconciles every
Gateway of its GatewayClass in ANY namespace and publishes the Gateway/Route status
conditions and per-listener `attachedRoutes`, which is what the upstream conformance
suite's `NamespacesMustBeReady` gate requires before any test runs.

Every class-`aether` Gateway also gets `status.addresses` (one `IPAddress` entry), so
the suite's "wait for at least one IP address in status" setup step passes and the
GATEWAY-HTTP *traffic* tests run. Each Gateway gets its **own** LoadBalancer Service
and therefore its own distinct address (proposal 021 Phase 2, unconditional since 031
round 2); the shared-address fallback of Phase 1 remains only when the edge Service
name is unset or port allocation fails. The address is resolved at runtime from the
Service's status (robust to MetalLB assignment): until the LB IP is assigned the
address is omitted rather than written empty.

The GatewayClass also publishes a machine-readable `status.supportedFeatures` list so
the suite skips (rather than fails) the features aether does not implement. The
advertised set (sorted) is: `Gateway`, `GatewayPort8080`, `HTTPRoute`,
`HTTPRouteHostRewrite`, `HTTPRouteMethodMatching`, `HTTPRoutePathRedirect`,
`HTTPRoutePathRewrite`, `HTTPRoutePortRedirect`, `HTTPRouteRequestTimeout`,
`HTTPRouteResponseHeaderModification`, `HTTPRouteSchemeRedirect`, `ReferenceGrant`.
Path/header/method match, weighted backends, and the `RequestHeaderModifier` filter
are part of HTTPRoute *core* and carry no separate feature flag. The `RequestRedirect`
filter (`HTTPRoutePortRedirect`/`HTTPRouteSchemeRedirect`/`HTTPRoutePathRedirect`) and
`URLRewrite` filter (`HTTPRouteHostRewrite`/`HTTPRoutePathRewrite`) are advertised now
that both are implemented on edge + GAMMA HTTPRoute; host redirect carries no separate
flag (host+status is core), and only the 301/302 redirect status codes are implemented
(the `303`/`307`/`308` status-code features are not advertised). Request mirroring is
deliberately omitted (not implemented). `ReferenceGrant` is advertised now that
cross-namespace backendRef admission, status enforcement, and namespace-qualified
resolution (proposal 020) are all implemented.

`GRPCRoute` is **not** advertised on the GatewayClass: aether serves `GRPCRoute` only
east-west via GAMMA (`parentRef: Service`), which is a mesh feature with no
GatewayClass, so the north-south Gateway does not serve `GRPCRoute`. Advertising
`SupportGRPCRoute` on the GatewayClass makes the upstream `GATEWAY-GRPC` suite send
gRPC traffic through a Gateway the edge cannot serve (the tests run and fail/timeout),
so it is omitted here while GAMMA continues to implement `GRPCRoute` for the mesh
profile (see "Route types" above).

### Data-plane / addressing

Per-Gateway addressing (proposal 021 Phase 2) is implemented and unconditional: each
class-`aether` Gateway gets its own LoadBalancer Service and its own distinct address,
published in `status.addresses`, with internal-port demux behind the one edge
Deployment. This satisfies both the address-dependent GATEWAY-HTTP traffic tests and
the tests asserting each Gateway has its OWN address. The Phase 1 shared-address
behavior survives only as the fallback for when the edge Service name is unset or a
per-Gateway port cannot be allocated.
