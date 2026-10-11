# Proposal 043: A mesh service is addressed by its real ports, and nothing else

**Status:** Draft — written 2026-10-11 for an owner decision already taken in
direction: issue #1672 is fixed by removing the per-service *primary* port and
protocol, not by refining it. Nothing here is built. The facts were read from
`main` at `2a132053`; the two measurements were made on the pinned proxy outside
any cluster. Seven decisions are still open (see *Decisions for the owner*).
**Resolves:** #1672. The defect reported there is the per-service primary
itself; the owner decided to remove it, not to patch it. Accepting this
proposal closes the issue: the phases below are the work, and Phase 1 is where
the reported behaviour is fixed.
**Author:** Bruno Palermo
**Date:** 2026-10-11
**Related:** #1672 (a raw-TCP primary with an HTTP secondary has no TCP path),
#1677 (draft fix, on hold: it measured both registry backends, and this proposal
says which parts of it survive), proposals
[037](037_multi-protocol-per-service.md) (per-port protocol; its Phase 4 is
subsumed here), [038](038_udp-tproxy-capture.md) (TPROXY capture; why UDP binds
18082), [022](022_arbitrary-service-interception.md) (redirect-all),
[018](018_gateway-api-gamma.md) (Gateway API and GAMMA),
[005](005_multi-port-routing.md) (per-port clusters),
[026](026_multi-cluster-config-propagation.md) (config propagation),
[030](030_port-range-migration.md) (the 18xxx ports).

## The decision

> **A mesh service is addressed only by its real ports. Port-less addressing is
> removed for every service, single-port ones included, and with it the
> per-service "primary" port and protocol. Protocol is a property of a port.**

Three client spellings exist today that do not name a real port
(`docs/workload-requirements.md:274-281`, read):

| Spelling | Means today |
|---|---|
| `<svc>.<ns>.<domain>:18081` | "the primary port, over HTTP" |
| `<svc>.<ns>.<domain>:18082` (TCP and UDP) | "the primary port, as raw TCP" / "as plaintext UDP" |
| `http://<svc>.<ns>.<domain>/` (the client dials 80) | "the primary port, over HTTP" |

Each needs the service to collapse to one port and one protocol. That collapse
is the *primary*. Proposal 037 made protocol a property of a port for every
spelling that names one, and kept the primary for the three that do not. #1672
is what the kept part costs: when the primary is raw TCP and a secondary is
HTTP, three readers disagree about what "the service's protocol" is, and the
answer depends on the registry backend.

After this proposal a client writes `<svc>.<ns>.<domain>:<port>`, where `<port>`
is a port of the Service, and the port's own declaration says what speaks there.
`http://<svc>.<ns>.<domain>/` still means port 80, as it does everywhere else on
the internet, and works when port 80 is a port of the Service.

Proposal 037 considered "require every caller to name a port" and rejected it
because it breaks every HTTP caller in the mesh
(`037_multi-protocol-per-service.md`, *Alternatives*). That cost is real and is
restated under *Risks*. The decision accepts it; this document is about paying
it in an order that never strands a client without a working spelling.

## Prior art

Described from general knowledge of Istio's sidecar data plane. **Not verified
against that project's current source or documentation**; cited as design
precedent, not as a specification to match.

- There is no mesh-specific client port and no per-service primary. Capture
  recovers the original destination, and the client's own `<service>:<port>` is
  the address.
- Listeners, routes and clusters are keyed per Service port.
- A port's protocol comes from the Service port (`appProtocol`, or a port-name
  prefix), with protocol sniffing as the fallback when neither says.
- A port the Service does not declare is passed through, not routed.
- A route that names no port applies to the ports of the Service.
- Application UDP is not handled by the proxy.

Aether differs in two ways that matter below. Its registry discovers pods by
ServiceAccount and reads ports from **pod** annotations, not from a Service
selector, so the only Service the mesh always has is the one the registrar
generates. And aether proxies application UDP today.

## Facts established

