# Proposal 037: Multi-Protocol Ports on One Mesh Service

**Status:** Accepted. Phase 0 shipped (#888, merged 2026-09-21); Phase 1 next.
**Author:** Bruno Palermo
**Date:** 2026-09-20
**History:** first draft resolved the bare-name spelling to "the primary port,
in the primary's protocol". The PR #881 review rejected that as re-introducing
the asymmetry the proposal removes. This revision adds a dedicated TCP mesh
port and a scheme-typed resolution rule — a portless URL resolves by its
scheme's default port — traced against the code rather than stated. A
"require a port from every caller" variant was considered and dropped in the
same review (see *Alternatives*).
**Related:** #878 (`protocol: tcp` silently downgraded on the kubernetes backend —
fixable today, Phase 0 below), #877 (TCP services silently unroutable with SPIRE
off — adjacent, not this proposal), #868/#879 (L4 e2e harness), proposals 005
(multi-port routing), 018 Phase 3a/3b (TCP floor, L4 routes), 022 (redirect-all),
030 (the 18xxx port convention).

## Problem Statement

A mesh workload is classified **HTTP or TCP, for the whole pod**, and by
extension for the whole service. The classification is the pod annotation
`endpoint.aether.io/protocol` (`AnnotationEndpointProtocol`,
`common/constants/annotations/annotations.go`), parsed once per registration by
`getProtocolFromAnnotations` (`registry/cni.go`) into a single
`registryv1.Service_Protocol` that becomes part of the registry key. A pod that
serves an HTTP API on `:8080` **and** a raw-TCP protocol on `:9000` (Postgres
wire, Redis, MQTT, a custom binary framing, an app that terminates its own TLS)
cannot say so. Its options today:

- Register as HTTP (the default). `:8080` works. A client that dials
  `<svc>.<ns>.aether.internal:9000` is captured, `http_inspector` sees no HTTP
  preface, the connection falls to the redirect-all passthrough
  (`BuildCapturePassthroughFilterChain`, `agent/internal/xds/proxy/capture.go`)
  and reaches kube-proxy for a ClusterIP whose generated Service exposes only the
  mesh port (`registrar/internal/services/generator.go`, one `ServicePort` on
  `MeshPort`). No mTLS, no registry health, no SAN pin: the TCP port simply is
  not on the mesh.
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
already exists for its class. It has two halves, and the second is not a
corollary of the first:

1. **A dedicated TCP mesh port** beside the HTTP one, so the mesh has a
   protocol-neutral name (`<svc>.<ns>.<domain>`) and two protocol-typed default
   ports (`:18081` HTTP, `:18082` TCP), instead of a name whose protocol flips
   with the workload's primary port.
2. **A scheme-typed resolution rule.** A portless URL resolves by its scheme's
   default port — `http://` is port 80 and HTTP by definition, `https://` is
   443 — and raw TCP, which has no URL scheme, names its port. What a spelling
   means then follows from the address the client wrote, never from the
   workload's annotation. The rule rests on a distinction that is intrinsic to
   the two protocols and that this proposal gives prominence to: **HTTP
   demuxes on the authority header; TCP demuxes on the 5-tuple.**

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

