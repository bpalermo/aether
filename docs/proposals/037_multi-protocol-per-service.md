# Proposal 037: Multi-Protocol Ports on One Mesh Service

**Status:** Draft — not yet accepted.
**Author:** Bruno Palermo
**Date:** 2026-09-20
**Related:** #878 (`protocol: tcp` silently downgraded on the kubernetes backend —
fixable today, Phase 0 below), #877 (TCP services silently unroutable with SPIRE
off — adjacent, not this proposal), #868/#879 (L4 e2e harness), proposals 005
(multi-port routing), 018 Phase 3a/3b (TCP floor, L4 routes), 022 (redirect-all).

## Problem Statement

A mesh workload is classified **HTTP or TCP, for the whole pod**, and by
extension for the whole service. The classification is the pod annotation
`endpoint.aether.io/protocol` (`AnnotationEndpointProtocol`,
`common/constants/annotations/annotations.go`), parsed once per registration by
`getProtocolFromAnnotations` (`registry/cni.go`) into a single
`registryv1.Service_Protocol` that becomes part of the registry key. A pod that
serves an HTTP API on `:8080` **and** a raw-TCP protocol on `:9000` (Postgres
wire, Redis, MQTT, a custom binary framing, a TLS-terminating app) cannot say so.
Its options today:

- Register as HTTP (the default). `:8080` works. A client that dials
  `<svc>.<ns>.aether.internal:9000` is captured, `http_inspector` sees no HTTP
  preface, the connection falls to the redirect-all passthrough
  (`BuildCapturePassthroughFilterChain`, `agent/internal/xds/proxy/capture.go`)
  and reaches kube-proxy for a ClusterIP whose generated Service exposes only the
  mesh port (`registrar/internal/services/generator.go`, one `ServicePort` on
  `MeshPort`) — connection refused. No mTLS, no registry health, no SAN pin: the
  TCP port simply is not on the mesh.
- Register as TCP. `:9000` works through the floor. Every other port is now
  raw-forwarded to the *primary* port by the destination's default chain
  (`buildInboundTCPFloorFilterChain`, `agent/internal/xds/proxy/ingress.go`) —
  so `:8080` HTTP is delivered to `:9000`, and the HTTP cluster, outbound
  vhost and GAMMA `cap_http` vhost are never built (`clustersEndpointsAndVhosts`
  skips `entry.tcp`, `agent/internal/xds/cache/cluster.go`). HTTPRoutes attached
  to the service are inert.
- Split the workload into two pods behind two ServiceAccounts. The registry key
  is `<ns>/<serviceAccount>` (`serviceref`, proposal 020 Part 1), so "two mesh
  services" means "two identities", which means two Deployments. See
  *Alternatives*.

The gap also has a **silent** form, which is what #878 is: on the kubernetes
registry backend (the chart default) a TCP registration is accepted and then
dropped on read, so the operator gets `http` behaviour with `Accepted=True` on
their TCPRoute and no diagnostic. That is fixable without touching the
invariant, and this proposal makes it Phase 0 precisely so it does not wait for
the rest.

The proposal is the per-port model: **a port has a protocol; a service has
ports.** L7 already works that way (proposal 005: per-port clusters, per-port
EDS, SNI-demuxed inbound). This extends the same shape to the L4 class of each
port, so one service can carry HTTP, gRPC and raw TCP ports and every one of
them rides mTLS, EDS health, SAN pinning and the L4/L7 route vocabulary that
already exists for its class.

## Where the invariant lives today

It is stated once and encoded in six places. The proposal has to change all of
them, so they are listed here rather than discovered mid-implementation.

| Encoding | Where |
|---|---|
| The sentence: "A service is HTTP or TCP, never both, so the two sets never share a name." | `agent/internal/xds/cache/cluster.go:202` (`LoadClustersFromRegistry`) |
| Both cluster passes write the same map key: `c.clusters[serviceName]` | HTTP `buildHTTPServiceEntryLocked` at `cluster.go:337`; TCP `buildTCPClustersLocked` at `cluster.go:559` |
| The registrar's watch-fed cache is partitioned by protocol on the assumption the partitions never share a name | `registry/internal/registrar/registrar.go:93-99` |
| The registrar's write-behind supersedes a pending register "under exactly one (HTTP or TCP)" | `registrar/internal/server/server.go:144-149` |
| The mesh-Service generator claims one Service per name, HTTP first, and stamps a single `aether.io/app-protocol` | `registrar/internal/services/generator.go:92-114` |
| The cross-backend contract test asserts the two listings are disjoint | `registry/registrytest/contract.go` `RequireProtocolDisjoint` |
| The kubernetes backend returns nothing for `PROTOCOL_TCP` *because* of the map clobber | `registry/internal/k8s/kubernetes.go:102-118, 162-171` |

The last row is the important one: the kubernetes backend's behaviour is a
workaround for the agent's map key, documented as such in its own comment. The
registry has no opinion of its own about one-protocol-per-service; the etcd
backend keys `<svc>/protocols/<protocol>/endpoints/<ip>`
(`registry/internal/etcd/etcd.go:8, 630`) and would happily hold a service under
both.

## The Constraint That Drives Everything

**The TCP floor has no per-port signal on the mesh hop, so the destination has
exactly one floor chain, and that chain forwards to exactly one port.**

Trace a raw-TCP connection end to end on today's data plane:

1. **Source capture.** The pod's capture listener carries one
   `cap_tcp_<svc>` chain per TCP service, matched on `prefix_ranges
   <ClusterIP>/32` and nothing else (`buildCaptureTCPFloorFilterChain`,
   `agent/internal/xds/proxy/capture.go:198-219`). Destination *port* is not in
   the match. Any port on that VIP is the floor — proposal 018 says so
   explicitly ("with redirect-all capture the floor is exercised on any port").
