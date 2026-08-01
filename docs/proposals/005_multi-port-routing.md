# Proposal: Multi-Port Routing per Pod

**Status:** Implemented — multi-port routing shipped (`endpoint.aether.io/ports`, per-port EDS, SNI demux); design validated by spike (Envoy 1.38.0/BoringSSL, 2026-06-13).
**Author:** Bruno Palermo
**Date:** 2026-06-13

## Problem Statement

A mesh pod today serves exactly **one** application port. The inbound listener
binds `pod_ip:15008`, terminates mTLS in a single filter chain, and forwards
all decrypted traffic to `127.0.0.1:<port>` (`endpoint.aether.io/port`, default
8080). A service is reachable only as `<service>.<mesh-domain>`, with no way to
express "port N of this service."

Real workloads expose multiple ports: an HTTP API on 8080 and a gRPC service on
9090, a metrics/debug port, or two containers in one pod each owning a port.
Because containers in a pod share the network namespace, `127.0.0.1:8080` and
`127.0.0.1:9090` are both reachable in the pod's netns regardless of which
container binds them — so **"multiple containers, different ports" and "one
container, multiple ports" are the same problem** to the data plane: route a
mesh request to the right loopback port inside one pod. The mesh just has no
selector for it on either end.

## Design Goals

1. Idiomatic client addressing: `FQDN:<port>` — what an app or gRPC stub
   already produces when it dials `payments.aether.internal:9090`.
2. Deterministic: the port in the authority is meaningful; a request for a port
   either reaches that exact port or fails cleanly — never silently lands on a
   different port, and never coalesces to the default.
3. **Safe rollout of new ports**: a client calling `:9090` must only ever land
   on pods that actually serve 9090, as a new version adding the port rolls
   out — never on an old pod lacking it.
4. Per-port protocol heterogeneity: one port may be gRPC/h2, another HTTP/1.1,
   another raw TCP.
5. Preserve every property of the current data plane: per-source mTLS, per-
   service SPIFFE SAN pinning, subset routing, locality failover, two-phase
   drain, demand-scoped distribution.
6. Zero change for single-port pods (the entire current fleet).

## Proposed Solution

Four pieces: an addressing grammar, per-port outbound clusters with **per-port
EDS**, an SNI-demuxed inbound, and per-endpoint port advertisement.

### Addressing: `FQDN:<port>`, port is the selector

```
payments.aether.internal           → default/primary port (back-compat)
payments.aether.internal:8080      → port 8080 (the default, addressed explicitly)
payments.aether.internal:9090      → port 9090
```

The `:port` is the actual numeric port the service exposes. This requires
turning **`strip_any_host_port` OFF** (it was added in the FQDN-only change to
ignore a habitual `:port`; the port is now a first-class selector). The cluster
name is the full authority (`payments.aether.internal:9090`), consistent with
today's authority-as-cluster-name, and ODCDS materializes it on demand exactly
as services are.

**Default-port vhost carries two domains** so the portless form and the
explicit-default form resolve to one cluster (no shadow duplicate):

```yaml
- name: payments.aether.internal
  domains: ["payments.aether.internal", "payments.aether.internal:8080"]  # default port
- name: payments.aether.internal:9090
  domains: ["payments.aether.internal:9090"]                              # non-default
```

The ODCDS resolver canonicalizes a cold `<svc>:<defaultPort>` to the default
cluster (it learns the default port — the existing `ServiceEndpoint.port` —
from the RPC-fill), so all three default spellings converge on one cluster.

**Catch-all + determinism.** With the port retained, the scoped
`*.<mesh-domain>` vhost no longer matches `host:port`. The catch-all becomes a
universal `*` vhost whose routes split on an `:authority` `safe_regex`:
mesh-shaped (`^[a-z0-9-]+\.<mesh-domain>(:[0-9]+)?$`) → `cluster_header:
:authority` (ODCDS); everything else → `direct_response: 404`. This keeps
instant foreign-authority 404s while letting cold mesh `host:port` warm.

