# Proposal: VirtualHost CRD — L7 path-based edge routing (supersedes EdgeRoute)

**Status:** Implemented, then superseded — the `VirtualHost` CRD shipped, but edge L7 routing has since migrated to the Gateway API (`HTTPRoute`) and the `VirtualHost` CRD was retired. See proposal 018. (2026-06-21 design.)
**Relates:** proposal 003 (edge proxy — the EdgeRoute this replaces), proposal 015
(MeshConfig / the controller webhook this extends), proposal 005 (multi-port);
[[project_edge_proxy_plan]].

## Summary

Replace the edge's host→single-service `EdgeRoute` with a richer **`VirtualHost`
CRD**: one external FQDN (or wildcard) carrying an **ordered list of routes**, each
matching a path and forwarding to a mesh service. This adds the L7 vocabulary the
edge has lacked (`api.example.com/users → svc-users`, `/orders → svc-orders`
under one hostname and one cert) while keeping the edge's direct-to-pod mTLS model.

`EdgeRoute` is **removed** (the edge is e2e-only, not production), not kept as
back-compat. A **validating webhook** on the controller rejects duplicate FQDNs
across manifests at apply-time.

## Motivation

Today (proposal 003) an `EdgeRoute` maps `hosts[] → one service`; the builder emits
one Envoy vhost per *cluster* with a single catch-all route (`prefix: /`). A whole
hostname goes to one service — no path or other L7 matching. The natural next step
(proposal 003 itself flagged it) is the Envoy `VirtualHost{ domains, routes[] }`
model: a list of matches, not one catch-all.

We choose a **custom `VirtualHost` CRD** over Gateway API HTTPRoute for the same
reasons proposal 003 chose EdgeRoute: it names the mesh **service** (= the pod's
ServiceAccount = registry key) directly — no `Service`-object indirection — is a
tiny namespaced watch (no GatewayClass/conformance controller), and reuses the
`config.aether.io` + controller-webhook machinery from proposals 003/015. The
spec is shaped to migrate to Gateway API later when richer L7 (weighted backends,
header/method matching, filters) is wanted.

## The CRD (`config.aether.io/v1`, namespaced)

```yaml
apiVersion: config.aether.io/v1
kind: VirtualHost
metadata: { name: api, namespace: aether-system }
spec:
  hosts: [api.example.com]          # FQDN(s) or *.suffix wildcard; >=1, unique across manifests
  tls:                              # optional downstream cert (SNI-selected, hot-rotated)
    secretName: example-tls
    provider: KUBERNETES            # reuses the SecretProvider enum from EdgeRouteTLS
  routes:                           # ORDERED — first match wins (operator controls specificity)
    - match: { prefix: /users }
      backend: { service: svc-users, port: 8080 }
    - match: { exact: /healthz }
      backend: { service: svc-health }
    - match: { prefix: / }          # catch-all default (put last)
      backend: { service: svc-web }
```

Proto (`api/aether/config/v1/virtual_host.proto`, edition 2024, protovalidate):

```proto
message VirtualHostSpec {
  // External hostnames (Host/SNI). Each may be an exact FQDN or a "*.suffix"
  // wildcard. >=1, items unique within the manifest; uniqueness ACROSS manifests
  // is enforced by the controller's validating webhook.
  repeated string hosts = 1;        // min_items=1, unique, hostname-or-wildcard
  // Ordered routes; Envoy evaluates them top-down, first match wins.
  repeated HTTPRoute routes = 2;     // min_items=1
  VirtualHostTLS tls = 3;            // optional
}

message HTTPRoute {
  RouteMatch  match   = 1;           // required
  RouteBackend backend = 2;          // required
}

message RouteMatch {
  // v1: path prefix or exact. regex / headers / method are Phase 2.
  oneof path { string prefix = 1; string exact = 2; }
}

message RouteBackend {
  string service = 1;                // mesh service (ServiceAccount = registry key)
  uint32 port    = 2;                // 0 = service default port (proposal 005)
}

// Reused verbatim from the removed edge_route.proto:
message VirtualHostTLS { string secret_name = 1; SecretProvider provider = 2; }
enum SecretProvider { SECRET_PROVIDER_UNSPECIFIED = 0; SECRET_PROVIDER_KUBERNETES = 1; SECRET_PROVIDER_AWS_SECRETS_MANAGER = 2; }
```

**Match ordering is list order** (Envoy route order = CR `routes` order). The
operator orders by specificity (`/api/v2` before `/api` before `/`). No automatic
precedence sort in v1 — predictable and simple; Gateway-API-style sorting is a
Phase-2 option.

## Data-plane projection

One Envoy `VirtualHost` per CR: `spec.hosts` → `domains`; each `routes[i]` →
an Envoy `Route{ match → RouteAction{ cluster } }`, emitted in list order.

- **match**: `prefix` → `RouteMatch_Prefix`; `exact` → `RouteMatch_Path`.
- **cluster**: `(backend.service, backend.port)` resolved by the existing
  `edgeClusterNameLocked` (service→`ServiceClusterName`, non-zero port→per-port
  cluster, proposal 005), with the existing outbound retry policy. The edge dials
  the destination pods directly over single-identity mTLS — unchanged.
