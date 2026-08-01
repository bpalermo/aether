# Proposal: Proxy-extension escape hatch for Gateway API / GAMMA

**Status:** Accepted — 2026-06-29 (**Option C**: ExtensionRef → typed CRD, opaque body, fail-closed
**in-process proto-validate** — no Envoy binary in the webhook). Implementation tracked as
milestones M1–M4 (below). The route-rule `ExtensionRef` form (caller-side, cluster-local) is
unblocked and is v1 (M1–M2); the Service-`targetRef` (policy-attachment) form is **blocked on
proposal 026** (multi-cluster config propagation, Accepted — Option E) — a service-global
escape-hatch policy cannot be consistent across clusters until 026's C/E channel or D-enforcement
lands, so it is deferred to M3+.
**Relates:** proposal 026 (multi-cluster config propagation — gates the Service-targetRef form),
proposal 018 (Gateway API/GAMMA), proposal 017 (VirtualHost CRD — the edge escape-hatch
precedent), proposal 015 (MeshConfig CRD — the policy-attachment precedent), proposal 011 / #396
(`envoy --mode validate` offline gate — the optional deeper CI gate), proposal 023
(route-by-Service). [[project_mesh_config_crd]], [[project_gateway_api_gamma]],
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
ride aether's existing strengths: the `config.aether.io` CRD family (MeshConfig, VirtualHost) and
a **fail-closed validation floor**. The floor is **in-process, no Envoy binary**: the admission
webhook resolves the opaque `@type` against the Envoy protos aether already links
(`go-control-plane`) and runs their protoc-gen-validate `ValidateAll()` — the same
`(validate.rules)` Envoy enforces at config load — so a bad payload is rejected at CRD apply,
turning the classic "one bad EnvoyFilter wedges the fleet" footgun into a fail-closed admission
check. The heavier offline `envoy --mode validate` gate (#396) stays as an *optional* deeper CI /
build check, not a runtime webhook dependency.

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
small allow-list of HCM `http_filters` aether knows how to order), and **(b)** validates the
opaque body **fail-closed at admission, in-process, with no Envoy binary** (see Design →
*Validate*): a validating webhook resolves the `@type`, unmarshals
into the concrete Envoy message, and runs its protoc-gen-validate (PGV) `ValidateAll()` —
rejecting anything malformed, unknown-typed, or constraint-violating *before* it reaches a live
proxy. The deeper `envoy --mode validate` stays an *optional* CI / snapshot-build gate, not a
runtime webhook dependency.

- **+** Full power (any `typed_config`) with a real safety floor; smallest delta from what aether
  already generates; the admission floor uses protos aether **already links** (no new dep, no
  Envoy binary in the controller); degrades to Option B over time (promote the popular ones to
  typed fields).
