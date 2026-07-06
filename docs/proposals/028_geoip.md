# Proposal: GeoIP at the edge

**Status:** Accepted — 2026-07-06
**Relates:** proposal 003/017/018 (the edge + Gateway API HTTPRoute — geo-routing
consumers), #476 RBAC / proposal 027 ext_authz (geo-blocking consumers), proposal
027 (the preset-sidecar pattern the DB updater follows).

## Summary

Geolocate the real client at the **edge** (`envoy.filters.http.geoip` + the MaxMind
provider — both already compiled into the proxy) and emit `x-geo-*` request headers.
No new policy surface: the headers compose with what is already shipped —
HTTPRoute header matches (geo-routing), RBAC/ext_authz (geo-blocking), access logs
(geo-observability). Mesh-internal geoip is a non-goal: east-west client addresses
are pod IPs.

## Findings that shaped the design (Envoy v1.36.0 source)

1. **The filter does NOT sanitize untrusted input.** `onLookupComplete` overwrites a
   header only on a non-empty lookup; on a miss (unknown IP, private address, driver
   failure) a client-supplied `x-geo-country` passes through untouched. Aether
   therefore treats `x-geo-*` as a RESERVED namespace: the edge emits an
   unconditional `header_mutation` strip of the configured geo headers BEFORE the
   geoip filter (route-level removal would run at router time and wipe the
   legitimate values) — and strips them even when geoip is disabled, so a
   geoip-less edge never launders spoofed geo headers into the mesh.
2. **`xff_num_trusted_hops` is a topology fact, not a geoip setting.** It exists on
   the geoip filter (`Geoip.XffConfig`) AND on the HCM; both must agree. Chart value
   `edge.xffNumTrustedHops` (top-level) feeds both.

## Design

```yaml
edge:
  xffNumTrustedHops: 0          # proxies in front of the edge; feeds HCM + geoip
  geoip:
    enabled: false
    headers: [country, city, asn]   # which x-geo-* to emit
    database:
      secretName: ""                # bring-your-own mmdb (licensing: aether never bundles)
      geoipupdate:                  # optional preset (the 027 OPA-preset pattern)
        enabled: false
        licenseSecret: ""           # MaxMind account creds
        editions: [GeoLite2-Country]
```

- Edge Deployment: mmdb volume (Secret, or emptyDir refreshed by the official
  `geoipupdate` sidecar preset); agent edge codegen (`--geoip-db-path`,
  `--geoip-headers`, `--xff-num-trusted-hops`) inserts [strip → geoip(MaxMind
  provider, header→field mapping)] after readiness, before the router, on the edge
  HTTP(S) listeners. Chain-level and always-on when enabled — Envoy has no
  GeoipPerRoute, which conveniently avoids the TPFC/chain-entry bug class
  (#470/#477) entirely.
- Header set v1: `x-geo-country` (ISO code), `x-geo-city`, `x-geo-asn`,
  `x-geo-anon` (anonymous/VPN flag) — each opt-in via `headers`.
- Composition (documented, e2e-tested): HTTPRoute `headers: [{name: x-geo-country,
  value: GB}]` matches route geo traffic; RBAC `permissions.header`/ext_authz read
  the same headers for geo-blocking.

## Milestones

- **M1:** chart (values, volumes, geoipupdate preset) + edge codegen (strip + filter
  + dual xff threading) + envoy-validate fixture using MaxMind's free test mmdbs +
  seam tests (spoofed x-geo-* never survives: hit, miss, and geoip-disabled cases).
- **M2 (e2e, talos):** real edge + test mmdb + `useXff` path: inject a test-DB IP
  via X-Forwarded-For through the LB → backend sees x-geo-country; HTTPRoute
  header-match geo-routes; spoof case (private source, forged header) never reaches
  the backend.

## Non-goals (v1)

Mesh-internal geoip; the network-level geoip filter; per-route geoip; bundling any
database; non-MaxMind providers; geo-based *default* policies (aether emits facts,
users write policy).
