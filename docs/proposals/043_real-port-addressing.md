# Proposal 043: A mesh service is addressed by its real ports, and nothing else

**Status:** Accepted 2026-10-11 — by the owner, with all twelve decisions taken
as recommended (see *Decisions*). Phase 1 is built (chart 2.7.0; what it
measured against this document is under *Phase 1 as built*). Planned: Phase 2
in 2.8.0, Phase 3 in 2.9.0, Phase 4 in 2.10.0. The facts
were read from `main` at `2a132053`; the measurements were made on the pinned
proxy outside any cluster.
**Resolves:** #1672. The defect reported there is the per-service primary
itself; the owner decided to remove it, not to patch it. The acceptance of
this proposal closes the issue: the phases below are the work, and Phase 1 is
where the reported behaviour is fixed.
**Author:** Bruno Palermo
**Date:** 2026-10-11
**Related:** #1672 (a raw-TCP primary with an HTTP secondary has no TCP path),
#1682 (a `cluster.local` name with a non-primary port reaches the primary
port; the same rule broken, fixed in Phase 1), #1677 (the first fix, closed; its branch is kept for Phase 1: it measured both
registry backends, and this proposal says which parts of it survive), proposals
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

**Decided (decision 1): (d), with (b) as the state until it ships.** The
evidence the recommendation rested on:

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
registrar, for the single-backend shape only: decision 1); a registrar that
writes
`EndpointSlice`s (new RBAC); endpoint freshness at the speed of the cluster's
service routing instead of EDS. East-west QUIC is not affected: it is mesh
transport on 18008, not application UDP. None of the carried patches goes away:
they serve the QUIC listeners. **(d) is not measured.** It is stock Kubernetes
behaviour, but no aether cluster has run it. Its work in Phase 2 therefore
starts with a measurement on kind, on both registry backends, before anything
is built on it.

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
  53/TCP **and** 53/UDP. Not solved here; it is its own change, due before
  Phase 3 (decision 7).
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
4. **For captured HTTP, the dialled port is the port; `Host` names the
   service.** The capture listener has one HCM chain and one route table per
   distinct HTTP port number in the mesh, selected by the connection's
   destination port and the sniffed application protocol. This is the prior
   art's shape, and it is the adopted design because the single-chain layout
   was measured not to work (*The addressing rule*, below).
5. **Raw TCP is matched on the destination address and port**, as today for
   non-primary ports: `/32 + destination_port`. The declaration decides; the
   bytes are not consulted.
6. **Routes.** A parentRef without a port attaches the rules to the virtual
   host of the Service in the table of every HTTP port it has; with a port, in
   that port's table. A backendRef port is a Service port, resolved through
   the port map to `<backend fqdn>:<pod port>`.
7. **A port nobody declared is not routed.** HTTP is refused with a `421`
   that lists the service's ports. Raw TCP has no chain and falls to the
   passthrough, where the answer is the cluster's: a port the Service does not
   have (decision 10).

### The addressing rule

Stated once. Everything else in this document that mentions a dialled port or
a `Host` header follows from this section.

#### Why the dialled port decides (measured)

The first version of this proposal kept one HCM chain for all captured HTTP and
selected the port from `Host` ("`Host` wins; a port-less `Host` is port 80").
The owner asked whether the filter-chain match could do it instead: the
dialled port and the sniffed protocol choose the chain, as the prior art does
(from general knowledge of Istio's sidecar; not verified against its source).
It can, and the single-chain layout turns out to be broken, today, on `main`.

Measured on the pinned proxy (Envoy 1.40.0-dev, proxy commit `3119155a`),
Linux 6.8, one network namespace, a transparent TCP listener on 18001 with the
shipped capture listener's filters and timeouts (`original_dst`,
`http_inspector`, `tls_inspector`; `listener_filters_timeout` 1 s, continue on
timeout; `proxy/capture.go:196-215,274-280`, read) and TCP diverted to it with
the shipped rule shape (mark on output, `tproxy to :18001` on prerouting).
Service A declares 9000 and 3306 as raw TCP; service B serves HTTP; address C
is not in the mesh. The method is in the appendix.

| # | Layout | Connection | Selected | First byte |
|---|---|---|---|---|
| L1 | **today**: one HCM chain with no destination port, plus A's `/32 + 9000` chain | HTTP to B:8080 | the HCM | 1 ms |
| L1 | | **HTTP to B:9000** | **the passthrough, not the HCM** | 2 ms |
| L1 | | raw bytes to A:9000; HTTP bytes to A:9000 | A's TCP chain, both | 2 ms |
| L2 | **per port**: an HCM chain for 8080 and for 9000 (destination port + sniffed HTTP), A's TCP chains, the port-less HCM chain, the passthrough | HTTP to B:8080, `Host` port-less or `:8080` | table 8080, B's virtual host | 1 ms |
| L2 | | HTTP to B:9000, `Host` port-less | table 9000, B's virtual host | 1 ms |
| L2 | | HTTP to B:8080, `Host: b:9000` | table 8080, no virtual host: the table's `421` | 1 ms |
| L2 | | raw bytes to A:9000; HTTP bytes to A:9000 | A's TCP chain, both | 2 ms |
| L2 | | non-HTTP bytes to B:9000 | the passthrough | 2 ms |
| L2 | | HTTP to B:7777 (no chain names 7777) | the port-less HCM chain | 1 ms |
| L2 | | h2c with prior knowledge to B:8080 | table 8080 (the proxy answers with HTTP/2 SETTINGS) | 1 ms |
| L2 | | a TLS ClientHello to B:8080 | the passthrough | |
| L2 | | `GET mykey\r\n` (no HTTP version) to B:8080 | the passthrough: not detected as HTTP | 1 ms |