- **−** Still Envoy-version-coupled for the opaque body (mitigated, not removed, by pinning
  `go-control-plane` + the proxy build); proto-validate proves *valid config for a known type*,
  not *the filter is compiled into aether's proxy build* — closed by an explicit filter allow-list
  (below); ordering of arbitrary `http_filters` needs an allow-list, not free placement.

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
- **Plumb:** `GammaRoute` gains `ExtensionFilters []ExtensionFilter` (filter name + placement +
  `*anypb.Any`). The route builder emits the per-route body into `Route.typed_per_filter_config`
  AND ensures the named filter is present in the HCM `http_filters` (default-disabled), because
  **Envoy `typed_per_filter_config` only *overrides* a filter already in the chain — it cannot
  *add* one** (see [Scope](#scope-route-vs-chain)). This is the SAME emission path that already
  produces HeaderMutation/Redirect, so it works identically on the edge and the GAMMA capture path.
- **Validate (the safety floor) — in-process, no Envoy binary:** a validating webhook (the
  aether-controller, which already hosts `/validate`) (1) resolves the payload's `@type` against
  the linked `go-control-plane/envoy` proto registry and unmarshals it into the concrete Envoy
  message — rejecting unknown/unsupported types and malformed bytes; (2) runs that message's
  **protoc-gen-validate `ValidateAll()`** — the SAME `(validate.rules)` constraints Envoy enforces
  at config load (required/range/regex/oneof) — rejecting constraint violations. Both use protos
  aether already imports (no new dependency, no version-matched Envoy binary in the controller).
  The snapshot builder re-checks and skips a bad filter rather than poisoning the whole snapshot
  (fail-open at runtime, fail-closed at admission). `envoy --mode validate` (`//test/envoy_validate`)
  remains an OPTIONAL deeper gate in CI / the snapshot-build path — where the proxy image already
  exists — for the imperative C++ checks PGV can't express; it is NOT a runtime webhook dependency.
- **Filter allow-list:** `spec.filter` must be in an aether-maintained allow-list of HTTP filters
  the aether proxy build (proposals 010/011) actually compiles in. Proto-validate proves the
  *config* is valid for a known type; the allow-list proves the *filter exists in the proxy* (a
  PGV-valid payload for a non-compiled filter would otherwise NACK at runtime). The allow-list
  doubles as the security surface — see [Tensions](#tensions--non-goals).

### Scope: route vs chain

A subtlety that shapes the model and the sequencing: **`typed_per_filter_config` can only
*override* an HTTP filter that is already present in the HCM `http_filters` chain — it cannot
*add* one.** So a "route-scope only" hatch cannot, by itself, deliver a *new* filter like
`header_to_metadata` (the motivating example); it could only re-configure filters aether already
installs. The model is therefore: aether **adds** the allow-listed filter to the HCM chain
**default-disabled** (via `disabled: true` / a per-route `FilterConfig{ disabled }` default), and
the route's `ExtensionRef` both **enables and configures** it through `typed_per_filter_config`.

This means the genuinely useful capability (introducing a new filter) inherently touches the HCM
chain, so the chain-insertion allow-list (ordering) is part of step 1, not a deferred step 3 —
chain *insertion* (allow-listed, aether-ordered) is in scope from the start; what stays deferred /
admin-gated is letting a CRD specify *arbitrary* `http_filters` placement. Route-scope vs
chain-scope is thus about *who controls ordering* (aether vs the CRD), not whether the filter
reaches the chain at all.

## Tensions / non-goals

- **Envoy-version coupling.** An opaque `typed_config` can break when the proxy/`go-control-plane`
  version bumps. The coupling now rides the `go-control-plane/envoy` pin aether *already* carries
  (and which must track the proxy's xDS API regardless) — not a separate Envoy binary in the
  controller. But a *stored* CRD can silently age across an upgrade. Mitigate: a controller
  reconcile re-runs proto-validate over stored HTTPFilters on startup / version change and surfaces
  staleness explicitly — an `Accepted=False` status condition + an event/metric — so a now-invalid
  filter is visible, not silently dropped (the runtime fail-open would otherwise just make the
  effect vanish). Pin the version (proposal 009/010).
- **Validate fidelity.** Proto-validate (`@type` resolve + PGV `ValidateAll`) reproduces Envoy's
  *message-level* checks, not its imperative C++ config-load checks (some filters validate further
  in code). The optional `envoy --mode validate` CI/build gate covers those; admission catches
  *malformed*, not *misguided* (a payload can be valid yet semantically wrong).
- **Filter must be in the proxy build.** A PGV-valid payload for a filter the aether proxy doesn't
  compile in would NACK at runtime — bounded by the `spec.filter` allow-list (see Design). Keep the
  allow-list in lockstep with the proxy build's compiled extensions (proposals 010/011).
- **Not arbitrary bootstrap.** This is route/chain HTTP-filter config, not listener/cluster/
  bootstrap surgery. No `xds`-level patches (Istio EnvoyFilter's most dangerous mode). Keep it to
  `http_filters` + `typed_per_filter_config`.
- **Ordering.** Arbitrary `http_filters` placement can break the chain (e.g. a filter after the
  router). Allow-list the insertion points; never let a CRD reorder the router/mTLS/health seams.
- **Security.** An escape hatch is privileged. RBAC on the CRD. Note the useful capability
  (introducing a new filter) requires chain insertion (see Scope), so it cannot be gated purely
  "route-scope for everyone, chain-scope for admins"; gate instead on *who controls placement* —
  aether-ordered insertion of an allow-listed filter is the default operators get; CRD-specified
  *arbitrary* `http_filters` ordering is the admin-owned (MeshConfig-flagged) surface.
- **Route status.** An `ExtensionRef` to a missing / non-allow-listed / proto-invalid HTTPFilter
  must surface on the route's `ResolvedRefs` condition (Gateway API contract), not just fail
  silently.
- **CRD schema.** `spec.typedConfig` is necessarily `x-kubernetes-preserve-unknown-fields` (the API
  server can't structurally validate an `Any`); the webhook's proto-validate is therefore the
  *only* schema enforcement — another reason the admission floor must be fail-closed.
- **GRPCRoute parity.** The same `ExtensionRef` path applies to GRPCRoute filters (aether supports
  GRPCRoute, #289); plumb both, not HTTPRoute only.
- **Multi-cluster (see proposal 026).** aether federates the *registry*, not *config*: an HTTPFilter
  applied in one cluster is visible only there, and GAMMA filters apply consumer-side (the caller's
  egress). So the caller-side route `ExtensionRef` form is naturally cluster-local (class-2 — fine,
  GitOps-replicate per consumer cluster), but the Service-`targetRef` form *reads* as service-global
  while actually being enforced per consumer cluster. That form is **blocked on proposal 026**'s
  class-1 propagation (Option C) / producer-waypoint enforcement (Option D); v1 ships route-scope
  only.
- **Conformance.** ExtensionRef is an *implementation-specific* extension; it does not affect
  GATEWAY-HTTP/MESH-HTTP conformance (those test the typed filters). This is purely additive.

## Verification

- Unit (webhook): an HTTPFilter with a `header_to_metadata` `typedConfig` proto-validates
  (`@type` resolves + PGV `ValidateAll` passes) → admitted; a malformed payload, an unknown
  `@type`, a non-allow-listed `spec.filter`, and a PGV-constraint violation are each rejected by
  the webhook — all in-process, no Envoy binary.
- Unit (builder): the admitted filter is added to the HCM chain default-disabled and the route
  emits the matching `typed_per_filter_config`; the builder skips (not poisons) a bad filter.
- CI gate (optional deeper): `//test/envoy_validate` accepts the rendered snapshot carrying the
  filter (catches the imperative C++ checks PGV can't).
- e2e: a meshed route with an `ExtensionRef` HTTPFilter applies the Envoy filter (assert the
  effect, e.g. the dynamic metadata / mapped header) on both the edge and the GAMMA capture path.

## Milestones

- **M1 — route-scope ExtensionRef (the v1 escape hatch).** `HTTPFilter` CRD (`config.aether.io/v1`)
  + `spec.filter` allow-list (filters the aether proxy build compiles in) + `GammaRoute.Extension­Filters`
  plumbing: add the allow-listed filter to the HCM chain default-disabled and enable+configure it
  per-route via `typed_per_filter_config` (aether-ordered insertion — NOT arbitrary CRD placement).
  The HTTPRoute/GRPCRoute rule's `filters: [{type: ExtensionRef, …}]` resolves to it. **Exit:** a
  meshed/edge route with an `ExtensionRef` HTTPFilter applies the Envoy filter (e.g.
  `header_to_metadata`), asserted by effect; demand-scoped + GAMMA-capture paths both work.
- **M2 — fail-closed validation (the safety floor).** Validating webhook on the CRD: resolve the
  opaque `@type` against the linked `go-control-plane` protos, unmarshal, run PGV `ValidateAll()`
  in-process (no Envoy binary); reject unknown-type / malformed / constraint-violating / non-allow-
  listed payloads and set the route `ResolvedRefs` condition. Builder skips (not poisons) a bad
  filter. Optional deeper `envoy --mode validate` stays a CI/build gate. **Exit:** a malformed
  payload is rejected at apply; a valid one admits; `//test/envoy_validate` accepts the rendered
  snapshot.
- **M3 — Service-`targetRef` (policy attachment), multi-cluster-aware.** GAMMA object-wide
  attachment to a Service; **gated on proposal 026** — a service-global filter is a class-1 payload
  that rides 026's C/E config channel (export the filter with the service) or D (waypoint
  enforcement). **Exit:** a Service-attached HTTPFilter applies to the service's callers consistently
  (within a cluster now; across clusters once 026 lands).
- **M4 — opportunistic typed promotion + chain-ordering hardening.** Promote popular filters to
  first-class typed fields (Option B) over time; admin-owned (MeshConfig-gated) arbitrary
  `http_filters` ordering for the rare case aether's default insertion order is wrong.

Independent of the MESH-HTTP route-selection debugging — this is a new surface, not a fix to the
existing typed path.