### Per-port clusters with PER-PORT EDS (the rollout-safety mechanism)

Each port-cluster's endpoint set is the subset of the service's pods that
**advertise that port** — not a shared service EDS. This is what makes goal 3
hold: as a v2 adding `:9090` rolls out, pods join the `:9090` EDS only as they
become Ready, so a `:9090` caller never lands on a v1 pod (→ no SNI mismatch /
reset); before the first v2 pod, `:9090` has zero endpoints → clean 503/reject.
Removal is symmetric (a pod dropping a port leaves that EDS as it drains).

This makes the port **per-endpoint** data:

```proto
ServiceEndpoint += repeated uint32 ports;  // ports this pod serves; port (existing) = default/primary
```

The CNI plugin reads `endpoint.aether.io/ports: "8080,9090"` (default port stays
`endpoint.aether.io/port`; omitted ⇒ `{port}`) and registers it; the registrar
carries it; the consuming agent filters the service's endpoints by port when
building each port-cluster's load assignment. Port existence ("does payments
expose 9090?") = "is 9090 in the union of payments' endpoints' port sets" — the
agent rejects an unadvertised port like a ghost service, riding the existing
endpoint pipeline (no new catalog dimension).

Port-clusters differ from the default cluster only in name, the SNI they
present, and their EDS filter — SAN pinning, subset selectors, outlier
detection, per-source transport-socket matcher are identical.

### SNI-demuxed inbound

The pod inbound listener at `:15008` demuxes by SNI = the port number:

```yaml
listener_filters: [ envoy.filters.listener.tls_inspector ]   # added explicitly
filter_chains:
- filter_chain_match: { server_names: ["9090"] }  → HCM(h2)  → app_<pod>_9090 → 127.0.0.1:9090
- filter_chain_match: { server_names: ["8080"] }  → HCM(AUTO) → app_<pod>_8080 → 127.0.0.1:8080
- # default chain (no server_names), mTLS: default port — back-compat / no-SNI clients
```

Every outbound port-cluster sets `UpstreamTlsContext.sni = "<port>"`
explicitly (NOT `auto_sni`). Each chain runs its own codec, so ports can differ
in protocol. SNI is routing-only — identity stays the SPIFFE SAN, so SAN
pinning, XFCC, and per-source cert selection are unchanged (cert by netns, SNI
by port, both on the same `UpstreamTlsContext`, orthogonal).

## Spike Findings (Envoy 1.38.0 / BoringSSL, 2026-06-13)

A standalone static-config spike validated all four must-verify items on the
exact Envoy build the mesh runs. Configs exercised `direct_response` vhosts
(authority/port matrix) and SNI filter-chain selection with TLS termination.

**1. `strip_any_host_port: false` + two-domain default vhost — CONFIRMED.**

| `:authority` | matched | result |
|---|---|---|
| `payments.aether.internal` | default vhost (domain 1) | `DEFAULT` |
| `payments.aether.internal:8080` | default vhost (domain 2) | `DEFAULT` (same cluster) |
| `payments.aether.internal:9090` | `:9090` vhost | `PORT9090` (no leak into default) |
| `payments.aether.internal:7777` | catch-all regex route | `ODCDS` |
| `unknownsvc.aether.internal[:9090]` | catch-all regex route | `ODCDS` |
| `evil.com`, `evil.com:9090` | catch-all 404 route | **instant 404** |

Exact-domain precedence is strict: a ported authority never matched the
portless default vhost. Determinism preserved end to end.

**2. `safe_regex` on `:authority` + port retention — CONFIRMED.** The regex
route matched mesh-shaped authorities (ported and portless) and `%REQ(:authority)%`
preserved the port (`x-seen-authority: newsvc.aether.internal:9090`) — so
`cluster_header: :authority` delivers the full `host:port` to ODCDS.

**3. SNI filter-chain demux — CONFIRMED, with two nuances:**
- Numeric `server_names` match exactly: `openssl -servername 9090 → SNI-9090`,
  `8080 → SNI-8080`, `7777 → default chain`. No-SNI → default chain.