The second row is the finding. Envoy's filter-chain match is a tree walked one
criterion at a time (destination port, then address, then server name,
transport protocol, application protocol), and **it does not backtrack**: once
any chain names destination port 9000, a connection to port 9000 is matched
only among chains that name 9000. One raw-TCP declaration of a port number by
any service therefore takes that port number away from the port-less HCM chain
for **every** service on that listener, and the HTTP request leaves through the
passthrough, outside the mesh. TCP chains are built for every mesh Service
with an address, not only for the pod's dependencies
(`cache/capture.go:329-336`, read), so the effect is mesh-wide. This holds on
`main` today for any port number that some service declares as raw TCP (a
non-primary `=tcp` port, or a TCP primary), and it is reported as a finding
with this pull request. The repository uses `filter_chain_match`; the unified
`filter_chain_matcher` is not used anywhere (grep) and was not measured.

Keeping one HCM chain would therefore need a per-port HCM chain anyway for
every port number that any service declares as raw TCP. Two rules for "which
port" depending on what an unrelated service declared is worse than either
rule alone, so the per-port layout is adopted for every HTTP port number.

Also measured: the route table itself cannot branch on the destination port
(a virtual-host `matcher` on `DestinationPortInput` is rejected at load:
"Route table can only match on request headers"), so the port has to be chosen
by the chain, with a route table per port.

#### The chains of a capture listener

| Chain | Match | Does |
|---|---|---|
| `cap_http_<P>`, one per distinct HTTP port number `P` in the mesh | destination port `P`, `raw_buffer`, sniffed `http/1.0`, `http/1.1` or `h2c` | HCM with route table `cap_http_<P>` |
| `cap_tcp_<cluster>_<P>`, one per (service, raw-TCP port) | destination port `P`, the service's address `/32` | `tcp_proxy` to `tcp:<fqdn>:<P>` (exists today, `proxy/capture.go:297-318`) |
| `cap_http_other` | `raw_buffer`, sniffed HTTP, no destination port | HCM for HTTP on a port number no service declares as HTTP: refuses mesh names, passes everything else through |
| default | none | passthrough to the original destination |

"Port number in the mesh" is read from the Services (generated ones carry
real ports from Phase 1; a user Service that owns a mesh name contributes its
own ports), which the agent already lists for every namespace
(`capture/reconciler.go:145`, read). It is not demand-scoped, so a first call
to a service the pod has never used still finds its port's chain.

Inside the tree for port `P`, an address with a `/32` chain is raw TCP whatever
the bytes are, and every other address is offered to the HCM chain if the
bytes are HTTP and to the passthrough if they are not.

#### Names

A mesh service answers to two families of name, with or without `:<port>`:

- the mesh name `<svc>.<ns>.<mesh-domain>`;
- the Kubernetes names `<svc>.<ns>.svc.cluster.local`, `<svc>.<ns>.svc`,
  `<svc>.<ns>`, and the bare `<svc>` when exactly one namespace owns it
  (today's rule, `cache/capture.go:1144-1156`, read).

The rule is the same for both. Today it is not: a Kubernetes name with a
non-primary port reaches the primary port (#1682, measured on kind), because
the known-target route accepts any `:port` and sends it to the default
cluster. Phase 1 fixes #1682, and under this layout it cannot recur: the
request to `:9090` is in table 9090, which has no route to anything else.

#### The rule, per entry path

- *(a) Captured.* The pod dialled a Service address and port `D`. Table
  `cap_http_<D>` holds, for each service with HTTP on `D`, one virtual host
  whose domains are every name of the service, port-less and with `:D`.
- *(b) Loopback.* The pod dialled `127.0.0.1:18081` and wrote the service into
  `Host`. No capture rule is involved (loopback is accepted before the marks),
  so this is identical with capture on and with
  `capture.aether.io/redirect-all: "false"`. There is no dialled service port,
  so the port in `Host` is the port. Mesh names only, as today.
- *(c) Edge to a mesh backend.* The request's authority selects nothing in the
  mesh: the virtual host comes from the Gateway listener's hostnames and the
  port from the route's `backendRef.port`, through the port map.

| Request | (a) captured, dialled `D` | (b) loopback | Today |
|---|---|---|---|
| `Host: <name>:P`, the Service declares `P` as HTTP | `P` = `D`: port `D`. `P` ≠ `D`: `421`, `x-aether-error: port-mismatch` | port `P` | mesh name: reaches `P`, without GAMMA rules unless a parentRef names `P` (inferred). Kubernetes name: the **primary** (#1682) |
| port-less `Host: <name>` | port `D`, if the Service declares `D` as HTTP | port 80 if the Service declares it, else `421`, `x-aether-error: port-required` | the primary, on both paths |
| the Service does not declare the port as HTTP (`D` on the captured path, `P` on loopback) and has no raw-TCP chain for it | `421`, `x-aether-error: port-not-declared` | `421`: `port-not-declared`, or `port-not-http` if `P` is raw TCP or UDP | mesh name: 503 after the 2 s on-demand timeout. Kubernetes name: the primary, silently |
| `D` is a raw-TCP port of the Service | the TCP chain: the bytes go to the port, `Host` is never read | not applicable | the same for a non-primary TCP port |
| non-HTTP bytes to a port the Service declares as HTTP | the passthrough | not applicable (the listener is HTTP) | the same |
| a name that is no mesh service | passthrough to the original destination | `404` | the same |

Every refusal is an immediate `direct_response`, status `421 Misdirected
Request`, with one of four `x-aether-error` values and a body listing the
service's ports and classes. A refusal, not a passthrough, because passing an
HTTP request for a known mesh service to the cluster's own routing fails open.
(The existing `no-http-port` answer for a service with no HTTP port at all
stays through Phase 2 and becomes one of these in Phase 3.)

What changed from the first version of this table, and why it is better:

- **A port-less `Host` means the port that was dialled**, as it does to every
  HTTP server. Dialling 8080 with a port-less `Host` works. Port 80 is not
  special on the captured path, and `port-required` does not exist there.
- **A `Host` port that disagrees with the dialled port is refused**, not
  obeyed. The first version let `Host` win, which meant a request could be
  answered by a port the client never connected to.
- **`:scheme` is irrelevant on the captured path**: nothing is defaulted.
- **Loopback is the one place a default is needed**, because nothing was
  dialled: port-less is port 80 when the Service declares it, else refused
  (decisions 5 and 9).

**Conformance.** The dialled port is the **Service** port. `http://echo/`
dials 80, lands in table 80, matches the virtual host of `echo`, and the
route's cluster comes from the Service port map (80 → `targetPort` 8080 →
`<fqdn>:8080`). Tables are keyed by Service port numbers, which for a
generated Service are the pod's.

**443.** The generated Service's `https` 443 port is a placeholder, like `http`
80 (`generator.go:157-176`, read), and goes in Phase 3. After that,
`https://<svc>/` reaches an application only if a pod declares 443 as raw TCP:
an ordinary TCP port carrying the application's own TLS inside the mesh's. A
TLS connection to a port declared HTTP is not `raw_buffer`, matches no HCM
chain and is passed through (measured). 443 is never a default for anything.

**UDP** is a different listener with no filter chains. None of this applies to
it (fact 3).

#### What sniffing is trusted for

Sniffing chooses between the HCM chain and the passthrough for a connection to
a port number that some service declares as HTTP, on an address that has no
raw-TCP chain for it. It is never the source of a port's class:

- A port declared raw TCP is `tcp_proxy` whatever the bytes are (measured:
  HTTP bytes to A:9000 reach A's TCP port).
- A port number that is HTTP for one service and raw TCP for another on the
  same listener is unambiguous: the address decides (measured, L2).
- h2c with prior knowledge, which is how cleartext gRPC arrives, is detected
  (measured). HTTP/1.1 is detected from its request line.
- A client that does TLS itself is not HTTP to the mesh: passthrough, or a
  `TLSRoute` chain on the service's address if one exists.
- Bytes that only resemble HTTP (a request line with no version) are not
  detected and are passed through (measured). Bytes that are a valid HTTP
  request line on a port declared HTTP are HTTP, by declaration.
- Non-HTTP bytes to a port declared HTTP are **passed through, not refused**.
  For a generated Service that ends at a Service port with no endpoints, which
  the cluster's routing rejects (inferred). For a user Service with a selector
  it reaches the pod without the mesh. That is true today and is not closed
  here: closing it needs an HCM chain per (service, port), which is the cost
  this layout avoids. Policy that must not be bypassed belongs on the
  destination's inbound side, which no client byte pattern can avoid.

#### The inspectors and server-first protocols (measured)

`http_inspector` and `tls_inspector` wait for client bytes before any chain is
chosen. A server-first protocol (MySQL, SMTP) sends none.

| Connection | Today's listener | Per-port layout | With the inspectors disabled for that destination port |
|---|---|---|---|
| server-first to A:3306, a declared raw-TCP port | first byte after **1.00 s** | 1.00 s | **1 ms** |
| server-first to C:5432, not in the mesh | 1.00 s | 1.00 s | 1.00 s (5432 was not disabled) |
| a 4-byte first write to A:9000 | 1.00 s | not re-measured | |
| a silent client on B:8080, an HTTP port | | 1.00 s, then the passthrough | |

The shipped listener disables nothing per port: it bounds the wait at 1 s and
continues (`proxy/capture.go:200-215`, read). So every server-first connection
through the mesh pays one second today, declared or not, and the per-port
layout neither adds to that nor removes it. A listener filter's
`filter_disabled` predicate does remove it, and it does see the recovered
destination port (measured, layout L3). The predicate matches a port, not an
address, so it can only be used for a port number that no service declares as
HTTP and no `TLSRoute` needs. This is an improvement that the per-port set
makes possible; it is listed in Phase 1 as optional and gated on its own leg.

#### Scale

`S` services with an address, `H` distinct HTTP port numbers in the mesh, `T`
(service, raw-TCP port) pairs, `N` pods on the node. Route tables are shared by
name across the node's pods (`cap_http` is one resource, `proxy/capture.go:29`,
read).

| | Chains per pod listener | HCM configs per pod | Route tables per node |
|---|---|---|---|
| Today | 1 + `T'` + default (`T'` counts three chains for a TCP-primary service) | 1 | 2 (`cap_http`, `out_http`) |
| One HCM chain (first version) | 1 + `T` + default | 1 | 2 |
| Per port (adopted) | `H` + 1 + `T` + default | `H` + 1 | `H` + 3 (`cap_http_<P>` × `H`, `cap_http_other`, `out_http`, and `cap_http_18081` until Phase 3) |

Worked: 200 services, 3 HTTP port numbers (80, 8080, 9090), 20 raw-TCP pairs,
50 pods on a node. Chains per pod: 22 today (if none of the 20 is a primary),
22 with one chain, **25** per port. Per node: 1,100 against **1,250** (+14%).
HCM configs per node: 50 against **200**. Route tables per node: 2 against
**6**, 5 after Phase 3. `H` grows with distinct port numbers, not with
services: 2,000 services on the same three numbers add nothing to the chain
count and lengthen the three tables.

What changes on RDS:

- **Fan-out is narrower.** A route change for one service rewrites the tables
  of that service's ports, not one table every HCM on the node holds.
- **Each table is a resource with a lifetime.** A chain that names a route
  table the snapshot does not carry leaves the listener warming until the
  initial-fetch timeout, and a table is torn down with the last chain that
  names it (#817, #844). A port number appearing in or leaving the mesh must
  add or remove its chain and its table in one snapshot. The ACK tracker
  follows `H` more resources.
- **The first-use path is not designed.** Today's catch-all turns `:authority`
  into a cluster name verbatim. With a port-less `Host` in table `P` the
  cluster is `<fqdn>:<P>`, which no header spells. The intended answer is an
  explicit virtual host per mesh Service in each of its ports' tables (the
  Service list is not demand-scoped), leaving the on-demand filter to fetch
  the named cluster; whether the pinned proxy's on-demand filter does that for
  a route that names its cluster directly was **not established**.
- GAMMA rules per port are what this proposal wants in any case (fact 1,
  row 10).

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

On the capture listener these are a dial to port 18081 (its own table,
`cap_http_18081`) and a dial to port 80 of a service that does not declare 80
(a legacy virtual host in table 80); on the loopback listener, a port-less
`Host` or one that names `:18081`.

- For a single-protocol service this is exactly today's behaviour.
- For a mixed service whose legacy target is HTTP it is today's behaviour too:
  legs M0 to M5 keep their meaning.
- For a mixed service whose legacy target is raw TCP (#1672), `:18082` starts
  working, and `:18081` and the bare name are **refused at once** with `421`
  and `x-aether-error: port-required`, the body listing the service's ports,
  where `main` answers 502. It is one of the refusals of the addressing rule,
  with the same body, so it does not describe a primary and does not have to
  be unlearned.
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
- The capture listener gets an HCM chain and a route table per HTTP port
  number (*The addressing rule*), each table holding a virtual host per
  service with HTTP on that port, for both name families, carrying the GAMMA
  rules. `out_http` gets a virtual host per HTTP port. The known-target safety
  net goes: its job is done by the per-port tables, which fixes #1682. The
  refusals for a named or dialled port (`port-mismatch`, `port-not-http`,
  `port-not-declared`). This closes the "real ports bypass the routes" gap
  and the passthrough of HTTP on a port number another service declares raw
  TCP.
- Optional, with its own leg (A13): `filter_disabled` on both inspectors for
  destination ports that no service declares as HTTP and no `TLSRoute` uses.
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
  backends in CI (today: `etcd` only; M6 and X6 read registry keys and run on
  `etcd` alone); MESH-HTTP 7/7 and GATEWAY-HTTP 46/46; the addressing legs
  below; the skew table re-measured on kind for both backends and both roll
  orders with zero cells worse than 2.6. M7 changes meaning: it asserts the
  real ports as well as the legacy four.

**Addressing legs (Phase 1).** In `e2e/multiprotocol.sh`, which already has
the client, the per-port echo and three of the five workloads. Each echo
answers with a string naming its own port, so "reached the wrong port" is a
failure and not a 200.

| Workload | Declares | In the suite today |
|---|---|---|
| `mp-httponly` | 8080 HTTP | yes |
| `mp-multi` | 8080, 9090 HTTP | new |
| `mp-eighty` | 80, 8080, 9090 HTTP | new (its container binds 80) |
| `mp-mixed` | 8080 HTTP, 9000 raw TCP | yes |
| `mp-tcpfirst` | 9000 raw TCP (declared port), 8080 HTTP | yes, ungated today |
| `mp-nine` | 9000 **HTTP** | new: the port number `mp-mixed` declares raw TCP |

Two clients: the existing captured one, and `mp-client-nocapture`, a managed
pod with `capture.aether.io/redirect-all: "false"`, which dials only
`127.0.0.1:18081`.

| Leg | Dial and `Host` | Path | Workloads | Must answer | On `main` |
|---|---|---|---|---|---|
| A1 | `<mesh name>:P`, `Host` the same, each HTTP port `P` | captured | all six | port `P`'s echo | passes, except `mp-nine` (A10) |
| A2 | `<svc>.<ns>.svc.cluster.local:P` and `<svc>.<ns>:P`, each HTTP port `P` | captured | `mp-multi`, `mp-eighty`, `mp-mixed` | port `P`'s echo | **fails**: the primary answers (#1682) |
| A3 | `127.0.0.1:18081`, `Host: <mesh name>:P`, each HTTP port `P` | loopback, capture off | all six | port `P`'s echo | expected to pass; guards decision 5 |
| A4 | an HTTPRoute with a port-less parentRef adds a response header; request `<mesh name>:9090` | captured and loopback | `mp-multi` | the header is present | **fails** (inferred): the rule is not on the port's virtual host |
| A5 | dial `<mesh name>:8080`, `Host: <mesh name>:9090` | captured | `mp-multi` | `421`, `x-aether-error: port-mismatch` | answers from 9090 (inferred) |
| A6 | `Host: <mesh name>:9000`, a raw-TCP port | loopback | `mp-mixed`, `mp-tcpfirst` | `421`, `x-aether-error: port-not-http`, under 1 s | not measured |
| A7 | dial and `Host` `<name>:7777`, both name families; and `Host: <mesh name>:7777` on loopback | both | `mp-multi` | `421`, `x-aether-error: port-not-declared`, under 1 s | **fails**: 503 after 2 s (mesh name), the primary's echo (Kubernetes name) |
| A8 | raw TCP to `<mesh name>:7777` | captured | `mp-mixed` | no application answers | passes; what the client sees is the cluster's (decision 10) and is not asserted |
| A9 | dial `<name>:P` with a **port-less** `Host`, each HTTP port `P` other than 80 | captured | `mp-multi`, `mp-eighty`, `mp-mixed` | port `P`'s echo | **fails** for every `P` but the primary: the primary answers |
| A10 | `<mesh name>:9000`, HTTP | captured | `mp-nine` | `mp-nine`'s echo | **fails** (measured in the rig): `mp-mixed`'s raw-TCP 9000 sends it to the passthrough |
| A11 | HTTP bytes to `mp-mixed:9000`; non-HTTP bytes to `mp-nine:9000` | captured | | the first reaches `mp-mixed`'s TCP echo; the second reaches no application | |
| A12 | dial port 80 or `:18081` with any `Host`; port-less `Host` on loopback | both | all six | **unchanged in this phase**: the legacy target, and for `mp-tcpfirst` `421`, `x-aether-error: port-required` | `mp-tcpfirst`: **fails** (502) |
| A13 (optional, with the inspector change) | a server-first exchange on a declared raw-TCP port whose number no service declares as HTTP | captured | a new server-first echo | first byte in under 200 ms | **fails**: 1 s |

A9 is a client-visible change in this phase: a port-less `Host` on a dialled
real port other than 80 reaches that port, where `main` sends it to the
primary. It is the same defect as #1682 seen through `Host`, and the answer on
`main` is the wrong application, so it is fixed and not deprecated.

A12 is where this phase deliberately does not yet apply the rule. The legacy
spellings are whole tables: `cap_http_18081`, and in table 80 a virtual host
per service that does not declare 80, both routing to the legacy target. For
`mp-eighty`, port 80 dialled is port 8080's echo now and port 80's later
(decision 11).

Runner time (estimate, not measured): the `multiprotocol` job took 2m24s with
both shapes in #1677, of which `verify` was 29 s for 14 legs. Three more
Deployments, one more client and about 60 more requests should add about a
minute to one backend. Running the suite on both backends doubles the job,
which is the larger cost: about 7 minutes of nightly runner time in place of
2.5.

- **Skew:** a 2.6 agent behind a 2.7 registrar sees the same annotation and
  extra Service ports it does not read; on `kubernetes` it also sees pods in
  more listings, which #1677 measured as "nothing gets worse" in that order. A
  2.7 agent behind a 2.6 registrar needs nothing from the Service but its
  address. Inferred from #1677's table for the parts that differ from it; the
  gate measures it.
- **Operator sees:** more ports on generated Services; `kubectl get svc` shows
  what a service serves; more filter chains and route tables per proxy
  (*Scale*). Two behaviours change for clients, both from a wrong answer to a
  right one or a refusal: a port-less `Host` on a dialled real port reaches
  that port (leg A9), and a `Host` port that contradicts the dialled port is
  refused (leg A5).
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
- **UDP (decision 1):** first a measurement on kind of a generated Service
  with a UDP port and registrar-written `EndpointSlice`s, on both backends
  (delivery, the reply's source, drain on a terminating pod). If it holds, the
  registrar writes the slices; `:18082/udp` keeps working and is counted. If
  it does not, 18082 stays for UDP and the finding comes back to the owner.
- **Gate (can fail):** every suite green on real ports only; the Phase 1
  addressing legs unchanged in their answers (nothing about the rule changes in
  this phase); in `multiprotocol`, leg D1: each legacy spelling dialled once
  (a dial to `:18081`, a dial to port 80, a port-less `Host` on loopback, `:18082`, an undeclared port of
  a pure-TCP service) and its counter asserted to have **moved** (a zero is
  evidence only if the counter can be seen to count); leg D2: a dial to port
  80 of `mp-eighty` moves the counter of decision 11 and not the plain
  one; in `l4-routes`, the UDP leg dials the real port on both backends;
  MESH-HTTP 7/7. D1 and D2 add a few seconds.
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

- The `cap_http_18081` chain and table, the legacy virtual hosts in table 80,
  the `:18082` chains and the alias clusters; the 421 body's mesh-port
  spellings; `ProxyL4OutboundPort`; the scoped mark rules for both.
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
- UDP (decision 1), if the Phase 2 measurement held: the per-pod UDP listener, the `udp:` clusters,
  `UDPStatKey`, the UDP mark rule.
- Sources switch to SNI = port for every TCP port; destinations have had the
  explicit chains since 2.7.

- **Gate (can fail):** conformance unchanged; an 8-hour soak on real ports;
  and the addressing legs below, where the answer changes in this phase.
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

**Addressing legs (Phase 3).** In `e2e/multiprotocol.sh`.

| Leg | Dial and `Host` | Path | Workloads | Must answer |
|---|---|---|---|---|
| R1 | dial port 80 (`http://<name>/`), both name families; port-less `Host` on loopback | captured and loopback | `mp-eighty` | port **80**'s echo (it was 8080's through 2.8) |
| R2 | dial port 80, both name families | captured | the other five | `421`, `x-aether-error: port-not-declared`, under 1 s, the body listing the ports |
| R3 | port-less `Host` | loopback | the other five | `421`, `x-aether-error: port-required`, under 1 s |
| R4 | dial `<name>:18081`; `Host: <mesh name>:18081` on loopback | captured and loopback | `mp-httponly` | `421`, `x-aether-error: port-not-declared` |
| R5 | raw TCP to `<mesh name>:18082` and to an undeclared port | captured | `mp-tcpfirst` | no application answers (A8's assertion; decision 10 says what else may be asserted) |
| R6 | A1 to A11 | | | unchanged |

A12 is retired by R1 to R4. No new workload; a few seconds of runner time.

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
3. **UDP.** The mesh stops seeing application UDP: no per-service UDP stats,
   no L4 log line, and `UDPRoute` is served by registrar-written slices for one
   backend only. The option has not run in any cluster. Until it has, port
   18082 stays for UDP, which is a standing exception to the decision.
4. **Multi-cluster peers.** A peer more than one chart minor behind exports
   routes with no port; at Phase 3 those backends cannot be resolved and are
   dropped with a counted WARN. Clusters must cross 2.8 together or within the
   window.
5. **Measured only in a rig, or only on kind.** The UDP table is one network
   namespace on one kernel with one user ID: no CNI, no cluster service
   routing, no second node. #1677's tables are single-node kind; its
   agent-first order was not run on the kubernetes backend. The reference
   cluster has run none of this.
6. **The addressing rule** (*Design*, *The addressing rule*). The dialled
   port decides and `Host` names the service. A client that sends a `Host`
   port different from the port it dialled is refused (`port-mismatch`), where
   today it is routed by `Host`; a proxy inside the pod that forwards requests
   with their original `Host` to a different port is the case to watch for.
   On the loopback listener a port-less `Host` is port 80 or refused. The cost
   is `H` + 1 HCM chains per pod and `H` + 1 capture route tables per node in
   place of one each, each table a resource whose chain and table must appear
   and disappear together (the #817 class), and a first-use path that is not
   designed yet. Legs A5, A9, A10, R1 to R3 hold the rule.
7. **The Service port map is a new reader.** A user Service with a named
   `targetPort`, or with a selector that is not the ServiceAccount's pods,
   resolves to nothing or to the wrong set. It is reported, not guessed.
8. **Capture listener churn.** More per-port chains mean more reasons to
   update a per-pod listener, which 037 (Risk 4) says drains it. The HTTP
   chains change only when a port **number** enters or leaves the mesh, which
   is rarer than a service changing; the trigger must stay edge-triggered on
   the derived sets. Whether the pinned proxy applies a change of filter
   chains alone without draining the unchanged ones was not measured.
9. **Two transports on one port number** cannot be declared (fact 4). DNS is
   the obvious case, and under (d) it is the first thing a user will try.

## Decisions

Decided by the owner on 2026-10-11, each as recommended. The alternative that
was not chosen is kept in one clause.

1. **Application UDP is no longer proxied.** The generated Service's UDP ports
   get `EndpointSlice`s written by the registrar, and the cluster's own service
   routing carries them. Port 18082 stays for UDP until that ships. `UDPRoute`
   is kept for the single-backend shape it supports today and withdrawn
   otherwise. Not chosen: keeping 18082 for UDP for good, and conntrack port
   translation to reserved listener ports. **This option is not measured**, so
   its phase starts with a measurement (Phase 2).
2. **The port 80 break is accepted.** `http://<svc>/` works only for a service
   with a port 80; a user who wants 80 listens on it or writes a Service that
   maps it. Not chosen: a way for the generated Service to map 80 to a
   declared port.
3. **`capture.aether.io/redirect-all: "false"` is kept and means "outbound not
   captured"** after Phase 3. Not chosen: removing the annotation and rejecting
   the value.
4. **When a service's pods disagree about the legacy target** during Phases 1
   and 2, the order-independent vote from #1677 decides, private to the legacy
   path and deleted in Phase 3. Not chosen: today's `endpoints[0]`.
5. **The loopback listener `127.0.0.1:18081` is kept.** It addresses the
   proxy, not a service; the CNI readiness probe and the prober's liveness
   tier use it. Nothing is dialled there, so the port in `Host` is the port:
   `Host: <mesh name>:<port>` with a port the Service declares as HTTP is
   matched by that port's virtual host; a port-less `Host` means port 80 and
   is served only when the Service declares 80, else `421 port-required`. It
   works the same for a pod with `capture.aether.io/redirect-all: "false"`
   (decision 3), which is that pod's only way into the mesh (legs A3, A6, R1,
   R3). Not chosen: removing the listener, which would be a separate proposal.
6. **`endpoint.aether.io/port` keeps its name.** With no primary it is one more
   declared port and the liveness probe port, and the documentation says so.
   Not chosen: a new `health-port` annotation and a contract bump.
7. **TCP and UDP on one port number is out of scope here.** It needs an
   additive registry field and is its own change, due before Phase 3 because
   of decision 1. Not chosen: solving it inside this proposal.
8. **Three chart minors:** Phase 1 in 2.7.0, Phase 2 in 2.8.0, Phase 3 in
   2.9.0, and the receiver-side tolerances in 2.10.0. Not chosen: Phases 1 and
   2 sharing 2.7.x with removal in 2.8.0; Phase 1 fixes a bug and does not
   wait for the prober and the harness to move.
9. **A port-less `Host` on the loopback listener is port 80 whatever `:scheme`
   says;** `:scheme` is never consulted. Not chosen: 443 for `:scheme: https`,
   which would route on a header the client controls. The question does not
   exist on the captured path, where nothing is defaulted.
10. **Raw TCP to a port the Service does not declare gets the cluster's
    answer** once the placeholder ports are gone: it reaches the passthrough
    and then a ClusterIP port that does not exist, which 037 describes as
    CNI-dependent and typically a hang. Phase 3 asserts only that no
    application answers. Not chosen: a per-service refusal chain, which under
    the per-port layout would catch only port numbers no chain names and so be
    partial without an HCM chain per (service, port); it is built if a hang is
    reported. HTTP, the common case, is refused by name either way.
11. **A service that declares port 80 while its legacy target is another
    port** (`mp-eighty`) keeps the legacy meaning of a dial to port 80 through
    2.8. Those requests are counted under their own name
    (`legacy_portless_shadows_80`), logged once per source, and the service is
    named in the 2.9 runbook section, because the change at 2.9 is from one
    working answer to another and would otherwise be silent. Not chosen: port
    80 from 2.7 when declared, which changes behaviour with no deprecation
    window.
12. **The capture listener has an HCM chain and a route table per HTTP port
    number; the dialled port decides.** It is the only layout in which a port
    number one service declares as raw TCP still carries HTTP for another
    (measured), and it makes a port-less `Host` mean what it means to any HTTP
    server. It costs `H` + 1 HCM chains per pod and as many route tables per
    node, with the resource-lifetime and first-use questions under *Scale*.
    Not chosen: one chain routed on `Host`, which would also have needed a
    per-port chain for every port number any service declares as raw TCP.

## Phase 1 as built

Added with the Phase 1 pull request (chart 2.7.0). What was built is the phase
above. This section records where a measurement or the code contradicted this
document, what was decided in each case and by whom it still has to be
confirmed, and what the phase did not do. The text above is otherwise as
accepted.

### Measured

- **The legs.** `e2e/multiprotocol.sh` on kind, both registry backends, the new
  suite against images built from `main` at `7a9cca0a` and then against the
  branch. On `main`: A2, A4, A5, A6, A7, A9, A10, A12 (through X4), X4, X5 and
  M7 fail on both backends; on `etcd` also X1 and X2 (#1672); on `kubernetes`
  also M4, X3, A1, A3 and A11 (the mixed shapes have no cluster for their
  second class). A1 on `etcd` fails for `mp-nine` alone, which is A10. On the
  branch every leg passes on both backends (M6 and X6 read registry keys and
  are skipped on `kubernetes`).
- **HTTP on a port number another service declares raw TCP leaves the mesh.**
  Listed under *Not established* as measured in the rig only. Leg A10 measured
  it on kind on `main`: `http://mp-nine...:9000/` gets no reply while
  `mp-mixed` declares `9000=tcp`.
- **The first-use path** (*Scale*, "not designed"; *Not established*, second
  bullet). It needs no new mechanism. Every mesh Service with an address is in
  the node's dependency set since 037, so its clusters are in the snapshot and
  each table names them directly; the on-demand route of a table is kept for a
  mesh authority this node holds no endpoint of yet, and it accepts only an
  authority that spells the table's own port (the two legacy tables also the
  port-less name). The pinned proxy's on-demand filter does fetch a cluster a
  route names directly: the QUIC twins have relied on that since #1020.
- **Conformance** on the branch: MESH-HTTP 7 passed, 0 failed; GATEWAY-HTTP
  36 + 10 passed, 0 failed (Gateway API v1.6.3, the nightly workflow dispatched
  on the branch).

### Where this document was wrong, or silent

1. **Scale: the chain count is one higher, and a raw-TCP-only service costs
   more.** The table's `H` + 1 + `T` + default leaves out the chain of the
   legacy table `cap_http_18081`, which the same row counts as a route table.
   Measured for the worked example (200 services, HTTP on 80, 8080 and 9090,
   20 raw-TCP ports beside an HTTP declared port, 50 pods): **26** chains per
   pod listener with the default (25 stated), 1,300 per node (1,250), **5** HTTP
   connection managers per pod and 250 per node (4 and 200), 6 route tables
   (6). The capture listener is 24 KiB per pod and the five capture tables
   1.3 MiB per node. If the 20 raw-TCP ports are instead the declared port of
   20 services with no HTTP port, a listener has **146** chains: each such
   service has its own port, `:18082`, the any-port chain and one copy of the
   any-port chain per HTTP port number (item 2). By this document's own count that
   shape is 62 on `main` (not measured there).
2. **The any-port chain needs a copy on every HTTP port number.** The match
   does not backtrack, so a connection to a port number that any chain names is
   never offered to a port-less chain. With an HTTP chain per port number, a
   pure-TCP service dialled on, say, 8080 would have stopped reaching its
   declared port the day another service declared 8080 as HTTP. The copies
   keep the legacy spelling working as it does on `main`, share the any-port
   stat prefix so the counter that gates its removal sees every port, and go
   with it in Phase 3. **Owner's call:** dropping the copies instead would save
   `H` + 1 chains per raw-TCP-only service per pod and turn those connections
   into a passthrough (raw bytes) or `421 no-http-port` (HTTP).
3. **`TLSRoute` chains get the same treatment**, on every named port except
   the ports the same service declares as HTTP (a per-address copy there would
   take the service's own cleartext HTTP away from the HTTP chain).
4. **A1 against A12 and decision 11, for a service that declares port 80
   beside another declared port (`mp-eighty`).** A12 says a dial to port 80
   "with any `Host`" reaches the legacy target; A1 says `<name>:80` with the
   same `Host` reaches port 80's echo, and passes on `main`. Built: a
   **port-less** `Host` on a dial to port 80 is the legacy target (decision
   11), and `Host: <name>:80` spelled out is port 80, as on `main`.
5. **A dial to `:18081` with a `Host` that names another port is refused
   (`port-mismatch`).** A12's "any `Host`" would route it to the legacy
   target; on `main` it was routed by `Host`. No documented spelling does this.
6. **A bare name that two namespaces share is no longer answered.** The
   known-target route matched a bare name for every namespace that had it and
   took the first, so half the callers got another namespace's service. A
   virtual-host domain cannot be claimed twice, and a table is shared by the
   node's pods, so the bare name is a name only when one namespace owns it
   (the rule *Names* states); otherwise the request is passed through and the
   cluster refuses it. This is a client-visible change the phase's summary did
   not list.
7. **A `backendRef` port that is not an HTTP port of the backend routes to
   nothing**, where it reached the backend's declared port. 18081, and 80 for a
   generated Service that does not declare it, keep the legacy meaning. The
   port is carried from the route to the agent beside the shared projection,
   not in it: the proto field is Phase 2's.
8. **The frozen annotation on `kubernetes`.** "The class of the declared port"
   is not what a 2.6 registrar stamped for a pod whose pod-level protocol is
   contradicted by a suffix on the declared port itself (`protocol: http`,
   `port: 8080`, `ports: "8080=tcp"`): 2.6 listed by the pod-level value. The
   row does not carry the pod-level annotation, so the registrar cannot
   reproduce it; it stamps the class of the declared port, as written here.
9. **Pods of one service that disagree** are decided once for the whole
   service, over every listing, and every builder reads that answer (the
   legacy target; the class of a port for the chains and the tables). Both
   endpoint sets of a port still exist, as fact 4 says.

### Not done in Phase 1

- **A13** and the `filter_disabled` inspector change (optional).
- **A named `targetPort` is reported as a WARN only**, not on the route's
  status.
- **The Service port map is read for the Services a route names** (as its
  parent or as a backend). A user Service that owns a mesh service's name and
  has no route is passed through, as on `main`.
- **Edge:** the per-port raw-TCP clusters are published; an edge `TCPRoute`
  backend still resolves to the floor cluster (row 19's "the port's cluster,
  always" is not built).
- **The skew table** was not re-measured on kind. What is stated for a roll is
  read from the code and from #1677's table.

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
- Whether the pinned proxy's on-demand filter fetches a cluster that a route
  names directly (the per-port first-use path depends on it).
- The filter-chain measurements on a cluster, with a real CNI, more than one
  worker, or the unified `filter_chain_matcher`. The passthrough of HTTP on a
  port number another service declares raw TCP was measured in the rig with
  the shipped chain shapes, not on `main` in a cluster.
- What a client sees for non-HTTP bytes to a declared HTTP port of a generated
  Service (inferred: the cluster rejects a Service port with no endpoints).
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
route-table check in *The addressing rule* is two listeners sharing one inline route table
whose virtual host carries a `matcher` on `DestinationPortInput`; the proxy
refuses the configuration at load.

## Appendix: the filter-chain measurement

The same network namespace, with TCP to `10.96.0.0/16` marked on output and
`iif lo meta mark 1 meta l4proto tcp tproxy to :18001 accept` on prerouting. One
static listener on `0.0.0.0:18001`, `transparent: true`, `use_original_dst:
false`, listener filters `original_dst`, `http_inspector`, `tls_inspector`,
`listener_filters_timeout: 1s`, `continue_on_listener_filters_timeout: true`,
and a `default_filter_chain`. HCM chains answer with a `direct_response` that
names their route table and virtual host; `tcp_proxy` chains and the default
chain lead to three local servers that tag what they receive, two of which
send a greeting on accept (the server-first application and the stand-in for
the original destination). Each case records which server accepted, the HTTP
status and body if any, and the time from `connect` to the first byte.

- L1: `{raw_buffer, HTTP}` → HCM; `{9000, A/32}` and `{3306, A/32}` → `tcp_proxy`.
- L2: L1 plus `{8080, raw_buffer, HTTP}` and `{9000, raw_buffer, HTTP}` → an HCM
  each, with a virtual host for `b`, `b:<port>` and a `*` virtual host that
  answers `421`.
- L3: L2 with `filter_disabled: {destination_port_range: [3306, 3307)}` on
  `http_inspector` and `tls_inspector`.

The TLS case is a real ClientHello from a TLS client; the h2c case is the
client preface and an empty SETTINGS frame.
