# 032 — Upgrade Gateway API to v1.6.1

Status: Proposed

## Motivation

Aether pins `sigs.k8s.io/gateway-api` v1.5.1 (Go dep, conformance runner, and
the CRDs installed out-of-band on clusters and in CI). v1.6 promoted TCPRoute
and UDPRoute to **GA (`v1`, standard channel)** and deprecated their
`v1alpha2` versions for future removal — and aether watches both at
`v1alpha2` (agent l4route reconciler and the edge reconciler). Staying on
1.5.1 rides a deprecation with a real (if unscheduled) removal deadline;
moving now also picks up a substantially de-flaked conformance suite. The
v1.5.1→v1.6.1 dep bump was deliberately held out of the 2026-07-18 dependency
cohort (#529) because the runner and CRDs must move together — this proposal
is that migration.

## What changed upstream (v1.5.1 → v1.6.1)

| Change | Aether impact |
|---|---|
| TCPRoute + UDPRoute promoted to `v1`, **standard channel**; `v1alpha2` deprecated | The core migration: l4route + edge watch `v1alpha2` TCP/UDPRoute. Also means L4 no longer needs the experimental-channel CRD install. TLSRoute is untouched (still not promoted). |
| `ReferenceGrant.spec` now required (breaking, admission-level) | We only read grants; fixtures already set spec. No code change. |
| HTTPRoute/GRPCRoute same-hostname rejection relaxed (MUST→MAY) | Our dup-hostname webhook stays valid (stricter is allowed). |
| `ListenerStatus.supportedKinds` `omitempty`→`omitzero` | Edge writes listener status — behavior-preserving, eyeball at migration. |
| HTTPRoute retry CEL tightened; TLSRoute hostname limit 16→1024; CA refs 8→16; infra annotations ≤16 | CRD-side only; inert for aether. |
| New conformance tests: `GatewayInvalidParametersRef`, listener `ListenersNotValid`/`UnsupportedProtocol`, ListenerSet suite, TCPRoute suite (GEP-2644, `SupportTCPRoute`), GATEWAY-UDP profile | `GatewayInvalidParametersRef` is the likely new failure — we implement `parametersRef` (029 EdgeConfig) but don't surface an InvalidParameters Gateway condition today. ListenerSet/UDP are unclaimed features (skipped). TCPRoute suite is a claimable win (edge serves TCPRoutes). |
| `MeshHTTPRoute307Redirect` wrong-manifest bug fixed | MESH-HTTP Core 8/8 was earned against the buggy test; re-run may newly exercise mesh 307 redirect. |
| Conformance machinery: readiness-gated weighted-routing asserts, binomial mirror tolerance, YAML-configurable timeouts, faster weight tests, lower poll interval (v1.6.1 = further timeout/flake fixes only) | Free flake reduction for both of our gated profiles. |
| Upstream deps: k8s 1.36, Go 1.26 | Aligned with #528/#529 — clean dep graph. |

## Plan

Single PR unless conformance triage (M3) grows; then split M3 out.

**M1 — move everything to 1.6.1 together.**
Go dep + `test/conformance` runner (self-replace module) + the
`GATEWAY_API_VERSION` CRD download in `conformance.yaml` (both jobs) →
v1.6.1. Talos-main CRDs upgraded out-of-band at deploy time (the
safe-upgrade VAP fixes in 1.6 make the in-place CRD upgrade clean).

**M2 — TCPRoute/UDPRoute to `v1`, with `v1alpha2` fallback.**
`common/crdcheck` already gates per-GVK: detect `v1` first, fall back to
`v1alpha2` (older clusters), warn on fallback. Touchpoints: l4route
reconciler (watch + List + scheme), edge reconciler (`v1alpha2.TCPRoute`
watch + list), RBAC unchanged (group-level). TLSRoute stays as-is.

**M3 — conformance re-run + triage.**
Re-run both gated profiles on kind. Expected work:
- `GatewayInvalidParametersRef`: set Gateway `Accepted=False/InvalidParameters`
  when `parametersRef` doesn't resolve to a live EdgeConfig (029 follow-up).
- `MeshHTTPRoute307Redirect` re-check under the fixed manifest.
- Skip-list ListenerSet/UDP tests via feature declarations (unclaimed).
Gates stay hard: GATEWAY-HTTP and MESH-HTTP Core must return to green
before merge.

**M4 (optional follow-up) — claim `SupportTCPRoute` at the edge** and add the
TCPRoute suite to the gated profile.

## Risks

- The conformance suite grew; M3 is the unbounded part (budget one
  `GatewayInvalidParametersRef` fix; anything else found is triaged on its
  merits).
- CRD upgrade on talos-main is in-place; do it outside a soak window and
  verify the l4route/gamma reconcilers pick the new served versions after an
  agent restart (crdcheck is setup-time).
- `v1alpha2` TCP/UDP objects on existing clusters remain served post-upgrade
  (deprecation window), so M2's fallback keeps mixed states working.

## Non-goals

UDP data path (parked), ListenerSet support, XBackend/experimental GEPs.
