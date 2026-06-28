# Proposal: Proxy-extension escape hatch for Gateway API / GAMMA

**Status:** Design — 2026-06-28 (options for review)
**Relates:** proposal 018 (Gateway API/GAMMA), proposal 017 (VirtualHost CRD — the edge
escape-hatch precedent), proposal 015 (MeshConfig CRD — the policy-attachment precedent),
proposal 011 / #396 (`envoy --mode validate` offline gate — the safety property this leans
on), proposal 023 (route-by-Service). [[project_mesh_config_crd]], [[project_gateway_api_gamma]],
[[project_edge_proxy_plan]].

## Summary

aether translates Gateway API HTTPRoute/GRPCRoute (north-south at the edge, east-west via
GAMMA) into Envoy config, implementing the **typed** Gateway API filters — RequestHeader­Modifier,
ResponseHeaderModifier, RequestRedirect, URLRewrite, and weighted backends. That set is
deliberately small; Envoy supports far more (e.g. `header_to_metadata`, `lua`, `ext_proc`,
`rate_limit`, custom `typed_per_filter_config`) that the Gateway API spec does not model. This
proposes an **escape hatch**: a way to attach proxy-specific (Envoy) configuration to a
route or a Service **through the standard Gateway API extension points**, so operators can use
proxy features aether hasn't (or won't) promote to a first-class typed field — without forking
the data plane or hand-writing bootstrap.