Each fact is marked **read** (the code or document that does it was read at the
cited line), **measured** (run for this proposal or in #1677, and by whom) or
**inferred** (derived from read code, not exercised).

### 1. Everything that needs "the service" to be one port or one protocol

Where the primary comes from (all read):

- The pod annotation `endpoint.aether.io/port`, default 8080, becomes
  `ServiceEndpoint.port` (`registry/endpointmeta/endpointmeta.go:55-60`,
  `registry/cni.go:42,69`, `api/aether/registry/v1/endpoint.proto:17`). The
  proto comment calls it "the default/primary port — what the portless FQDN
  resolves to" (`endpoint.proto:19-24`).
- A service's primary is the first endpoint's: `defaultPort :=
  endpoints[0].GetPort()` (`agent/internal/xds/cache/cluster.go:518-520`), and
  again at `cluster.go:495`, `:773`, `:968`, `:381-386`, and in the registrar at
  `registrar/internal/services/generator.go:119,130`.
- A service's protocol is whichever listing it is found in first: the generator
  iterates `registry.ServedProtocols` (HTTP, TCP, UDP) and "the first protocol
  iterated wins" (`generator.go:106-126`); the agent reads the result back off
  the Service (`agent/internal/capture/reconciler.go:186-193`).

| # | Consumer | What it assumes | Evidence | What replaces it |
|---|---|---|---|---|
| 1 | `:18081` on a mesh name | The service has one HTTP port. `cap_http` claims `<name>:18081` for the default cluster; an alias cluster `<fqdn>:18081` exists for the on-demand path | read: `cache/capture.go:872-893`, `:1158-1161`; `cache/cluster.go:634-646` | Removed. `<name>:<p>` for each declared HTTP port |
| 2 | `:18082` on a mesh name, TCP | The primary is raw TCP: chain `cap_tcp_<cluster>_18082` to the floor cluster `tcp:<fqdn>` | read: `proxy/capture.go:160-162`, `:623-635` | Removed. `cap_tcp_<cluster>_<p>` for each declared TCP port (exists since 037 for non-primary ports, `proxy/capture.go:297-318`) |
| 3 | `:18082` on a mesh name, UDP | The service has one UDP port, the primary; needs a `UDPRoute` | read: `proxy/l4route.go:245-331`, `cache/capture.go:657-674` | See fact 3 |
| 4 | The bare name (`http://<name>/`, dials 80) | Portless authority = the primary, over HTTP | read: `cache/cluster.go:528`, `cache/capture.go:889-892` | Port 80, if the Service has it |
| 5 | The any-port shim `cap_tcp_anyport_<cluster>` | Any port of a TCP-primary VIP is the primary | read: `proxy/capture.go:634` | Removed (037 Phase 4, subsumed) |
| 6 | The generated Service's ports | Four name-only TCP ports: `mesh` 18081, `mesh-tcp` 18082, `http` 80, `https` 443. No real port, no UDP port, no `appProtocol` | read: `generator.go:169-176` | The service's real ports, each with its protocol |
| 7 | `aether.io/app-protocol` on the Service | One class per Service | read: written `generator.go:255-259`; only reader `capture/reconciler.go:186-193` | Per-port protocol from the endpoints; the annotation is frozen, then removed |
| 8 | `aether.io/port` on the Service | The primary | read: written `generator.go:257`; **no reader** outside the generator's own compare (`generator.go:232`) | Removed |
| 9 | Default HTTP cluster `<fqdn>` | One port per service; SNI = the primary port; the only subscriber of the bare EDS name | read: `cache/cluster.go:524-532`, `cache/mtls.go:189-205` | Removed. `<fqdn>:<p>` per port (005) |
| 10 | Default virtual host | GAMMA rules attach to it only; per-port virtual hosts get the chain filter and no rules | read: `cache/cluster.go:528`, `:1200-1206`; `cache/capture.go:843-857` | Rules attach to the virtual host of every port the route's parent selects |
| 11 | HTTPRoute `backendRef.port` | Ignored: every backend is the default cluster | read: `common/gammaproject/project.go:222-241` | Resolved to the backend's `<fqdn>:<p>` |
| 12 | The known-target safety net | `<svc>.<ns>.svc.cluster.local:<any port>` is pinned to the default cluster | read: `cache/capture.go:953-1041` | Per-port targets |
| 13 | QUIC twins | Built for the default entry only; the name refuses a port; SNI `<primary>.<fqdn>` | read: `cache/cluster.go:1278-1304`, `proxy/egress.go:193-230` | One twin per (source, destination, port) |
| 14 | On-demand (ODCDS) catch-all | None: `:authority` is the cluster name verbatim, port included; admission is per service | read: `proxy/route.go:176-295`, `server/odcds.go:295-315` | Unchanged. A portless authority stops naming a cluster |
| 15 | TCP mesh hop | Empty SNI means "the primary"; the destination's default chain forwards to the primary | read: `cache/capture.go:461-467`, `proxy/ingress.go:248-258` | SNI = port for every TCP port; the default chain is removed last |
| 16 | HTTP mesh hop | The destination's no-SNI h2 chain is the primary | read: `proxy/ingress.go:188-190` | A `server_names` chain per port (already built for every port, `ingress.go:199-205`) |
| 17 | Delegated liveness | Probes the primary port, typed by the **pod-level** protocol, not the port's | read: `proxy/listener.go:101-117`, `proxy/cluster.go:365-449` | The probe port's own protocol (decision 6 names the probe port) |
| 18 | Mesh DNS | Nothing: A records only, name to ClusterIP | read: `agent/internal/meshdns/server.go:692-709`, `capture/reconciler.go:162-167` | Unchanged |
| 19 | Edge backend resolution | Port 0, or a port equal to the primary, means the default cluster | read: `cache/edge.go:663-691` | The port's cluster, always. Edge TCP publishes only `tcp:<fqdn>` (`cache/capture.go:495-533`); a port-qualified backend names a cluster the edge never publishes (inferred) |
| 20 | Cross-cluster config (026) | `GammaBackend` is `{service, cluster, weight}` with the default cluster's name; the projection carries no port | read: `api/aether/registry/v1/config.proto:21-37,67-71` | An additive `port` on the backend and on the projection's parent |
| 21 | Clusterset VIP and `ServiceImport` (MCS) | One port, `mesh` 18081; an existing clusterset Service is never converged | read: `registrar/internal/mcs/import_generator.go:250-256`, `:275`, `:301-306` | Real ports, converged |
| 22 | East-west waypoint (019) | SNI is already `<port>.<fqdn>`; "the primary port needs no change" on the destination | read: `cache/mtls.go:197-205`, `cache/listener.go:84-104` | A chain per port on the destination |
| 23 | UDS-delivered services | Nothing: every declared port dials the one socket | read: `proxy/cluster.go:155,207-209` | Unchanged |
| 24 | Registry listings | `ListEndpoints(svc, protocol)`; the kubernetes backend filters by the **pod-level** annotation alone | read: `registry/registry.go:27-36`, `registry/internal/k8s/kubernetes.go:498-509` | A pod is listed under every protocol it serves a port of, on both backends |
| 25 | `registry.ServedProtocols` | Three callers. The generator uses it as a service class (first wins). The registrar sync and the agent's ghost sweep use it as the key space, with a comment that "a service is registered under exactly one protocol" | read: `registry/protocols.go:34-38`; `generator.go:109`; `registrar/internal/server/sync.go:36`; `agent/internal/cni/server/ghostsweep.go:173,423-424` | Stays as the key space. No consumer reads a class from it |
| 26 | `registrytest.RequireProtocolDisjoint` | "a service is HTTP xor TCP" | read: `registry/registrytest/contract.go:49-77` | The per-port contract 037 specified and never built (`contract.go:58-62`) |
| 27 | Stat names | `cap_tcp_<cluster>_18082` and `cap_tcp_anyport_<cluster>` chain prefixes; the floor key `tcp_<ns>/<svc>`; `udp_<ns>/<svc>` | read: `proxy/capture.go:629,634`; `proxy/egress.go:151-181` | Per-port keys only (`tcp_<ns>/<svc>_<port>` exists) |
| 28 | Access logs and metrics | No service-level protocol field; no `protocol` or `port` metric attribute | read: `proxy/accesslog.go:100-145,257-277`; grep of agent Go | Unchanged. Documented samples show `:18081` authorities (`docs/configuration.md:290,294`) |
| 29 | Harness contract | `port.outbound_http` "18081", `port.outbound_l4` "18082", the any-port stat prefix, and the sentence "a mesh service is addressed as `<service>.<namespace>.<mesh-domain>:<port.outbound_http>`" | read: `test/harnesscontract/external-harness.yaml:262-304`, version 3 at `:39` | Two contract bumps, see phases |

Who dials the spellings today (read; counts are lines naming 18081 or 18082):

| Client | Spelling | Where |
|---|---|---|
| Prober, liveness and reachability tiers | loopback listener `127.0.0.1:18081`, **portless** `Host` | `prober/internal/prober/prober.go:86,220-230` |
| Prober, mesh_dns tier | `<target>:18081`; a bare target gets `:18081` appended | `prober.go:46,244,261-265`; `charts/prober/values.yaml:58-59` |
| Prober authz canary | `:18081` | `charts/prober/templates/authz-canary.yaml:174` |
| e2e `uds`, `authz`, `multicluster-config`, `waypoint`, `replicator`, `eastwest-quic` | `:18081` | `e2e/uds.sh:70`, `e2e/authz.sh:74`, `e2e/multicluster_config.sh:434`, `e2e/multicluster_waypoint.sh:393`, `e2e/multicluster_replicator.sh:583`, `e2e/eastwest-quic.sh:212` |
| e2e `multiprotocol` | `:18081` (M0, M1), bare name (M2), real ports (M3, M4), `:18082` (M5, X1) | `e2e/multiprotocol.sh:559-634` |
| e2e `l4-routes` | TCP and TLS on real ports; **UDP on `:18082` only** | `e2e/l4routes.sh:182,193,207,724,776` |
| `charts/udsecho` client | `:18081` | `charts/udsecho/templates/client.yaml:52` |
| The external soak harness | `:18081`, by contract | `external-harness.yaml:278-281`; `042_sortie-soak-driver.md:106,151` |
| Gateway API conformance | **real Service ports already** (80, 8080); no mesh port anywhere | `test/conformance/mesh/manifests.yaml:51-65`; grep of `test/conformance` |
| Docs | `getting-started.md` 14 lines, `workload-requirements.md` 10, `runbook.md` 9, `configuration.md` 4, `README.md` 2 | grep |

Three things the table turned up that are not about clients:

- **Real HTTP ports work today, but bypass the routes.** `cap_http` has no
  virtual host for `<name>:<app port>` unless a route's parentRef names that
  port (`cache/capture.go:872-893`, `:1163-1170`, read). The request falls to
  the catch-all, whose `cluster_header` sends it to the per-port or alias
  cluster with no GAMMA rule applied (inferred). The nightly leg M3 passes for
  this reason and asserts only that the echo answers.
- **Two documents contradict each other** on whether the authority's port is
  stripped: `getting-started.md:514-516` says it is, `workload-requirements.md:258-262`
  says it is not. The code does not strip it (`proxy/filterchain.go:27-30`, read).
- **The any-port counter has no reader.** 037 promised "one WARN per service
  per hour naming the count". No such log exists in Go (grep for `anyport`
  finds only `proxy/capture.go:558,634`).