- **`tls_inspector` must be added explicitly.** Auto-injection did not fire in
  the spike (with a mixed plaintext/TLS chain set); the design adds it
  explicitly and the real inbound is all-mTLS anyway.
- **Use explicit `sni:`, not `auto_sni`, and beware numeric-SNI tooling.** curl
  silently suppresses SNI for an all-numeric name, so `curl --resolve 9090:…`
  sent no SNI and hit the default chain. Irrelevant to the data plane (the
  sending Envoy sets `sni` from config, proven to work via openssl), but it
  means: (a) never rely on `auto_sni` deriving a usable value from a numeric
  authority; (b) debugging must use `openssl -servername`, not curl. A
  DNS-shaped SNI (e.g. `p-9090` or the full FQDN) would sidestep the tooling
  quirk if we ever want curl-debuggability — deferred; bare port keeps inbound
  chains service-agnostic.

**4. gRPC `:authority` propagation — CONFIRMED for the routing path** by item 2
(Envoy routes on the full `host:port` authority and `cluster_header` captures
it). gRPC clients set `:authority` to the dial target (host:port) natively; the
capture path does not alter the HTTP `:authority`.

## What Stays Exactly As-Is

- Single-port pods: no `ports` annotation ⇒ `{port}`, one default chain, one
  cluster, `FQDN` address — byte-identical to today.
- mTLS identity, SAN pinning, XFCC, per-source cert selection, two-phase drain,
  outlier detection, locality priorities, subset selectors (compose with
  `:port` — a subset of a specific port works), demand scoping (port-clusters
  warm on demand; per-port EDS is a filtered projection, not new mesh-wide
  state).

## Failure Modes

- **Unknown port** (`:7777` not advertised): catalog/port reject → clean 503,
  no dependency-set pollution. (Without the per-endpoint check it would be an
  SNI miss → connection reset; the check upgrades it to a 503.)
- **New port mid-rollout**: per-port EDS grows with advertising pods; a caller
  never reaches a pod lacking the port. Zero endpoints before the first
  advertising pod → 503, not reset.
- **Foreign authority**: instant 404 (catch-all 404 route) — determinism kept.
- **Pre-upgrade client (no SNI)**: default chain (back-compat) → default port.

## Implementation Phases

### Phase 1 — Per-endpoint ports
`ServiceEndpoint.ports`; CNI reads `endpoint.aether.io/ports`; registrar carries
it; agent exposes the per-service advertised-port union. No routing change yet.

### Phase 2 — SNI inbound demux
Per-port inbound filter chains (explicit `tls_inspector`), `app_<pod>_<port>`
clusters; default chain preserved. Single-port pods emit the current shape.

### Phase 3 — Per-port outbound clusters
`strip_any_host_port` off; two-domain default vhost; per-port clusters with
per-port EDS + explicit `sni`; universal catch-all with the `safe_regex`
authority split; ODCDS resolver parses `<svc>:<port>`, canonicalizes the
default, rejects unadvertised ports.

### Phase 4 — e2e
Multi-port workload (svc with 8080+9090); validate `FQDN`, `FQDN:8080`,
`FQDN:9090` routing; new-port rollout safety (add :9091 to a new version, drive
`:9091` under churn, expect zero resets/landings on old pods); foreign-404;
subset+port composition; protocol heterogeneity (h2 + HTTP on one pod).

## Open Questions

1. **SNI value**: bare port (service-agnostic inbound, curl-undebuggable) vs.
   DNS-shaped (`p-<port>` or FQDN — curl-debuggable, slightly more config). Lean
   bare port; revisit if debuggability bites.
2. **Default-cluster/`:defaultPort` dedup**: handled by the two-domain vhost +
   ODCDS canonicalization; confirm no stray `:8080`-named cluster is ever minted
   under concurrent cold requests.
3. **Per-port subset vocabulary / locality**: inherited from the service today;
   confirm no port needs its own subset keys (unlikely).
