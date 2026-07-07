# Proposal: EdgeConfig — edge best-practices, per-instance, native Gateway API

**Status:** Accepted — 2026-07-06
**Relates:** proposal 018 (Gateway API edge), 015 (MeshConfig — the CRD + proto.Merge
default/override precedent), 028 (geoip — folds onto this config), the Envoy edge
best-practices doc (envoyproxy.io/docs/.../best_practices/edge).

## Summary

An `EdgeConfig` CRD (`config.aether.io/v1`) carrying the Envoy edge-hardening knobs +
HTTP/3, resolved per **edge instance (Gateway)** through the NATIVE Gateway API
parameters chain: `GatewayClass.spec.parametersRef` = the fleet default (chart-shipped),
`Gateway.spec.infrastructure.parametersRef` = the per-instance override, merged
`effective = proto.Merge(classDefault, gatewayOverride)` (override wins per field;
MeshConfig's #267 pattern). Every setting has a best-practice compiled default, so an
empty/absent config still yields a hardened edge. Both parametersRef fields exist in
gateway-api v1.5.1; QUIC + Http3ProtocolOptions + the quic transport socket are already
in the proxy build — no image work.

## Why a CRD (not chart values)

The requirement is chart-provided DEFAULTS with per-INSTANCE overrides. Chart values are
global to a release; multiple edge instances (multiple Gateways, possibly multiple edge
Deployments) each need independent, runtime tuning. A CRD referenced by the native
parametersRef chain is the Gateway-API-idiomatic answer and reuses aether's shipped
MeshConfig/HTTPFilter machinery (opaque edition-2024 proto + jsonshim + deepcopy +
proto.Merge).

## EdgeConfigSpec (best-practice defaults)

```yaml
useRemoteAddress: true                  # edge trusts the connection source, manages XFF
xffNumTrustedHops: 0                     # additional trusted proxies in front
headersWithUnderscoresAction: REJECT_REQUEST
http2:
  maxConcurrentStreams: 100
  initialStreamWindowSize: 65536         # 64 KiB
  initialConnectionWindowSize: 1048576   # 1 MiB
streamIdleTimeout: 300s
requestTimeout: 300s                     # 0 = disabled
idleTimeout: 3600s
perConnectionBufferLimitBytes: 32768     # 32 KiB (listener + edge cluster)
http3:
  enabled: false                         # QUIC/HTTP3 UDP listener on the HTTPS port
geoip: { ... }                           # proposal 028, folded here
```
The current `edge.xffNumTrustedHops`/`edge.geoip` chart values become deprecated aliases
that populate the default EdgeConfig for one release.

## Resolution (native)

- The chart ships a default `EdgeConfig` and sets it as the aether GatewayClass's
  `spec.parametersRef` (group `config.aether.io`, kind `EdgeConfig`).
- A Gateway overrides per instance via `spec.infrastructure.parametersRef` → its own
  `EdgeConfig` (same-namespace `LocalParametersReference`).
- The edge reconciler resolves, per Gateway: compiled defaults ⊕ class default ⊕
  gateway override (`proto.Merge`, later wins). The effective config configures that
  Gateway's edge listeners.

## `use_remote_address` — a latent fix

The edge currently sets `xff_num_trusted_hops` but NOT `use_remote_address`, so it
trusts XFF verbatim — forgeable by an internet client, and the 028 geoip client-IP
resolution depends on it. Default `useRemoteAddress: true` fixes this.

## HTTP/3

When `http3.enabled`, the edge adds a **UDP/QUIC listener on the HTTPS port** (same SDS
certs via the quic downstream transport socket) with `Http3ProtocolOptions`, and
advertises `alt-svc: h3=":<port>"` from the TCP HTTPS listener so clients upgrade.
MetalLB/Service must expose the UDP port alongside TCP (chart wires both). h3 downstream
limits share the http2/quic caps.

## Milestones

- **M1 — CRD + fixed hardening:** EdgeConfig proto/jsonshim/deepcopy/chart-CRD (mirror
  HTTPFilter); apply useRemoteAddress + headersWithUnderscoresAction + downstream h2 caps
  + edge-cluster buffer from the compiled defaults (no resolution yet). Ships the
  use_remote_address fix. Unit + `envoy --mode validate`.
- **M2 — native resolution:** GatewayClass.parametersRef default + Gateway
  infrastructure.parametersRef override via proto.Merge; edge reconciler resolves
  effective config per Gateway; webhook validation; chart ships the default EdgeConfig +
  wires the GatewayClass. Fold in the timeouts.
- **M3 — HTTP/3:** QUIC UDP listener on the HTTPS port + alt-svc + Service UDP port,
  gated on `http3.enabled`. `envoy --mode validate` the QUIC listener.
- **M4 — e2e (talos):** hardening (use_remote_address → real client IP, request_timeout
  408, underscore reject), a per-Gateway override taking effect (one Gateway
  requestTimeout=1s → 408 while the default doesn't), HTTP/3 (curl --http3), geoip
  composition; Gateway API conformance re-run. **e2e is the exit gate.**

## Non-goals (v1)

Node-proxy internal HCM tuning (mesh-internal); per-route overrides of these; QUIC for
east-west; annotation-based binding (native parametersRef only).
