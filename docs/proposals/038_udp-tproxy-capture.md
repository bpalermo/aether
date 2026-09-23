# Proposal 038: TPROXY Capture for UDP

**Status:** Draft. P0 spike run (2026-09-23) — three of four premises verified by
experiment; one remains, and it must be settled on a node before Phase 1 starts.
**Author:** Bruno Palermo
**Date:** 2026-09-23
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
original destination port. Route per service on `destination_ip`.

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

### The premise that is NOT yet established

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

**Phase 0 exists to settle exactly this, on a node, and nothing downstream starts
until it does.**

## Phases

Each phase is independently revertable, following 037's pattern.

**Phase 0 — node spike.** Answer one question: does a `transparent` listener bound
via `network_namespace_filepath` into a pod netns observe the pre-TPROXY
destination? Proving it needs real root, which the workstation rig cannot supply.
If the answer is no, this proposal stops here and #916 stands as a documented
limit — that is a legitimate outcome and cheaper than discovering it in Phase 2.

**Phase 1 — CNI, behind a flag, off by default.** UDP TPROXY rules in the pod
netns alongside the existing TCP REDIRECT, plus the `ip rule` / `ip route local`
pair TPROXY requires. Two things make this smaller than it first appeared: the
rules live in the **pod** netns, created at pod setup and dying with it, so Talos
machine config never touches them and there is no persistence question; and the
`tproxy` nftables statement is prerouting-only, so locally-originated pod egress
uses the mark-and-divert shape instead — which is what the spike exercised.

The CNI has no route or rule capability today (only `mdlayher/netlink`, indirect
via `google/nftables`), so this phase adds one, and a dependency.

The mixed TCP-REDIRECT + UDP-TPROXY ruleset is the risky part of this phase and
deserves review on its own terms.

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

**Phase 3 — e2e, then flip the default.** `e2e/l4routes.sh`'s UDPRoute leg
currently asserts *delivery*, not *selection*, precisely because selection fails
by design today. This is where that assertion becomes real — and, per house
discipline, where it must be seen red before it is trusted.

## What this does NOT deliver

**East-west QUIC.** TPROXY is necessary for it and this proposal is the
foundation, but it is not sufficient, and the two should not be conflated.

HTTP/3 exists today only at the **edge** (proposal 029): `BuildEdgeGatewayH3Listener`
binds the gateway address directly as a north-south terminator, so it never
traverses the capture path and never needed the original destination. East-west
QUIC would.

But east-west QUIC also needs a security-model decision that this proposal
deliberately does not make. UDP rides the mesh in **plaintext** today, because
mTLS is a TCP/TLS construct and there is no DTLS. QUIC brings its own TLS 1.3,
which is a different model from the per-source mTLS H2 invariant proposal 031
settled on. That is an architecture question, not plumbing, and it should not
arrive as a side effect of a CNI change.

So: this proposal unblocks the option and is worth doing on #916's merits alone.
It does not deliver QUIC, and Phase 0 passing should not be read as QUIC being
approved.

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

**Do nothing.** Legitimate. UDP is the least-exercised path in the mesh, the limit
is now documented and counted rather than silent, and #914 made the one-winner
choice principled. The case for acting is that the same capture gap blocks
east-west QUIC, so the cost is paid once for two goals.
