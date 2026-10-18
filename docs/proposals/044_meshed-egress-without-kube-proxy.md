# Proposal 044: Meshed pods reach non-mesh Services without kube-proxy

**Status:** Proposed. Phase 0 is measurement, and every later phase waits on
what it shows. The facts below were read from `main` at `bb1d1625`.
Phase 0 progress: the agent learns the Service range (chart 3.0.4); the
capture listener's Service-range branch to `cap_passthrough_svc`, the L4
split, is built (chart 3.0.6). The cleartext-HTTP split is not.
**Author:** Bruno Palermo
**Date:** 2026-10-15
**Related:** proposals [022](022_arbitrary-service-interception.md)
(redirect-all and the `ORIGINAL_DST` passthrough),
[038](038_udp-tproxy-capture.md) (TPROXY capture),
[043](043_real-port-addressing.md) (a service is addressed by its real
ports), [037](037_multi-protocol-per-service.md) (refusal of a port nobody
registered), [004](004_demand-scoped-distribution.md) (demand-scoped
distribution and the on-demand CDS cold path), #1020 / #1033 / #1036 (QUIC
twins built on first use, the model for demand scoping here), #1641 (the
outcome header; the rule that proxies signal with a header and never by
changing a status code), #1702 (the agent reads a Service's EndpointSlices).

## The decision

> **A meshed pod's captured TCP connection to the ClusterIP of a Kubernetes
> Service that is not a mesh service is load-balanced by the node proxy over
> that Service's EndpointSlices, and no longer by kube-proxy's DNAT on the
> host. kube-proxy stays installed and keeps every job it has for anything
> that is not a meshed pod's captured egress.**

This is *Option A*. It does not save CPU and it does not remove kube-proxy.
Both are stated as non-goals below, with the measurement that makes the first
one not worth pursuing and the list of things that keep the second one out of
reach.

What it buys is correctness and visibility on a path the mesh already carries
and today treats as an anonymous pipe.

## Context

### What a meshed pod's egress does today

The CNI programs a mark-and-divert in every managed pod's network namespace
(`cni/internal/plugin/capture.go:84-98`, `divertOutputRules` at `:171`):

| traffic from the pod | what happens |
|---|---|
| TCP, any port (redirect-all, the managed-pod default) | `OUTPUT` marks it `0xae71`; a policy route delivers it to `lo` with the header intact; `PREROUTING` on `lo` `tproxy`s it to the capture listener on `:18001` |
| TCP to `18081`/`18082` (scoped mode) | the same, only for the two mesh ports |
| UDP to `18082` | marked and delivered to the per-VIP `udp_proxy` listener on `18082`; no other UDP port is captured |
| TCP/UDP to `:53` | not marked; DNATed to `HOST_IP:18054`, the node's mesh-dns (`cni/internal/plugin/dns.go:35`) |
| to `127.0.0.0/8`, `169.254.0.0/16`, `224.0.0.0/4`, an excluded port or range | not captured |
| a reply on a tracked flow (`ct direction reply`) | not captured (the load-bearing early accept) |

