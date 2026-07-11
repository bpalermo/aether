# Proposal: Multi-cluster pod-to-pod via a per-node waypoint (non-routable pod IPs)

**Status:** Design — 2026-06-24
**Relates:** proposal 018 (Gateway API/GAMMA — the *connectivity modes* this refines),
proposal 006 (origin-partitioned per-region registry — the cross-cluster plane),
proposal 003 (edge proxy — the SNI-passthrough primitive this reuses),
proposal 004 (demand-scoped distribution); [[project_transport_migration]] (the HBONE
node tunnel this re-introduces, scoped to cross-cluster), [[project_multicluster_registry]],
[[project_gateway_api_gamma]].

## Summary

Proposal 018 named two cross-cluster connectivity modes: **flat network** (dial the
remote `pod_ip:15008` directly — needs routable pod IPs) and a **central east-west
gateway** (fallback — dial a per-cluster gateway that forwards). This proposal adds a
**third mode for the common case where pod IPs are *not* routable across clusters but
*node* IPs are** (e.g. Flannel: per-cluster pod CIDRs with no inter-cluster routing,
nodes on a shared/routed underlay).

The insight: aether's proxy is already a **host-network DaemonSet**, so **every node is
a waypoint**. A cross-cluster endpoint is dialed at the **destination pod's node IP**,
and that node's proxy SNI-forwards the raw mTLS to the local pod (`pod_ip:15008` *is*
routable within its own cluster). No separate gateway deployment, no LoadBalancer
chokepoint, and **intra-cluster traffic stays direct pod-to-pod, unchanged**. This is
the ambient / ztunnel-per-node pattern, and a deliberately scoped re-introduction of the
HBONE node tunnel removed in [[project_transport_migration]] — the trade-off that argued
against a node hop *intra*-cluster flips when direct dial is *impossible*.

## Motivation

Aether's data plane is **direct pod-to-pod**: after the HBONE node tunnel was removed
([[project_transport_migration]]), the source proxy dials the destination
`pod_ip:15008` with per-source SPIRE mTLS. That assumes the destination pod IP is
routable from the source.

- **Flannel breaks 018's flat-network mode.** Each cluster has its own pod CIDR with no
  inter-cluster routing, so a cluster-A proxy cannot open a socket to a cluster-B pod
  IP. The registry hands out `pod_ip:15008`; the dial fails. Flat network is out
  unless the network is flattened (non-overlapping CIDRs + a routable underlay).
- **018's central east-west gateway works but adds a component + a chokepoint.** A
  per-cluster LB-fronted gateway is one more thing to build, run, scale, and route
  every cross-cluster byte through.
- **Nodes are routable.** In the common topology, node IPs sit on a shared/routed
  network even when pod CIDRs don't. And aether **already runs a proxy on every node**.
  So the per-node proxy is a free, horizontally-scaled waypoint — no new deployment.

## Design — the per-node waypoint

```
intra-cluster (unchanged):
  src pod proxy ──SPIRE mTLS──▶ destPodIP:15008            (direct dial)

cross-cluster (new):
  src pod proxy ──mTLS──▶ destNodeIP:<tunnel-port> ──SNI passthrough──▶ destPodIP:15008
                           (dest node's host-network proxy; reads SNI,
                            L4-forwards raw bytes — never decrypts)
```

- **Intra-cluster** stays `pod_ip:15008` (one identity-preserving hop, current code).
- **Cross-cluster** dials the **destination node IP** (routable) at a tunnel port; the
  destination node's proxy forwards to the local pod (`pod_ip:15008`, routable within
  its own cluster).
- **mTLS is end-to-end** (source pod cert ↔ destination pod cert). The node proxy is a
  pure L4 SNI router — `tls_inspector` reads the cleartext ClientHello SNI, `tcp_proxy`
  forwards the raw stream. It never terminates TLS, never sees plaintext, never holds
  the destination identity. This is exactly the **TLSRoute-passthrough** primitive
  already shipped and validated (proposal 018 Phase 3b edge L4).

### Split-horizon EDS (the one real change)

The registrar/agent chooses the dial target per endpoint, by origin:

| Endpoint's cluster | Dial target (in EDS) | SNI |
|---|---|---|
| == local cluster | `pod_ip:15008` | port (proposal 005 multi-port) — unchanged |
| != local cluster | `destNodeIP:<tunnel-port>` | structured: pod target **+** port |

Proposal 006 already replicates cross-cluster endpoints (origin-partitioned per-region
etcd); this is a **rewrite at snapshot time** keyed on `endpoint.cluster == myCluster?`.
The only new registry datum is the **destination node IP** — `ServiceEndpoint` already
carries `NodeName` + locality (region/zone); add `NodeIP` (or resolve it at registration
and store it) so a consumer in another cluster has a routable target.