The HTTP path has none of this because **the HCM never looks at the destination
port**. It routes on `:authority`, whose port is retained (`strip_any_host_port`
is set nowhere in `agent/`; `route.go:113, 279` document this as 005's choice),
against vhost domains that spell the portless name, `:18081`, and any per-port
or GAMMA-declared port (`captureVhostDomains`, `agent/internal/xds/cache/capture.go:550-551`;
`routeTargetDomains`, `capture.go:820-829`). For HTTP the *bytes* and the
*authority* carry the protocol and the port; for TCP the destination port is
the only signal there is. That asymmetry in signal is the whole reason the two
protocols need different rules below, and why one of them can have a
protocol-neutral default and the other cannot.

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

- **There is one mesh port, and it is HTTP-shaped.** `ProxyOutboundPort = 18081`
  (`common/constants/mesh/mesh.go:9`) is at once the per-pod outbound HTTP
  listener's port, the single `ServicePort` on every generated mesh Service
  (`generator.go:197-202`; the MCS clusterset VIP has the same single port,
  `registrar/internal/mcs/import_generator.go:301`), and the only destination
  port the *scoped* capture rule redirects (`programCaptureRedirect`,
  `cni/internal/plugin/capture.go:95-99`, one `dport == meshPort` compare in
  `captureRedirectExprs`, `capture.go:132-143`). Every other port on a mesh VIP
  is captured only under redirect-all (`programCaptureRedirectAll`,
  `capture.go:197`), the managed-pod default since 022.
- **The registry key carries the protocol.** `RegisterEndpointRequest.protocol`
  (`api/aether/registrar/v1/registrar.proto`), the etcd key segment, the
  registrar snapshot key `serviceKey{ServiceName, Protocol, IP}`
  (`registrar/internal/server/snapshot.go:17`) and every watch event
  (`WatchEndpointsResponse.protocol`) treat protocol as a partition, not a
  property of a port. That is a modelling choice, and it is wire-visible, so it
  constrains the migration (below) more than the design.

## Two mesh ports

`ProxyTCPOutboundPort = 18082` joins `ProxyOutboundPort = 18081` in
`common/constants/mesh/mesh.go`, a chart-fixed constant like every other
data-plane port (030's argument against per-node flags applies verbatim: a
port two ends must agree on is mesh-wide by nature).

**18082 is free.** The 18xxx numbers in use are 18001 (capture), 18008 (inbound),
18009 (E/W tunnel), 18021 (edge readiness), 18054 (mesh DNS), 18080 (e2e
fixtures) and 18081. The only occurrences of `18082` in the tree are an
arbitrary capture-port argument in `agent/internal/xds/proxy/l4route_test.go:236-255`;
nothing binds, documents or templates it. It is not IANA-registered and sits
outside Istio's 15000-15090 band. Adjacent to 18081 so the pair reads as one
convention: **18081 speaks HTTP, 18082 speaks TCP.**

What the port means, precisely:

- `<svc>.<ns>.<domain>:18081` — the service's **default HTTP port**, over the
  HCM path. Unchanged.
- `<svc>.<ns>.<domain>:18082` — the service's **default TCP port**, over the
  floor. New.
- "Default port of protocol P" = the pod's primary port (`endpoint.aether.io/port`)
  if it is of protocol P, else the lowest-numbered port of protocol P, else
  none. Deterministic, order-independent, and equal to "the primary" for every
  single-protocol service that exists today.

The bare name is protocol-neutral: mesh DNS answers it with the ClusterIP
(`SetMeshDNSRecords`, `agent/internal/capture/reconciler.go:112`), which has no
protocol. The **address the client writes** decides what happens next — the URL
scheme for HTTP (`http://` dials 80, `https://` dials 443), an explicit port for
raw TCP (`:18082` or a registered TCP port). Nothing about the *service*
decides. That is the whole of what the two ports buy: the bare name stops
meaning "the primary's protocol" not because clients must name a port, but
because TCP gains a well-known port of its own while HTTP keeps the
authority-keyed spelling it has always had.

What the port does to the plumbing, each verified against the code:

1. **The portless `/32` floor chain and its special cases collapse into one
   uniform chain.** `cap_tcp_<svc>` becomes `prefix_ranges <VIP>/32 +
   destination_port 18082`, emitted for every in-scope service that has a TCP
   port. Match ordering holds: `destination_port` is Envoy's first tier, so the
   chain wins over the HCM catch-all (which has no `destination_port`) and over
   the passthrough `DefaultFilterChain`, and it cannot shadow HTTP to
   `<VIP>:8080` or `<VIP>:18081`. The reconciler's rule that "HTTP VIPs must NOT
   get per-IP chains" (`reconciler.go:114-118`) is what this replaces: the chain
   is per-IP-and-port now, so an HTTP-bearing VIP can have one.
2. **The mesh hop for the default TCP port is byte-identical to today** when
   the default TCP port is the pod's primary — the pure-TCP case, i.e. every
   TCP service in the current fleet: same `tcp:<fqdn>` cluster, same bare-name
   CLA, no ALPN, no SNI, same destination default floor chain. Only the *source*
   chain's match changes (it gains `destination_port`), which is an LDS update
   on the agent roll, not a data-path change. When the default TCP port is
   **not** the primary (a mixed service with an HTTP primary), `tcp:<fqdn>`
   carries `SNI=<port>` and lands on the per-port inbound floor chain (design
   (e)) — a shape that only exists on destinations built by this proposal, so
   it can never meet an old inbound.
3. **The scoped CNI rule extends to `dport ∈ {18081, 18082}`.**
   `captureRedirectExprs` is one `expr.Cmp` per rule; a second TCP rule for
   18082 is the whole change (UDP stays 18081-only — the UDP floor is a
   different design). This makes the default TCP spelling work **without
   redirect-all**, the same footing HTTP's default spelling has. Per-app-port
   TCP (`:9000`, `:5432`) still needs redirect-all, exactly as per-app-port HTTP
   (`:8080`) does today. It is a two-tier story and the doc says so wherever it
   matters (resolution table, phasing, Risk 7).
4. **Every generated mesh Service exposes the spellings it answers to.**
   `generator.go:197-202` grows from one `ServicePort` to four name-only ports:
   `mesh` 18081 (today), `mesh-tcp` 18082, `http` 80 and `https` 443; the
   clusterset VIP (`import_generator.go:301`) the same. Uniform — a Service is
   a VIP-plus-name handle, a port on it is a name, not a promise of a backend,
   and the port list becomes the documented dial surface. Three consequences:
   (i) the selectorless shape carries four ports as cleanly as one (no
   EndpointSlices either way); (ii) a dial to any of these four that **no
   capture chain claims** reaches a real Service port with no endpoints, which
   kube-proxy programs as a REJECT — `ECONNREFUSED`, immediately, on every CNI
   — instead of a ClusterIP port no rule owns (today's `:9000` case, which is
   CNI-dependent and typically a hang); this is the deterministic floor under
   the skew case (Risk 9), the "service has no TCP port" case, the `https://`
   case (below) and, under scoped capture, the `http://` case; (iii) 80 and 443
   on a mesh Service must not be read as "the mesh serves plaintext HTTP / the
   mesh terminates TLS" — the doc says what they are. One gap to close: `apply`
   (`generator.go:161-176`) converges only annotations on an *existing*
   Service, so a pre-037 Service would keep one port forever; the generator
   must converge `Spec.Ports` too (the MCS `ServiceImport` path already does,
   `import_generator.go:194-196`; its clusterset Service create is create-only
   and needs the same).

What must **not** learn about 18082: `aliasAuthorityPorts` (`cluster.go:439`)
must not publish an h2 `<fqdn>:18082` alias, and neither `captureVhostDomains`
nor `routeTargetDomains` may spell `:18082` as an HTTP domain. An HTTP request
whose `Host` names `:18082` is HTTP to a TCP port; it falls to the on-demand
catch-all for a cluster name that is never published and 503s after
`onDemandClusterTimeout` (2s, `agent/internal/xds/proxy/httpfilter.go:32`).
Correct, and loud.

## What a spelling means: the scheme types the URL

```
http://<svc>.<ns>.<domain>/     → dport 80    → default HTTP port
https://<svc>.<ns>.<domain>/    → dport 443   → port 443, iff registered as TCP (app-level TLS); else refused
<svc>.<ns>.<domain>:18081       → dport 18081 → default HTTP port (explicit spelling; unchanged)
<svc>.<ns>.<domain>:18082       → dport 18082 → default TCP port  (the new TCP mesh port)
<svc>.<ns>.<domain>:<p>         → dport p     → port p, in p's protocol
```

Why the two protocols get one rule with two mechanisms: **HTTP demuxes on the
authority header; TCP demuxes on the 5-tuple.** An HTTP URL's port is a string
in `Host`, matched as a vhost domain, and its scheme fixes the protocol before
a byte is sent; the TCP destination port is never consulted for HTTP and the
proposal keeps it that way. Raw TCP has no scheme and no header, so the
destination port is the only signal it has — which is exactly why a raw-TCP
client names its port and an HTTP client need not. Every trace below is
against the code, assuming redirect-all (the default) with scoped mode called
out where it differs.

### `http://<svc>.<ns>.<domain>/` — port 80

The client dials 80. Under redirect-all the connection is captured
(`redirectAllTCPExprs`, every non-excluded outbound TCP). On the capture
listener no `cap_tcp` chain matches (today they are `/32` on TCP VIPs only;
after this proposal every one carries a `destination_port` that is not 80);
`http_inspector` detects HTTP on a `raw_buffer` stream, so the HCM chain matches
(`buildCaptureHTTPFilterChain`, `capture.go:273-281`). The HCM routes on
`:authority`; every mainstream client omits a default port from `Host`, so the
authority is the portless FQDN, which the service's `cap_http` vhost claims
(`captureVhostDomains`, `capture.go:550-551`, emits it) → `<fqdn>` cluster →
the default HTTP port.

**This needs no new config. It is already correct; it is merely undocumented.**
The HCM chain has no destination-port criterion and must not gain one: HTTP
callers dial 80, 18081, 8080, and every `cluster.local` port, and the authority
does the demux for all of them. A `destination_port: 80` chain would be config
that asserts nothing the authority does not already assert, and it would need
a twin for every other HTTP port. What this proposal changes for `http://` is
one sentence in `docs/workload-requirements.md` — which today documents only
the `:18081` spelling and, at line 161, claims "a `:port` on the authority is
stripped before routing", which the code does not do (`strip_any_host_port` is
set nowhere; `route.go:113, 279`) — plus the two name-only Service ports.