The capture listener is bound in the pod's netns, but the proxy that owns it
runs in the **host** network namespace (proposal 038: "the proxy is
hostNetwork"). What happens next depends on the destination.

**A mesh service.** The generated mesh Service is selectorless and has no
EndpointSlices (`registrar/internal/services/generator.go:1-7`). Its HTTP is
selected by authority in the `cap_http` route table; its raw-TCP ports by a
`/32` branch on the destination address in the listener's matching tree
(`agent/internal/xds/proxy/capturematcher.go:205-229`). Either way the
connection goes to an EDS cluster of the service's endpoints, the pods' IPs on
the inbound port `18008`, over mTLS with the source pod's own SVID. kube-proxy
is not on this path.

The code relies on kube-proxy for one thing here: a connection to a mesh VIP
that no chain claims (a port nobody registered, or the raw-TCP floor while
identity is not ready) falls to the passthrough, and kube-proxy REJECTs it
because the Service has no endpoints. That is the refusal the caller sees
(`agent/internal/xds/cache/capture.go:119-124`,
`agent/internal/xds/proxy/capture.go:166-170` and `:490-493`,
`docs/workload-requirements.md`, "A port nobody registered").

**Anything else.** What the matching tree does not select falls to the
listener's `DefaultFilterChain`, `cap_passthrough`, a `tcp_proxy` to the
cluster `passthrough_original_dst` (`agent/internal/xds/proxy/capture.go:265-274`,
`:444`). Cleartext HTTP to a name that is not a mesh name goes through the
`cap_http` connection manager first and leaves by the same cluster from its
catch-all route (`agent/internal/xds/proxy/route.go:175-191`). That cluster
(`agent/internal/xds/proxy/cluster.go:307-356`) is `ORIGINAL_DST` with
`CLUSTER_PROVIDED` load balancing, an `SO_MARK` of `0xae7e`, and no
`source_address`. Its upstream socket is therefore opened in the host netns
and sourced from a node address, and **kube-proxy's host `OUTPUT` DNAT is what
turns a ClusterIP into an endpoint**. (The per-pod `app_` clusters, by
contrast, bind into the pod's netns at `127.0.0.1`, `cluster.go:186-199`; the
passthrough does not.)

**DNS.** A pod's `:53` reaches mesh-dns. mesh-dns answers mesh names and
forwards everything else to its upstream: `--mesh-dns-upstream`, else the
nameservers of its own `/etc/resolv.conf` (`agent/cmd/mesh-dns/main.go:171-191`).
The DaemonSet runs with host networking and `dnsPolicy: ClusterFirstWithHostNet`
(`charts/aether/templates/mesh-dns-daemonset.yaml:46`), so that file names the
cluster DNS Service's ClusterIP, and the forward is DNATed by kube-proxy on
the host.

So a meshed pod depends on kube-proxy for every connection to a non-mesh
ClusterIP and for every non-mesh DNS answer.

### What that costs

Not CPU. On the reference cluster kube-proxy runs in nftables mode and costs
about 0.010 cores in total and 15-20 MiB per node: under 1% of the mesh data
plane's own CPU. Moving its work into the node proxy would not make the node
cheaper, and this proposal does not claim it would.

What it costs is that the mesh can see and say nothing about this traffic:

1. **No attribution.** Every non-mesh connection of every pod on the node is
   one cluster, `passthrough_original_dst`. Its stats are one block per proxy,
   whatever the destination. The L4 passthrough chain is deliberately not
   access-logged (`agent/internal/xds/proxy/capture.go:171-177`: "every non-mesh egress connection, the
   volume the log must not carry"). The HTTP passthrough is logged, with the
   dialled VIP in `downstream_local_address`, but nothing ties it to a Service
   and nothing counts it. The upstream connection is node-sourced, so the
   destination sees the node, not the pod or its identity.
2. **Load balancing the proxy cannot see.** The proxy connects to the VIP and
   kube-proxy picks an endpoint at random. A dead endpoint costs the caller a
   connect timeout or a refusal, every time it is picked, with no outlier
   detection and no retry onto another endpoint. Locality and the Service's
   topology hints are applied by kube-proxy if at all; the registry's locality
   handling does not apply.
3. **Refusals nobody attributes.** A Service with no ready endpoint is
   REJECTed by kube-proxy. The caller sees `ECONNREFUSED` (raw TCP) or a 503 with
   `UF` and `delayed_connect_error:_Connection_refused` (HTTP through the
   connection manager): the same answer as a crashed backend, a wrong port, or
   a kube-proxy that has not yet programmed a new Service.
4. **Session affinity is per node.** `sessionAffinity: ClientIP` keys on the
   source address, and every pod's passthrough has its node's address: all of
   a node's pods stick to one backend.

### What Option A gives, and what it does not

Once the proxy resolves the Service itself, it holds a named cluster per
Service port, and from that:

- **per-Service stats and access logs that carry the source SPIFFE identity**,
  where today there is an anonymous, node-sourced passthrough;
- **egress authorization**: source ServiceAccount to destination Service, at
  L4 or L7, with the same `HTTPFilter` machinery (RBAC, ext_authz) the mesh
  already has, now that the destination is a name and not an address;
- **retries and outlier detection** over the Service's real endpoints;
- **deterministic, attributed refusals** (Phase 2).

It does **not** give:

- **mTLS.** mTLS needs a proxy and an SVID at both ends. A non-mesh
  destination has neither, and a mesh destination already has mTLS. The way to
  more mTLS is to mesh more workloads, not to replace kube-proxy.
- **Anything more if kube-proxy were also removed.** Every gain above comes
  from the node proxy resolving the Service. Removing kube-proxy on top adds
  none of them, and takes on everything in *Non-goals*.
- **Any of it for non-meshed pods.** Extending it to them (Option C) would make
  the node proxy originate their traffic under a node identity, which breaks
  the rule that every hop carries the workload's own identity.

## Non-goals

kube-proxy stays installed, and keeps:

- **Non-meshed pods**, and meshed pods' traffic that is not captured (excluded
  ports and ranges, UDP to any port other than `18082`).
- **Host-netns processes**: the kubelet, the node agent, the proxy itself,
  mesh-dns, every hostNetwork DaemonSet.
- **The apiserver Service** (`kubernetes.default`, the first address of the
  Service range). Clients bootstrap through it before any agent exists. On
  Talos, KubePrism serves a local apiserver endpoint to host processes only;
  pods still dial the ClusterIP.
- **Control-plane nodes**: admission webhooks and aggregated APIs called by the
  apiserver from nodes where no agent runs.
- **NodePort and LoadBalancer**: external traffic arrives on the host, never
  through a pod's capture. MetalLB's BGP announcements, `externalTrafficPolicy:
  Local` health-check node ports and UDP/443 QUIC to the edge stay kube-proxy's.
- **UDP Services on any port but `18082`** (not captured, proposal 038).
- **conntrack hygiene** on endpoint removal for the traffic above.
- **DNS by default.** mesh-dns's forward stays on the cluster DNS ClusterIP
  unless Phase 4 is taken.

Also out of scope: removing kube-proxy, or running without it (Option B, D);
dual-stack and IPv6 (the capture is IPv4-only today, `TableFamilyIPv4`);
headless Services (a dial to a pod IP is not a ClusterIP and keeps the
`ORIGINAL_DST` passthrough); `externalIPs` and LoadBalancer ingress addresses
dialled from inside (kube-proxy short-circuits those; they stay
passthrough); `ExternalName` Services (DNS only).

## Design

### The destination classes

Every connection that reaches the capture listener and is not a mesh
destination falls into one of three classes, decided by its restored
destination address:

| class | how it is told | Phase 1 path |
|---|---|---|
| **known non-mesh Service**: `VIP:port` is a port of a Service with a selector or a hand-managed EndpointSlice | the address is in the Service range, and the agent's Service index has `VIP:port` | a per-Service-port EDS cluster, `svc:<ns>/<name>:<port>`, fed from EndpointSlices |
| **unknown address in the Service range**: no Service owns it, or the port is not one of the Service's | the address is in the Service range, and the index has no entry | Phase 2: an Aether refusal. Phase 1: passthrough, unchanged |
| **external** | the address is outside the Service range | `passthrough_original_dst`, unchanged |

The Service range comes from the cluster's `ServiceCIDR` objects
(`networking.k8s.io/v1`, GA in Kubernetes 1.33), watched by the agent, with a
chart value as the override for clusters that do not publish them. The agent
needs the range for one reason: an address outside it is never offered to the
on-demand path, so an external connection never waits on a lookup.

The MCS API (`multicluster.x-k8s.io`) leaves the allocation of a
`ServiceImport`'s ClusterSetIP to the implementation, so it can lie outside
every `ServiceCIDR`. Where that CRD is installed the agent also watches the
`ServiceImport`s and adds a `/32` for each IPv4 `spec.ips` entry of a
`ClusterSetIP`-type import that the range does not already contain. Aether's
own imports (labelled `aether.io/managed-service-import`, whose VIP is the
ClusterIP of a mesh Service) and `Headless` imports are skipped. The chart
override replaces only the `ServiceCIDR` part: ClusterSetIPs are added under it
too.

### Matching: one branch for the range, the cluster named per connection

The capture listener is per pod. Giving it a `/32` branch for every non-mesh
Service would make every pod listener's size grow with the cluster's Service
count, and every Service change rewrite every pod listener on every node.
Instead:

1. The matching tree gains one branch, below every mesh address: destination
   IP in the Service range (a `CidrMatcher` / range input) → chain
   `cap_svc`.
2. `cap_svc` is a `set_filter_state` network filter that names the cluster for
   this connection, `envoy.tcp_proxy.cluster` =
   `svc:%DOWNSTREAM_LOCAL_ADDRESS%` (the restored `VIP:port`), followed by a
   `tcp_proxy` with **on-demand cluster discovery**.
3. The first connection to a `VIP:port` finds no cluster; `tcp_proxy` asks the
   agent for it by name. The agent resolves the name through its index to a
   Service port, admits it into the node's demand set, and publishes the
   cluster with its load assignment in the same snapshot (the #1008 rule the
   QUIC twins follow). Later connections find it.

The cluster is named by address and not by Service so that the proxy can
name it without knowing anything; the agent publishes it with
`alt_stat_name` set to `svc.<ns>.<name>.<port>`, so stats and access logs say
which Service it was. A Service whose ClusterIP changes (it is deleted and
recreated) is a new cluster under a new name; the old one is pruned with its
demand (below).

Cleartext HTTP to a non-mesh ClusterIP is sniffed as HTTP today and goes
through the `cap_http` connection manager to the passthrough. Phase 1 sends it
through `cap_svc` as L4 instead: the branch for the Service range is tried
before the HTTP branch. HTTP-level behaviour for non-mesh Services (retries,
L7 authorization) then needs its own chain; that is *Open question 2*,
because it costs a second connection manager per pod listener and nothing in
Phase 1 needs it.

**To establish before Phase 1 is built**, on the pinned proxy, in
`//agent/test/envoy_validate` and a live harness beside `//agent/test/mtlspool`:
that `tcp_proxy`'s `on_demand` accepts the cluster name from
`envoy.tcp_proxy.cluster` filter state, that a connection waiting on the
discovery is held and not reset, and what the timeout does. The agent has
never used `tcp_proxy` on demand (`agent/internal/xds/cache/capture.go:113-118`
says "tcp_proxy has no ODCDS cold path" of the floor as built). If it does not
work, the fallback is the `/32` branch per Service in the node's demand set,
with demand learned from Phase 0's counter, and its listener cost measured.

### The cluster

One EDS cluster per (Service, port) in the node's demand set:

- **Endpoints** from the Service's EndpointSlices, the address with the
  slice's port of the same name. A port the slices do not carry (a named
  `targetPort` that no pod resolves yet) has no endpoint, and the cluster is
  empty: Phase 2 says so.