### The host-bound tunnel listener

The proxy gains a **host-netns** listener on `node_ip:<tunnel-port>` (it is host-network,
so it can bind the node address). It runs `tls_inspector` → `tcp_proxy` and forwards to
the target local pod's `:15008`. No TLS termination; the source's mTLS passes through.

### Structured SNI

A single connection's SNI must satisfy **two** demuxes — the node (which local pod?) and
the pod's own inbound (which served port? — proposal 005). L4 passthrough cannot rewrite
the SNI without terminating (and breaking end-to-end mTLS), so both signals are encoded
in one structured SNI, e.g. `<pod-token>.<port>.<meshDomain>`:

- the **node** routes on `<pod-token>` (→ the local pod's `:15008`),
- the **pod inbound** demuxes on `<port>` (its existing per-port chains).

`<pod-token>` may address a specific pod (per-pod targeting) or the service (let the node
load-balance locally — see below). aether owns the SNI format end to end.

### SPIRE trust across clusters

End-to-end mTLS requires the source to trust the destination's SVID issuer: **one shared
trust domain** (`aether.internal` issued by a shared upstream CA) **or SPIRE
federation** (per-cluster trust domains + bundle exchange). Same requirement as both of
018's modes; unchanged by the waypoint.

## Why aether's identity model keeps this simple

aether pins the **service** SPIFFE ID (per-ServiceAccount SAN), not a per-pod identity.
So the destination node may load-balance the SNI to **any** local pod of the service and
the source's `match_typed_subject_alt_names` still passes (every `sa/X` pod presents
`spiffe://…/sa/X`). The cross-cluster remote side can therefore be **service-level**
(node re-LBs locally) while end-to-end mTLS holds — no per-pod SNI gymnastics required.
Per-pod targeting remains available via `<pod-token>` when a route needs it.

## Relationship to 018's modes and to eBPF / ClusterMesh

- **Refines 018's "east-west gateway (fallback)."** The gateway *is* the per-node proxy:
  no separate component, no LB chokepoint, every node a waypoint, horizontal scale,
  better failure isolation. The registry rewrite (remote endpoint → waypoint address +
  routing SNI) is the same shape 018 already anticipated — the waypoint address is just
  the *node* rather than a central gateway.
- **vs 018 flat network (default).** The waypoint needs **no routable pod IPs and no
  global CIDR coordination** — it costs one node hop instead.
- **vs eBPF / Cilium ClusterMesh.** That is the *flat-network* mode realized in the
  **CNI datapath**: eBPF node-to-node encap (non-overlapping CIDRs + VXLAN/Geneve) makes
  remote pod IPs reachable, after which aether's direct-dial mTLS works **with zero mesh
  changes**. Two framings matter: (1) **eBPF does not do the mTLS** — SPIRE mTLS is an
  L7/userspace handshake in Envoy; eBPF only solves *reachability*, identity stays in
  the proxy, so the two compose. (2) **inter-cluster pod reachability is the CNI's job,
  not the mesh's** — building an eBPF datapath + cross-cluster CIDR/endpoint sync inside
  aether reinvents Cilium. The sane "eBPF route" is to **adopt Cilium ClusterMesh
  (replace Flannel)**, or Submariner for a lighter single-gateway-node encap that keeps
  the CNI. Then this proposal is unnecessary; pick it only if you want to stay on
  Flannel / keep connectivity self-contained in the mesh.

| | Flat network (018) | **Per-node waypoint (this)** | Central E/W gw (018) | eBPF / ClusterMesh |
|---|---|---|---|---|
| Layer | network | mesh (Envoy) | mesh (Envoy) | CNI datapath |
| Pod IPs routable? | required | **not required** | not required | made routable (encap) |
| Keep Flannel? | maybe | **yes** | yes | no (→ Cilium) |
| New component | — | **none (DaemonSet)** | gateway + LB | CNI swap |
| Cross-cluster hops | 0 | **1 (node)** | 1 (gateway) | 0 (encap) |
| Chokepoint | — | **none** | the gateway LB | — |

## Phasing

1. **Registry**: add the destination `NodeIP` to `ServiceEndpoint`; ensure proposal 006
   replicates it cross-cluster.
2. **Split-horizon EDS**: origin-keyed rewrite (local → `pod_ip:15008`; remote →
   `nodeIP:tunnel-port` + structured SNI) in the agent snapshot.
3. **Host-bound tunnel listener** + the per-target SNI-forward `tcp_proxy` clusters on
   the proxy.