Precondition, stated once: **80 and 443 are captured only under redirect-all.**
The scoped rule redirects `dport == ProxyOutboundPort` alone
(`cni/internal/plugin/capture.go:95-99`). This is the same precondition
per-app-port HTTP (`:8080`) and per-app-port TCP (`:9000`) already have;
redirect-all has been the managed-pod default since 022, so it bites scoped-
capture pods only. On such a pod `http://<svc>/` goes to kube-proxy, and with
port 80 now a name-only Service port it is REJECTed (`ECONNREFUSED`) instead of
today's CNI-dependent dead end — refused deterministically, and the
`:18081` spelling is the documented alternative there.

A hand-built `Host: <fqdn>:80` (no mainstream client does this) matches no
exact domain, so the universal catch-all's `meshAuthorityRegex` (`route.go:130`,
optional `:port`) sends it to ODCDS as `<fqdn>:80`, which `aliasAuthorityPorts`
(`cluster.go:439`) never publishes → 503 after `onDemandClusterTimeout` (2s,
`httpfilter.go:32`). Unchanged and deterministic.

### `http://` to a service that serves no HTTP

Port 80, HTTP detected, HCM, RDS on the portless authority. Today,
`appendSABackedCaptureVhosts` (`capture.go:514`) builds a `cap_http` vhost
for **every** in-scope mesh Service regardless of protocol, routing to
`<fqdn>` — a cluster `clustersEndpointsAndVhosts` never emits for a TCP entry.
So today the request gets an immediate 503 with `cluster_not_found` in
`%RESPONSE_CODE_DETAILS%`: deterministic, but indistinguishable from a cluster
that vanished mid-reload, and it never reaches ODCDS (so the coordinator's
expected 404 does not occur either).

Intended behaviour under this proposal: the caller asked for HTTP on a service
that serves none, and the answer names that. For a service in scope with **no
HTTP port**, the cache emits the `cap_http` vhost on its HTTP spellings with a
single `direct_response` route: **`421 Misdirected Request`**, header
`x-aether-error: no-http-port`, body naming the service and its TCP spellings.
421 is chosen because it is the status whose definition is "the server is not
able to produce a response for the combination of scheme and authority" —
precisely this case — and because no application behind the mesh ever produces
it, so it cannot be confused with an app's own 404/503. It is immediate (no 2s
ODCDS wait) and it is a route the mesh already knows how to emit
(`Route_DirectResponse`, `route.go:145, 170`). Exact-domain vhosts outrank the
wildcard catch-all (the `route.go:279` "spike-verified" note covers exactly
this: an exact domain beats `*`), so the response cannot be shadowed. What
makes this fail: emitting the ordinary vhost for a service with no HTTP port —
a cache test asserts the 421 route on a TCP-only entry and its absence the
moment the entry gains an HTTP port.

### `https://<svc>.<ns>.<domain>/` — port 443, TLS bytes

Captured under redirect-all. `tls_inspector` (in `buildCaptureListenerFilters`,
"for future downstreams that speak TLS at the app layer") marks
`transport_protocol: tls`; the HCM chain requires `raw_buffer` and does not
match. Today, for an HTTP service nothing else matches → passthrough
`DefaultFilterChain` → `ORIGINAL_DST` → `ClusterIP:443`, a port the generated
Service does not expose → CNI-dependent failure, usually a hang through the
client's handshake timeout. For a TCP service today the `/32` chain matches →
floor → primary port, raw — which is how an app that terminates its own TLS
works behind the floor at present, on any port. **An accident, not a
decision**, in both branches.

Three options were weighed:

- **(a) Refuse explicitly.** TLS is a TCP-shaped stream to the mesh; it reaches
  an app iff the app registered that port as TCP. The generic per-port rule
  already says this: `https://<svc>/` is `(443, TCP)`, served through
  `cap_tcp_<svc>_443` iff `ports: "443=tcp"` is declared, else unclaimed. With
  443 a name-only port on the generated Service, "unclaimed" is a kube-proxy
  REJECT: the passthrough's upstream connect fails and Envoy closes the
  downstream during the ClientHello — `ECONNREFUSED`/EOF within one RTT, on
  every CNI, in both capture modes. The cause is visible in the source proxy's
  `cap_passthrough` upstream-connect-failure stats and access log, not in-band
  (the mesh holds no certificate the client would trust, so it cannot answer a
  TLS client with a message).
- **(b) Terminate app-level TLS at the capture listener and re-encrypt over
  mesh mTLS.** Requires a mesh-issued certificate for `<svc>.<ns>.<domain>`
  that the *application's* trust store accepts, a CA and rotation story, and
  an answer to why the bytes should be encrypted twice when the hop is already
  mTLS between workload identities. That is a proposal of its own and is
  scoped out here; nothing in (a) forecloses it — it would add a
  `transport_protocol: tls` HCM chain, more specific than the passthrough,
  without touching the per-port TCP chains.
- **(c) Leave the passthrough and document it.** Rejected: "document the hang"
  is the failure mode this repo keeps paying for.

**Recommendation: (a).** An app that speaks TLS registers its port as TCP and is
reached through the floor, its TLS riding inside the mesh's mTLS — double
encryption is then the app's choice, not the mesh's. Everything else is refused
fast. 443 is **not** mapped onto 18082: that would make `https://<svc>/` reach
whichever TCP port is the default, which for a mixed service is unrelated to
where the TLS listener is.

### A port nobody registered

- HTTP bytes to `<VIP>:<q>` with a portless `Host`: HCM → default HTTP port.
  Works today, keeps working — the destination port is irrelevant to HTTP.
