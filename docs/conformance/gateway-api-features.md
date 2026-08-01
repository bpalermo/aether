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
| GAMMA rules on the transparent-capture path | Supported | applies to clients dialing `<svc>.<meshDomain>` (the default path) |
| East-west L4 on the transparent-capture path | Supported | TCPRoute/TLSRoute/UDPRoute (parentRef=Service) project onto the capture floor; gated behind `--l4-routes` |
| `ReferenceGrant` (cross-namespace backendRefs) | Supported (admission + status; namespace-blind resolution pending 020 Part 1) | A cross-namespace backendRef (namespace set and != the route's) is admitted only when a `ReferenceGrant` in the backend's namespace has a `from` matching `{group: gateway.networking.k8s.io, kind: <route kind>, namespace: <route ns>}` and a `to` matching `{group: "", kind: Service}` (optionally `name`). Without a grant the route's `ResolvedRefs` is `False`/`RefNotPermitted` and the backend is DROPPED from the data plane (rest of the route still applies). Enforced on edge HTTPRoute/TCPRoute/TLSRoute and east-west GAMMA HTTPRoute/GRPCRoute + L4 TCPRoute/TLSRoute/UDPRoute. CAVEAT: aether's data-plane cluster name is still namespace-free (`<svc>.<meshDomain>`), so a *granted* cross-ns ref resolves by name exactly as today; proper per-namespace resolution lands with proposal 020 Part 1 |
| MCS `ServiceExport`/`ServiceImport`, `clusterset.local` | Planned | registry-backed (proposal 006), DNS strictly local |

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

Every class-`aether` Gateway also gets `status.addresses` (one `IPAddress` entry =
the shared edge LoadBalancer IP), so the suite's "wait for at least one IP address in
status" setup step passes and the GATEWAY-HTTP *traffic* tests run (proposal 021 Phase
1). The address is resolved at runtime from the edge's own LoadBalancer Service status
(robust to MetalLB assignment): until the LB IP is assigned the address is omitted
rather than written empty. All class-`aether` Gateways currently share the one edge
address; distinct per-Gateway addresses are proposal 021 Phase 2.

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
cross-namespace backendRef admission + status enforcement is implemented (resolution
stays namespace-blind until proposal 020 Part 1).

`GRPCRoute` is **not** advertised on the GatewayClass: aether serves `GRPCRoute` only
east-west via GAMMA (`parentRef: Service`), which is a mesh feature with no
GatewayClass, so the north-south Gateway does not serve `GRPCRoute`. Advertising
`SupportGRPCRoute` on the GatewayClass makes the upstream `GATEWAY-GRPC` suite send
gRPC traffic through a Gateway the edge cannot serve (the tests run and fail/timeout),
so it is omitted here while GAMMA continues to implement `GRPCRoute` for the mesh
profile (see "Route types" above).

### Data-plane / addressing gap

The edge data plane is still a single Deployment on a single LoadBalancer address.
Every class-`aether` Gateway now publishes that shared address in `status.addresses`
(proposal 021 Phase 1), which unblocks the address-dependent GATEWAY-HTTP traffic
tests; routes are served through the one shared address (demuxed by listener
hostname/port). Tests that assert each Gateway has its OWN *distinct* address are not
yet satisfied — distinct per-Gateway addressing (per-Gateway LoadBalancer Service +
internal-port demux) is proposal 021 Phase 2.