4. **SPIRE federation / shared trust domain** across clusters.
5. **MCS wiring**: the clusterset-VIP EDS (proposal 006) resolves local pods directly +
   remote pods via the waypoint — the split-horizon logic lives behind the VIP.

## Verification

- Two Flannel clusters, shared trust domain. Source pod in A dials `svc-B.<meshDomain>`;
  the registry resolves a B endpoint to **B's node IP** + routing SNI; the connection
  lands on B's node proxy and is forwarded to a B pod; the B app sees XFCC =
  `spiffe://…/sa/<src>`; mTLS is end-to-end (B pod cert presented).
- Intra-cluster traffic is **unchanged** — direct `pod_ip:15008`, no node hop (assert no
  waypoint stat increments for same-cluster dials).
- Kill a destination node → its endpoints drop from EDS (registry health); traffic
  re-LBs to other nodes' pods.

## Tensions / non-goals / open questions

- **Re-introduces a node hop we removed intra-cluster.** Scoped to cross-cluster only,
  where direct dial is impossible; intra-cluster stays direct. The HBONE head-of-line
  concern doesn't apply — the waypoint is per-connection L4 `tcp_proxy` forwarding, not
  an H2-multiplexed tunnel.
- **Per-pod vs service-level remote targeting.** Service-level (node re-LBs) is simplest
  and works with per-SA identity; per-pod via `<pod-token>` is available but couples the
  consumer to remote pod identities. Default to service-level. *Open.*
- **Underlay MTU** if nodes themselves sit on a tunneled fabric.
- **Not flattening the network** — that is the eBPF/CNI path above, deliberately out of
  scope for the mesh.
- **Egress audit.** Unlike a central gateway, the per-node waypoint has no single
  cross-cluster egress point; identity (SPIRE) is the boundary, not a chokepoint — same
  trade-off 018 notes for flat network.

## Implementation plan (2026-07-11)

Grounded in the current data-plane code and an Envoy-source spike. Design decisions:
**shared trust domain first** (federation deferred); **service-level remote targeting**
(the node re-LBs locally — works because identity is pinned per-ServiceAccount).

### Resolved mechanism: per-endpoint SNI in one cluster (spike outcome)

The hard question was how to make a service cluster's SNI differ per endpoint — local
endpoints need `SNI = <port>` (the existing multi-port demux), remote/waypoint endpoints
need a structured SNI that survives the extra node hop — **without losing per-source
client-cert selection** (the source pod presents its own SVID; the destination logs the
real caller). Envoy source (`transport_socket_match_impl.cc`, `TransportSocketMatchingData`)
confirms the unified `Cluster.transport_socket_matcher` sees the **chosen endpoint's
metadata** (`envoy.matching.inputs.endpoint_metadata`, already compiled into the aether
proxy — `extensions_build_config.bzl`) *alongside* the filter-state input we use today.

So the split-horizon lives in **one** service cluster:

- EDS holds local endpoints (`pod_ip:15008`) and remote endpoints (`node_ip:<tunnel-port>`
  with `LbEndpoint.metadata{envoy.lb: {waypoint: "true"}}`).
- The transport-socket matcher becomes a **two-level tree**: branch first on the endpoint
  `waypoint` label → {local socket, waypoint socket}, then on the source netns → the
  source pod's cert. The two sockets differ **only** in SNI (cert / SAN-pin / ALPN are
  identical — mTLS still terminates end-to-end at the destination *pod*, never the node).
- Local endpoints keep `SNI = <port>`; waypoint endpoints get `SNI = <port>.<svc>.<ns>.<meshDomain>`.
- Local-vs-remote preference reuses `EndpointPriority`, extended with a **same-cluster-first
  tier** so the local cluster wins even when the remote cluster shares the locality
  (today priority is locality-only).

*(Fallback if the two-level matcher ever misbehaves: an `envoy.clusters.aggregate` cluster
over a local + a waypoint sub-cluster, each with a uniform SNI and the existing per-source
matcher — confirmed viable, but more moving parts.)*

### Structured SNI + the destination double-demux

One wire SNI must satisfy two demuxes and L4 passthrough cannot rewrite it:

- **SNI = `<port>.<svc>.<ns>.<meshDomain>`.**
- The **host-netns tunnel listener** (`node_ip:<tunnel-port>`, `tls_inspector → tcp_proxy`,
  reusing the 018 Phase 3b / edge TLSRoute-passthrough primitive) matches per local service
  on `server_names: ["*.<svc>.<ns>.<meshDomain>"]` → forwards to that service's **local**
  pods at `:15008`. NB Envoy's leftmost `*` is a **suffix** match, not single-label; this is
  safe because aether owns the SNI and each service has a unique `.<svc>.<ns>.<meshDomain>`
  suffix.