- HTTP bytes with `Host: <fqdn>:<q>`: ODCDS name never published → 503 after
  2s. Today and after.
- Raw TCP to `<VIP>:<q>` where `q` is one of the four Service ports and no
  chain claimed it: `ECONNREFUSED`. New, deterministic.
- Raw TCP to `<VIP>:<q>`, any other `q`, HTTP service: passthrough → kube-proxy
  → no port → CNI-dependent, typically a hang. Today and after.
- Raw TCP to `<VIP>:<q>`, **pure-TCP service: reaches the primary port today**
  (the `/32` chain has no port). After: no `destination_port q` chain →
  passthrough → same dead end. **This is the one spelling that changes for an
  existing caller.** It was never a documented contract (018 describes it as
  a consequence of redirect-all), 005's second design goal says a port must
  never silently land on a different port, and it is handled with an
  observe-then-remove shim (Phase 2, Phase 4) rather than left implicit.

### Scoped mode and the TCP mesh port

The CNI now redirects `:18082` unconditionally in scoped mode (it cannot know
which VIPs have TCP ports), and the scoped HCM chain is a catch-all with no
match criteria (`buildCaptureHTTPFilterChain`, `scopeToCleartext == false`).
Raw TCP to `:18082` on a service with no TCP port would therefore hit the HCM
and receive a fake `HTTP/1.1 400` — the #460 shape. The agent emits, in scoped
mode only, a last `cap_tcp_blackhole` chain matching `destination_port 18082`
alone (less specific than every `/32 + port` chain, so it only catches what
none of them claimed) that `tcp_proxy`s to a static, endpoint-less `blackhole`
cluster: immediate close, counted in
`cluster.blackhole.upstream_cx_none_healthy`. In redirect-all mode the
passthrough → REJECT path is already deterministic and the chain is not
emitted.

### What existing callers see

- Every HTTP caller — `http://<fqdn>/`, `:18081`, `:<port>`, `cluster.local`
  spellings, GAMMA-declared ports — is unchanged in every phase. The HCM chain,
  the vhost domain set and the ODCDS catch-all are not modified; the only
  HTTP-visible additions are the 421 vhost for services with no HTTP port and
  a deterministic refusal on scoped-capture pods where there was a hang.
- Every TCP caller that dials a registered port, or the primary of a
  `protocol: tcp` service, is unchanged: the primary gets a
  `destination_port <primary>` chain as well as the `18082` one.
- TCP callers of an arbitrary unregistered port on a pure-TCP service are the
  one affected group, handled by the Phase 2 shim and its counter.

## Design

### The resolution rule

Every client spelling resolves to a `(port, protocol)` pair; the chain,
cluster and EDS shape follow the protocol of that pair. "Default P port" is
defined under *Two mesh ports*.