### 2. Capture

What the two modes are (read, `cni/internal/plugin/capture.go:171-210`): the pod
netns gets a mark in the output chain and a divert in prerouting. The mark rules
are

```
udp dport 18082            mark     (both modes)
tcp dport {18081, 18082}   mark     (both modes: this is "scoped")
meta l4proto tcp           mark     (redirect-all only: every other TCP port)
```

and the divert is `tproxy to :18001` for TCP and a bare `accept` for UDP.

- **Scoped capture is the two mesh ports and nothing else.** Remove the mesh
  ports and the scoped rule set captures no TCP at all.
- **Redirect-all is not "all" for UDP.** UDP is captured on 18082 only, in both
  modes; there is no any-port UDP rule (`capture.go:194`, read; restated in
  `workload-requirements.md:294-295`).
- **Who chooses.** Per pod: `capture.aether.io/redirect-all` `"true"`/`"false"`,
  else the node default (`cni/internal/plugin/plugin.go:742-760`, read). The
  node default is the chart value `agent.captureRedirectAllDefault`, **true**
  (`charts/aether/values.yaml:325`); the CNI flag's own default is false
  (`cni/internal/cmd/root.go:47`). There is no namespace-level switch (grep).
- **Who uses scoped.** One shipped manifest sets the opt-out: the edge
  Deployment (`charts/aether/templates/edge-deployment.yaml:51`), on a pod that
  is also `aether.io/managed: "false"` and therefore gets no capture at all
  (`plugin.go:131-140`), so the annotation is redundant there (inferred). No
  e2e suite, test manifest or chart sets either the value or the annotation
  (grep). Conformance relies on redirect-all (`test/conformance/README.md`).
- **The agent already has only one mode.** It hardcodes the redirect-all
  listener shape for every pod: `snapshotCache.SetCaptureRedirectAll(true)`
  (`agent/internal/cmd/root.go:567-568`, read; the only non-test caller). The
  scoped listener shape (`cap_tcp_blackhole`, the catch-all HCM chain, the 404
  fallthrough, the `blackhole` cluster) is built only in unit tests. A pod
  opted out at the CNI gets scoped *rules* and a redirect-all *listener*.

Consequence: removing the mesh ports makes redirect-all the only TCP capture
mode. What is lost, exactly:

- A managed pod can no longer ask for "mesh only what I dial on a mesh port,
  leave every other connection untouched by the proxy". The replacements are
  the per-pod carve-outs that stay (`capture.aether.io/exclude-outbound-ports`,
  `capture.aether.io/exclude-outbound-ip-ranges`) and the loopback listener
  (decision 5), which loopback traffic reaches without any capture rule
  (`capture.go:175`: `127.0.0.0/8` is accepted before the marks).
- `capture.aether.io/redirect-all: "false"` changes meaning from "scoped" to
  "this pod's outbound TCP is not captured". `"true"` becomes a no-op.
- `agent.captureRedirectAllDefault` and `--capture-redirect-all-default` go.
- Nothing changes for unmanaged pods and ignored namespaces: they get no
  capture today (`plugin.go:80-85,131-140`).

### 3. UDP

