# Proposal 038: TPROXY Capture for UDP, and East-West QUIC on Top of It

**Status:** Accepted; **revised 2026-09-26** to fold east-west QUIC in as a
requirement rather than a deferred question. All four premises verified by
experiment; Phase 0 settled on a node 2026-09-25. Phase 1 is unblocked, pending
two decisions listed under *Decisions required before Phase 1*.
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
- **R3 — one port number per role, both transports.** The per-pod inbound
  (`defaultInboundPort`, 18008) and the east-west gateway
  (`DefaultEastWestTunnelPort`, 18009) each bind UDP on the same number as TCP,
  the way the edge's HTTP/3 listener already shares `internalPort` with the TCP
  HTTPS listener. A second number means a second CNI rule, a second Service
  port, a second cross-cluster agreement and a second capture path. Pinned for
  18009 by `TestEastWestPortIsSharedAcrossTransports`; extend to 18008 in Phase 4.
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
| a declared UDP service port (`PORT_PROTOCOL_UDP`) | **plaintext service** | `udp_proxy`, `matcher` on `DestinationIPInput` → per-service `udp:` cluster | plaintext, no identity, as today |
| anything else | not captured | — | — |

The classes cannot bleed into each other because they are separated at the
port, before any matcher runs: a datagram for 18008 is never offered to the
plaintext `udp_proxy`, and a datagram for a service port is never offered to the
QUIC transport socket. The mesh ports are constants; the service ports come from
the registry, which since #933/#934 carries a per-port `PORT_PROTOCOL_UDP` and
the `=udp` suffix on `endpoint.aether.io/ports` — so the CNI's rule set is
derived from declared facts, not guessed.

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
bind it. That makes the shape **one transparent listener per distinct UDP service
port**, with `destination_ip` disambiguating the services that share a port. A
mesh's distinct UDP port set is small (53, 161, 514, 8125…), so per-service
fan-out stays in the matcher rather than in listeners — materially cheaper than
the listener-per-service alternative, which was the obvious fallback and which
would also have been disqualified by QUIC (below).

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
local` pair, for exactly two port sets: the mesh UDP ports (18008, 18009) and
the declared UDP service ports. Two things make this smaller than it first
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

**Phase 2 — agent.** One transparent listener per distinct UDP service port;
`matcher` on `DestinationIPInput`, `ip_range_matcher` where a CIDR is cleaner than
enumerating ClusterIPs. Note `UdpProxyConfig.matcher` is annotated
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

- **D1 — the plaintext UDP dial spelling.** Today a client dials
  `<svc>.<ns>.<mesh-domain>:18081` and the CNI captures UDP for 18081 only. The
  per-service-port design implies clients dial the service's **application**
  port instead, with `:18081` for UDP retired over a deprecation window.
  *Recommendation:* adopt the application port — it is what the registry now
  declares, it is what makes "the distinct UDP port set is small" true, and it
  removes the one-port bottleneck that made the single-service limit look
  natural. Keep `:18081` captured and routed to the plaintext class for one
  release so nothing dialling it breaks on the flip.
- **D2 — the divert mark.** A single well-known mark value, disjoint from
  `0xae7e`, reserved in `common/constants/mesh` beside it.

## Open questions

- **Q1.** How does the CNI learn the declared UDP service port set at pod ADD?
  It programs per-pod rules and today knows only the mesh ports. The registry
  has the facts; the plumbing (agent → CNI, or a node-local snapshot the CNI
  reads) is Phase 1 design.
- **Q2.** Does `transport_socket_matches` select a QUIC upstream transport socket
  the way it selects a TLS one? Determines whether Phase 4's outbound is one
  cluster or one per source.
- **Q3.** Migration: a QUIC connection that migrates keeps its identity (the
  validated chain lives on the session), but does the *source* proxy's
  per-source binding — keyed by source SPIFFE ID since #822 — survive a path
  change that alters the 5-tuple? Expected yes, since the key is the identity,
  not the tuple; verify in Phase 4's e2e.
- **Q4.** When the Envoy pin can move. The registry publishes exactly one
  snapshot (`1.40.0-dev.20260904.13144fb`) and #47219/#47341 postdate it. R4's
  explicit gate carries the security model until then.

## Dependencies

- Envoy pin: #45980 and #47076 present; #47219 (safe resumption default) and
  #47341 (optional mTLS) absent and not resolvable as of 2026-09-26.
- CNI: route/rule capability, new dependency (Phase 1).
- Registry: `PORT_PROTOCOL_UDP` and the `=udp` suffix (#933, #934) — present.
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