The decision to make is **which of three shapes** to adopt (see [Options](#options)). All three
ride aether's existing strengths: the `config.aether.io` CRD family (MeshConfig, VirtualHost)
and the **offline `envoy --mode validate` gate**, which lets aether validate an opaque
extension *before* it reaches a proxy — turning the classic "one bad EnvoyFilter wedges the
fleet" footgun into a fail-closed admission check.

## Motivation

- **The typed filter set is a floor, not a ceiling.** GAMMA conformance and real workloads
  need things the spec doesn't model. `header_to_metadata` (the user's example) maps a request
  header into dynamic metadata for downstream RBAC/routing/telemetry — there is no Gateway API
  filter for it. Today the only path is a code change to aether's route builder per feature.
- **Gateway API anticipated this.** The spec provides first-class extension points (below); not
  using them means either under-serving users or unbounded typed-field sprawl in aether's CRDs.
- **aether already has the policy-attachment muscle.** MeshConfig (config.aether.io/v1,
  namespaced + aether-system fallback) and the edge VirtualHost CRD are exactly the
  implementation-CRD pattern. An escape hatch is the same shape pointed at Envoy filter config.
- **aether has a safety property Istio's EnvoyFilter lacks.** The `//test/envoy_validate` gate
  runs `envoy --mode validate` over generated config offline. An escape-hatch payload can be
  validated at admission/build time, so a malformed `typed_config` fails the CRD apply (or the
  snapshot build) instead of NACKing CDS/LDS across every proxy.

## The Gateway API extension points (what we're plugging into)

1. **`ExtensionRef` filters (route rule scope).** `HTTPRouteFilter`/`GRPCRouteFilter` define a
   standard `type: ExtensionRef` beside the typed filters; it references an implementation CRD
   (`group`/`kind`/`name`). The natural hook for a **per-route** proxy filter.
2. **Policy Attachment (object scope, GEP-713 / -2648 direct / -2649 inherited).** An
   implementation CRD with `targetRefs` attaching to a Gateway, (HTTP|GRPC)Route, **or Service**
   — the last makes it work east-west for GAMMA. The hook for **object-wide** proxy config.
3. **`GatewayClass.spec.parametersRef`** — class-wide settings (out of scope here).

Whatever shape we pick rides points 1 and/or 2, so it is portable Gateway API, not a bespoke
side-channel.

## Options

> This is the part to review. All three share the same data-plane plumbing
> (`GammaRoute.ExtensionFilters []*anypb.Any` emitted into `Route.typed_per_filter_config` or
> the HCM `http_filters` list) and the same validate-gate. They differ in **what the operator
> writes** and **how much aether constrains it**.

### Option A — Opaque `typed_config` passthrough (EnvoyFilter-style, maximal power)
A CRD (`config.aether.io/HTTPFilter`) whose body is a raw Envoy filter `typed_config` (`Any`)
plus a placement (the named HTTP filter + route/chain scope). aether splices it verbatim.

- **+** Anything Envoy can do, day one; zero per-feature work in aether.
- **−** Couples users to Envoy internals and the pinned `ENVOY_VERSION`; a payload can be valid
  yet semantically wrong; largest blast radius. This is Istio's EnvoyFilter — powerful and
  famously a footgun. The validate-gate catches *malformed* config, not *misguided* config.

```yaml
apiVersion: config.aether.io/v1
kind: HTTPFilter
metadata: { name: h2m, namespace: team-a }
spec:
  filter: envoy.filters.http.header_to_metadata
  typedConfig:          # raw Envoy HeaderToMetadata Any
    "@type": type.googleapis.com/envoy.extensions.filters.http.header_to_metadata.v3.Config
    requestRules: [{ header: x-tenant, onHeaderPresent: { metadataNamespace: aether, key: tenant } }]
```

### Option B — Typed per-feature fields (safe, version-stable, bounded)
No opaque payload; aether grows a typed CRD field per supported feature (a real
`headerToMetadata:` schema, a real `rateLimit:` schema, …), validated by the CRD schema and
translated by aether.

- **+** Safe, self-documenting, version-stable, schema-validated; no Envoy coupling leaks to users.
- **−** aether implements every feature; users wait for each; defeats the point of an "escape
  hatch" (it's just more first-class fields).

### Option C — `ExtensionRef` → typed CRD, opaque body, fail-closed validate (recommended middle)
The `ExtensionRef`/`targetRef` mechanism (portable Gateway API) points at a
`config.aether.io/HTTPFilter` CRD that carries an **opaque `typedConfig` (`Any`)**, but aether
**(a)** restricts the *placement* to known-safe seams (a route's `typed_per_filter_config`, or a
small allow-list of HCM `http_filters` aether knows how to order), and **(b)** runs the
generated config through `envoy --mode validate` at admission (a validating webhook on the CRD)
and again at snapshot build — rejecting anything that NACKs *before* it reaches a live proxy.

- **+** Full power (any `typed_config`) with a real safety floor; smallest delta from what aether
  already generates; uses the validate-gate aether already owns; degrades to Option B over time
  (promote the popular ones to typed fields).
- **−** Still Envoy-version-coupled for the opaque body (mitigated, not removed, by the pin +
  the gate); ordering of arbitrary `http_filters` needs an allow-list, not free placement.

**Recommendation: Option C.** It's the only one that is *both* an actual escape hatch and
fail-closed, and it's the natural extension of aether's CRD + validate-gate posture.

## Design (for the recommended Option C)

- **CRD:** `config.aether.io/v1 HTTPFilter` — `spec.filter` (the Envoy HTTP filter name),
  `spec.typedConfig` (opaque `Any`), `spec.scope` (route | chain), and for the policy form a
  `targetRefs`. Lives in `common/apis/config/v1` beside MeshConfig.
- **Attach:** an HTTPRoute rule's `filters: [{type: ExtensionRef, extensionRef: {group:
  config.aether.io, kind: HTTPFilter, name: …}}]` (per-route), and/or `targetRefs` to a Service
  (GAMMA object-wide). The GAMMA reconciler resolves the ref(s) and attaches them to the
  `GammaRoute`.
- **Plumb:** `GammaRoute` gains `ExtensionFilters []ExtensionFilter` (placement + `*anypb.Any`).
  `BuildOutboundServiceVirtualHost` / the route builder emit them into `Route.typed_per_filter_config`
  (route scope) or merge into the HCM `http_filters` (chain scope, allow-listed ordering) — the
  SAME path that already emits HeaderMutation/Redirect, so it works identically on the edge and
  the GAMMA capture path.
- **Validate (the safety floor):** a validating webhook (the aether-controller, which already
  hosts `/validate`) runs `envoy --mode validate` over a synthetic listener carrying the payload
  and rejects the CRD on NACK; the snapshot builder re-checks and skips a bad filter rather than
  poisoning the whole snapshot (fail-open at runtime, fail-closed at admission).

## Tensions / non-goals

- **Envoy-version coupling.** An opaque `typed_config` can break when `ENVOY_VERSION` bumps. The
  validate-gate catches it at the next build/admission, but a stored CRD can silently age. Pin
  the version (proposal 009/010) and re-validate stored HTTPFilters on proxy upgrade.
- **Not arbitrary bootstrap.** This is route/chain HTTP-filter config, not listener/cluster/
  bootstrap surgery. No `xds`-level patches (Istio EnvoyFilter's most dangerous mode). Keep it to
  `http_filters` + `typed_per_filter_config`.
- **Ordering.** Arbitrary `http_filters` placement can break the chain (e.g. a filter after the
  router). Allow-list the insertion points; never let a CRD reorder the router/mTLS/health seams.
- **Security.** An escape hatch is privileged. RBAC on the CRD; consider gating chain-scope behind
  a cluster-admin-owned MeshConfig flag while route-scope `typed_per_filter_config` (which can't
  reorder the chain) is the default operators get.
- **Conformance.** ExtensionRef is an *implementation-specific* extension; it does not affect
  GATEWAY-HTTP/MESH-HTTP conformance (those test the typed filters). This is purely additive.

## Verification

- Unit: an HTTPFilter with a `header_to_metadata` `typedConfig` → the route emits the matching
  `typed_per_filter_config`; offline `//test/envoy_validate` accepts it; a malformed payload is
  rejected by the webhook and skipped by the builder.
- e2e: a meshed route with an `ExtensionRef` HTTPFilter applies the Envoy filter (assert the
  effect, e.g. the dynamic metadata / mapped header) on both the edge and the GAMMA capture path.

## Sequencing

1. `HTTPFilter` CRD + `GammaRoute.ExtensionFilters` plumbing into the route builder (route scope
   only first — `typed_per_filter_config`, which can't reorder the chain).
2. The validating webhook running `envoy --mode validate` on the payload (the fail-closed floor).
3. Chain-scope `http_filters` with an allow-listed insertion set (gated, admin-owned).
4. Promote popular filters to typed fields (Option B) opportunistically.

Independent of the MESH-HTTP route-selection debugging — this is a new surface, not a fix to the
existing typed path.
