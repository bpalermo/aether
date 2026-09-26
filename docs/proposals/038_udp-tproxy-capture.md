# Proposal 038: TPROXY Capture for UDP, and East-West QUIC on Top of It

**Status:** Accepted; **revised 2026-09-26** to fold east-west QUIC in as a
requirement rather than a deferred question. All four premises verified by
experiment; Phase 0 settled on a node 2026-09-25.
**Superseded in part (2026-09-26):** Phases 1, 2 and 5 below were replaced by the
*TPROXY for both transports* plan and shipped as one breaking change with no
mode flag, no `redirect` fallback and no REDIRECT kept for a release — #944
(Phase 0b spike: the full ruleset incl. `tproxy` proven on a node), #945
(constants), #946 (transparent TCP listener, landed *before* the CNI change on
purpose), #947 (CNI mark-and-divert for TCP **and** UDP, the per-VIP UDP
listener on 18082, the root-only kernel gate in CI), #948 (e2e selection
assertion), and the chart bump + soak in the PR that carries this note. The
decision that made Phase 5's "written analysis" moot: the `0xae7e` passthrough
mark never matched in the pod netns to begin with (the proxy is hostNetwork), so
there is nothing for the divert mark to compose with. Phase 3 (e2e) and Phase 4
(QUIC) stand as written; the shared-port table and D1/D2 are unchanged.
**Author:** Bruno Palermo
**Date:** 2026-09-23 (revised 2026-09-26)
**History:** grew out of #873, whose stated fix direction turned out to be wrong
on both halves — see *Why #873's fix cannot work*. The premise that this is
fixable in the control plane was tested and refuted before this proposal was
written, which is why the answer is a CNI change rather than a matcher change.
**Related:** #916 (this proposal's issue), #873 (UDPRoute weights — the half that
WAS fixable, shipped in #914), #882 (`udp_unsupported` counter, seeded in #905),
#868/#912 (L4 route e2e, UDPRoute leg asserts delivery not selection *because* of
this limit), proposals 018 Phase 3b (L4 routes), 022 (redirect-all), 029 (HTTP/3
at the edge), 031 (transport decision: per-source mTLS H2, no HBONE/CONNECT).

## Problem Statement

A pod may have at most **one** UDP service routed to it. UDPRoute cannot express
a traffic split, and multiple backend services collapse onto whichever one wins a
tie-break. After #914 that tie-break is principled — heaviest live backend, ties
broken by cluster name, `weight: 0` honoured as drain — and the discarded shapes
are reported through `UnsupportedUDPRouteShapes` and
`aether.agent.l4route.udp_unsupported` rather than silently dropped. But the limit
itself is unchanged.

The cause is not in the control plane. It is that **the capture path destroys the
information needed to tell one service from another**.

For TCP, the CNI installs an nftables `REDIRECT` in the pod netns and Envoy's
`use_original_dst` recovers the pre-DNAT destination. That is what makes a
per-service filter chain possible. For UDP the same trick is unavailable:

- a datagram listener's only addressing cmsg is `IP_PKTINFO`, so Envoy reads the
  **post-DNAT** header and the ClusterIP is already gone;
- Envoy never reads `IP_RECVORIGDSTADDR`/`IP_ORIGDSTADDR` anywhere in the tree;
- `original_dst` is structurally TCP-only — `OriginalDstFilter` implements
  `Network::ListenerFilter`, not `UdpListenerFilter`, and LDS rejects it on a UDP
  listener (listing it by name is a hard reject; `use_original_dst: true` is
  silently ignored, which is worse);
- `DestinationPortInput` is not packet-derived at all: the port is stamped from
  the listening socket, so it is a constant.

Only five inputs exist for `Network::UdpMatchingData` — source and destination IP
and port, plus `network_namespace` (always `nullopt` here). None of them can
distinguish two services behind a REDIRECT.