How it works today (read): one listener per pod, `capture_udp_<pod>`, bound
`0.0.0.0:18082` in the pod's network namespace with `transparent: true`, built
only when a `UDPRoute` arm exists (`proxy/l4route.go:245-331`). Its `udp_proxy`
matches on the destination **address** (one arm per parent ClusterIP) and
forwards plaintext to `pod_ip:<port>` from the proxy's own network namespace;
there is no inbound listener, no mTLS and no identity on this path
(`cache/capture.go:735-738`). The port it forwards to is the service's primary,
not a port declared `=udp` (`cache/capture.go:657-674`, `cache/cluster.go:968`;
inferred, and contrary to what 038 says at `038_udp-tproxy-capture.md:167-169`).
Endpoints behind a waypoint are dropped (`proxy/egress.go`, `UDPLoadAssignment`).
A weighted split and a multi-service route are not representable (#914).

Why 18082: a datagram socket replies from the port it is bound to, so the
client must dial the port the listener binds (038, *The port is not a free
variable*).

**Measured for this proposal** on the pinned proxy (Envoy 1.40.0-dev at proxy
commit `3119155a`), Linux 6.8, one unprivileged network namespace, no aether
code, no cluster. A client at `192.0.2.1`, a service address `10.96.0.10`, two
echo backends tagged A (the matcher's arm) and B (`on_no_match`). The method is
in the appendix.

| # | Arrangement | Connected client socket | Unconnected socket sees | Reading |
|---|---|---|---|---|
| T1 | Client dials the real port 5353; divert to the listener bound `0.0.0.0:18082` | **no reply** | `A` from `10.96.0.10:18082` | The reply leaves from the bound port. A connected client drops it |
| T1c | Control: client dials 18082 (today) | `A` | `A` from `:18082` | Today's shape works |
| T1p | As T1, matcher on destination **port** = 5353 | no reply | `B` from `:18082` | The matcher's port input is the listener's port, not the datagram's |
| T2 | Listener bound `0.0.0.0:5353` (the real port) | `A` | `A` from `10.96.0.10:5353` | Binding the real port works |
| T2m | Two listeners, 5353 and 5354, same address | `A`, `B` | from `:5353`, `:5354` | One listener per port distinguishes ports |
| C1 | The application already holds `0.0.0.0:5353`; then the listener, on `0.0.0.0:5353` or on `10.96.0.10:5353` | — | — | **The proxy fails to bind:** `Address already in use`, also when the application set `SO_REUSEADDR` |
| C2 | The listener holds `0.0.0.0:5353` or `10.96.0.10:5353`; then the application binds `0.0.0.0:5353` | — | — | **The application fails to bind:** `EADDRINUSE`, with and without `SO_REUSEADDR`. It binds only with `SO_REUSEPORT` |
| S | Wildcard listener on 5353, no divert; client dials the pod's own address `:5353` | `B` | `B` | A wildcard listener also takes datagrams addressed **to the pod** |
| N1 | Conntrack DNAT of the port only (5353 and 5354 to 18082, address kept), one listener on 18082 | `A`, `A` | from `:5353`, `:5354` | Conntrack repairs the reply's source port. The proxy cannot tell the two ports apart |
| N2 | DNAT 5353 to 18100 and 5354 to 18101, a listener per reserved port | `A`, `A` | from `:5353`, `:5354` | Works, distinguishes ports, binds no application port |

So, to the question asked: **a transparent UDP listener in the pinned proxy
cannot reply from the original destination port without being bound to it**
(T1), and its matcher cannot see that port either (T1p). Only two things put
the right source port on the reply: binding the port (T2), or letting conntrack
rewrite it (N1, N2).

The options:

- **(a) One bound listener per distinct real UDP port, per pod.** Works on the
  wire (T2, T2m) and fails in the namespace: the listener lives in the pod's
  network namespace, where the application binds the same number whenever a
  pod is both a server and a client of a UDP protocol (DNS, gossip, NTP peers,
  most game and media protocols). Either the proxy's listener is rejected (C1)
  or the application cannot start (C2); with `SO_REUSEPORT` on the application
  the two would share the port, and a wildcard listener takes the pod's own
  inbound datagrams (S). Under different user IDs, which is the case in a pod,
  the `SO_REUSEPORT` case fails too (inferred from the kernel's bind rule; the
  rig had one user). It also needs the capture rule to follow the port set,
  which today is written once at CNI ADD and names one constant. Listener
  count: pods × distinct UDP ports in each pod's scope, each with one socket per
  worker (inferred for `udp_proxy`; stated for QUIC at `proxy/ingress.go:418-420`),
  and each paused and forwarded across a hot restart by the same generic
  listener code the carried patches change (`proxy/README.md:125-133`). No
  listener count for a real cluster is recorded in the repository, so the
  number is not established. **Rejected: C1 and C2 are not edge cases.**
- **(b) Keep 18082 for UDP only.** No work and no risk. It keeps a mesh port,
  a per-service primary for UDP, and the rule that a UDP service has one port:
  the three things the decision removes.
- **(c) Conntrack port translation to reserved listener ports** (N2). Honours
  the decision and stays in the proxy. It needs a per-pod kernel map from
  (service address, real port) to a reserved listener port, kept current as
  services and ports come and go; nothing programs a pod's rules after CNI ADD
  today (inferred: `plugin.go:142-172` installs once). It needs a listener per
  distinct real UDP port per pod, and a port allocator. 038 rejected the same
  shape for this cost (`038_udp-tproxy-capture.md:757-763`).
- **(d) Stop proxying application UDP.** Give the generated Service its UDP
  ports as ordinary Service ports with `EndpointSlice`s the registrar writes
  from the registry, and let the cluster's own service routing carry them:
  no capture, no listener, no port translation of ours, and the reply is
  repaired by the same machinery that repairs every other Service. This is the
  prior art's position.

**Recommendation: (d), with (b) as the state until it ships.** The evidence:

1. Every proxy-based option needs either a bind the pod can collide with
   (measured) or a new per-pod dynamic kernel mechanism plus a listener family
   (N2), to carry traffic the mesh neither encrypts nor authenticates (read).
2. What the proxy adds for UDP today is endpoint selection from the registry,
   registry health, and `udp_` stats with an L4 access-log line. It does not
   add identity, mTLS, weights, more than one backend, more than one port per
   service, or a path through a waypoint (all read, above). `EndpointSlice`
   conditions carry the first two.
3. It deletes the per-pod UDP listener, the `udp:` clusters, the UDP mark rule
   and the UDP half of port 18082, and it lets an unmanaged client reach a mesh
   UDP service, which it cannot today.

What (d) costs: the per-service UDP stats and the UDP lines of
`aether_l4_access_logs`; `UDPRoute` served by the proxy (it becomes "the
parent Service's slices carry the chosen backend's endpoints", written by the
registrar, or it is withdrawn: decision 1); a registrar that writes
`EndpointSlice`s (new RBAC); endpoint freshness at the speed of the cluster's
service routing instead of EDS. East-west QUIC is not affected: it is mesh
transport on 18008, not application UDP. None of the carried patches goes away:
they serve the QUIC listeners. **(d) is not measured.** It is stock Kubernetes
behaviour, but no aether cluster has run it, and its gate is in Phase 2.

### 4. The generated Service as an ordinary multi-port Service

- **Today** (read): selectorless, `ClusterIP`, named after the ServiceAccount in
  the service's namespace, four name-only TCP ports, three annotations
  (`generator.go:169-176`, `:252-266`). `apply` converges two annotations and
  the ports by name, port and protocol (`generator.go:231-243`); `targetPort`
  and `appProtocol` are not compared (`:182-197`).
- **Target:** one `ServicePort` per declared port, `port` = `targetPort` = the
  pod's port, `protocol` TCP or UDP, `appProtocol` `http` or `tcp` (a UDP port
  needs none), named `p<port>-<tcp|udp>`.
- **Where the ports come from:** `ServiceEndpoint.port_protocols`, which both
  backends already populate (`registry/cni.go:61-89`,
  `registry/internal/k8s/kubernetes.go:513-538`, read).
- **A rollout that changes ports.** The Service carries the **union** over the
  service's endpoints. That is safe for the reason 005 gave: membership of a
  port's endpoint set is filtered per port, so a client of `:9090` only reaches
  pods that declare 9090. A port leaves the Service when the last endpoint
  declaring it leaves the registry.
- **A rollout that changes a port's protocol.** Two endpoint sets exist for the
  number (037 design (c): membership by port **and** protocol) but capture can
  build one chain for it. Rule: the class most endpoints declare, ties to HTTP
  then TCP, order-independent, with a WARN. A client speaks one protocol, so
  this cannot be made hitless by any rule; the documented way to retype a port
  is a new port number.
- **Port 80.** Today `http://<name>/` reaches the primary because the portless
  authority is the default cluster, and the Service's port 80 is a name-only
  placeholder. **After removal it works only if a pod declares port 80.** The
  default application port is 8080 (`common/constants/endpoint.go:5`), so for
  nearly every existing service the bare URL stops working. See *Risks*.
- **Collisions.** A user Service that owns the name is never touched; the
  generator logs and skips, writes no status and no event
  (`generator.go:221-224`, read). Then there is no mesh VIP and no mesh-DNS
  record for the name, and the service is reachable only by its `cluster.local`
  name under redirect-all. With real ports this case matters more: see fact 5.
- **One number, two transports.** `port_protocols` is `map<uint32, PortProtocol>`
  (`endpoint.proto:117`): a port number has one class. A pod cannot declare
  53/TCP **and** 53/UDP. Not solved here (decision 7).
- **The two backends.** They differ in one way that matters: `etcd` lists a
  pod under every protocol it was registered under (the agent registers once
  per distinct class, `agent/internal/cni/server/dualregistration.go:36-48`);
  `kubernetes` lists by the pod-level annotation alone
  (`kubernetes.go:498-509`). #1677 **measured** the consequence on kind: on the
  chart's default backend the nightly-gated shape (HTTP primary, `9000=tcp`)
  fails leg M4 (a capture chain for `:9000` with no cluster behind it), and CI
  never sees it because every multi-protocol suite runs on `etcd`.
- **Multi-cluster.** Endpoint keys mirrored between regions keep their
  `/protocols/<P>/` segment and are replayed verbatim
  (`registrar/internal/replicator/replicator.go:251-349`, read), so the key
  layout must not change, and this proposal does not change it. A peer on an
  older release still lists by protocol (compatible) and still dials `:18081`
  on its own nodes, which is that peer's node proxy's business, not ours: the
  mesh hop between clusters carries a port in the SNI already
  (`cache/mtls.go:197-205`). What does cross is config: an exported route names
  the default cluster (fact 1, row 20).

### 5. Routes that name no port

- **The Gateway API rule** for a mesh route (GAMMA): a `parentRef` to a Service
  with no `port` applies to all ports of the Service; with a `port`, to that
  port. A Service `backendRef` carries a port.
- **Today** (read): the parentRef port only adds `<name>:<port>` domains to the
  service's one virtual host; it never scopes which rules apply
  (`agent/internal/gamma/reconciler.go:292-331`, `cache/capture.go:1163-1170`).
  Two routes with different parentRef ports merge into one virtual host that
  matches both (inferred). The backendRef port is not read at all
  (`project.go:222-241`). Nothing in aether rejects a missing backendRef port;
  the CRD does (`proxy/egress.go:133-135`, comment).
- **What conformance requires.** MESH-HTTP is 7/7 on Gateway API v1.6.3
  (`docs/conformance/baseline-2026-10-11-rev22.md`). Its fixtures are **user
  Services** named `echo`, `echo-v1`, `echo-v2` with `http` 80 → `targetPort`
  8080, `http-alt` 8080, `https` 443 → 8443, `tcp` 9090, `grpc` 7070
  (`test/conformance/mesh/manifests.yaml:51-65`), and the client dials
  `http://echo/`: port 80. It passes today for the reason this proposal
  removes: the portless authority is the primary, and the primary defaults to
  8080.
- **No code reads a Service's port map.** No non-test Go in `agent/` or
  `common/` reads `spec.ports[].targetPort` (grep). The only `targetPort` in
  the mesh is the one the generator writes.

So "real ports" has to mean **the ports of the Service the client named**, not
the ports of the pod. For the generated Service the two are equal by
construction. For a user Service that owns the name, port 80 is real and maps
to pod port 8080, and aether must read that map. Without it MESH-HTTP goes from
7/7 to failing on its first request. `HTTPRouteParentRefPort` and
`HTTPRouteDestinationPortMatching` are unsupported and skipped today; the first
becomes supportable as a side effect and is not claimed here.

## Design

### The model

1. **A service has ports. A port has a transport (TCP or UDP) and a class
   (HTTP or raw TCP; UDP has no class).** The source of truth is the registry's
   per-endpoint `port_protocols`. No field, annotation, listing or name carries
   a protocol for a service.
2. **A Service is a port map.** The generated Service maps each declared port
   to itself. A user Service that owns a mesh service's name maps its own ports
   to `targetPort`s. A client addresses a Service port; the mesh resolves it to
   a pod port and uses that pod port's class. A numeric `targetPort` resolves.
   A named one does not (the registry carries no port names) and is reported
   on the route's status and as a WARN.
3. **One cluster family per port.** HTTP `<fqdn>:<p>` (005), raw TCP
   `tcp:<fqdn>:<p>` (037), QUIC `quic:<fqdn>:<p>@<ns>/<sa>`. No default
   cluster, no floor cluster, no alias cluster.
4. **HTTP is still routed on `:authority`.** A portless authority is port 80,
   because that is what an HTTP client means when it omits the port, and every
   mainstream client writes any other port into `Host`. This keeps one HCM chain
   and one route table per pod. Routing on the connection's destination port
   was tested and is **not available in a route table on the pinned proxy**
   (measured: a virtual-host `matcher` on `DestinationPortInput` is rejected at
   load with "Route table can only match on request headers"). The alternative,
   one HCM chain and one route table per distinct port, is the prior art's
   shape and stays open if the authority rule proves wrong (*Risks*, 6).
5. **Raw TCP is matched on the destination port**, as today for non-primary
   ports: address `/32` plus `destination_port`.
6. **Routes.** A parentRef without a port attaches the rules to the virtual
   host of every HTTP port of the Service; with a port, to that port's. A
   backendRef port is a Service port, resolved through the port map to
   `<backend fqdn>:<pod port>`.
7. **A port nobody declared is refused**: HTTP by a `421` naming the service's
   ports, TCP by the Service having no such port (`ECONNREFUSED`).

### What stays

- Pod annotations do not change. `endpoint.aether.io/ports` declares ports
  with per-port suffixes; `endpoint.aether.io/protocol` stays the default class
  for unsuffixed ports (a pod-level shorthand, not a service fact);
  `endpoint.aether.io/port` stays an accepted way to declare one port and
  names the liveness probe port (decision 6). No harness-contract entry for an
  annotation changes.
- The registry API, the wire, the etcd key layout and the registrar snapshot
  key. `ServiceEndpoint.port` stays on the wire for older readers.
- The mesh hop: `pod_ip:18008`, SNI selects the port, EDS is a membership
  filter. The waypoint's SNI form. Mesh DNS. UDS delivery.
- TCP capture by TPROXY to 18001. East-west QUIC on 18008.

### The legacy spellings during Phases 1 and 2

They keep working where they work on `main`, and stop being broken where they
are broken, by one rule that names no service-level protocol:

> The *legacy target* of a service is the port its pods declare in
> `endpoint.aether.io/port`. `:18081` and the bare name reach it **if that port
> is declared HTTP**; `:18082` reaches it **if that port is declared raw TCP**
> (or UDP, for the UDP listener). Otherwise the spelling names nothing.

- For a single-protocol service this is exactly today's behaviour.
- For a mixed service whose legacy target is HTTP it is today's behaviour too:
  legs M0 to M5 keep their meaning.
- For a mixed service whose legacy target is raw TCP (#1672), `:18082` starts
  working, and `:18081` and the bare name are **refused at once** with `421`
  and `x-aether-error: port-required`, the body listing the service's ports,
  where `main` answers 502. That header and body are the ones Phase 3 gives
  every service for a spelling that names no port, so the refusal does not
  describe a primary and does not have to be unlearned.
- No any-port chain for a service that declares any HTTP port. On `main` the
  portless `/32` chain of a TCP-primary mixed service outranks the HCM and takes
  the service's HTTP ports (measured in #1677 on the kubernetes backend).

The alternative, making the legacy spellings work for mixed services by a new
rule (lowest port of the class, as 037 wrote and never built), adds semantics
to something scheduled for deletion. Refusing is the least surprising: a
client either gets what it got before or an answer that tells it what to dial.

## Phases

Chart versions follow the compatibility window (`AGENTS.md`, *Compatibility
window*): the chart is 2.6.1 today; a thing deprecated in a published 2.8.x
chart may be removed in 2.9.0 and not before. Each phase ships alone and can be
rolled back to the chart before it.

### Phase 1 (chart 2.7.0): per-port truth, no client-visible change

Fixes #1672 and the kubernetes-backend failure of the gated shape. After it, a
call to a **real** port works for both mixed shapes on both backends.

Registry and registrar:

- kubernetes backend: a pod is listed under every protocol it serves a port
  of. `RequireProtocolDisjoint` is replaced by the per-port contract and run
  against both backends.
- The generated Service gains the real ports with per-port `appProtocol`,
  **beside** the four legacy ports. `apply` converges `targetPort` and
  `appProtocol`.
- `aether.io/app-protocol` and `aether.io/port` are frozen: the registrar
  stamps what the 2.6 registrar would have stamped on the same backend (on
  `etcd`, the first class the service is listed under; on `kubernetes`, the
  class of the declared port). No 2.7 component reads them. They exist for 2.6
  agents and are deleted in Phase 3.

Agent:

- Every reader in fact 1 takes a per-port answer from the endpoints it holds.
  The capture reconciler stops reading the annotation; `PrimaryIsTCP` goes.
- HTTP clusters and endpoint sets are built per (port, class): no h2 cluster
  for a port declared raw TCP (`cache/cluster.go:671-686` builds one today,
  inferred).
- A virtual host per HTTP port on `cap_http` and `out_http`, carrying the GAMMA
  rules; the known-target safety net becomes per-port. This closes the
  "real ports bypass the routes" gap.
- The Service port map: the agent reads the ports of the Service that owns
  each mesh name and resolves parentRef and backendRef ports through it.
- Destination: an explicit chain for every port, including the declared one
  (`in_tcp_<pod>_<p>` for a raw-TCP port instead of an HCM chain). The no-SNI
  chains stay. Sources keep sending what they send today.
- Liveness: the probe is typed by the probe port's own class.
- Edge: per-port TCP clusters are published.
- The legacy rule above, in one function used by one reader.

What of #1677 this phase keeps, and what it must not ship:

| #1677 | Phase 1 |
|---|---|
| The measurements: both backends, both shapes, the skew table | **Keep**, as this proposal's evidence and as the Phase 1 gate's baseline |
| `e2e/multiprotocol.sh` legs X1 to X3 in the gate, X6, `MP_REGISTRY_BACKEND=kubernetes` | **Keep** |
| The kubernetes listing change | **Keep** |
| Per-port HTTP clusters only for ports declared HTTP | **Keep** |
| The capture floor decided from the endpoints, not the label | **Keep** |
| No any-port chain for a service that also serves HTTP | **Keep** |
| The liveness probe fix | **Keep** |
| The tests for the above, and the two corrected cache fixtures | **Keep** |
| `ServicePrimaryOf` with the majority rule, as the service's primary and protocol | **Drop** as an exported, service-level answer. Its order-independent vote survives in two private places: the per-port class conflict (fact 4) and the legacy target when pods disagree (decision 4) |
| `421` with `x-aether-error: primary-not-http` | **Drop.** `port-required` instead |
| Labelling the whole Service with the primary's class, and the WARN about disagreeing primaries | **Drop.** The annotation is frozen at its 2.6 value. This also avoids the one skew cell #1677 measured as a regression (new registrar, old agent, `etcd`: the HTTP secondary stopped) |
| Leg X4 (asserts the `primary-not-http` refusal) and X5 (asserts the Service is labelled `tcp`) | **Rewrite**: X4 asserts `port-required`; X5 asserts the real ports and their `appProtocol` |
| The edits to `workload-requirements.md`, the runbook and proposal 037 that define the spellings by the primary | **Drop**; Phase 2 rewrites those pages |

- **Gate (can fail):** `multiprotocol` M0 to M7 and X1 to X6 on **both**
  backends in CI (today: `etcd` only); MESH-HTTP 7/7 and GATEWAY-HTTP 46/46;
  a new leg asserting that a GAMMA rule applies on a real port; the skew table
  re-measured on kind for both backends and both roll orders with zero cells
  worse than 2.6. M7 changes meaning: it asserts the real ports as well as the
  legacy four.
- **Skew:** a 2.6 agent behind a 2.7 registrar sees the same annotation and
  extra Service ports it does not read; on `kubernetes` it also sees pods in
  more listings, which #1677 measured as "nothing gets worse" in that order. A
  2.7 agent behind a 2.6 registrar needs nothing from the Service but its
  address. Inferred from #1677's table for the parts that differ from it; the
  gate measures it.
- **Operator sees:** more ports on generated Services; `kubectl get svc` shows
  what a service serves. No client changes.
- **Harness contract:** version 3, unchanged (new facts only).
- **Rollback:** to 2.6.x. The extra Service ports are removed by the 2.6
  registrar's own convergence.

### Phase 2 (chart 2.8.0): deprecation, counted

- **Deprecated, removed in chart 2.9.0:** `:18081` and `:18082` on a mesh
  name; the bare name where the Service has no port 80; a portless `Host` on
  the loopback listener; the any-port chain; `agent.captureRedirectAllDefault`
  and scoped capture; `aether.io/app-protocol` and `aether.io/port`. Announced
  in the runbook's "Chart 2.8.0" section and where each is defined, with the
  removing version, as the window requires.
- **Counted.** Each legacy spelling is served by its own named route or chain:
  `legacy_mesh_port`, `legacy_portless`, `cap_tcp_legacy18082_<cluster>`, the
  existing `cap_tcp_anyport_<cluster>`, and the UDP listener's own prefix.
  Per destination and spelling this is a native proxy counter (a per-route
  `stat_prefix`; the chain's `tcp_proxy` prefix). Per **source** namespace and
  service it needs the `aether_stats` filter to carry the route's name as a
  tag, which is our filter but C++ that was not read for this proposal; the
  floor, if that turns out to be more than a tag, is the `route_name` and
  `filter_chain_name` fields the access logs already carry
  (`proxy/accesslog.go:145,265-269`). The agent logs each (source, destination,
  spelling) once.
- **Moved to real ports:** the prober (mesh_dns targets with real ports, a
  `Host` with a port on the loopback listener, the default for a bare target
  no longer `:18081`), every e2e suite in fact 1, `charts/udsecho`, the docs
  and website pages, and the contract's addressing sentence.
- **Twins and config:** QUIC twins become per-port (new names; the old names go
  dormant, never forgotten, as #1036 requires). `GammaBackend` and the
  projection gain an additive `port`; exporters write it beside the legacy
  cluster name.
- **UDP, if decision 1 is (d):** the registrar writes `EndpointSlice`s for UDP
  ports; `:18082/udp` keeps working and is counted.
- **Gate (can fail):** every suite green on real ports only; a leg that dials
  each legacy spelling once and asserts its counter **moved** (a zero is
  evidence only if the counter can be seen to count); the UDP leg of
  `l4-routes` dials the real port on both backends; MESH-HTTP 7/7.
- **Skew:** none new on the wire. A 2.7 importer ignores the new `port` field.
- **Operator sees:** WARN lines naming clients on a legacy spelling; a
  dashboard row per spelling; the deprecation in the runbook.
- **Harness contract:** version 4. The meaning of how a service is addressed
  changes (a bump by the README's last row); the counter is a new entry.
- **Rollback:** to 2.7.x; nothing was removed.

### Phase 3 (chart 2.9.0): removal

Entry condition: the legacy counters at zero on the reference cluster over the
whole of 2.8.x, with the witness leg showing they count. If not zero, the
counter names the client and the removal waits a minor, as 037 did.

Deleted:

- The `:18081` and `:18082` domains, chains and alias clusters; the 421 body's
  mesh-port spellings; `ProxyL4OutboundPort`; the scoped mark rules for both.
- The any-port chain and its contract entry (037 Phase 4).
- The default cluster `<fqdn>`, the floor cluster `tcp:<fqdn>`, the bare EDS
  name's special owner, the default virtual host, `aliasAuthorityPorts`.
- The portless authority as "the primary": it is port 80 or it is refused.
- The Service's `mesh`, `mesh-tcp`, `http` and `https` placeholder ports; the
  clusterset Service's `mesh` port.
- `aether.io/app-protocol`, `aether.io/port`, `PrimaryIsTCP`,
  `CaptureTCPService` as a class carrier, the generator's first-wins loop,
  every "a service is registered under exactly one protocol" comment.
- The legacy-target function and both private uses of the vote that served it.
- Scoped capture: the chart value, the CNI flag's effect (the flag is accepted
  and ignored for this minor, because a 2.8 chart still passes it), the scoped
  listener shape and its `blackhole` cluster.
- UDP per decision 1: under (d), the per-pod UDP listener, the `udp:` clusters,
  `UDPStatKey`, the UDP mark rule.
- Sources switch to SNI = port for every TCP port; destinations have had the
  explicit chains since 2.7.

- **Gate (can fail):** a leg per removed spelling asserting the documented
  refusal (`421 port-required`, `ECONNREFUSED`) and not a hang; conformance
  unchanged; an 8-hour soak on real ports.
- **Skew:** a 2.8 agent beside a 2.9 registrar loses the legacy Service ports
  and the annotation; it stopped reading both in 2.7. A 2.8 source beside a
  2.9 destination still sends the no-SNI forms, which the destination keeps
  for this minor.
- **Operator sees:** a client still on a legacy spelling gets `421` with the
  ports to dial, or a refused connection. The runbook's "Chart 2.9.0" section
  says what a cluster that skipped 2.8 sees: the same, with no counter having
  warned it.
- **Harness contract:** version 5 (entries removed).
- **Rollback:** to 2.8.x restores every spelling; clients moved in 2.8 keep
  working on both.

### Phase 4 (chart 2.10.0): receiver-side tolerances

The destination's no-SNI h2 chain and its default raw-TCP chain, kept through
2.9 for 2.8 sources, and the CNI flag. `ServiceEndpoint.port` stays on the wire
as the liveness probe port.

## Risks

1. **Every existing client breaks at Phase 3.** There is no service whose
   callers are unaffected: the documented way to call a mesh service is
   `:18081` (`getting-started.md:69-72`). The mitigation is the counter and one
   whole minor; a cluster that skips 2.8 gets no warning.
2. **The port 80 convenience goes.** `http://<svc>.<ns>.<domain>/` works only
   for a service with a port 80. With the default application port at 8080
   that is almost none. The two ways back are to listen on 80, or to own the
   name with a Service that maps 80 to the pod's port, which only covers the
   `cluster.local` name today (fact 4, *Collisions*).
3. **UDP.** Under (d) the mesh stops seeing application UDP: no per-service
   UDP stats, no L4 log line, and `UDPRoute` changes hands or goes. Under (b)
   the decision has a standing exception. Under (c) there is a new mechanism in
   every pod's network namespace. None of the three has run in a cluster.
4. **Multi-cluster peers.** A peer more than one chart minor behind exports
   routes with no port; at Phase 3 those backends cannot be resolved and are
   dropped with a counted WARN. Clusters must cross 2.8 together or within the
   window.
5. **Measured only in a rig, or only on kind.** The UDP table is one network
   namespace on one kernel with one user ID: no CNI, no cluster service
   routing, no second node. #1677's tables are single-node kind; its
   agent-first order was not run on the kubernetes backend. The reference
   cluster has run none of this.
6. **The authority rule.** A client that dials port 8080 and sends a portless
   `Host` is routed as port 80 and refused. No mainstream client does this; the
   prober does on purpose and moves in Phase 2. If it turns out to matter, the
   fallback is a chain per port, with its cost in filter chains per pod.
7. **The Service port map is a new reader.** A user Service with a named
   `targetPort`, or with a selector that is not the ServiceAccount's pods,
   resolves to nothing or to the wrong set. It is reported, not guessed.
8. **Capture listener churn.** More per-port chains mean more reasons to
   update a per-pod listener, which drains it (037, Risk 4). The trigger must
   stay edge-triggered on the derived port set.
9. **Two transports on one port number** cannot be declared (fact 4). DNS is
   the obvious case, and under (d) it is the first thing a user will try.

## Decisions for the owner

1. **UDP.** (b) keep 18082 for UDP, (c) port translation to reserved listener
   ports, or (d) stop proxying application UDP. *Recommended: (d), with (b)
   until it ships.* If (d): is `UDPRoute` kept (registrar-written slices) or
   withdrawn? *Recommended: kept for the single-backend shape it supports
   today, withdrawn otherwise.*
2. **Port 80.** Is the plain break accepted, or should the generated Service
   be able to map 80 to a declared port? *Recommended: accept the break.* A
   mapping is a second port vocabulary on the pod; a user who wants 80 writes
   a Service, as in Kubernetes.
3. **`capture.aether.io/redirect-all: "false"`** after Phase 3: "outbound not
   captured", or the annotation is removed and the value rejected?
   *Recommended: keep it, meaning not captured.*
4. **The legacy target when a service's pods disagree** during Phases 1 and 2:
   the order-independent vote from #1677, private to the legacy path, or
   today's `endpoints[0]`? *Recommended: the vote.* It is deleted in Phase 3
   either way.
5. **The loopback listener `127.0.0.1:18081`.** It is a mesh-specific port,
   but it addresses the proxy, not a service; the CNI readiness probe and the
   prober's liveness tier use it. *Recommended: keep it, require a port in
   `Host`.* Removing it is a separate proposal.
6. **`endpoint.aether.io/port`.** With no primary it is one more declared port
   and the liveness probe port. *Recommended: keep the name and say so.* The
   alternative is a new `health-port` annotation and a contract bump.
7. **TCP and UDP on one port number.** Needs an additive registry field.
   *Recommended: out of scope, its own change, before Phase 3 if decision 1 is
   (d).*
8. **Pace.** Phases 1 and 2 could share 2.7.x, with removal in 2.8.0.
   *Recommended: three minors as written*; Phase 1 fixes a bug and should not
   wait for the prober and the harness to move.

## Out of scope

- TLS termination for `https://<svc>/` (037 option (b)); a TLS port is a
  raw-TCP port the application declares.
- Protocol sniffing as a fallback for an undeclared port.
- Inferring ports from a user Service's `appProtocol` instead of the pod.
- mTLS or identity for UDP.
- Dropping the protocol segment from the registry key.
- SPIRE-off raw TCP (#877).
- `HTTPRouteParentRefPort` conformance.

## Not established

- Listener and filter-chain counts on a real cluster, for any option.
- Whether `aether_stats` can tag a request with its route's name without a
  filter change (the C++ was not read).
- Whether anything can program a pod's capture rules after CNI ADD (option
  (c) depends on it).
- The `SO_REUSEPORT` case of the bind conflict under two user IDs.
- Option (d) end to end, on any cluster, on either backend.
- Which upstream GATEWAY-HTTP fixtures use which Service ports (they are not
  in this repository); only the MESH overlay was read.
- Whether the clusterset Service is pruned by the mesh-Service generator's
  label selector (it carries `aether.io/mesh-service=true` and is never in the
  generator's desired set; inferred from `import_generator.go:285-288` and
  `generator.go:139-152`, not exercised).

## Appendix: the UDP measurement

One network namespace (`unshare -Urn`), the pinned proxy with `--concurrency 1`,
one static listener per case with `transparent: true` and a `udp_proxy` whose
matcher routes to cluster A on a match and to B on `on_no_match`. A dummy
interface holds `192.0.2.1/24` and the default route; `10.96.0.10` is not a
local address.

```
ip rule add fwmark 1 lookup 100
ip route add local 0.0.0.0/0 dev lo table 100

# divert (T cases): mark on output, deliver locally on prerouting
nft add chain ip t out '{ type route hook output priority mangle; }'
nft add chain ip t pre '{ type filter hook prerouting priority mangle; }'
nft add rule ip t out ip daddr 10.96.0.10 udp dport 5353 meta mark set 1
nft add rule ip t pre ip daddr 10.96.0.10 udp dport 5353 tproxy to :<listener port> meta mark set 1 accept

# port translation (N cases): the address is kept, conntrack owns the port
nft add chain ip t nat '{ type nat hook output priority -100; }'
nft add rule ip t nat ip daddr 10.96.0.10 udp dport 5353 dnat to 10.96.0.10:<listener port>
nft add rule ip t pre udp dport <listener port> meta mark set 1
```

Each case sends one datagram from a connected socket and one from an
unconnected socket and records what came back and from where; the listener's
`downstream_sess_total` confirmed in every case that the datagram arrived. T1
uses an explicit `tproxy` port rewrite where the shipped CNI rule uses a bare
`accept` to a listener on the dialled port; the kernel delivers to the same
socket either way, and T1c is the shipped shape. The bind cases (C1, C2, S) use
no rule: a plain socket binds the port before or after the proxy starts. The
HTTP check in *Design* point 4 is two listeners sharing one inline route table
whose virtual host carries a `matcher` on `DestinationPortInput`; the proxy
refuses the configuration at load.