- The single-`prefix:/`-route vhost (`BuildOutboundClusterVirtualHost`) becomes
  the degenerate case the builder still uses internally.

**Dependency set** = the union of *every backend's* service across all
VirtualHosts (so the registrar watch + ODCDS clusters follow every exposed
backend). **TLS-secret set** = the union of referenced cert names; SNI selection
and hot rotation are unchanged from EdgeRoute.

## TLS / wildcard certs — no new machinery

The existing per-route `secretName` + SDS-by-SNI already serves a wildcard cert:
Envoy matches a cert's `*.example.com` SAN against SNI `api.example.com`. Multiple
VirtualHosts reference the same wildcard Secret. The only change is relaxing host
validation to accept a leading `*.` so `hosts: [*.example.com]` (a wildcard
*vhost*, distinct from the wildcard *cert*) is allowed.

## Validating webhook — duplicate-FQDN guard

Extend the **`aether-controller`** (already serving the MeshConfig webhook at
`/validate`, proposal 015) with a VirtualHost validating webhook:

- **Rule:** on `CREATE`/`UPDATE`, reject if any `spec.hosts` entry (exact string)
  is already claimed by **another** `VirtualHost` cluster-wide (excludes self on
  update). The error names the conflicting object, failing `kubectl apply` so the
  collision never reaches the data plane.
- **Within-manifest** duplicate hosts → protovalidate `unique` on the repeated
  field (structural; no webhook).
- **Wildcard scope:** reject only *exact-string* duplicates. `*.example.com` and
  `api.example.com` in separate manifests are NOT a conflict — Envoy resolves
  most-specific-first. (Wildcard-overlap *warnings* are a Phase-2 nicety.)
- **Race backstop:** admission can't close a TOCTOU (two simultaneous applies of
  the same FQDN both pass, both commit). So the edge **keeps a deterministic
  runtime dedup** (keep-first by `namespace/name`, log + drop the loser) as
  defense-in-depth. Webhook = the clean apply-time error for the common case;
  runtime dedup = the safety net that keeps Envoy from NACKing duplicate domains.
- **Wiring:** a `ValidatingWebhookConfiguration` for `virtualhosts`
  (CREATE/UPDATE) → controller Service `/validate-virtualhost`; controller RBAC
  gains `virtualhosts: get/list/watch`; reuses the controller's existing webhook
  serving cert (self-signed default or SPIRE, proposal 015).

## Removing EdgeRoute

The edge is e2e-only (no production consumers), so EdgeRoute is removed, not
aliased:

- Delete `edge_route.proto` (fold `VirtualHostTLS`/`SecretProvider` into
  `virtual_host.proto`), `common/apis/config/v1/edgeroute_*.go`, and
  `charts/crds/templates/edgeroute.yaml`.
- Cache: `EdgeRoute`/`SetEdgeRoutes`/`edgeRouteVhosts`/`EdgeRouteTLS` →
  `edgeVirtualHost{ Hosts, Routes []edgeHTTPRoute, TLSSecret }` (internal name
  avoids clashing with Envoy's `routev3.VirtualHost`) + `SetVirtualHosts`.
- Reconciler watches `VirtualHost` instead of `EdgeRoute`.
- The `secret` provider / SDS / TLS code is untouched.
- The live talos EdgeRoute (svc-1 + wildcard, from #244) is migrated to a
  VirtualHost as part of the e2e.

## Verification

- **Unit:** projection builds one vhost per CR with ordered prefix/exact routes →
  correct clusters; multi-route ordering preserved; wildcard host accepted; TLS
  secret-name union; webhook rejects a cross-manifest duplicate FQDN and admits a
  wildcard-vs-specific pair; runtime dedup keeps-first on a forced collision.
- **e2e on talos-main** (publish, then deploy from the published chart per
  [[feedback_no_manual_publish]]; edge LB IP is MetalLB, not laptop-routable —
  drive from in-cluster):
  1. Ensure the `*.palermo.dev` `kubernetes.io/tls` Secret is in the edge's
     watched namespace.
  2. Apply a `VirtualHost api.palermo.dev` with 2–3 path routes to existing test
     services (echo / svc-1 / svc-2) under that Secret; deploy edge with
     `tls.enabled`.
  3. In-cluster `curl --resolve api.palermo.dev:443:<edge-LB-IP> https://api.palermo.dev/<path>`
     → each path hits the right service; `openssl s_client -servername` shows the
     Let's Encrypt `*.palermo.dev` cert; upstream XFCC SAN = the edge identity.
  4. Apply a second VirtualHost reusing `api.palermo.dev` → **rejected by the
     webhook**.
  5. Rotate the cert → new serial without an edge pod roll.

## Scope / phasing

- **v1 (this proposal):** path `prefix` + `exact` match, single backend per route,
  wildcard host + wildcard cert, the duplicate-FQDN webhook, EdgeRoute removal.
- **Phase 2 (later):** `regex` / header / method matches, weighted backends, path
  rewrite/redirect, Gateway-API-style precedence sorting, wildcard-overlap
  warnings. The spec is shaped so these are additive.