**The same gap blocks east-west QUIC**, and that is no longer hypothetical. The
original text deferred QUIC on a security-model objection — its own TLS 1.3 was a
different model from the per-source mTLS H2 invariant proposal 031 settled on.
That objection was resolved upstream in the direction that *preserves* the
invariant: Envoy's QUIC handshake carries client certificates since
envoyproxy/envoy #47076 (2026-09-03), validated through the same `CertValidator`
as TCP, and both it and the upstream half (#45980) are in aether's pinned
snapshot. So this proposal now carries two requirements on one capture path:
plaintext per-service UDP, and an identity-bearing QUIC mesh transport. They
have different security properties and the design has to keep them apart.

## Requirements

- **R1 — per-service plaintext UDP.** More than one UDPRoute-backed service per
  node, selected by destination. The original goal.
- **R2 — east-west QUIC carrying per-source mTLS identity.** The 031 invariant
  unchanged: every hop authenticates the source workload by SPIFFE ID, the
  identity reaches XFCC, RBAC and the access log, and SDS/SPIRE deliver the
  certificates. QUIC is a transport under that invariant, not a replacement for
  it.
- **R3 — one port number per role, both transports.** Three roles, three
  numbers, each serving TCP and UDP on the same value, the way the edge's HTTP/3
  listener already shares `internalPort` with the TCP HTTPS listener:

  | role | number | TCP today | UDP under this proposal |
  |---|---|---|---|
  | L4 mesh spelling | 18082 (`ProxyL4OutboundPort`, renamed from `ProxyTCPOutboundPort`) | raw TCP floor (037) | **plaintext UDP services** |
  | per-pod inbound | 18008 (`defaultInboundPort`) | mTLS H2 | QUIC, mTLS |
  | east-west gateway | 18009 (`DefaultEastWestTunnelPort`) | mTLS SNI tunnel | QUIC, mTLS |

  A second number for any role means a second CNI rule, a second Service port, a
  second cross-cluster agreement and a second capture path. Pinned for 18009 by
  `TestEastWestPortIsSharedAcrossTransports`; Phase 1 extends the gate to 18082
  and Phase 4 to 18008. `ProxyTCPOutboundPort`'s name and the CNI comment "18082
  is a TCP spelling" both become stale under this rule; Phase 1 corrects the
  comment and may rename the constant to `ProxyL4OutboundPort`.
- **R4 — no resumption, no early data on an mTLS QUIC chain.** QUIC never
  re-verifies the client certificate on a resumed session. #47219 makes the safe
  default automatic but is not in any resolvable snapshot, so every mTLS QUIC
  filter chain sets `enable_resumption: false` and `enable_early_data: false`
  explicitly, gated in `//test/envoy_validate`. Otherwise an SVID revocation
  would not reach resumed sessions — the silent-identity-drift class of #829.
- **R5 — the two traffic classes are told apart on the capture path**, so a
  plaintext datagram can never be routed into a QUIC chain or vice versa.
- **R6 — TCP capture can follow later without a rewrite.** Phase 1 builds the
  rule set with a capture-*mode* parameter (`redirect` | `tproxy`), so moving TCP
  is a flag flip over a tested path when Phase 5 is reached.

### Why #873's fix cannot work

#873 proposed: *"Envoy's `udp_proxy` supports `matcher`-based route selection (and
weighted clusters via the matcher API) in current versions; the comment's premise
that it is unavailable predates the version now in use."* Both clauses are wrong,
verified against the pinned proxy build.

**There is no weighted route action, in any version.** `UdpProxyConfig.route_specifier`
is a oneof of `cluster` (deprecated) and `matcher`, and the matcher's terminal
action is `udp_proxy.v3.Route` — a message with exactly one field, `cluster`. The
action registry is keyed on udp_proxy's private `RouteActionContext` and holds
that single action; the HTTP router's weighted `route_list` lives in a disjoint
registry and is unreachable. `tcp_proxy.weighted_clusters` has no udp_proxy
counterpart.

**And switching to `matcher` buys nothing today**, because the input it would
match on carries no service identity behind a REDIRECT. That is the deeper
problem, and it is one layer below anything xDS can reach.

## Proposal

Capture UDP with **TPROXY instead of REDIRECT**, in the pod netns, preserving the
original destination port. Route plaintext services per service on
`destination_ip`; route the mesh transport by **destination port class**.

### Two traffic classes, one capture path

Everything TPROXY delivers arrives with its header intact, so the destination
port is real. That is the discriminator (R5):

| destination port | class | listener | security |
|---|---|---|---|
| 18008 (inbound) or 18009 (east-west gw) | **mesh transport** | QUIC listener with `require_client_certificate`, same `validation_context` as the TCP inbound | mTLS, identity-bearing |
| 18082 (L4 mesh spelling) | **plaintext service** | ONE `udp_proxy` listener, `matcher` on `DestinationIPInput` → per-service `udp:` cluster, which forwards to the backend's application port | plaintext, no identity, as today |
| anything else | not captured | — | — |

The classes cannot bleed into each other because they are separated at the
port, before any matcher runs: a datagram for 18008 is never offered to the
plaintext `udp_proxy`, and a datagram for 18082 is never offered to the QUIC
transport socket. All three are constants. The CNI needs no knowledge of which
application ports UDP services bind — that is the `udp:` cluster's business,
exactly as `tcp:` clusters already reach a raw-TCP backend's application port
from the shared `:18082` spelling.

This is the same shape as the TCP floor under 037, transport for transport: a
client dials `<svc>.<ns>.<mesh-domain>:18082`, the CNI captures that one port,
the capture listener matches the destination IP to a service, and the service's
cluster carries the application port. The per-port `PORT_PROTOCOL_UDP` the
registry gained in #933/#934 is what lets `UDPLoadAssignment` know which
application port to rewrite to; it is not consulted by the CNI.

TPROXY does not rewrite the packet. It steers delivery to a local socket while
leaving the IP header intact, so `IP_PKTINFO`'s `ipi_addr` — which Linux fills
from `ip_hdr(skb)->daddr`, and which is the field Envoy actually reads — still
carries the ClusterIP. Every service has a distinct ClusterIP, so that alone is
sufficient to tell them apart.