| Client dials | Signal | Resolves to | Source path | Mesh hop | Destination chain |
|---|---|---|---|---|---|
| `http://<fqdn>/` (dials 80, `Host` portless) or `<fqdn>:18081` | scheme + authority | default HTTP port | HCM → `<fqdn>` (today, unchanged; needs redirect-all for 80, not for 18081) | h2 + SNI=default HTTP port | `in_<pod>_<p>` HCM (today) |
| `http://<fqdn>:<p>/`, `p` an HTTP app port | authority | `(p, HTTP)` | HCM → `<fqdn>:<p>` (005, today) | h2 + SNI=p | `in_<pod>_<p>` HCM (today) |
| `http://…` to a service with no HTTP port | scheme + authority | none | **new** 421 `direct_response` vhost, `x-aether-error: no-http-port` | — | — |
| `https://<fqdn>/` (dials 443, TLS bytes) | scheme; `tls_inspector` (never HCM) | `(443, TCP)` iff registered `443=tcp`; else refused | `cap_tcp_<svc>_443` if registered; else passthrough → kube-proxy REJECT (`ECONNREFUSED`, name-only port 443) | as TCP | as TCP |
| raw TCP to `<fqdn>:18082` | destination port | default TCP port | **new** `cap_tcp_<svc>`: `/32 + destination_port 18082` → `tcp:<fqdn>`. Works under scoped capture | no ALPN; **no SNI** when the default TCP port is the primary (today's bytes), else SNI=port | default floor `in_tcp_<pod>` (today) or **new** `in_tcp_<pod>_<p>` |
| raw TCP to `<fqdn>:<p>`, `p` a TCP app port (incl. the primary) | destination port | `(p, TCP)` | **new** `cap_tcp_<svc>_<p>`: `/32 + destination_port p` → **new** `tcp:<fqdn>:<p>`. Needs redirect-all | same SNI rule | same |
| raw TCP to `:18082` on a service with no TCP port | destination port | none | redirect-all: passthrough → REJECT. Scoped: `cap_tcp_blackhole` | — | — |
| HTTP bytes, `Host` carries a port nobody registered | authority | ODCDS name never published | 503 after 2s (today) | | |
| raw TCP to any other port nobody registered | — | none | passthrough → kube-proxy → CNI-dependent (today); pure-TCP services: Phase 2 shim, Phase 4 removal | | |

Consequences:

- **A pure-TCP service is byte-for-byte unchanged on the mesh hop** and gains
  two working spellings (`:18082`, `:<primary>`) while losing "any port"
  (deprecated, Phase 2/4).
- **No spelling's protocol depends on the service.** `http://` and `:18081`
  are HTTP, `:18082` is TCP, `:<p>` is whatever `p` was registered as, for
  every service; a service without a port of that protocol refuses that
  spelling deterministically and by name (HTTP: 421; TCP: `ECONNREFUSED`). The
  "primary's protocol decides" rule of the first draft is gone, and nothing a
  current HTTP caller does stops working.

### (a) Key `c.clusters` by cluster identity

`clusterEntry` (`cache.go:535`) already has the fields it needs: `service` is
the bare key for dependency-set and SAN purposes, `sni` is the port, `tcp`
marks the entry kind. What is wrong is only that the TCP entry is stored under
the bare service key. Per-port and alias entries are already keyed by their
Envoy cluster name (`c.clusters[portName]`, `c.clusters[alias]`).

Change: TCP entries are keyed by their Envoy cluster name — `tcp:<fqdn>` for the
default-TCP-port entry, `tcp:<fqdn>:<port>` for per-port entries. The three
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
**bare-name CLA is owned by the HTTP default entry when the service has one,
else by the `tcp:<fqdn>` entry**. The other references the CLA by name and
carries `loadAssignment == nil`, exactly as the `:<port>` aliases do today
(`buildPortAliasesLocked`, and the `entry.loadAssignment != nil` guard in
`retainAbsentClustersLocked`).

`RemoveEndpoint` (`cluster.go:25`) takes a cluster name and has no non-test
callers in the tree; it follows the new keys and nothing else changes.

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
default cluster: `entry.sanURIs` is computed for every entry, TCP included.
Cluster stats stay keyed by the bare service (`AltStatName`).

**The SNI rule for TCP clusters:** `InjectUpstreamTCPMTLS(cl, …, sanURIs, sni)`
with `sni = ""` when the target port is the pod's primary port, else
`sni = strconv.Itoa(port)`. Empty-SNI lands on the destination's default floor
(which forwards to the primary — the only chain a pre-037 inbound has), so the
one target that can exist on a mixed fleet is reached the way it is reached
today; a non-primary TCP port is a post-037 fact on both ends by construction.
`tcp:<fqdn>` (the `:18082` cluster) is simply the per-port cluster of the
default TCP port under the bare name, and inherits the rule.

### (d) Capture: one chain shape, per port

The per-VIP chain of today (`buildCaptureTCPFloorFilterChain`) becomes, for a
service `S` with VIP `V` and TCP port set `T`:

```
cap_tcp_<S>:        prefix_ranges [V/32], destination_port 18082 → tcp_proxy → tcp:<fqdn>        (iff T ≠ ∅)
cap_tcp_<S>_<p>:    prefix_ranges [V/32], destination_port p     → tcp_proxy → tcp:<fqdn>:<p>    (∀ p ∈ T)
```

No portless chain, for any service (except the Phase 2 deprecation shim for
pure-TCP services, which carries its own stat prefix and is removed in Phase 4).
HTTP to `V:8080`, `V:18081` or `V:80` matches none of these and falls to the
HCM chain as today. TLSRoute SNI chains (`BuildCaptureTLSRouteFilterChains`)
and TCPRoute weighted floors (`BuildCaptureTCPRouteFilterChain`,
`agent/internal/xds/proxy/l4route.go`) attach to `cap_tcp_<S>`, the default-TCP
chain; port-qualified L4 routes are Phase 3.

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

`aether.io/app-protocol` on the mesh Service stays, stamped `http` when the
service has any HTTP port and `tcp` only for a pure-TCP service. That is the
value that keeps a pre-037 agent correct: `http` makes it build no per-VIP
chain (so HTTP keeps working through it and TCP ports are refused, loudly,
until the node is upgraded), and `tcp` for a pure-TCP service is what it gets
today. The generator additionally stamps `aether.io/port-protocols:
"8080=http,9000=tcp"` for tooling and for the webhook; the agent does not
depend on it.

### (e) Inbound: per-port floor chains

`buildInboundFilterChains` (`ingress.go:167-185`) emits, per served port, an HCM
chain matched on `server_names:[<port>]`. For a TCP port it emits instead

```
in_tcp_<pod>_<p>:  server_names [<p>], DownstreamTransportSocket(...)  → tcp_proxy → app_<pod>_<p>
```

— the floor chain's shape (`buildInboundTCPFloorFilterChain`) with an SNI match.
The default no-match floor chain stays and still targets the primary port, so a
no-SNI floor connection (every source built before this proposal, and every
default-TCP-port dial whose default is the primary after it) behaves as today.
There is never an HCM chain and a TCP chain with the same `server_names`, so
the listener is unambiguous — Envoy rejects a listener whose chains it cannot
disambiguate, which is what `envoy --mode validate` will assert.

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
- The HTTP path — outbound listener, capture HCM chain, vhost domains, ODCDS
  catch-all, `aliasAuthorityPorts` — is not modified. Every HTTP spelling that
  works today works identically.
- The UDP floor (`udp:` clusters, plaintext, app-port addressed) is a separate
  concept and is out of scope. UDP ports are declared by UDPRoute today, not by
  the endpoint annotation; the CNI's UDP redirect stays on 18081 only.
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
  re-registers with `HEALTH_HEALTHY` (`liveness.go:395-508`); it must do so
  under every key the pod holds, or the TCP listing stays UNHEALTHY forever
  while HTTP is promoted. This is the one registration-side change that is easy
  to miss and silent if missed — see *Risks*.

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
either listing — endpoints of one service share it), `appProtocol` is `http`
if any HTTP port exists else `tcp`, and the new `port-protocols` annotation is
the union. The merge is order-independent (a sorted union; map order is
protocol-visible, #135). The generated Service exposes `mesh` (18081) and
`mesh-tcp` (18082), and `apply` converges `Spec.Ports` on existing Services,
which it does not do today.

## Migration path

The annotation model is additive and the old shape is the default for the new
one, so there is no flag day. The mixed-fleet cases, in the direction each
piece of state actually flows:

| State | Written by | Read by | Old writer / new reader | New writer / old reader |
|---|---|---|---|---|
| Pod annotation `ports: …=tcp` | the user | the pod's **own node** agent (registration + inbound listener) | n/a | Old agent strips the suffix (`getPortsFromAnnotations`) and `AppPortProtocols` treats it as HTTP/1.1: the port is registered HTTP and served by an HCM chain. **Silent.** Mitigation: the pod-mutating webhook (`controller/internal/podmutate`, rolled with the controller ahead of the agent) rejects `=tcp` until the chart's agent version supports it; documented skew rule: roll the agent DaemonSet before annotating |
| `port_protocols` on the endpoint | registering agent | every client agent | Absent → the client applies the key protocol to every port (today's behaviour, by definition of the field) | Old client ignores it, sees the TCP port in `ports`, builds an h2 `<fqdn>:9000` cluster + vhost domain. A raw-TCP dial is captured, fails `http_inspector`, passes through to kube-proxy, and dead-ends. Loud, and identical to what that client does today |
| Second registration key | registering agent | registrar, backends, generator | n/a | Old registrar: two keys, generator claims HTTP-first → `app-protocol: http`, old agents treat the service as HTTP (today's behaviour). Old etcd/k8s backend: stores both, harmless |
| SNI=port on a per-port floor cluster | client agent | destination pod's inbound | n/a | Cannot occur across versions: SNI is set only for a non-primary TCP port, which only a post-037 agent advertises — the same agent that built the destination's `in_tcp_<pod>_<p>` chain |
| `:18082` (and name-only 80/443) on the generated Services | registrar | kube-proxy; every client's CNI + capture listener | Old registrar (one port): `:18082` is a Service port that does not exist → CNI-dependent dead end, i.e. today's behaviour for any unknown port; the new agent's chain is never reached. `http://` keeps working through the HCM regardless | New registrar, **old agent**: no chain matches 18082 (redirect-all → passthrough; scoped → not even redirected) → kube-proxy → Service port with no endpoints → **`ECONNREFUSED` immediately**. Loud and fast; the port "does not work yet" on that node, it does not misroute. Risk 9. 80/443 name-only ports only ever *improve* an old agent's failure (hang → refused) |
| `aether.io/app-protocol` on the mesh Service | registrar | capture reconciler (pre-037 agents only) | Old registrar stamps HTTP-first; a new agent ignores the annotation for chain emission (cache-derived) | New registrar stamps `http` for any HTTP-bearing service → old agent builds no per-VIP chain, HTTP works, TCP refused; `tcp` only for pure-TCP → old agent behaves as today |

A cluster running `endpoint.aether.io/protocol: tcp` on every pod of a
service, with one port, sees on the mesh hop no change at any phase: the key
protocol is TCP, `port_protocols` is `{primary: TCP}`, the cache builds
`tcp:<fqdn>` under its new key with the same bytes, no SNI, and the inbound's
default floor is the only floor. Its capture chain gains `destination_port`,
and dialing an unregistered port stops reaching it (deprecated, below).

The only removals in the whole plan are the kubernetes backend's `return nil`
for TCP (Phase 0 makes that safe) and the portless floor chain (Phase 4, after
a deprecation release with a hit counter).

## Phasing

### Phase 0 — #878, on the existing invariant (SHIPPED, #888)

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
fixes that for both. Independent of the rest.

**Shipped in #888.** Two deviations from the text above, both found in the
writing:

- The shared parser landed as a leaf package `//registry/endpointmeta`
  (following the `//registry/export` precedent) rather than as a
  `registry.PortProtocols` helper, because `//registry/internal/k8s` importing
  its parent interface package would have been the wrong direction.
- The health-check-mode parser was deliberately **excluded** from the sharing.
  The two backends' readings differ on the UNSET case and the difference is
  load-bearing: the CNI path defaults to EDS (delegated liveness is its
  default) while the Kubernetes backend must default to UNSPECIFIED, because it
  derives endpoints from the API server and the delegated active-HC path
  applies only to the write-based backends. Unifying them — which a tidy-minded
  dedup does by reflex — would have silently flipped the CNI registration path
  in production. Both sites now carry a note against it.

A third fact worth carrying into Phase 2: **only `e2e/uds.sh` runs on the
chart-default `kubernetes` backend.** Every other suite, `e2e/l4routes.sh`
included, sets `registrar.registryBackend=etcd` — which is #878's complaint in
harness form. Phase 2 should make the L4 harness run on both backends rather
than inherit the etcd assumption.

### Phase 1 — key the cache by cluster identity

Design (a). TCP entries keyed `tcp:<fqdn>`; the three readers follow; the
bare-name CLA has one owner.

**Deviation, recorded during implementation:** `RequireProtocolDisjoint` is
*not* retired here. The replacement this proposal specifies asserts that the
two listings' `port_protocols` agree, and that field does not exist until
Phase 2 — so retiring in Phase 1 would drop a working cross-backend guard and
put nothing in its place, while no test exercises the mixed case at registry
level. It is annotated in place as a convention rather than an invariant, with
the swap deferred to Phase 2. Likewise `envoy_validate` gains no case: Phase 1
changes no Envoy resource *shape*, and the risk it introduces — two EDS
resources published under one name — is a snapshot-consistency error that
`envoy --mode validate` cannot see, because it does not resolve dynamic
cluster references. The cache unit test is the gate that can actually fail. Behaviour change: a
service whose pods split across protocols now gets **both** an h2 cluster +
vhost and a `tcp:` floor cluster, instead of the TCP set clobbering the HTTP
one. That is the *service-level* multi-protocol milestone, and it is what a
user who put `protocol: tcp` on half a Deployment gets today by accident. No
proto, no annotation, no wire change; fully covered by unit tests on the cache
and `envoy_validate`. The capture chain is still the portless `/32` for
TCP-classified services in this phase (18082 does not exist yet), so a split
service's HTTP side is still shadowed by it — Phase 1 is a correctness fix for
the cache, not yet a usable mixed service.

### Phase 2 — the TCP mesh port and per-port protocol

Two chart releases. **Release A, control plane:** `ProxyTCPOutboundPort`
constant; both generators expose `mesh-tcp` 18082 plus the name-only `http` 80
and `https` 443 ports and converge `Spec.Ports`; the CNI rule for `dport 18082`
(the CNI ships in the agent chart but is inert without the chain, and the
REJECT floor makes the interim safe).

**Shipped as #890.** Two items named here moved or were dropped: the `=tcp`
webhook guard is **dropped** (Risk 6 records why), and the `port-protocols`
annotation moves to Release B, because it needs per-port data that only arrives
with the proto field — stamping a primary-port-only form in Release A would
advertise a feature that does not work yet. Release A also had to fix a
migration gap not anticipated here: the generator's apply path compared
annotations alone and returned early, so a mesh Service created before 037
would have kept its single `mesh` port forever and the new spellings would have
appeared only on a fresh install. **Release B, agent:**
design (b)–(e) — the `=tcp` suffix, `port_protocols` on the proto, dual
registration and dual promotion, typed buckets, the SNI rule, per-port
`tcp:<fqdn>:<p>` clusters, `destination_port`-qualified capture chains from the
cache-derived map (18082 + every registered TCP port), the scoped-mode
blackhole chain, the 421 vhost for services with no HTTP port, per-port
inbound floor chains, the #877 WARN, and the workload-requirements rewrite
(the annotation table lists neither `protocol` nor `ports` today,
`docs/workload-requirements.md:42-49`; line 161's "port is stripped" claim is
false; and the client contract gains the scheme rule: `http://<fqdn>/`,
`:18081`, `:18082`, `:<port>`, and what `https://` means).

**Deprecation shim, this release only:** for a service with zero HTTP ports the
agent also emits today's portless `/32` chain *after* the port-qualified ones,
under stat prefix `cap_tcp_anyport_<svc>`, so a client still dialing an
unregistered port of a pure-TCP service keeps working and is **counted**
(`tcp.cap_tcp_anyport_<svc>.downstream_cx_total`). The agent logs one WARN per
service per hour naming the count. What makes this gate fail: a non-zero
counter on talos over a full release — that blocks Phase 4 and names the
client.

**18082 is not the feature.** It gives every service one redirect-all-free TCP
spelling that reaches its default TCP port. A service with `:9000` and `:5432`
both TCP reaches `:5432` only by dialing `:5432` under redirect-all, through
its own `cap_tcp_<svc>_5432` chain and `tcp:<fqdn>:5432` cluster; `:18082`
reaches whichever of the two is the default. Both tiers ship in Release B.

### Phase 3 — port-qualified L4 routes and the edge

`common/l4project` backends are "NOT port-qualified" by design
(`l4project.go:53-55`) because the floor had one port. A TCPRoute/TLSRoute
`backendRef.port` naming a non-default TCP port resolves to
`tcp:<fqdn>:<p>`; the edge's `edgeTCPClusters` builds the same per-port
clusters with `EdgeUpstreamTCPTransportSocket` grown an SNI parameter. GAMMA's
`ProjectHTTPRule` should reject (ResolvedRefs=False, reason
`UnsupportedProtocol`) a `backendRef.port` that resolves to a TCP port rather
than build an h2 cluster to it — open question below on whether the projector
can see the protocol.

### Phase 4 — remove the portless floor chain

One release after Phase 2, if `cap_tcp_anyport_*` stayed at zero on talos for
the whole release: delete the shim. If it did not, the counter names the
service and the release notes name the spelling; the shim stays another
release. This is the only step in the plan that changes what an existing
client observes, and it is gated on evidence that no client observes it.

## Alternatives considered and rejected

- **Keep the invariant; require users to split into two Services.** The mesh
  Service is *generated* from the registry, one per `<ns>/<serviceAccount>`
  (`generator.go`); users cannot create a second mesh Service for the same pods.
  "Two Services" therefore means two ServiceAccounts, which means two pods —
  the app has to be split at the process boundary, and every identity-keyed
  policy (SAN pins, RBAC on XFCC, `HTTPFilter` attachment) has to be duplicated.
  For a database with an HTTP admin port, or an app with a metrics port and a
  binary wire protocol, that is not an option the user has.
- **Resolve the bare name to "the primary port, in the primary's protocol"**
  (the first draft). Rejected in review: it makes `<svc>.<ns>.<domain>` mean
  HTTP for one service and a raw floor for another, so a client cannot know
  what a name means without knowing the workload's annotation — the same
  asymmetry the proposal exists to remove, one level up. Two protocol-typed
  mesh ports cost one constant and one Service port and delete the rule.
- **Infer per-port protocol from `Service.spec.ports[].appProtocol`.** Three
  problems. The registry discovers pods by ServiceAccount, not by Service
  selector, so there is no user Service to read — the only Service the mesh
  knows is the generated selectorless one. Every other registration fact
  (`port`, `ports`, `weight`, `health-check-mode`, `uds-socket`) lives on the
  pod, and a pod that is scheduled before its Service exists must still
  register correctly. And `appProtocol` is free-form (`kubernetes.io/h2c`,
  `kubernetes.io/ws`, anything), which would push vocabulary policing to every
  reader. A later convenience — the controller projecting a user Service's
  `appProtocol` onto the pod annotation at admission — is compatible with this
  design and can be its own proposal; it is not a substitute for the registry
  carrying the fact.
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
- **Require every caller to name a port** (`<fqdn>` with no port → error, for
  HTTP too). Considered in review as the fully symmetric rule. Rejected: the
  portless authority is what every current HTTP caller in the mesh uses
  (`captureVhostDomains` emits it deliberately, `BuildOutboundClusterVirtualHost`
  documents it, `route.go:275-281`), so it is a breaking change for the whole
  fleet — and it buys nothing, because the URL scheme already types an HTTP
  dial. The asymmetry the rule was meant to remove is removed by the TCP mesh
  port instead.
- **Map 80 → 18081 and 443 → 18082 explicitly.** 80 needs no mapping (the HCM
  routes on authority, traced above). 443 is app-level TLS, which is a TCP port
  the app declares like any other; mapping it would make `https://<svc>` reach
  the default TCP port of a service whose TLS listener is elsewhere.
- **A `destination_port: 80` capture chain for `http://`.** Config that asserts
  nothing the authority does not already assert, and it would need a twin for
  every other port HTTP callers use (18081, 8080, `cluster.local` ports). The
  HCM chain stays port-agnostic on purpose.
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
   (`mtls.go:190`), which WARNs and counts per snapshot.
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
   node).

   **DECIDED during Release A: the webhook guard is dropped.** The mitigation is
   the documented skew rule — *roll the agent DaemonSet before annotating* —
   plus a loud WARN from the new agent when it parses a `=tcp` suffix, so the
   supported case is visible in logs and the unsupported case is a known
   ordering constraint rather than a surprise.

   The guard was specified as "the pod-mutating webhook rejects `=tcp` until the
   chart's agent version supports it". Two things make it unable to do that job:

   - There is no pod **validating** webhook. The controller's `/validate`
     dispatches by Kind (`MeshConfig`, `HTTPFilter`, `EdgeConfig`,
     `EndpointPolicy`, `HTTPRoute`); pods reach only `/mutate`. A mutating
     webhook *can* deny, but that is an odd shape for a pure admission check.
   - It must know the agent's version, and the only non-drifting source is a
     chart value set by the same release that ships the agent. That is sound as
     far as it goes — the chart is the unit of deployment — but within a single
     `helm upgrade` the controller rolls **before** the agents, so a pod
     annotated `=tcp` in that window still registers as HTTP on an old agent's
     node. The guard narrows the window it exists for without closing it.

   A gate that cannot fail in the case it was built for is decoration (#853).
   Preferring an honest ordering constraint to a partial gate is the same call
   made about `envoy_validate` in Phase 1: state the limit, do not dress it up.

   What would reopen this: evidence that the skew window is actually being hit —
   an agent WARN for `=tcp` arriving from a node whose agent predates support,
   which the WARN itself makes visible.
7. **Per-app-port TCP under scoped capture.** `:18082` works in both capture
   modes (the CNI rule is extended); `:<p>` for a TCP app port is not captured
   under scoped mode, the same "configured, accepted, inert" shape per-app-port
   HTTP already has there. The agent WARNs once per service with TCP ports
   other than the default when the node's capture mode is scoped, naming the
   ports. What makes it fail: the WARN test fixture runs a scoped-mode cache
   with a two-TCP-port service and asserts the log line; dropping the WARN
   fails it.
8. **Registrar generator flapping.** Two listings for one service could produce
   two `desiredService` values and a Service whose annotations oscillate across
   reconciles (map order is not deterministic; #135 is the same mechanism on the
   route table). The merge must be order-independent: primary from `port`, which
   is shared; protocol map as a sorted union.
9. **Registrar advertises `:18082` before a node's agent can serve it** (mixed
   fleet during the Phase 2 Release A → B window, or any node whose agent roll
   lags). Traced above: on that node the dial reaches kube-proxy — via the
   passthrough under redirect-all, directly under scoped mode — and hits a
   Service port with no endpoints, which kube-proxy REJECTs. The failure is
   `ECONNREFUSED` within one RTT, never a hang, never a misroute to the HTTP
   path, never a fake 400 (the old agent's scoped HCM catch-all is not in the
   path because the old CNI does not redirect 18082). Gate: the kind e2e's
   negative control — a pod whose capture listener carries no 18082 chain (the
   Phase 2 harness can build one by pinning a service with no TCP ports) dials
   `:18082` and must see `ECONNREFUSED` within 1s. What makes it fail: a
   kube-proxy mode or CNI that DROPs instead of REJECTing for an endpoint-less
   Service port; then the dial hangs, the test times out, and the assumption
   this risk rests on is shown false on that platform. The gate is run on kind
   (iptables) and the talos roll (nftables kube-proxy) both.
10. **The portless shim silently keeps a client alive that Phase 4 will break.**
    Gate: the `cap_tcp_anyport_*` counter, exported per service, with the
    Phase 4 PR required to quote its talos value for the preceding release. What
    makes it fail: a non-zero value — the PR is blocked and the client is named.

## Testing and validation

**`test/envoy_validate/`** (every PR, the exact proxy binary):

- Mixed-pod inbound bootstrap (risk 3).
- Capture bootstrap with `cap_tcp_<svc>` (`destination_port 18082`) and
  `cap_tcp_<svc>_<p>` beside the HCM catch-all and the passthrough default
  chain; a scoped-mode variant with the blackhole chain; the Phase 2 shim
  variant with the portless chain last. Validate proves each listener is
  accepted and unambiguous; it cannot prove which chain a connection selects.
  Negative control: a fixture with a portless `/32` chain on a service that
  also has HTTP ports must be flagged by a config-shape assertion over the
  generated bytes (the mixed-service swallow) — validate itself accepts it,
  which is exactly why the assertion has to be structural.
- Per-port `tcp:<fqdn>:<p>` cluster with SNI, through `UnpinnedMeshClusters`.
- The existing `TestCaptureTCPRoute*`/`TestCaptureTLSRoute*` fixtures keep
  attaching to the default-TCP chain and must not change bytes until Phase 3.

**Cache unit tests** (`agent/internal/xds/cache`): risk 1's chain⇔cluster
resolver with its negative control; risk 4's pointer-identity test; the
Phase 1 "both listings" case (h2 cluster + vhost + `tcp:` cluster + one
bare-name CLA, no duplicate EDS names — assert on the snapshot's resource list,
not on the map); the SNI rule (empty for the primary, port otherwise); no
`:18082` in any vhost domain or alias.

**CNI unit tests** (`cni/internal/plugin`): the scoped rule set contains a
TCP redirect for 18082 and no UDP redirect for it.

**Registry contract** (`registry/registrytest`): the per-protocol honesty
contract, run against etcd (testcontainers, `integration` tag) and the
kubernetes fake; risk 5's dual promotion.

**kind e2e**: extend the #879 harness with a mixed workload (HTTP `:8080`
primary, raw echo `:9000=tcp`) and a pure-TCP workload, and assert from a
client pod, under **both** capture modes:

- HTTP: `http://<fqdn>/`, `:18081`, `:8080` all reach the HTTP app (access log
  shows the vhost). Unchanged behaviour is asserted, not assumed. Under scoped
  mode `http://<fqdn>/` must be `ECONNREFUSED` ≤ 1s (name-only port 80) and
  `:18081` must work.
- No-HTTP service: `http://<tcp-svc>/` returns 421 with
  `x-aether-error: no-http-port`, immediately (the timing assertion is what
  separates it from the 2s ODCDS 503).
- TLS: `https://<fqdn>/` on a service without `443=tcp` fails the handshake
  within 1s (`ECONNREFUSED`/EOF, never a hang); with `443=tcp` registered the
  ClientHello reaches the app through `cap_tcp_<svc>_443`.
- TCP tier 1: raw bytes to `:18082` reach the echo through `cap_tcp_<svc>`
  (chain `tcp_proxy` stats increment) and the inbound's default floor (for the
  pure-TCP service) or `in_tcp_<pod>_9000` (for the mixed one, whose default
  TCP port is not its primary) — asserting **both ends** is what makes the
  mesh-hop SNI rule observable. In scoped mode this is the case that proves
  the CNI rule extension.