2. **Source cluster.** The chain's `tcp_proxy` targets `tcp:<svc>.<ns>.<domain>`
   (`TCPClusterName`, `agent/internal/xds/proxy/egress.go:61`), built by
   `captureTCPClusters` (`agent/internal/xds/cache/capture.go:291`) with
   `InjectUpstreamTCPMTLS(cl, …, sanURIs, "")`. The empty last argument is the
   SNI, and the ALPN is suppressed (`upstreamTransportSocket`,
   `agent/internal/xds/proxy/transportsocket.go:322-336`: `alpnOverride == ""`
   → `alpn = nil`). The floor handshake therefore carries **no SNI and no ALPN**.
   This is deliberate and load-bearing: #306 established that a non-empty SNI
   would land on the destination's per-port HCM chain (`server_names` outranks
   `application_protocols` in Envoy's match order), and #304 removed the
   bespoke `aether-tcp` ALPN in favour of "h2 means HTTP, nothing means TCP".
3. **EDS.** The cluster's `EdsClusterConfig.ServiceName` is the bare service key
   — the same load assignment the HTTP cluster uses — and every LbEndpoint's
   address is `<pod_ip>:18008`, the mesh inbound
   (`ServiceLocalityLbEndpointFromRegistryEndpoint`, `egress.go:601`). The
   application port is **not** in the address. It never was, for HTTP either:
   005's per-port EDS is a membership filter (which pods advertise port P), not
   a port in the address.
4. **Destination inbound.** `buildInboundFilterChains` (`ingress.go:167-185`)
   emits the floor as the listener's **default** chain — no match criteria —
   and one HCM chain per served port matched on `server_names:[<port>]`. A
   no-SNI/no-ALPN connection matches nothing more specific and lands on the
   floor, whose `tcp_proxy` targets `app_<pod>_<defaultPort>`
   (`buildInboundTCPFloorFilterChain`, `ingress.go:199-209`). **The port choice
   is made here, from the pod's primary port, with no input from the caller.**

So the chain is: floor egress carries no port → inbound floor is one default
chain → one app port per pod → one TCP port per service. The single-map-key
collision in `c.clusters` is real and must be fixed, but fixing it alone yields a
service with HTTP ports *and* one TCP port, not a service with several TCP
ports, and not a service whose TCP port is anything but the primary.

### What is an Envoy limitation, and what is not

Almost none of this is Envoy.

- Envoy's filter-chain matcher evaluates `destination_port` **first**, ahead of
  `prefix_ranges`, `server_names`, `transport_protocol` and
  `application_protocols`, and with `use_original_dst: true` (which the capture
  listener already sets, `capture.go:152`) that port is the pre-REDIRECT
  destination port recovered by `original_dst`. A per-port capture chain is a
  one-field addition to a match that already exists.
- SNI is already the port selector on the mesh hop for HTTP (`entry.sni`,
  `agent/internal/xds/cache/cache.go:550-553`; `InjectUpstreamMTLS`,
  `egress.go:462`). Nothing stops a TCP floor cluster from carrying one; the
  only reason it does not is the collision with the HCM chain that has the same
  `server_names`. That collision exists because today the agent builds an HCM
  chain for *every* served port. Once a port is known to be TCP, the agent
  builds a `tcp_proxy` chain for that SNI instead of an HCM chain, and the
  collision is gone — the two chains have different `server_names`.
- EDS resource names are independent of cluster names. The default HTTP
  cluster, the `:<port>` aliases and the `tcp:` cluster already share one CLA by
  name (`buildPortAliasesLocked`, `cluster.go:419-428`). A `tcp:<fqdn>:<port>`
  cluster referencing the `<fqdn>:<port>` per-port CLA is the same trick.

What is genuinely constrained:

- **The mesh port is one port.** The generated mesh Service exposes exactly
  `MeshPort` (18081) and the *scoped* capture rule redirects only
  `dport == meshPort` (`cni/internal/plugin/capture.go:95-99, 132-143`). Dialing
  `<svc>:9000` is only captured under redirect-all
  (`programCaptureRedirectAll`, `capture.go:197`), which is the managed-pod
  default since 022. Per-port TCP therefore **requires redirect-all** (or an
  extension of the scoped rule to the registered port set — out of scope). This
  is already true for per-port HTTP: `<svc>:8080` on a scoped-capture pod is not
  captured either.
- **The registry key carries the protocol.** `RegisterEndpointRequest.protocol`
  (`api/aether/registrar/v1/registrar.proto`), the etcd key segment, the
  registrar snapshot key `serviceKey{ServiceName, Protocol, IP}`
  (`registrar/internal/server/snapshot.go:17`) and every watch event
  (`WatchEndpointsResponse.protocol`) treat protocol as a partition, not a
  property of a port. That is a modelling choice, and it is wire-visible, so it
  constrains the migration (below) more than the design.

## Design

### The resolution rule

Every client spelling of a service resolves to a `(port, protocol)` pair, and
the chain, cluster and EDS shape follow the protocol of that pair. This is the
one table the rest of the design serves.