### The port is not a free variable

The obvious design — TPROXY everything to one well-known listener port — is
wrong, and wrong in a way that would not show up until production.

Envoy's `sendmsg` sets only `ipi_spec_dst` from the per-packet local address.
**`self_ip->port()` is never read.** A reply's source port is therefore always
the socket's bound port. A connected UDP client filters on `(saddr, sport)` and
drops anything else — so a port-rewriting TPROXY yields *matching that works and
traffic that silently fails one way*.

So the rule must preserve the original destination port, and the listener must
bind it. Under R3 there is exactly one dialed port for plaintext UDP — the L4
mesh spelling, 18082 — so the shape is **one transparent listener on UDP:18082**,
with `destination_ip` disambiguating every service behind it. Per-service fan-out
lives entirely in the matcher; the listener count does not scale with services
or with ports. (The 2026-09-23 draft said "one listener per distinct UDP service
port" and reasoned that a mesh's distinct UDP port set is small; folding the
shared-port rule in makes that set exactly one, which is smaller still, and it
retires the question of how the CNI would learn the set at all.)

### What the spike established

Run 2026-09-23 in a network namespace, no aether code. Same Envoy, same
`udp_proxy` matcher (`DestinationIPInput` exact-match on a fake ClusterIP → cluster
A, `on_no_match` → cluster B). Only the nftables rule differed:

| capture | matcher arm | reply to a **connected** socket |
|---|---|---|
| **TPROXY** (mark + `ip route local`) | **A** — saw the ClusterIP | delivered, source `<ClusterIP>:<service port>` |
| REDIRECT (status quo) | **B** — `on_no_match` | delivered, via **conntrack un-DNAT** |
| TPROXY, dport preserved, listener on another port | neither (0/0) | **nothing delivered** |

Row 2 is the control that makes row 1 mean something: arm B is reachable, so arm
A was not a matcher that always picks its first branch. Row 3 is what forces
listener-per-port.

Row 2 also explains why this was never noticed. **REDIRECT produces working
traffic with lost routing information**, because conntrack repairs the reply.
Nothing ever failed visibly; the mesh simply routed every datagram to one service.

`transparent: true` genuinely applies to a datagram socket — Envoy's option block
sits outside the `if (socket_type == Datagram)` branch, and the experiment
confirmed 20 sockets bound with it. Failure is loud, not silent: a missing
capability throws at bind and the listener is rejected. `NET_ADMIN` is already
granted on the proxy container.

### Phase 0: settled (2026-09-25, `main-worker-04`, kernel 6.18.34-talos)

**A transparent socket created inside a pod netns via `setns` and read from
another netns DOES observe the pre-TPROXY destination.** Phase 1 is unblocked.

The rig tests the kernel half directly, on the socket Envoy would use, rather
than standing up Envoy: `transparent: true` applying to a datagram socket and
Envoy reading `ipi_addr` via `addressFromMessage` were already established above,
so the only open question was the `setns` interaction — and isolating it removes
Envoy's config surface as a confound.

It binds the **service** port, not the capture port, because nftables `tproxy` is
prerouting-only: locally-originated pod egress uses mark-and-divert, which does
not rewrite the destination port. That is the same constraint as the third row of
the table above, arrived at independently.

| case | `ipi_addr` | |
|---|---|---|
| **C1** socket created *and* read in-netns, mark-and-divert | `10.250.0.7` | the VIP — the rig can produce a positive |
| **C2** REDIRECT, read in-netns | `127.0.0.1` | rewritten — `ipi_addr` tracks the REAL header |
| **C3** no capture rule | *nothing delivered* | delivery depends on the rule |
| **MAIN** created in-netns via `setns`, read from **outside** | `10.250.0.7` | **the VIP survives** |

C2 is the load-bearing control and it is the reason to believe MAIN. The
2026-09-23 attempt failed precisely because its control agreed with its negative
result, making both unusable; C3 alone is not a substitute, since it shows only
that a rule is needed, not that the measurement reflects the header.

The first run of this spike ALSO lost C2, and quietly: `redirect` is terminal in
nftables, so a trailing `counter` invalidated the rule, no rule installed, and C2
became an accidental duplicate of C3 — while still satisfying an assertion
written as "C2 must not report the VIP", which `None` trivially passes. The rig
now raises on a failed rule, verifies the ruleset with `nft list ruleset` before
measuring, and treats *no delivery* in C2 as a broken rig rather than a pass.

Reproducer: a Job in `aether-system` (which already enforces PodSecurity
`privileged`), image `nicolaka/netshoot`, `runAsUser: 0` with `NET_ADMIN` and
`SYS_ADMIN` and **no host namespaces and no `privileged: true`** — the pod's own
netns is the outer one. `SYS_ADMIN` is the single reason the rootless workstation
rig could not host this.