- **Conditions**: `ready` endpoints are `HEALTHY`. A `serving` and
  `terminating` endpoint is `DRAINING` and used only when no ready endpoint
  exists, which is the rule kube-proxy applies (KEP-1669). An endpoint that is
  neither is left out.
- **`internalTrafficPolicy: Local`**: only the endpoints whose `nodeName` is
  this node. None is an empty cluster, as kube-proxy drops it.
- **Topology**: `hints.forZones` and `trafficDistribution: PreferClose` become
  locality priorities, the zone of this node first, through the locality
  handling the registry endpoints already use. Where the two disagree the
  Service's own hints win, because that is what kube-proxy would have done.
- **`sessionAffinity: ClientIP`**: a ring-hash policy on the downstream source
  address, which is now the pod's address and not the node's. The timeout of
  `sessionAffinityConfig` has no Envoy counterpart and is not honoured; this
  is the one Service field the cluster cannot mirror exactly, and it is
  documented.
- **Outlier detection**: consecutive connect failures eject an endpoint, with
  the mesh's existing defaults.
- **Plaintext**, as today's passthrough: no transport socket.
- **Upstream bind**: unchanged from the passthrough in Phase 1 (host netns, a
  node source address, `SO_MARK 0xae7e`). See Phase 3.

### Where the endpoints come from