- The **pod inbound** per-port chains add the **exact** server name
  `<port>.<svc>.<ns>.<meshDomain>` alongside the existing bare `<port>`, so the forwarded
  (un-rewritten) SNI still selects the right loopback port (exact > wildcard precedence).

### Phased PRs

1. **Registry `node_ip`** *(this PR)* — re-add `node_ip` to
   `ServiceEndpoint.KubernetesMetadata` at **field 5** (field 4, the old HBONE target,
   stays reserved — do not reuse the number). Populate from the node's `InternalIP` on
   every registration path: the CNI server (`queryNodeMetadata` → all
   `NewServiceEndpointFromCNIPod` call sites) and the Kubernetes backend
   (`podToEndpoint`). Empty when unknown / for non-Kubernetes backends.
2. **Split-horizon EDS** — in the snapshot build, key on `endpoint.cluster_name != ownCluster`:
   local → `pod_ip:15008` (unchanged); remote → `node_ip:<tunnel-port>` + `waypoint`
   metadata. Add the waypoint transport socket + the two-level matcher; extend
   `EndpointPriority` with the same-cluster tier. Gate behind `--east-west-waypoint`
   (default off). Golden-snapshot tests.
3. **Host tunnel listener + pod-inbound server-name** — the `node_ip:<tunnel-port>`
   passthrough listener with per-exported-service `*.<svc>.<ns>.<meshDomain>` chains →
   `ew_ingress_<svc>_<ns>` (local pods at `:15008`, raw passthrough), plus the exact
   structured server-name on the pod-inbound per-port chains.
4. **MCS/VIP + demand-scope** — fold the split-horizon behind the clusterset VIP; scope
   waypoint clusters to the node's dependency set (proposal 004).
5. **E2E** — extend the 026 two-kind harness with **non-routable pod CIDRs but routable
   node IPs**: assert A→svc-B lands on B's node proxy → a B pod, B's XFCC = the source
   SPIFFE ID, mTLS end-to-end (B pod cert); intra-cluster unchanged (no waypoint stats);
   kill a B node → EDS drops + re-LB.

### Prerequisites / open

- **Cross-cluster endpoint visibility** (MCS `ServiceImport` and/or the 006 replicator,
  currently deferred) must deliver remote endpoints — carrying `node_ip` + `cluster_name` —
  into the consumer's registry view before Phase 2 has anything to rewrite.
- **Shared trust domain** across clusters (one upstream CA) so the source validates the
  destination pod's SVID; SPIRE federation is a later proposal.
- Re-verify the matcher-input `type_url`s at the pinned Envoy rev before committing Phase 2
  config (framework + filter-state input already ship on 1.38.x; `endpoint_metadata` input
  confirmed compiled).

### Deferred possibility: a uniform-waypoint mode (noted, not planned)

The Phase-2 gate is a boolean (`--east-west-waypoint`, default off) that routes **only
cross-cluster** endpoints through the waypoint; intra-cluster stays direct pod-to-pod. It
is worth recording that the same mechanism could be widened to route **all cross-node**
traffic through the dest-node waypoint, e.g. by turning the gate into a mode —
`off | cross-cluster | always`. In Design A that is a one-line change to the split-horizon
predicate (`endpoint.cluster != own` becomes `endpoint.cluster != own || (mode==always &&
endpoint.node != myNode)`) — same clusters, same matcher, same tests; `always` merely widens
which endpoints get rewritten. Same-node traffic is a local forward regardless, so even
`always` is not a single uniform path — it just moves the direct/waypoint boundary from
"same-cluster" to "same-node."

This is **explicitly out of the phased plan** and carries no commitment. It would only be
justified for a deployment whose CNI can't route pod IPs even intra-cluster, or that wants a
uniform per-node policy-enforcement point — and aether already enforces authz/RBAC/telemetry
at the **pod inbound**, so that motivation is weak today. The extra hop is pure overhead on a
routable intra-cluster network, which is why direct pod-to-pod is and remains the default
(the deliberate outcome of [[project_transport_migration]]).

Note also that this "uniform addressing" idea is distinct from **node-to-node multiplexing**
(HBONE-style: the node terminates and re-originates, streams share a node↔node H2 tunnel,
O(nodes²) connections). Multiplexing would reverse the transport migration, reintroduce
head-of-line blocking, and put the node proxy in the identity/plaintext path — a separate,
larger question that must not ride on this proposal.
