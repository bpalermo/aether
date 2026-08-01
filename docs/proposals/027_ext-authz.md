# Proposal: external authorization (ext_authz) via a node-proxy sidecar

**Status:** Accepted — 2026-07-05
**Relates:** proposal 025 (proxy-extension escape hatch — the enablement machinery),
proposal 015 (MeshConfig — the system-config half), proposal 026 (config propagation —
policy parameters ride the channel), proposal 019 (waypoint — the alternative
enforcement point, unbuilt).

## Summary

Add `envoy.filters.http.ext_authz` support: per-request authorization decisions from
an **authorization sidecar co-located in the aether-proxy DaemonSet pod**, reached
over a Unix domain socket. The filter's transport (target, timeout, failure mode) is
**system-owned** (chart/agent); *what is checked where* is **user-owned** through the
shipped 025 scopes (per-route `ExtensionRef`, per-service `targetRefs`, service-wide
CHAIN) carrying per-route `context_extensions` as policy parameters. An opt-in OPA
preset ships as the batteries-included policy engine; bring-your-own image is the
general path. Default: everything off.

## Why a sidecar (not a remote authz mesh service)

`ExtAuthzPerRoute` — the typed_per_filter_config type — can only carry
`disabled`/`check_settings{context_extensions,…}`; the gRPC target, timeout, and
failure mode can ONLY live on the HCM chain entry. That forces a split-ownership
design, and once the transport is system-owned, a node-local sidecar strictly
dominates a remote service:

- **No cluster problem.** The authz target is a STATIC bootstrap cluster (UDS pipe in
  the shared pod) — no demand-scoping, no ODCDS cold path, always warm. A remote
  service would need dependency-set plumbing and cold-start handling for a
  fail-closed filter.
- **Node-local failure domain.** Sidecar down = that node degrades (per the declared
  failure mode); no cross-node fan-in to a single authz deployment.
- **~zero added latency.** UDS round-trip, no network hop, no mTLS handshake.
- Cost accepted: per-node authz resources; policy distribution to every node is the
  policy engine's job (e.g. OPA bundles); toggling the sidecar edits the bootstrap →
  proxy hot-restart (proven hitless).

## Design

### System half (chart + agent — the transport)

```yaml
proxy:
  authzSidecar:
    enabled: false          # master switch
    opa:
      enabled: false        # built-in OPA preset (openpolicyagent/opa envoy plugin)
      policy: ""            # rego, mounted via ConfigMap; required when opa.enabled
    image: {}               # bring-your-own container (repository/tag/args/ports)
    timeout: 200ms
    failureMode: DENY       # DENY | ALLOW — REQUIRED, no silent Envoy default
```

- The aether-proxy DaemonSet gains the sidecar container + a shared emptyDir for the
  UDS (`/run/aether/authz.sock`); the proxy bootstrap gains a static `authz_sidecar`
  cluster (pipe address).
- The agent (flag `--authz-sidecar`, set by the chart when enabled) emits an
  `envoy.filters.http.ext_authz` HCM entry — FULL config (UDS cluster, timeout,
  failure mode, `transport_api_version: V3`), **`disabled: true`** — at the 025
  extension anchor on the capture, outbound, and (M3) inbound HCM chains. Disabled =
  zero effect until a route opts in; the entry is emitted only when the sidecar runs
  (a TPFC naming an absent filter rejects config — the 025/#470 invariant).
- OPA preset: `openpolicyagent/opa:<ver>-envoy` speaking the ext_authz gRPC API on
  the UDS, policy from a chart-templated ConfigMap. It is an OPT-IN preset —
  `opa.enabled: false` by default; aether is not opinionated about policy engines.

### User half (HTTPFilter — what is checked where)

Typed-only authoring (an opaque ext_authz payload could name arbitrary clusters —
rejected by the webhook):

```yaml
spec:
  scope: SCOPE_ROUTE | SCOPE_CHAIN     # all shipped 025 scopes apply
  targetRefs: [{kind: Service, name: echo}]
  extAuthz:
    contextExtensions: {policy: "payments-rw"}   # per-route parameters to the engine
    disableRequestBodyBuffering: true
```

Renders to `ExtAuthzPerRoute{check_settings{…}}` — enabling the (disabled) chain
filter for exactly the targeted routes/vhosts and delivering the parameters with
every check. Per-route exemption = `extAuthz: {disabled: true}`. Health/readiness
stay exempt (answered before the anchor). Propagates cross-cluster via 026's
projection unchanged (`extension_filters`/`service_filter`).

### Identity input

Inbound (M3) enforcement receives the caller's VERIFIED SPIFFE ID via the
SANITIZE_SET XFCC header — the payload that makes mesh authz decisions real.
Outbound/capture checks are egress policy ("may this pod call X"): the source pod is
implied by the listener; explicit source-identity injection into the check request is
a follow-up if policies need it.

## Milestones

- **M1 — system half:** chart sidecar (+OPA preset flag) + bootstrap UDS cluster +
  agent flag → disabled ext_authz entry on capture+outbound HCMs; envoy-validate
  fixture. Exit: sidecar runs, entry present, zero traffic impact.
- **M2 — user half:** allow-list (typed-only) + `extAuthz` authoring form + webhook
  validation. Exit: a targeted route 403s/200s by sidecar decision.
- **M3 — inbound enforcement:** the same machinery on the destination pods' inbound
  HCM (extension threading parity, listener regen) — unbypassable in-mesh authz on
  verified caller identity.
- **M4 — talos e2e:** OPA preset with a header+SPIFFE policy: allow/deny,
  per-route context_extensions, sidecar-down under BOTH failure modes, filter
  deletion restores. (025-M4 lesson: the e2e is where the truth lives.)

## Non-goals (v1)

HTTP-protocol authz services; `with_request_body` buffering; network-level
ext_authz; the RBAC filter (compiled in — natural follow-up as "local authz without
a sidecar"); remote/off-node authz services; source-identity injection on egress
checks.