The agent already has some of this, and less than a quick read suggests:

- It watches **Services** (ports and ownership) in the gamma reconciler, and
  reads a Service's **EndpointSlices** through the API reader, one Service at
  a time, to resolve named `targetPort`s (`agent/internal/gamma/reconciler.go:275`
  and `:1032-1033`, #1702). That informer is **metadata-only** and filtered to
  Services a route names; it carries no endpoints.
- Mesh endpoints come from the **registry**, not from EndpointSlices. The
  locality and two-phase drain the registry path has (`common/drain`) are
  registry concepts; the EndpointSlice path must reproduce their effect from
  slice conditions, not reuse their code as-is.

So Phase 1 adds a new source: a full EndpointSlice informer, but scoped to the
Services in the node's demand set (a label selector per Service on
`kubernetes.io/service-name`, or one cluster-wide informer with the cache
filtering by demand; Phase 1 measures both on the reference cluster's slice
count and picks one). The Service index (`VIP:port` → Service port) comes
from a cluster-wide Service informer; Services are few and small, and the
gamma reconciler already holds one.

### Demand scoping

Only Services a pod on the node has actually dialled get a cluster, the way
QUIC twins are built only for observed pairs (`agent/internal/xds/cache/quicpairs.go`,
`agent/internal/xds/quicdemand`, `agent/internal/xds/server/odcds.go`). The
same rules carry over, because the same Envoy behaviour forces them:

- Envoy keeps one on-demand subscription per name for the life of the proxy
  process. A subscribed `svc:` cluster is never forgotten while that stream
  lives: when its demand lapses it stays published (dormant) and is dropped only
  with the stream (the ODCDS strand rule of #1036/#1052).
- Demand is persisted with the observed upstreams, so a restarted agent does
  not make every pod's first connection to every Service wait again.
- A request for a name the index does not know is refused and counted, never
  admitted (the `quic:` admission rule, #1033), so a scan of the Service range
  cannot grow the snapshot.

On a small node this bounds CDS and EDS to the Services its pods use, not the
cluster's.

### Refusal (Phase 2)

Today's refusal of an empty Service is kube-proxy's REJECT. Under Option A it
is the proxy's, and it says so:

- **Raw TCP**: the connection is reset, and `aether.proxy.svc_refused{reason}`
  counts it on the proxy, with `reason` one of `no-endpoints` (the Service
  has no ready, serving or terminating endpoint, or none on this node under
  `internalTrafficPolicy: Local`), `port-not-declared` (the address is a
  Service's, the port is not), `no-service` (the address is in the Service
  range and no Service owns it). The L4 access log (#1023) records the
  connection with its reason, so the refusal names its Service.
- **HTTP**, once Open question 2 gives non-mesh HTTP a connection manager:
  the status is the one Envoy produces (503 with `UH`,
  `no_healthy_upstream`), unchanged, with `x-aether-error: no-endpoints` (the
  header the mesh's other refusals use, `NoHTTPPortErrorHeader`,
  `agent/internal/xds/proxy/route.go:1133`) added. The rule is the repository's:
  signal between proxies with a header, never by rewriting the status (#1641).

There is no fallback to kube-proxy. A Service the proxy cannot serve is
refused, by name. Falling back would hide exactly the cases Phase 2 exists to
expose, and would make the answer depend on whether kube-proxy happened to
know better, which it does not: it reads the same EndpointSlices.

Mesh VIPs keep kube-proxy's REJECT in Phase 1 and 2: they have no
EndpointSlices, so they never enter `cap_svc` (mesh addresses are matched
first), and what no mesh chain claims still falls to the passthrough. Moving
that refusal into the proxy too is *Open question 4*.

### Pod-sourced egress (Phase 3, optional)

Every proxy upstream connection is opened in the host netns today, mesh and
passthrough alike: the mesh identity rides in the certificate, not in the
source address. For a non-mesh destination there is no certificate, so the
source address is the only identity the destination sees, and it is the
node's.

Binding the upstream into the pod's netns would make it the pod's: Envoy's
`BindConfig` can name a netns (the `app_` clusters do,
`agent/internal/xds/proxy/cluster.go:186-199`), and the CNI's `meta mark
0xae7e accept` rule, defensive today, would become what keeps the proxy's own
connection from being captured again.

The cost is why it is optional:

- **Cardinality.** A netns is a property of a cluster's bind config, not of a
  connection. Pod-sourced egress means a cluster per (pod, Service port):
  O(pods × Services) on the node instead of O(Services). An upstream bind
  chosen per connection from filter state would avoid it, and the pinned
  Envoy has none; that is a proxy change, and a carried patch.
- **NetworkPolicy.** A destination's ingress policy that admits pods by
  selector starts to match (today it sees the node and either admits nodes or
  breaks); one written around today's node source stops matching. This is a
  behaviour change for whoever wrote policy against the mesh as it is.
- **conntrack and masquerade.** The connection is tracked in the pod netns and
  leaves through the pod's veth. To an external address it is masqueraded by
  the underlying CNI's rules (flannel's on the reference cluster) like an
  uncaptured pod's; to a pod address it is not.

Phase 3 is taken only if Phase 1's per-Service attribution leaves a real need
for the source address itself.

### mesh-dns upstream (Phase 4, optional)

mesh-dns could forward to the cluster DNS Service's endpoints directly,
resolved from its EndpointSlices, instead of to its ClusterIP. That removes the
last kube-proxy dependency of a meshed pod's ordinary traffic. It is a change
to the slim mesh-dns binary, which today links no Kubernetes client
(`//agent/cmd/mesh-dns:deps_test`); the agent would have to hand it the
endpoints through the snapshot file it already writes. Taken only if Phase 0
or an incident shows DNS through kube-proxy failing.

## Phases

The repository accepts breaking changes; this proposal needs none. Each
phase is additive to the snapshot, behind an agent flag the chart sets, and
an agent of the previous chart minor ignores what it does not know.

### Phase 0: measure

Before anything is built: how much of a node's egress is the "known non-mesh
Service" class, against "external"?

Nothing answers it today. The passthrough is one cluster per proxy, so
whatever label the cluster name reaches the metrics store under, the
destination class is not in it. Phase 0 adds, with no change of path:

- the Service-range branch of the matching tree, sending its connections to a
  second passthrough chain `cap_passthrough_svc`, a copy of `cap_passthrough`
  with its own stat prefix; the `tcp_proxy` downstream counters of the two
  stat prefixes are the split, per node;
- the same split for cleartext HTTP, by a second `ORIGINAL_DST` cluster
  `passthrough_service_range` that the Service-range branch's connections use;
- the `ServiceCIDR` watch the later phases need.

Gate to go on: the share of meshed egress connections that are the
Service-range class, per node, over a soak, and the number of distinct
`VIP:port`s per node (from the HTTP access log's `downstream_local_address`
and a sampled L4 log, if needed). If the share is negligible, this proposal
stops here and says so.

### Phase 1: EDS for known non-mesh Services

`cap_svc` with on-demand `svc:` clusters, the Service index, the scoped
EndpointSlice source, demand persistence. Behind an agent flag, default off in
the first chart minor that ships it and on in the next, after a soak. When the
flag is off the Service-range branch stays the Phase 0 passthrough copy.

### Phase 2: refusals

The `svc_refused` counter and reset for `no-endpoints`, `port-not-declared`,
`no-service`; the access-log reason. This is the phase that changes what a
caller sees (a reset from the proxy instead of a REJECT from the host), so it
ships default on only after a soak shows no caller depended on the difference.

### Phase 3: pod-sourced egress (optional)

See the design. Not started without a stated need and a measured cardinality.

### Phase 4: mesh-dns upstream (optional)

See the design.

## Failure modes

| case | today | under Option A |
|---|---|---|
| agent or proxy down | captured egress has no listener and fails | unchanged: captured egress still fails |
| new Service, first connection | kube-proxy programs it after its sync; until then REJECT | the agent learns it from its Service informer; until then `no-service` (Phase 2) or passthrough (Phase 1). Watch lag against kube-proxy's is measured in Phase 1 |
| endpoint becomes not-ready | kube-proxy removes it after its sync; established flows continue | EDS removes it after the agent's watch; established connections continue; outlier detection ejects a failing one sooner |
| Service ClusterIP changes (delete and recreate) | a new VIP | a new `svc:` cluster name; the old one is dropped with its demand, or kept empty while its subscription lives |
| port by name vs number | `targetPort` resolved per endpoint by the slice | the same: the slice's port of the Service port's name, per endpoint; a port the Service does not declare is `port-not-declared`, as 043 refuses an undeclared port of a mesh service |
| a user's Service fronting meshed pods | passthrough to the pods' application ports, plaintext, outside the mesh (`docs/runbook.md`, "a mesh service fronted by a Service of yours") | the same hop, now attributed. Not upgraded to mTLS: *Open question 3* |
| IPv6, dual-stack | not captured | not captured |
| Service range unknown (no `ServiceCIDR`, no chart value) | — | the branch is not built and everything stays on the passthrough, with a WARN and a gauge |

## Testing

- **Unit**: the matching tree with the Service-range branch (mesh addresses
  first, the range second, the HTTP branch after); the `svc:` cluster builder
  for each row of *The cluster* (conditions, `internalTrafficPolicy`, hints,
  affinity); the Service index; the admission and refusal of on-demand names,
  with the ODCDS strand rule held as the QUIC twins' tests hold it.
- **`//agent/test/envoy_validate`**: a capture listener with `cap_svc` and an
  `svc:` cluster validated by the pinned proxy; the on-demand `tcp_proxy`
  behaviour established above, as a live test.
- **kind e2e** (a new leg; seen red first): from a meshed pod, a non-mesh
  ClusterIP Service is reachable **with kube-proxy's rules for that Service
  deleted** from every node (`nft delete` of its chains, re-asserted through
  the test because kube-proxy re-syncs); red on today's build, green on Phase
  1. And an endpoint-less non-mesh Service yields the Phase 2 refusal and its
  counter, not `ECONNREFUSED` from the host.
- **Soak**: graded as today, plus Phase 0's split and, from Phase 1, the
  `svc:` cluster count per node, the snapshot size, and the first-connection
  latency of a Service (the on-demand wait).

## Alternatives

**Option B: the agent programs the host's nftables or eBPF.** That is writing
kube-proxy (or Cilium's kube-proxy replacement) again, for every pod and
every host process, with the conntrack, NodePort and health-check-node-port
semantics that come with it. It gains nothing that Option A does not, since
the gains come from the proxy resolving the Service, and a host datapath does
not see identities.

**Option C: the node proxy serves every ClusterIP, for every pod.** An
ambient-style node proxy. Non-meshed pods' traffic would be originated by the
node proxy under a node identity, breaking identity on every hop, and it would
still not cover host processes, the apiserver bootstrap or control-plane
nodes, so kube-proxy stays anyway.

**Option D: shrink kube-proxy** (fewer Services in its scope, a lighter mode).
It already costs under 1% of the data plane's CPU on the reference cluster.
There is nothing to win.

**A `/32` branch per non-mesh Service in every pod listener.** Simple and
needs no on-demand `tcp_proxy`, but each pod listener grows with the cluster's
Service count and every Service change rewrites every listener on every node.
Kept as the fallback if on-demand `tcp_proxy` does not work on the pinned
proxy, scoped to the node's demand set.

**Keep kube-proxy behind the proxy, but name the cluster** (an `ORIGINAL_DST`
cluster per Service port, for stats only). Attribution without the rest: no
outlier detection, no locality, and refusals still kube-proxy's. Considered as
a smaller Phase 1, and rejected because Phase 0 already gives the split it
would measure.

## Open questions

1. **Service range discovery.** `ServiceCIDR` objects need Kubernetes 1.33.
   Is a chart value acceptable as the only source on older clusters, and is a
   Service range that grows at runtime (a second `ServiceCIDR`) handled by
   rebuilding every capture listener, or does the branch take a list from the
   start?
2. **Non-mesh HTTP at L7.** Phase 1 carries it at L4. Retries, L7
   authorization and the HTTP refusal header need it in a connection manager
   with the cluster named per request (a header set from the restored address,
   then `on_demand`). Is that worth a second connection manager on every pod
   listener?
3. **A user's Service fronting meshed pods.** Its endpoints are mesh pods.
   Should `cap_svc` route it into the mesh (mTLS to `18008`, the service's
   routes and filters) instead of plaintext to the application port? That
   would change the "known limit, by decision (2026-10-12)" in the runbook,
   so it is the owner's call, and not part of this proposal unless taken.
4. **Mesh-VIP refusals.** Should a mesh VIP's unclaimed port also be refused
   by the proxy, with the same counter, instead of falling to the passthrough
   and kube-proxy's REJECT?
5. **Demand lifetime.** The QUIC twins' prune windows were tuned for twins.
   Is the same window right for `svc:` clusters, whose first connection waits
   on discovery and whose demand is often periodic (a batch job's database)?
6. **Egress authorization.** With a named destination, should the
   `HTTPFilter` / `EndpointPolicy` surface grow a way to attach a source-side
   policy to a non-mesh Service, or is that a later proposal?