### Why the workstation rig could not establish it

aether's real shape is not the spike's shape. The proxy DaemonSet is
`hostNetwork: true` and binds capture listeners **into each pod's netns** via
`network_namespace_filepath`. The spike could not settle whether a transparent
listener bound that way sees the ClusterIP:

- with Envoy inside the netns, the matcher chose arm A, reproducibly;
- with Envoy outside and the listener bound via the filepath, it chose arm B —
  **but so did the control**, i.e. the same in-netns shape that reliably chooses A
  in the other rig. Two rigs disagreeing on one nominal configuration means the
  rig is at fault, not the feature, and neither reading is usable.

A rootless user namespace cannot host this test: `setns()` back to the original
netns needs `CAP_SYS_ADMIN` in that namespace's user namespace, and under
`unshare -r` the original netns is the host's.

**Phase 0 settled this on a node on 2026-09-25 — see above. Phase 1 may start.**

## Phases

Each phase is independently revertable, following 037's pattern.

**Phase 0 — node spike. DONE (2026-09-25): the answer is YES.** A transparent
socket created inside a pod netns via `setns` and read from another netns observes
the pre-TPROXY destination, with a REDIRECT control confirming the measurement
tracks the real header. See *Phase 0: settled*.

**Phase 1 — CNI, behind a flag, off by default.** Mark-and-divert rules in the
pod netns alongside the existing TCP REDIRECT, plus the `ip rule` / `ip route
local` pair, for exactly three constant UDP destination ports: 18082 (plaintext
services), 18008 and 18009 (QUIC transport). No declared-port-set plumbing —
the rule set is three literals. The existing `udp dport 18081 → :18001` REDIRECT
stays for one release, routed to the plaintext class, so a client still dialling
the old UDP spelling keeps working until D1's deprecation window closes. Two things make this smaller than it first
appeared: the rules live in the **pod** netns, created at pod setup and dying
with it, so Talos machine config never touches them and there is no persistence
question; and the `tproxy` nftables statement is prerouting-only, so
locally-originated pod egress uses the mark-and-divert shape — which is what
both spikes exercised.

Built with a capture-**mode** parameter from day one (R6):
`captureRedirectExprs(proto, meshPort, capturePort)` is already
protocol-parameterised because the transport dport offset is identical for TCP
and UDP; this extends it to `redirect | tproxy` rather than growing a second rule
path. The mark used for divert must not collide with
`CapturePassthroughFwMark` (`0xae7e`), which is the only thing preventing the
proxy's own forwarded egress from being re-captured into a loop on the TCP path;
choose a disjoint mark and pin it in a test.

The CNI has no route or rule capability today (only `mdlayher/netlink`, indirect
via `google/nftables`), so this phase adds one, and a dependency.

The mixed TCP-REDIRECT + UDP-TPROXY ruleset is the risky part of this phase and
deserves review on its own terms. It also has an expiry date now — see Phase 5.

**Phase 2 — agent.** One transparent listener on UDP:18082 per pod;
`matcher` on `DestinationIPInput`, `ip_range_matcher` where a CIDR is cleaner than
enumerating ClusterIPs. Each arm names the service's `udp:` cluster, whose load
assignment already carries the application port (`UDPLoadAssignment`). Note `UdpProxyConfig.matcher` is annotated
`work_in_progress`: a warning plus a `server.wip_protos` counter increment, not a
reject. aether currently uses the deprecated single-`cluster` specifier, so
adopting `matcher` newly lights that counter.