- TCP tier 2: raw bytes to `:9000` through `cap_tcp_<svc>_9000` under
  redirect-all; under scoped mode the dial must fail and the agent WARN must be
  present (risk 7).
- Refusals: `:18082` on the HTTP-only service → `ECONNREFUSED` ≤ 1s (risk 9's
  gate, same mechanism); raw TCP to `:9001` (unregistered) on the mixed
  service → not the echo.
- Shim: raw bytes to `:9999` on the pure-TCP service reach the echo in Phase 2
  and increment `cap_tcp_anyport_<svc>`; the same dial is refused after Phase 4.

SPIRE on (the floor is mTLS-only; #877). Backend: both etcd and kubernetes,
since the point of Phase 0/2 is that they agree.

**talos**: Release A rolled alone first — the prober and k6 stay flat, and a
raw-TCP dial to `:18082` from a mesh pod returns `ECONNREFUSED` (the Risk 9
gate, on nftables kube-proxy). Then Release B under the external prober (100%
bar), with `listener_modified` watched for the drain risk and
`cap_tcp_anyport_*` read at the end of the soak. Grade after a post-upgrade
proxy roll, per the standing rule that stale series are not zero series.

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
- **Prober coverage.** The prober (013) is HTTP; nothing black-box probes
  `:18082`. A raw-TCP `mesh_tcp` tier would make the Phase 2 rollout gradeable
  by the same SLI as everything else. Separate proposal or a 013 amendment.
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