| Client dials | Resolves to | Source path | Mesh hop | Destination chain |
|---|---|---|---|---|
| `<svc>.<ns>.<domain>` or `:18081` (mesh port) | primary port, primary's protocol | HTTP primary: HCM → `<fqdn>` (today). TCP primary: `cap_tcp_<svc>` (today's portless `/32` chain, see below) | HTTP: h2 + SNI=primary. TCP: no ALPN, no SNI (today) | HTTP: `in_<pod>_<p>` HCM. TCP: `in_tcp_<pod>` default floor → primary (today) |
| `<svc>…:<p>`, `p` is an HTTP port | `(p, HTTP)` | HCM → `<fqdn>:<p>` per-port cluster (005, today) | h2 + SNI=p (today) | `in_<pod>_<p>` HCM (today) |
| `<svc>…:<p>`, `p` is a TCP port | `(p, TCP)` | **new** `cap_tcp_<svc>_<p>` chain: `prefix_ranges <VIP>/32` + `destination_port p` → **new** `tcp:<fqdn>:<p>` cluster | no ALPN, **SNI=p** | **new** `in_tcp_<pod>_<p>`: `server_names:[p]`, `tcp_proxy` → `app_<pod>_<p>` |
| `<svc>…:<q>`, `q` registered by nobody | — | pure-TCP service: portless floor → primary (today's "any port" behaviour, kept). Otherwise: HCM catch-all → ODCDS/passthrough (today) | | |

Two consequences worth stating up front:

- **A pure-TCP service is byte-for-byte unchanged** for its primary port: same
  chain, same cluster, same no-SNI handshake, same default floor. Per-port
  additions only appear for non-primary TCP ports. The existing fleet does not
  move.
- **The primary port's protocol decides what the bare name means.** A mixed
  service with an HTTP primary keeps its `<fqdn>` h2 cluster and vhost; a mixed
  service with a TCP primary has no h2 default cluster and its HCM vhost
  carries only the `:<httpPort>` domains. The mesh-port spelling follows the
  primary in both cases. This is 005's "portless = primary" rule with the
  protocol attached; nothing new to learn.

### (a) Key `c.clusters` by cluster identity

`clusterEntry` (`cache.go:535`) already has the fields it needs: `service` is
the bare key for dependency-set and SAN purposes, `sni` is the port, `tcp`
marks the entry kind. What is wrong is only that the TCP entry is stored under
the bare service key. Per-port and alias entries are already keyed by their
Envoy cluster name (`c.clusters[portName]`, `c.clusters[alias]`).

Change: TCP entries are keyed by their Envoy cluster name — `tcp:<fqdn>` for the
primary-port floor entry, `tcp:<fqdn>:<port>` for per-port entries. The three
readers that look a TCP service up by bare key follow the key:
`captureTCPClusters` (`capture.go:326`), `edgeTCPClusters` (`capture.go:377`),
`captureUDPClusters` (`capture.go:461`, which reads `entry.sni` as the UDP app
port and is otherwise untouched), plus `edgeMeshClusterNameLocked`
(`agent/internal/xds/cache/edge.go:673`).

De-duplicating the shared CLA is the one subtlety. Today the bare-name load
assignment is emitted by whichever entry holds the bare key. Once an HTTP
default entry and a `tcp:<fqdn>` entry can coexist, both would carry a CLA
named `<svc-key>` and `clustersEndpointsAndVhosts` (`cluster.go:86`) would
emit two EDS resources with the same name — go-control-plane's snapshot
consistency check rejects that, or the later one silently wins. Rule: the
**bare-name CLA is owned by the entry that also owns the bare-name cluster**
(the HTTP default entry when the primary is HTTP; the `tcp:<fqdn>` entry when
it is TCP). The other entry references the CLA by name and carries
`loadAssignment == nil`, exactly as the `:<port>` aliases do today
(`buildPortAliasesLocked`, and the `entry.loadAssignment != nil` guard in
`retainAbsentClustersLocked`).

`RemoveEndpoint` (`cluster.go:25`) takes a cluster name and has
no non-test callers in the tree; it follows the new keys and nothing else
changes.

### (b) Per-port protocol classification

The `endpoint.aether.io/ports` grammar already carries a per-port suffix:
`"8080,9090=h2"`, parsed by `AppPortProtocols`
(`agent/internal/xds/proxy/cluster.go:105`) for the loopback codec and
stripped by `getPortsFromAnnotations` (`registry/cni.go:146-151`) for the
registry. The proposal extends the suffix vocabulary rather than adding an
annotation:

```
endpoint.aether.io/port:     "8080"
endpoint.aether.io/ports:    "8080,9090=h2,9000=tcp,5432=tcp"
endpoint.aether.io/protocol: "http"   # unchanged meaning: the DEFAULT for ports with no suffix
```

Suffixes: `h1` (explicit HTTP/1.1, also the default), `h2`/`http2` (as today),
`tcp` (new). `grpc` is accepted as an alias of `h2` — gRPC is h2 on the wire
and the mesh does nothing gRPC-specific at L4. A port with no suffix takes the
pod-level `endpoint.aether.io/protocol` value, which is why the pod-level
annotation is **kept, not deprecated**: it becomes the default for the port set,
and every existing manifest (`protocol: tcp` with one port) means exactly what
it meant before. A pod with `protocol: tcp` and `ports: "9000,8080=h1"` has a
TCP primary and an HTTP secondary.

One derived helper — `registry.PortProtocols(annotations) (map[uint32]Protocol,
error)` — replaces the three parsers that currently disagree by omission
(`registry/cni.go` `getPortsFromAnnotations`; `agent/internal/xds/proxy/cluster.go`
`AppPortsFromPod`/`AppPortProtocols`; `registry/internal/k8s/kubernetes.go`
`getPortFromAnnotations`, which reads neither `ports` nor `protocol`). An
unknown suffix is an error at registration, as an unknown `protocol` value is
today (`getProtocolFromAnnotations` rejects it "so a typo never silently
registers a service under the wrong protocol"). A rejected registration is
loud: the CNI ADD fails the pod's mesh registration and the agent logs it. The
alternative — today's `AppPortProtocols` silently treating an unknown suffix as
HTTP/1.1 — is exactly the class of failure this repo keeps paying for.

The pod's health probe follows the **primary** port's protocol, as it does
today (`NewAppDeliveryClusters`, `agent/internal/xds/proxy/listener.go:103`,
`isTCP` from the pod-level annotation; it becomes `portProtocols[primary] ==
TCP`). Liveness stays pod-level (034 made the same call for UDS).

### (c) EDS when ports carry different protocols

Keep 005's per-port buckets and type them. `buildHTTPEndpointBuckets`
(`cluster.go:459`) becomes `buildEndpointBuckets` and partitions each
endpoint's advertised ports by protocol:

- Bucket `(p, HTTP)` → CLA `<fqdn>:<p>`, consumed by the h2 cluster `<fqdn>:<p>`
  (today's shape).
- Bucket `(p, TCP)` → CLA `<fqdn>:<p>`, consumed by the `tcp:<fqdn>:<p>` cluster.
  Same CLA naming — a port has one protocol, so the name cannot collide.
- The bare-name CLA stays "every endpoint" (005: endpoints of one service share
  the primary), owned per rule (a).

Membership is filtered by **(port, protocol)**, not port alone. That is what
makes the rollout of a re-typed port safe, for the same reason 005 made adding a
port safe: a client of `tcp:<fqdn>:9000` only ever lands on pods that advertise
9000 *as TCP*; a pod still registered under the old annotation shape (9000 as
HTTP, if it declared it at all) is not in that CLA. Subset endpoints
(`envoy.lb` metadata) were considered for this and rejected: the subset LB
selects among hosts already in the cluster, so a wrong-protocol host would be
in the pool with a subset key on it, and criteria-less traffic
(`ANY_ENDPOINT` fallback, `NewServiceCluster`) could reach it. Per-port CLAs
keep the wrong host out of the pool entirely.

Per-port TCP clusters are `NewTCPServiceCluster(tcp:<fqdn>:<p>, <fqdn>:<p>,
<svc-key>)` (`egress.go:323`) and are SAN-pinned through the same
`refreshEntryMTLSLocked` path (`agent/internal/xds/cache/mtls.go:95`) as the
floor cluster: `entry.sanURIs` is computed for every entry, TCP included, and
`InjectUpstreamTCPMTLS(cl, nodeSpiffeID, vctx, sanURIs, strconv.Itoa(p))` sets
the SNI. Cluster stats stay keyed by the bare service (`AltStatName`).

### (d) Capture: `cap_tcp_*` beside `cap_http` for the same service

The reason a TCP service today must not be an HTTP service is stated in the
capture reconciler (`agent/internal/capture/reconciler.go:114-118`): a `/32`
destination-IP chain outranks the HCM catch-all's `application_protocols`
match, so a per-VIP chain would swallow HTTP to that VIP. `destination_port`
resolves it: the per-port chain

```
cap_tcp_<svc>_<p>:  prefix_ranges [<VIP>/32], destination_port p  → tcp_proxy → tcp:<fqdn>:<p>
```

matches only connections to that VIP **and** that port. HTTP to `<VIP>:8080`
does not match it and falls to the HCM chain as today. The portless
`cap_tcp_<svc>` chain (`/32` alone) is emitted **only when the primary is TCP
and the service has no HTTP ports** — the pure-TCP case, preserving "any port
reaches the floor". A mixed service with a TCP primary gets
`cap_tcp_<svc>` qualified with `destination_port 18081` (the mesh-port
spelling of the primary) plus one chain per TCP port; a mixed service with an
HTTP primary gets only the per-port chains.

TLSRoute SNI chains (`BuildCaptureTLSRouteFilterChains`) and TCPRoute weighted
floors (`BuildCaptureTCPRouteFilterChain`, `agent/internal/xds/proxy/l4route.go`)
attach to the service's floor chain today; they keep attaching to the
primary-port chain. Port-qualified L4 routes are Phase 3.

**Who decides which chains a service gets.** Today it is the capture reconciler,
reading `aether.io/app-protocol` off the generated mesh Service — a value the
*registrar* projected from a *per-pod* fact via one map
(`protocolAppProtocol`, `generator.go:67`). That is a second copy of the
classification, one hop removed from its source, and #878 is what happens when
the two copies disagree. The proposal moves the decision to the cache: the
reconciler keeps delivering `(service, ClusterIP)` for every mesh Service
(`CaptureTCPService` grows into `CaptureService`, emitted for all of them, not
just the non-HTTP ones), and the cache derives each service's TCP port set from
the **endpoints it already holds** — the same `port_protocols` the buckets are
built from. Chain and cluster then come from one source in one snapshot, which
is also the structural fix for the #877 asymmetry (a chain whose cluster was
never built).

The cost is a new regeneration trigger: capture listeners are per pod and
rebuilt on `SetCaptureTCPServices` (`capture.go:223-260`); they must now also be
rebuilt when a service's derived `(VIP, TCP port set)` changes after a registry
reload. This must be **edge-triggered on the derived map** — compare before
regenerating — because an LDS update of a per-pod listener drains that
listener's connections, and endpoint churn (the common reload) must not touch
listeners. See *Risks*.

`aether.io/app-protocol` on the mesh Service stays, stamped with the
**primary's** protocol, so an agent from before this proposal keeps classifying
exactly as it does now. The generator additionally stamps
`aether.io/port-protocols: "8080=http,9000=tcp"` for tooling and for the
webhook (below); the agent does not depend on it.

### (e) Inbound: per-port floor chains

`buildInboundFilterChains` (`ingress.go:167-185`) emits, per served port, an HCM
chain matched on `server_names:[<port>]`. For a TCP port it emits instead

```
in_tcp_<pod>_<p>:  server_names [<p>], DownstreamTransportSocket(...)  → tcp_proxy → app_<pod>_<p>
```

— the floor chain's shape (`buildInboundTCPFloorFilterChain`) with an SNI match.
The default no-match floor chain stays and still targets the primary port, so a
no-SNI floor connection (every source built before this proposal, and every
pure-TCP primary after it) behaves as today. There is never an HCM chain and a
TCP chain with the same `server_names`, so the listener is unambiguous — Envoy
rejects a listener whose chains it cannot disambiguate, which is what
`envoy --mode validate` will assert.

The per-port app cluster `app_<pod>_<p>` for a TCP port is `NewAppCluster(…,
http2=false)` (`agent/internal/xds/proxy/cluster.go:208`): no HTTP protocol
options are set unless `http2`, and `tcp_proxy` ignores them anyway. No change.

The cleartext (SPIRE-off) inbound (`buildInboundCleartextFilterChain`) has no
floor chain at all — that is #877's destination half — and gets no per-port
floor chains either. This proposal does not extend TCP to SPIRE-off; it makes
the gap loud (Phase 2 emits the same WARN #877 asks for, from the same derived
map).

### (f) What deliberately does not change

- The mesh hop address stays `<pod_ip>:18008` for every port and protocol. EDS
  is a membership filter; SNI is the port selector; the inbound demuxes.
  Proposal 019's waypoint (`<port>.<svc>.<ns>.<domain>` SNI) and 030's port
  migration are untouched.
- The floor's no-ALPN convention (#304) stays. HTTP is h2; TCP is nothing.
- `tcp:<fqdn>` for the primary port keeps **no SNI**. Changing that would break
  every destination whose inbound predates this proposal (they only have the
  default floor chain), and there is no need: the primary is what the default
  chain already forwards to.
- The UDP floor (`udp:` clusters, plaintext, app-port addressed) is a separate
  concept and is out of scope. UDP ports are declared by UDPRoute today, not by
  the endpoint annotation; unifying that is a later proposal.
- The health gateway, delegated liveness, two-phase drain, ODCDS cold path and
  the `:<port>` alias clusters all key on names that this proposal only adds
  to.

## Registry implications

### Semantics of `ListEndpoints(svc, protocol)`

Today: "the endpoints of a service, which is of this protocol". After: **"the
endpoints of a service that serve at least one port of this protocol."** A
mixed-service pod appears in both listings; each listing's `ServiceEndpoint`
carries the full port set with per-port protocols, and the consumer picks the
ports of the protocol it asked for. `RequireProtocolDisjoint`
(`registry/registrytest/contract.go`) is replaced by a contract that asserts the
opposite direction of honesty: every endpoint returned under protocol P
advertises at least one P port, and the two listings' `port_protocols` agree
for the same `(service, ip)`.

`ListAllEndpoints(TCP)` on a fleet with no TCP ports stays an empty map, so
`buildTCPClustersLocked` on today's fleet is a no-op, as it is now.

### Proto

`api/aether/registry/v1/endpoint.proto` (edition 2023, `field_presence =
IMPLICIT` — the registry protos are pinned at 2023, and this stays wire-compatible
by construction: new field, old readers skip it):

```proto
message ServiceEndpoint {
  // ... port = 3, ports = 11, health_check_mode = 10 ...

  // port_protocols is the wire protocol of each advertised port (proposal 037).
  // A port in `ports` that is absent here takes the protocol the endpoint was
  // registered under (RegisterEndpointRequest.protocol / the etcd key segment):
  // the pre-037 whole-pod classification, kept as the per-port default.
  // Writers populate it for every port; readers must tolerate its absence.
  map<uint32, Service.Protocol> port_protocols = 12;
}
```

Field 12 is the next free number (11 is `ports`). `Service.Protocol` gains no
values: `PROTOCOL_HTTP` covers h1/h2/gRPC (the codec is a loopback-hop detail
the registry never carried — the `=h2` suffix is stripped before registration
today and stays that way), `PROTOCOL_TCP` is the floor. The registrar wire
(`registrar.proto`) is untouched: `RegisterEndpointRequest.protocol` keeps its
meaning as the key protocol, and a pod that serves both is registered twice.

### Registration: one pod, up to two keys

The registering agent (`agent/internal/cni/server/pod.go:118-134`,
`liveness.go`, `termination.go`, `ghostsweep.go` — every caller of
`NewServiceEndpointFromCNIPod`) computes `port_protocols`, and calls
`RegisterEndpoint` **once per distinct protocol** the pod serves, with the same
`ServiceEndpoint` (full `ports`, full `port_protocols`) under each key. This is
the wire- and key-compatible option; the alternative — dropping the protocol
dimension from the key — touches every backend's key layout, the registrar
snapshot key, the write-behind key and every watch consumer, for no
functional gain.

What already copes with a two-key pod:

- etcd `UnregisterEndpoints` enumerates the service's protocol directories and
  deletes the IP from each (`etcd.go:288-330`), and the registrar's unregister
  fans out over `syncedProtocols` (`server.go:144-150`) — the comment there
  ("registered under exactly one") describes why it *already* loops, so the
  behaviour is right and only the comment moves.
- The registrar snapshot keys `(service, protocol, ip)` and counts services
  "across protocols" (`snapshot.go:53`); the watch stream tags each event with
  its protocol; the registrar client keeps per-protocol partitions
  (`registrar.go:99`). All of them hold two entries for one pod without
  modification.
- Delegated liveness promotes health per key. The agent's promotion path
  re-registers with `HEALTH_HEALTHY` (`liveness.go:508`); it must do so under
  every key the pod holds, or the TCP listing stays UNHEALTHY forever while
  HTTP is promoted. This is the one registration-side change that is easy to
  miss and silent if missed — see *Risks*.

### The two backends converge

`podToEndpoint` (`kubernetes.go:324-362`) today reads `port`, `weight`,
`metadata` and `health-check-mode` — **neither `protocol` nor `ports`**. The
second omission means the kubernetes backend has never carried 005's per-port
EDS membership either; every endpoint it returns has `Ports == nil`, so
`buildHTTPEndpointBuckets` falls back to `{port}` and per-port clusters are
empty on that backend. Both parsers move to the shared helper, and
`ListEndpoints`/`ListAllEndpoints` filter pods by "has a port of protocol P"
instead of returning nil for TCP. Phase 0 does the `protocol` half of this on
the existing per-pod model.

### Registrar generator

`buildDesiredServices` (`generator.go:90-124`) stops claiming HTTP-first and
instead merges the two listings per service key: `port` is the primary (from
either listing — endpoints of one service share it), `appProtocol` is the
primary port's protocol, and the new `port-protocols` annotation is the union.
The generated Service still exposes only `MeshPort`; per-port dialing rides
redirect-all, as it already does for HTTP ports.

## Migration path

The annotation model is additive and the old shape is the default for the new
one, so there is no flag day. The mixed-fleet cases, in the direction each
piece of state actually flows:

| State | Written by | Read by | Old writer / new reader | New writer / old reader |
|---|---|---|---|---|
| Pod annotation `ports: …=tcp` | the user | the pod's **own node** agent (registration + inbound listener) | n/a | Old agent strips the suffix (`getPortsFromAnnotations`) and `AppPortProtocols` treats it as HTTP/1.1: the port is registered HTTP and served by an HCM chain. **Silent.** Mitigation: the pod-mutating webhook (`controller/internal/podmutate`, rolled with the controller ahead of the agent) rejects `=tcp` until the chart's agent version supports it; documented skew rule: roll the agent DaemonSet before annotating |
| `port_protocols` on the endpoint | registering agent | every client agent | Absent → the client applies the key protocol to every port (today's behaviour, by definition of the field) | Old client ignores it, sees the TCP port in `ports`, builds an h2 `<fqdn>:9000` cluster + vhost domain. A raw-TCP dial is captured, fails `http_inspector`, passes through to kube-proxy, and is **refused** (the Service has no port 9000). Loud, and identical to what that client does today |
| Second registration key | registering agent | registrar, backends, generator | n/a | Old registrar: two keys, generator claims HTTP-first → `app-protocol: http`, old agents treat the service as HTTP (today's behaviour). Old etcd/k8s backend: stores both, harmless |
| SNI=port on the per-port floor cluster | client agent | destination pod's inbound | n/a | Cannot occur across versions: a `tcp:<fqdn>:<p>` cluster is built only for pods advertising `(p, TCP)`, and only a new agent advertises that — the same agent that built the destination's `in_tcp_<pod>_<p>` chain |
| `aether.io/app-protocol` on the mesh Service | registrar | capture reconciler | Old registrar stamps the HTTP-first value → new agent's derived map disagrees for TCP-primary mixed services; the agent trusts its own endpoint-derived map and logs the disagreement once | New registrar stamps the primary's protocol → old agent behaves as today |

A cluster running `endpoint.aether.io/protocol: tcp` on every pod of a
service, with one port, sees no change at any phase: the key protocol is TCP,
`port_protocols` is `{primary: TCP}`, the cache builds `tcp:<fqdn>` under its
new key with the same bytes, the capture listener carries the same portless
`/32` chain, and the inbound's default floor is the only floor.

The only removal in the whole plan is the kubernetes backend's `return nil`
for TCP, and Phase 0 makes that removal safe before anything else moves.

## Phasing

### Phase 0 — #878, on the existing invariant (prior art, ships first)

`podToEndpoint` honours `endpoint.aether.io/protocol` (and populates `Ports`
from `endpoint.aether.io/ports`, closing the 005 gap on this backend);
`ListEndpoints(svc, P)`/`ListAllEndpoints(P)` return the pods whose pod-level
protocol is P, **for both protocols** — returning HTTP-declaring pods under TCP
is the bug #430 fixed, and returning TCP-declaring pods under HTTP would be the
same bug mirrored. Since every pod behind one ServiceAccount carries the same
annotation in any sane manifest, "HTTP or TCP, never both" still holds;
`RequireProtocolDisjoint` keeps passing and the map key is not touched. The
shared parser (`registry.PortProtocols`, still with only the pod-level
annotation feeding it) lands here so the two backends stop drifting. A
mixed-annotation ServiceAccount — pods of one service disagreeing — now
collapses to TCP on this backend exactly as it already does on etcd; Phase 1
fixes that for both. Independent of the rest; can merge this week.

### Phase 1 — key the cache by cluster identity

Design (a). TCP entries keyed `tcp:<fqdn>`; the three readers follow; the
bare-name CLA has one owner. `RequireProtocolDisjoint` is retired in the same
PR and replaced by the per-protocol honesty contract. Behaviour change: a
service whose pods split across protocols now gets **both** an h2 cluster +
vhost and a `tcp:` floor cluster (the primary port of each set), instead of the
TCP set clobbering the HTTP one. That is the *service-level* multi-protocol
milestone, and it is what a user who put `protocol: tcp` on half a Deployment
gets today by accident. No proto, no annotation, no wire change; fully
covered by unit tests on the cache and `envoy_validate`.

### Phase 2 — per-port protocol

Design (b)–(e): the `=tcp` suffix, `port_protocols` on the proto, dual
registration and dual promotion, typed buckets, per-port `tcp:<fqdn>:<p>`
clusters with SNI, `destination_port`-qualified capture chains from the
cache-derived map, per-port inbound floor chains, the generator's merged
classification, the webhook guard, and the workload-requirements docs (which
today list neither `protocol` nor `ports` in the annotation table,
`docs/workload-requirements.md:42-49`). Two chart releases: controller +
registrar first (webhook guard, generator), then the agent. Validated on kind
(the #879 harness, extended) and then a talos roll under the prober.

### Phase 3 — port-qualified L4 routes and the edge

`common/l4project` backends are "NOT port-qualified" by design
(`l4project.go:53-55`) because the floor had one port. A TCPRoute/TLSRoute
`backendRef.port` naming a non-primary TCP port resolves to
`tcp:<fqdn>:<p>`; the edge's `edgeTCPClusters` builds the same per-port
clusters with `EdgeUpstreamTCPTransportSocket` grown an SNI parameter. GAMMA's
`ProjectHTTPRule` should reject (ResolvedRefs=False, reason
`UnsupportedProtocol`) a `backendRef.port` that resolves to a TCP port rather
than build an h2 cluster to it — open question below on whether the projector
can see the protocol.

## Alternatives considered and rejected

- **Keep the invariant; require users to split into two Services.** The mesh
  Service is *generated* from the registry, one per `<ns>/<serviceAccount>`
  (`generator.go`); users cannot create a second mesh Service for the same pods.
  "Two Services" therefore means two ServiceAccounts, which means two pods —
  the app has to be split at the process boundary, and every identity-keyed
  policy (SAN pins, RBAC on XFCC, `HTTPFilter` attachment) has to be duplicated.
  For a database with an HTTP admin port, or an app with a metrics port and a
  binary wire protocol, that is not an option the user has.
- **Infer per-port protocol from `Service.spec.ports[].appProtocol`.** Three
  problems. The registry discovers pods by ServiceAccount, not by Service
  selector, so there is no user Service to read — the only Service the mesh
  knows is the generated selectorless one with a single `mesh` port. Every
  other registration fact (`port`, `ports`, `weight`, `health-check-mode`,
  `uds-socket`) lives on the pod, and a pod that is scheduled before its Service
  exists must still register correctly. And `appProtocol` is free-form
  (`kubernetes.io/h2c`, `kubernetes.io/ws`, anything), which would push
  vocabulary policing to every reader. A later convenience — the controller
  projecting a user Service's `appProtocol` onto the pod annotation at admission
  — is compatible with this design and can be its own proposal; it is not a
  substitute for the registry carrying the fact.
- **Sniff the protocol in Envoy.** The capture listener already sniffs at the
  source (`http_inspector`, `tls_inspector`), and that is exactly where the
  known failure modes live: server-first protocols and sub-6-byte first writes
  never satisfy the inspector, hence the 1s `ListenerFiltersTimeout` +
  `ContinueOnListenerFiltersTimeout` hack (`capture.go:140-151`). More
  fundamentally the *destination* cannot sniff: the bytes arrive inside mTLS,
  listener filters run before TLS, and the chain — HCM or `tcp_proxy` — must be
  chosen before a byte of plaintext exists. And clusters are typed at snapshot
  time (h2 protocol options or none); there is nothing to sniff when the config
  is generated. A declared fact is the only thing that works on both ends.
- **A new annotation instead of a suffix.** `endpoint.aether.io/port-protocols:
  "9000=tcp"` alongside `ports`. Rejected because the two lists then have to be
  reconciled (a port in one but not the other) and because `ports` already has
  a per-port suffix with an established parser; one grammar, one place.
- **Drop the protocol dimension from the registry key.** Cleaner model, but it
  changes the etcd key layout (`<svc>/protocols/<p>/endpoints/<ip>`), the
  registrar snapshot key, the write-behind key and the watch event shape, and
  needs a dual-read migration across every backend. Dual registration under the
  existing keys gets the same result with zero wire change.
- **Subset endpoints instead of per-port CLAs.** See (c): a wrong-protocol host
  would be in the pool.

## Risks

Every gate below states what would make it fail. A gate that cannot fail is
decoration (#853).

1. **A capture chain naming a cluster that is not in the snapshot** (the #877
   class; `tcp_proxy` has no ODCDS cold path, so this is a silent connection
   kill). Structural mitigation: chains and clusters are derived from the same
   cache entries in the same snapshot generation. Gate: a cache unit test that,
   for every pod's capture listener in a generated snapshot, resolves every
   `tcp_proxy` cluster (single and `weighted_clusters`) against the snapshot's
   CDS set. What makes it fail: deleting one `tcp:` entry from the map before
   snapshot generation must trip it — the test includes that mutation as a
   negative control. `envoy --mode validate` cannot stand in for this: dynamic
   cluster references are not resolved in validate mode.
2. **A per-port TCP cluster with no SAN pin** (the #832 class). `entry.sanURIs`
   is computed for every entry by `refreshEntryMTLSLocked`; the new clusters
   take it through `InjectUpstreamTCPMTLS`. Gate: `UnpinnedMeshClusters`
   (`test/envoy_validate/builders.go:994`) is structural — every cluster with an
   `UpstreamTlsContext` is checked — so a new `tcp:<fqdn>:<p>` cluster is in
   scope the moment the builder emits it. What makes it fail: the bootstrap
   builder emitting the per-port cluster with `sanURIs == nil` is caught by the
   existing assertion. The runtime counterpart is `reportUnpinnedClusters`
   (`mtls.go`), which WARNs and counts per snapshot.
3. **Inbound listener NACK from ambiguous chains.** An HCM chain and a TCP chain
   with the same `server_names` would make Envoy reject the pod's LDS update,
   leaving the pod on its previous listener — or, for a new pod, unreachable.
   Gate: `envoy_validate` gains a mixed-pod inbound bootstrap (an HTTP primary,
   an h2 port, a TCP port). What makes it fail: emit both chains for the TCP
   port; validate rejects the listener.
4. **Listener drain on capture regeneration.** Regenerating a pod's capture
   listener drains its connections. The derived `(VIP, TCP port set)` map must
   be compared before any regeneration, and the comparison must be over the
   derived map, not the reload event. Gate: a cache test that runs two registry
   reloads differing only in endpoint IPs and asserts the capture listener
   resource is pointer-identical (not re-generated). What makes it fail:
   regenerating on every reload. Soak signal: `listener_manager.listener_modified`
   flat during churn.
5. **Half-promotion.** Delegated liveness re-registers `HEALTH_HEALTHY` under
   one key; the other key's endpoint stays UNHEALTHY and the TCP port is never
   routable — no error, the cluster is just empty of healthy hosts. Gate: a
   registry contract test that registers a dual-protocol endpoint, promotes it,
   and asserts both listings report HEALTHY. What makes it fail: promoting under
   the key protocol only (which is the pre-change code).
6. **Old agent, new annotation** (silent downgrade to HTTP on the pod's own
   node). Mitigation is the webhook guard plus the documented skew rule. Gate:
   the webhook test rejects `=tcp` when the chart's agent version is below the
   floor. What makes it fail: nothing on the agent side — this is a *process*
   gate, and it is stated as such rather than pretended to be a data-plane one.
7. **Redirect-all off.** Per-port TCP is not captured under scoped capture. Not
   a regression (per-port HTTP has the same property), but it is a
   "configured, accepted, inert" shape. The agent WARNs once per service with
   TCP non-primary ports when the node's capture mode is scoped, naming the
   ports, so the operator can find it.
8. **Registrar generator flapping.** Two listings for one service could produce
   two `desiredService` values and a Service whose annotations oscillate across
   reconciles (map order is not deterministic; #135 is the same mechanism on the
   route table). The merge must be order-independent: primary from `port`, which
   is shared; protocol map as a sorted union.

## Testing and validation

**`test/envoy_validate/`** (every PR, the exact proxy binary):

- Mixed-pod inbound bootstrap (risk 3).
- Capture bootstrap with `cap_tcp_<svc>_<p>` (`destination_port`) beside the
  HCM catch-all and the passthrough default chain; a second variant with the
  portless `/32` floor plus per-port chains (TCP primary, mixed service).
  Validate proves the listener is accepted and unambiguous; it cannot prove
  which chain a connection selects.
- Per-port `tcp:<fqdn>:<p>` cluster with SNI, through `UnpinnedMeshClusters`.
- The existing `TestCaptureTCPRoute*`/`TestCaptureTLSRoute*` fixtures keep
  attaching to the primary-port chain and must not change bytes until Phase 3.

**Cache unit tests** (`agent/internal/xds/cache`): risk 1's chain⇔cluster
resolver with its negative control; risk 4's pointer-identity test; the
Phase 1 "both listings" case (h2 cluster + vhost + `tcp:` cluster + one
bare-name CLA, no duplicate EDS names — assert on the snapshot's resource list,
not on the map).

**Registry contract** (`registry/registrytest`): the per-protocol honesty
contract, run against etcd (testcontainers, `integration` tag) and the
kubernetes fake; risk 5's dual promotion.

**kind e2e**: extend the #879 harness with a mixed workload (HTTP `:8080`
primary, raw echo `:9000=tcp`) and assert, from a client pod: HTTP on 8080
through `cap_http` (access log shows the vhost), raw bytes on 9000 through
`cap_tcp_<svc>_9000` (the chain's `tcp_proxy` stats increment, and the
inbound's `in_tcp_<pod>_9000` chain stats increment — asserting **both ends**
is what makes the mesh hop's SNI selection observable), and 9000 refused when
the annotation is removed (the negative control). SPIRE on (the floor is
mTLS-only; #877). Backend: both etcd and kubernetes, since the point of Phase
0/2 is that they agree.

**talos**: a roll of the agent DaemonSet with one mixed workload deployed, under
the external prober (100% bar on liveness/reachability), then a short soak with
the `k6` mesh_dns leg and `listener_modified` watched for the drain risk. Grade
after a post-upgrade proxy roll, per the standing rule that stale series are not
zero series.

## Open questions

- **GAMMA and TCP ports.** `ProjectHTTPRule` (`common/gammaproject/project.go`)
  resolves `backendRef.port` to `<fqdn>:<port>` without knowing the port's
  protocol — the projector runs in the registrar's config-export controller as
  well as the agent, and neither reads endpoints there. Either the projector
  grows a protocol lookup (a new input, threaded through 026's export path) or
  the agent's cache refuses to build an h2 per-port cluster for a TCP port and
  the route 503s. The second is loud but late; the first is the right place.
  Not resolved from the code.
- **Delegated liveness for the TCP port.** The probe is primary-port only and
  follows the primary's protocol. A mixed service whose TCP port is down while
  its HTTP primary is up is reported HEALTHY under both keys. Per-port probes
  are a 034-style "deferred until demanded" item; the proposal keeps liveness
  pod-level and says so in the docs.
- **The `grpc` suffix.** Accepting it as an h2 alias is convenient and matches
  `isHTTPAppProtocol` (`reconciler.go:78`), but it advertises a distinction the
  data plane does not make. Leave it out until something consumes it?
- **Waypoint (019) and per-port TCP.** `InjectUpstreamTCPMTLS` has no waypoint
  SNI parameter; cross-cluster raw TCP through the E/W waypoint is not port
  demuxed today either. Out of scope, but the per-port SNI (`<p>`) and the
  waypoint SNI (`<p>.<svc>.<ns>.<domain>`) must not be confused when someone
  does it — the two-level matcher in `InjectUpstreamMTLS` is the template.

## Adjacent, not this proposal

**#877** — with `--spire-enabled=false`, `captureTCPClusters` returns early
(`capture.go:315`) while `generateCaptureListener` (`capture.go:83`) has no
matching gate, so the capture listener carries `cap_tcp_*` chains with no
cluster behind them; the cleartext inbound has no floor chain at all. Design (d)
happens to close the source half structurally (chain and cluster from one map),
but the fix #877 asks for — say it out loud — should not wait for Phase 2, and
the destination half (no cleartext floor) is a separate decision about whether
raw TCP should ride the mesh without mTLS at all. This proposal assumes it
should not.