The chain→CDS resolver gate added in #895 does **not** cover this path and
structurally cannot: it walks filter *chains* for `tcp_proxy`, and a
connection-less UDP listener has none. `TestUDPCaptureListenerResolvesAgainstCDS`
(#914) is the UDP arm and must be extended to the per-port listeners.

**Phase 3 — e2e, then flip the default (plaintext UDP).** `e2e/l4routes.sh`'s
UDPRoute leg asserts *delivery*, not *selection*, precisely because selection
fails by design today. Delivery itself only started working on 2026-09-25
(#931 series) — before that the leg could not carry a datagram at all, so this
phase now has a working floor to build on. Here the assertion becomes selection
(two UDPRoute-backed services on one node, each reached) — and, per house
discipline, it must be seen red before it is trusted.

**Phase 4 — east-west QUIC transport, behind a flag, off by default.** The
identity-bearing class (R2):

- *Inbound.* A QUIC listener bound into each pod's netns on **UDP:18008** beside
  the TCP inbound (R3), `require_client_certificate: true`, the **same**
  `validation_context` (trusted bundle + SPIFFE SAN pinning) and the same SDS
  secrets as the TCP inbound, via `envoy.transport_sockets.quic`. Resumption and
  early data off (R4), gated. Routes to the same per-port app clusters.
- *Outbound.* The per-source mTLS cluster grows an HTTP/3 upstream variant that
  presents the source's client certificate (#45980). **Open question Q2** below:
  whether `transport_socket_matches` — how the TCP path selects the per-source
  certificate — is honoured for a QUIC upstream transport. If not, Phase 4 needs
  a per-source cluster instead, which changes the cluster-count budget.
- *East-west gateway.* The waypoint tunnel gains **UDP:18009** beside TCP:18009,
  same SNI-forwarding role. Cross-cluster QUIC is the last step of this phase,
  not the first.
- *Selection.* The source proxy chooses QUIC per destination by capability, not
  by client request: there is no alt-svc east-west. Start with an explicit
  allow-list flag; graduate to "destination advertises UDP:18008" via the
  registry once the inbound is proven.

Required mode only until the pin passes #47341; the extra peer-cert fields of
#45978 are not needed by anything today.

**Phase 5 — TCP capture moves to TPROXY.** Gated on a written analysis of the
`0xae7e` mark interaction and on Phase 4 being on in a soak. Not for symmetry:
TCP loses nothing under REDIRECT, which has `original_dst`. For the reason
already stated — TCP and QUIC sharing a port with two capture mechanisms
underneath is the exact split that produced #916. R6 makes this a flag flip.

## What this does NOT deliver

**East-west QUIC.** TPROXY is necessary for it and this proposal is the
foundation, but it is not sufficient, and the two should not be conflated.

HTTP/3 exists today only at the **edge** (proposal 029): `BuildEdgeGatewayH3Listener`
binds the gateway address directly as a north-south terminator, so it never
traverses the capture path and never needed the original destination. East-west
QUIC would.

**Superseded by the Requirements section above (2026-09-26).** Kept for the
record; the original text here deferred east-west QUIC on a security-model
objection: QUIC brings its own TLS 1.3, a different model from the
per-source mTLS H2 invariant proposal 031 settled on, and that decision should
not arrive as a side effect of a CNI change. That objection has since been
resolved upstream, in the direction that **preserves** the invariant.

Envoy's QUIC TLS 1.3 handshake carries client certificates since envoyproxy/envoy
#47076 (merged 2026-09-03), validated through the **same `CertValidator` as TCP**
— `trusted_ca`, `match_typed_subject_alt_names`, the SPIFFE validator — with the
identity reaching XFCC, RBAC and access logs. #45980 (2026-07-28) is the upstream
half. Both are in aether's pinned snapshot (`1.40.0-dev.20260904.13144fb`). SDS
works through the same `onSecretUpdated` path, so SPIRE-delivered certs carry
over; connection migration does not re-handshake, so identity persists across a
path change the way a TCP connection keeps its identity.

So QUIC can carry per-source mTLS identity rather than replacing it, and
east-west QUIC becomes a **planned requirement** instead of a deferred question.
It is still its own proposal — a UDP inbound listener with a QUIC transport
socket and the SAN-pinning machinery reproduced against it — and this proposal
still does not deliver it. But Phase 0 passing now IS the foundation for it, not
merely an option.

One hard requirement at the current pin: QUIC never re-verifies the client cert
on a resumed session. #47219 (2026-09-09) defaults resumption and early data off
when a client cert is required, but the registry publishes exactly one snapshot
and it predates that PR. Until the pin moves, **every mTLS QUIC filter chain must
set `enable_resumption: false` and `enable_early_data: false` explicitly**, gated
in `//test/envoy_validate`; otherwise an SVID revocation would not reach resumed
sessions — the silent-identity-drift class of #829. Optional mTLS (#47341) is
likewise not yet available.

This also settles the direction of a question Phase 0 raised: whether TCP capture
should move to TPROXY too. Not now — REDIRECT loses nothing for TCP, which has
`original_dst`, and the `CapturePassthroughFwMark` loop-prevention RETURN is a
mark-space collision that must be analysed before any TCP move. But TCP and QUIC
sharing a port with two capture mechanisms underneath is the exact split that
produced #916, so the mixed state has an expiry date. Phase 1 should build the
rule set with a capture-*mode* parameter so the eventual switch is a flag flip
over a tested path. The full assessment is on #916.

## Decisions required before Phase 1

- **D1 — the plaintext UDP dial spelling. DECIDED 2026-09-26: `:18082`, the
  L4 mesh spelling, shared with raw TCP (R3).** A client dials
  `<svc>.<ns>.<mesh-domain>:18082` for a UDP service exactly as it does for a
  raw-TCP one; the transport is the only difference. `:18081/udp` — today's
  spelling — is kept captured and routed to the plaintext class for one release,
  then retired. The alternative considered was the service's *application* port,
  which would have made the CNI rule set depend on registry state and the
  listener count scale with distinct ports; the shared spelling makes both
  constant. Dialling an application port directly is uncaptured for UDP, which
  is the same non-goal as for raw TCP without redirect-all.
- **D3 — pursue a per-connection client-certificate selector for QUIC
  upstream in Envoy?** Q2a is an Envoy limitation, not a design constraint: the
  selector would have to run before the chain is installed on a per-connection
  `SSL` instead of the shared `SSL_CTX`. That is a new envoyproxy/envoy change,
  unsized. If landed, Phase 4b collapses back to one cluster and the #842
  invariance returns for QUIC too. **Bruno's call**; the plan does not depend on
  it, and per-source clusters are correct in the meantime. Recorded so the
  per-source-cluster shape is read as a workaround with a known exit, not a
  design.
- **D2 — the divert mark. DECIDED 2026-09-26: `0xae71`**, reserved as
  `CaptureDivertFwMark` in `common/constants/mesh` beside
  `CapturePassthroughFwMark = 0xae7e`. Same `0xae7x` family so the two read as
  related in `nft list ruleset`; a distinct value so the passthrough RETURN rule
  (`meta mark 0xae7e accept`) never matches a diverted datagram and the divert
  rule never re-marks a passthrough. Disjoint from kube-proxy's masquerade/drop
  marks (`0x4000`, `0x8000`, masked) and from Istio's `1337`. Phase 1 pins both
  constants in one test that asserts they differ and that neither is a bitmask
  superset of the other.

## Open questions

- **Q1.** *Dissolved by D1.* The CNI captures three constant ports and needs no
  declared-port-set plumbing.
- **Q2.** *Restated 2026-09-26 — the first phrasing named the wrong mechanism.*
  aether's per-source identity on the TCP path is NOT `transport_socket_matches`
  (that shape was retired in #842). It is one upstream TLS socket whose
  `custom_tls_certificate_selector` — the `filter_state_override` cert mapper,
  `envoy.tls.certificate_mappers.on_demand_secret` — resolves the client
  certificate per connection from a HASHABLE filter-state key the originating
  listener stamps, and `CommonUpstreamTransportSocketFactory::hashKey` folds
  that key into the pool hash so pools partition per (host, source identity).
  That pairing is what keeps source B from getting source A's pooled connection
  (#831, which fails *open*). So, for `QuicUpstreamTransport`:
  **Q2a** does it honour the cert mapper, choosing the handshake cert per
  connection from filter state? **Q2b** does the HTTP/3 pool fold the hashable
  key into its hash, partitioning QUIC connections per source identity?
  **Answered 2026-09-26 from source at `13144fb`, unchanged on main: Q2a = no,
  Q2b = yes.**

  *Q2a.* `QuicClientTransportSocketFactory::create` rejects a certificate
  selector at config load —
  `source/common/quic/quic_client_transport_socket_factory.cc:93-94`:
  `if (config->tlsCertificateSelectorFactory()) return
  absl::UnimplementedError("Client certificate selector not supported on QUIC")`.
  Loud, not silent: a `QuicUpstreamTransport` carrying the mapper fails cluster
  load. The client cert is installed **once per crypto config**, not per
  connection — `configureQuicClientCertChain` (line 38) pulls cert and key from
  the context's `SSL_CTX` and installs them with `SSL_CTX_set_chain_and_key`
  (line 80) on the QUICHE client `SSL_CTX`, because the QUICHE handshaker needs
  direct access to the private key. It changes only when SDS rotates the
  context.

  *Q2b.* The HTTP pool key is built in one protocol-agnostic place
  (`cluster_manager_impl.cc`, `ClusterEntry::httpConnPool`), which calls
  `transportSocketFactory().hashKey(...)`; `QuicClientTransportSocketFactory`
  derives from `CommonUpstreamTransportSocketFactory` and does not override
  `hashKey`, so it inherits the fold over hashable
  `downstreamSharedFilterStateObjects()` the TCP path relies on. HTTP/3 pools
  partition per (host, source identity) exactly as h2 pools do.

  *Net for Phase 4.* Pooling does not reopen #831 — two identities never share
  a QUIC connection. But partitioning alone does not give per-source identity,
  because the certificate presented is the cluster's regardless of partition.
  So **Phase 4's outbound is one QUIC cluster per source ServiceAccount** — the
  pre-#842 shape, for QUIC only — until QUIC gains a per-connection selector
  upstream (D3).
- **Q3.** Migration: a QUIC connection that migrates keeps its identity (the
  validated chain lives on the session), but does the *source* proxy's
  per-source binding — keyed by source SPIFFE ID since #822 — survive a path
  change that alters the 5-tuple? Expected yes, since the key is the identity,
  not the tuple; verify in Phase 4's e2e.
- **Q4.** When the Envoy pin can move. **Answered 2026-09-26:** the registry
  published `1.40.0-dev.20260926.726d7ac.envoy` that morning and the proxy pin
  moved to it the same day. It carries #47219 (resumption default off) and
  #47341 (optional client certificates), and — the one that mattered —
  envoyproxy/envoy#47623 "quic: make it support SDS for mTLS": at `13144fb` a
  QUIC downstream requiring a client certificate was rejected at load unless
  its trust anchor was a static `trusted_ca`, which the mesh's SDS-served
  bundle is not, so Phase 4a was impossible on the old pin. R4's explicit gate
  stays regardless of #47219's default: an absent field is a default, and
  defaults move.

## Plan

Each PR is independently revertable and merges on green (`ci` + `proxy`,
squash). Talos validation where a PR changes what every pod does; a soak after
each phase's default flips. Sizes are relative: S = one sitting, M = a day,
L = several with a spike.

> **Phases 1, 2 and 5 are superseded** — see the status note at the top. They are
> kept below as the record of what was planned; what shipped is #944–#948.

### Phase 1 — CNI (three PRs, all behind `--capture-udp-mode`, default `redirect`)

| PR | scope | size | gate |
|---|---|---|---|
| 1a | `CaptureDivertFwMark = 0xae71` beside `0xae7e`; test that the two differ and neither masks the other. Extend the port-role gate (#942) to cover 18082 as an L4 spelling for both transports. Correct the CNI comment "18082 is a TCP spelling"; rename `ProxyTCPOutboundPort` → `ProxyL4OutboundPort` (mechanical, call sites only). | S | unit |
| 1b | Capture-mode parameter on the rule builder (R6): `captureRedirectExprs` grows a mode; `tproxy` emits mark-and-divert (`type route hook output`, `meta mark set 0xae71`) plus the `ip rule fwmark → table` / `ip route local default dev lo table` pair. Adds the route/rule capability and its dependency. The three UDP ports are literals: 18082, 18008, 18009. | M | unit + a netns integration test in the shape of `e2e/spike/udp-tproxy-phase0.py`, gated on `NET_ADMIN`/`SYS_ADMIN` and skipped without them — never silently green |
| 1c | Keep `udp dport 18081 → :18001` REDIRECT for one release, routed to the plaintext class, so the old spelling keeps working through the flip. Chart: the flag, default off. | S | e2e unchanged (T3 still passes on REDIRECT) |

Datagrams diverted to 18008/18009 before Phase 4's listeners exist are dropped
at the socket — the same "no listener = discard" behaviour today's 18081 rule has
before a UDPRoute exists, and equally deliberate.

### Phase 2 — agent (two PRs, inert until the mode is `tproxy`)

| PR | scope | size | gate |
|---|---|---|---|
| 2a | `GenerateUDPCaptureListener` becomes ONE transparent listener on UDP:18082 (`transparent: true`), `udp_proxy` `matcher` on `DestinationIPInput` with one arm per UDPRoute-backed service → its `udp:` cluster; `on_no_match` → the existing single-cluster behaviour while the mode is `redirect`, a blackhole cluster once it is `tproxy`. Adopting `matcher` lights `server.wip_protos`; document it and pin the count. | M | `//test/envoy_validate` accepts it; **extend `TestUDPCaptureListenerResolvesAgainstCDS`** by hand — the #895 chain→CDS gate structurally cannot see a connection-less listener |
| 2b | Retire the single-service discard in `selectUDPRoute`; keep the weight-discard reason and `udp_unsupported` for #873, which TPROXY does not fix. `udp_no_healthy_backend` (#937) applies per arm unchanged. | S | unit |

### Phase 3 — e2e, then flip (two PRs)

| PR | scope | size | gate |
|---|---|---|---|
| 3a | `e2e/l4routes.sh` T3 grows a second UDPRoute-backed service on the same node and asserts **selection**: each service's datagrams reach its own backend. Run first with the mode `redirect` and **seen red** (second service dropped, as today), then with `tproxy` green. | M | red-then-green on kind |
| 3b | Flip `--capture-udp-mode` to `tproxy`. Chart bump. Deploy to talos-main; 8h soak with the UDP workload in the churn set. Then delete the 18081/udp REDIRECT one release later. | S + soak | soak PASS |

### Phase 4 — east-west QUIC (behind `--east-west-quic`, default off)

| PR | scope | size | gate |
|---|---|---|---|
| 4a **(shipped, unconditional — no flag; no 18008 divert, inbound QUIC arrives on eth0 and needs none)** | Inbound: a QUIC listener bound into each pod's netns on **UDP:18008**, `envoy.transport_sockets.quic` wrapping the SAME `DownstreamTlsContext` (SDS server cert, validation context with SPIFFE SAN pinning) as the TCP inbound; `require_client_certificate: true`; `enable_resumption: false`, `enable_early_data: false` (R4). Routes to the same per-port app clusters. Extend the port-role gate to 18008. | M | `//test/envoy_validate` asserts the two `false`s on every mTLS QUIC chain — a chain without them must FAIL validation, and the test must be seen red |
| 4b | Outbound — **Q2 answered: one QUIC cluster per source ServiceAccount.** `QuicUpstreamTransport` rejects the cert mapper at load (Q2a=no), so the per-source identity has to be the cluster's own `UpstreamTlsContext`, one per local ServiceAccount, named `quic:<svc>@<source-sa>` and selected by the source's filter-state identity at the route. The identity-set re-push cost #842 removed returns for these clusters only: the first pod of a new ServiceAccount on a node adds a QUIC cluster, the last one leaving removes it, and each add re-warms only that cluster — bounded by local ServiceAccounts, not by mesh services, and never touching the TCP clusters. Pooling still partitions per identity (Q2b=yes), so `//test/mtlspool` gains a QUIC arm asserting source B never rides source A's connection — the pooling guarantee is real even though the certificate is per cluster. | L | mtlspool QUIC negative control red-then-green; a cluster-count budget stated before commit (local SAs × QUIC-enabled destinations) |
| 4c | Selection: explicit allow-list flag first; then "destination advertises UDP:18008" via the registry once the inbound has soaked. | S | e2e |
| 4d | East-west gateway: UDP:18009 beside TCP:18009, same SNI-forwarding role; cross-cluster last. | M | 019's cross-cluster e2e over QUIC |

Then a soak with `--east-west-quic` on for the whole 8h, graded on the same
prober SLI, before any default flips.

### Phase 5 — TCP capture to TPROXY

One written analysis first: the `0xae7e` passthrough RETURN under a
mark-and-divert TCP path — whether Envoy's `SO_MARK` on `passthrough_original_dst`
still short-circuits the divert, or whether the two marks need to compose. Only
then one PR flipping the TCP mode, gated on Phase 4 having soaked. R6 makes the
PR itself small; the analysis is the work.

### Order and what each unlocks

1 → 2 → 3 delivers R1 and closes #916. 4 delivers R2 and is the first
identity-bearing UDP in the mesh. 5 removes the mixed-mechanism state. Nothing in
4 depends on 3's default flip — Phase 4 can proceed on a `tproxy`-mode cluster
before the flip — but 4 should not merge before 3a's selection assertion exists,
or the two traffic classes share a path with no e2e proving they stay apart (R5).

## Dependencies

- Envoy pin: #45980 and #47076 present; #47219 (safe resumption default) and
  #47341 (optional mTLS) absent and not resolvable as of 2026-09-26. Presenting
  a client cert on a QUIC upstream at all is behind
  `envoy.reloadable_features.quic_upstream_client_certificates` (default on);
  `//test/envoy_validate` should assert it is not disabled by any runtime layer
  aether ships.
- No per-connection certificate selector on QUIC upstream (Q2a). Phase 4b's
  per-source clusters are the workaround; D3 is the exit.
- CNI: route/rule capability, new dependency (Phase 1).
- Registry: `PORT_PROTOCOL_UDP` and the `=udp` suffix (#933, #934) — present;
  consumed by `UDPLoadAssignment` for the application-port rewrite, not by the
  CNI.
- UDP delivery working at all (#931 series, #936) — present since 2026-09-25.

## Alternatives considered

**A listener per service on its own port.** Works with today's REDIRECT, since the
socket's own port would identify the service. Rejected: the agent must allocate
ports, the CNI must rewrite rules on every service add, listener count scales with
services rather than with ports, and it churns per-pod capture listeners — which
drains their connections (037 Risk 4). It is also disqualified by QUIC, which
dials arbitrary mesh authorities and whose connection migration breaks any
5-tuple-keyed assumption.

**Weighted endpoints merged into one cluster.** Mechanically easy —
`NewUDPServiceCluster` is `STATIC` with an inline load assignment, and
`use_per_packet_load_balancing` would re-select per datagram. Rejected for now: it
collapses the per-backend `udp:<svc>` clusters that the dependency set, the port
rewrite, outlier detection and every `cluster.udp:*` stat are keyed on, and exact
ratios need weights scaled by endpoint counts, re-churning on every scale event.
Worth its own issue, not a drive-by.

**A sticky `runtime_fraction` bucket** as a `custom_match` predicate. A real N/D
split, but it needs the WiP `matcher` specifier anyway, it hashes an input so it
splits per *source socket* rather than per datagram, and more than two backends
needs cascaded conditional probabilities.

**A separate port for QUIC.** Rejected (R3): a second number means a second
CNI rule, a second Service port, a second cross-cluster agreement and a second
capture path, and TCP and QUIC on different capture mechanisms is the split that
produced this proposal. The edge already shares its port; the mesh does the
same. Pinned by `TestEastWestPortIsSharedAcrossTransports`.

**QUIC via HBONE / CONNECT-UDP tunnelling.** Rejected on the same grounds as
proposal 031 rejected HBONE for TCP: it reintroduces a tunnel layer the mesh
chose not to have, and it would make the per-source identity a property of the
tunnel rather than of the connection.

**Do nothing.** Legitimate before 2026-09-26; weaker now. UDP was the
least-exercised path in the mesh, the limit is documented and counted, and #914
made the one-winner choice principled. But the same capture gap blocks east-west
QUIC, QUIC mTLS is now available in the pinned Envoy, and #931 showed the
plaintext path had been silently broken for its whole life — the cost of acting
is paid once for two goals, and the cost of not acting is no longer just a
documented limit.
