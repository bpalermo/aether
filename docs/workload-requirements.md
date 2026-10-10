# Mesh Workload Requirements

What a Kubernetes workload needs to participate in the Aether mesh, and what it
needs to be rolled with **zero dropped requests**. Validated end-to-end on
talos-main (2026-06-10): three consecutive rolling restarts of three services
under ~250 rps with 0 failed requests across every stream.

## Joining the mesh

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-svc
spec:
  replicas: 4
  minReadySeconds: 10                  # see "Hitless rolling restarts"
  strategy:
    rollingUpdate: { maxSurge: 1, maxUnavailable: 0 }
  template:
    metadata:
      labels:
        app: my-svc
        aether.io/managed: "true"      # CNI manages this pod
    spec:
      serviceAccountName: my-svc       # SERVICE NAME = service account name
      containers:
        - name: app
          readinessProbe: { httpGet: { path: /healthz, port: 8080 } }
          lifecycle:
            preStop: { sleep: { seconds: 10 } }  # see "Hitless rolling restarts"
```

- **Service identity**: the registry service name is the pod's
  **ServiceAccount name**. Pods sharing a ServiceAccount are endpoints of one
  service. The SPIFFE ID is `spiffe://<trust-domain>/ns/<ns>/sa/<sa>`.
- **`aether.io/managed: "true"`** label opts the pod into mesh management.
- Pods in control-plane/mesh-internal namespaces are always ignored.

### Annotations (optional)

| Annotation | Default | Meaning |
|---|---|---|
| `endpoint.aether.io/port` | `8080` | Primary application port — what a portless authority resolves to |
| `endpoint.aether.io/ports` | `<port>` | Every port the app serves, comma-separated, each optionally suffixed `=h1`, `=h2` or `=tcp` (e.g. `8080,9090=h2,9000=tcp`). A port with no suffix takes `endpoint.aether.io/protocol` |
| `endpoint.aether.io/protocol` | `http` | The **default** L4 class for ports with no suffix. `http` or `tcp`; a raw-TCP service rides the transparent-capture TCP floor as an mTLS passthrough |
| `endpoint.aether.io/weight` | `1024` | Load-balancing weight |
| `endpoint.aether.io/health-path` | `/` | Path the node-local agent health-checks (delegated liveness) |
| `endpoint.aether.io/health-check-mode` | `eds` | `eds`: node-local agent vets the endpoint once and publishes health over EDS (endpoints enter clients pre-warmed). `active`: every client proxy probes the endpoint itself |
| `metadata.endpoint.aether.io/<key>` | — | Free-form endpoint metadata (subset keys) |
| `endpoint.aether.io/uds-socket` | — | Deliver to a Unix socket instead of a TCP port (see "Serving on a Unix domain socket"); overrides an `EndpointPolicy` on the service |
| `config.aether.io/upstreams` | — | Comma-separated services this pod **calls** (see "Declaring upstreams") |
| `aether.io/identity-gate` | — | `"false"` opts this pod out of the egress identity gate (see "Identity before the first request") |

### Identity before the first request

Every connection a mesh pod makes is mTLS with **the pod's own SVID** as the
client certificate — and SPIRE issues that SVID a few seconds after the pod is
created (its registration entry has to be created and synced to the node's
SPIRE agent: ~7.5 s measured on talos-main). An app that sends in that window
used to get `503 UF` (`connection_timeout`): the source proxy had no certificate
to present yet (#1053).

So the controller's pod-mutating webhook injects an init container,
**`aether-identity-ready`**, first in line in every mesh pod it admits. It asks
the SPIRE Workload API (a `csi.spiffe.io` volume mounted into that init
container only — your app containers are not given the socket) for the pod's
X.509 SVID and exits 0 the moment SPIRE has issued it. Your containers start
after that, so the app's first request already has an identity. Cost: pod
start waits for the SVID (what the first request would have waited for anyway,
or failed on); nothing afterwards.

- **Default ON**, chart-wide knob `controller.webhook.identityGate.enabled`.
  Never injected when `spire.enabled=false`.
- **Opt one pod out** with the annotation `aether.io/identity-gate: "false"`
  (its first requests may then fail until the SVID lands).
- Not injected into `hostNetwork` pods or pods in mesh-ignored namespaces.
- **A pod stuck in `Init:0/N`** with `aether-identity-ready` running means SPIRE
  has not issued the pod's SVID: its log says so every 10 s and names the
  socket and the last error. Usual causes: no registration entry matches the
  pod (e.g. a `ClusterSPIFFEID` whose `podSelector` misses it, or an entry keyed
  on a `k8s:container-name` selector — the init container has its own name),
  spire-agent down on the node, or the SPIFFE CSI driver missing. It fails
  closed on purpose: a pod without an identity cannot talk to the mesh anyway.
  See docs/runbook.md, "Pod held in Init by aether-identity-ready".

## Serving on a Unix domain socket

An app that serves on a Unix socket instead of a TCP port joins the mesh with
`endpoint.aether.io/uds-socket: <volume>/<socket-file>`. The node proxy then
delivers inbound requests to that socket. Nothing changes for callers: the pod
is still reached at its pod IP over mTLS and is indistinguishable from a
TCP-serving pod.

The socket lives in an inline **`csi.aether.io`** volume: a small per-pod
tmpfs the mesh's own CSI node plugin mounts for the pod (proposal 039). It is
the only supported carrier. Since chart `2.0.0` an `emptyDir` is **not** (see
"Upgrading from an emptyDir socket" below).

```yaml
    metadata:
      labels:
        aether.io/managed: "true"
      annotations:
        endpoint.aether.io/port: "8080"
        endpoint.aether.io/uds-socket: "uds/app.sock"
    spec:
      securityContext:
        fsGroup: 65532                 # REQUIRED: owns the socket directory
      containers:
        - name: app
          volumeMounts:
            - name: uds
              mountPath: /run/app      # the app binds /run/app/app.sock
      volumes:
        - name: uds
          csi:
            driver: csi.aether.io      # the ONLY socket carrier
```

Requirements:

- **An inline `csi: {driver: csi.aether.io}` volume, exactly one per pod.** The
  plugin mounts a tmpfs at `/run/aether/uds/<pod-uid>` on the node
  (`nosuid,nodev,noexec,nosymfollow`, 1 MiB by default) and binds it onto the
  volume's mount path. The proxy dials the socket there, and sees nothing else
  of the pod. Two such volumes, or an annotation naming any other volume, are
  refused.
- **`securityContext.fsGroup` is required.** The tmpfs is
  `root:<fsGroup>`, mode `2770`: the kubelet adds the fsGroup to every
  container of the pod, which is how a nonroot app creates its socket. A pod
  without one is denied at admission (and the plugin would refuse it with a
  `FailedMount` event anyway). There is no world-writable fallback.
- **The annotation is `<volume>/<socket-file>`** — the volume *name* from the
  pod spec (not its mount path) and a file directly inside it. Anything else
  (extra path segments, `..`, absolute paths) is rejected.
- **The socket file name has a 54-byte budget.** The host path
  `/run/aether/uds/<36-byte pod UID>/<file>` must fit an `AF_UNIX` address (107
  bytes). The volume name is not part of the path and costs nothing. A value
  over budget is denied at admission; one that slips past is refused by the
  agent (not sent to Envoy, which would reject the cluster).
- **Ports are still required and still meaningful.** `endpoint.aether.io/port`
  (and `endpoint.aether.io/ports` for multi-port) name the *service* ports
  clients dial and drive inbound demux and endpoint registration; the
  annotation only changes what the proxy dials on delivery. A pod declares one
  socket and every declared port is delivered to it — multiplexing protocols on
  the one socket is the app's affair (normal for gRPC).
- **The app creates the socket**, ideally `0600`–`0660`. The proxy runs as root
  and is never blocked by the mode; the directory's setgid bit makes the socket
  group `<fsGroup>`, so `0660` lets the pod's other containers in and keeps
  everyone else out.
- **The node needs the plugin.** It ships with the chart and is on by default
  (`udsCsi.enabled`). A pod scheduled onto a node where it is not running waits
  in `ContainerCreating` with `FailedMount: driver name csi.aether.io not found`
  until it is. Talos needs nothing extra.

### Declaring it once per service instead (EndpointPolicy)

The same delivery can be declared for a whole service with an `EndpointPolicy`
CR, so a platform owner can own it separately from the Deployment:

```yaml
apiVersion: config.aether.io/v1
kind: EndpointPolicy
metadata:
  name: echo-uds
  namespace: team-a
spec:
  targetRef:
    kind: Service          # core group, same namespace, Service only
    name: echo
  udsSocket: s/app.sock    # same <volume>/<file> value as the annotation
```

The admission webhook rejects a malformed value — including a socket file over
the 54-byte budget — at `kubectl apply`, instead of leaving the agent to count
a resolve failure and fall back.

- **The pod annotation wins.** A pod carrying
  `endpoint.aether.io/uds-socket` uses its own value; the policy is the
  service-level default for the pods that do not.
- **The pod spec still has to match.** The CR cannot see the workload, so a
  policy naming a volume the pods do not mount as `csi.aether.io` is not
  rejected at apply time. The agent refuses it per pod, counted with a reason
  (below).
- **One policy per service.** A second policy targeting the same Service is
  ignored (the lexicographically smallest policy name wins) and the conflict is
  logged by the agent.
- The CRD ships in the `crds` chart, and the agent only reads it when
  `udsCsi.enabled` is true.

### When delivery cannot happen

Admission catches what it can see on the pod: the controller's pod webhook
**denies** a pod whose `endpoint.aether.io/uds-socket` names an `emptyDir` (or
any volume that is not its `csi.aether.io` volume), a socket file over budget,
a `csi.aether.io` volume without `fsGroup`, or two of them. The denial names
the fix.

Everything else — a policy-declared socket, a pod admitted while the webhook
was down — the agent refuses at resolution and **counts**:
`aether.agent.uds.resolve_failures{reason}` (Prometheus
`aether_agent_uds_resolve_failures_total`, every reason exported at zero), plus
one log line per pod per reason naming the pod. Reasons: `not_csi` (the named
volume is declared, but not as `csi.aether.io` — an old `emptyDir`),
`volume_not_declared`, `bad_file`, `path_too_long`, `multiple_csi_volumes`,
`no_uid`, `bad_request`, and `disabled` (`udsCsi.enabled: false`).

A refused pod keeps TCP delivery. Failure semantics then match an app that has
not bound its port: a UDS-only app has nothing listening on TCP, so the
delegated-liveness probe fails and the endpoint stays **unpromoted**; no
traffic is sent to it. The same holds until the socket file exists and
accepts.

### Upgrading from an emptyDir socket

Chart `2.0.0` removed the `emptyDir` carrier with no compatibility window. Every
UDS workload must switch its socket volume **before** (or in the same rollout
as) the chart upgrade:

1. `emptyDir: {}` → `csi: {driver: csi.aether.io}` on the volume the annotation
   (or policy) names; drop any `subPath` on its mount.
2. Add `securityContext.fsGroup` (any gid your app runs with; distroless
   nonroot is `65532`).
3. Check the socket file name fits 54 bytes (it almost certainly does; the old
   budget was ~16 for volume and file together).

A workload left on an `emptyDir` is not deleted: new pods are denied at
admission, and running pods fall back to TCP and stay unpromoted, counted as
`resolve_failures{reason="not_csi"}`.

## Calling other services

With transparent capture + mesh DNS (both on by default), apps dial the
destination by name — `http://<service>.<namespace>.<meshDomain>:18081` (mesh
DNS) or the generated Kubernetes Service
`<service>.<namespace>.svc.cluster.local:18081` — and the CNI-programmed
capture listener routes it. Apps that prefer zero interception assumptions
can instead address the outbound listener explicitly: `http://127.0.0.1:18081`
with the mesh FQDN in the `Host` header. Either way every hop is mTLS between
workload identities; the callee sees the caller's SPIFFE ID in
`x-forwarded-client-cert`. Between proxies an HTTP request rides HTTP/3 over
QUIC (HTTP/2 for a weighted GAMMA split or a destination behind the east/west
waypoint); your application speaks what it always did, to its own proxy. That
needs two DNS SANs on every workload SVID and UDP `18008` open beside TCP
`18008`: see the getting-started guide, "HTTP/3 between proxies".

**Authorities are FQDN-only, namespace-qualified, and deterministic.**
`<service>.<namespace>.<mesh-domain>` (default domain `aether.internal`,
agent `--mesh-domain` / chart `meshDomain`; proposal 020) is the accepted
mesh form — it is simultaneously the vhost domain, the data-plane cluster
name, and the on-demand (ODCDS) lookup key, declared or not. The capture path
also honors the standard `<service>.<namespace>.svc.cluster.local` name.
Anything else — bare names (`Host: my-svc`), foreign domains, nested labels —
matches no route and 404s immediately; only authorities under the mesh domain
can reach the cold path.

**The authority's port is NOT stripped — it is part of the match.** This
document previously said the opposite (#883). `strip_any_host_port` is off, and
deliberately so: the portless FQDN and `<fqdn>:<primary port>` both reach the
service's primary cluster, while `<fqdn>:<port>` reaches that port's own
cluster. If the port were stripped, per-port routing (proposal 005) could not
work at all. See `BuildOutboundClusterVirtualHost` in
`agent/internal/xds/proxy/route.go`. The SPIFFE trust domain is resolved from each component's own SVID and
matches the mesh domain by design, so addressing and identity share one
domain.

### How a spelling resolves (proposal 037)

Every way of addressing a mesh service resolves to a `(port, protocol)` pair.
The **URL scheme types the address**, which is why a portless HTTP URL is not
ambiguous and raw TCP needs a port:

| Client dials | Reaches |
|---|---|
| `http://<svc>.<ns>.<domain>/` | the primary port, over HTTP |
| `https://<svc>.<ns>.<domain>/` | the primary port, app-terminated TLS |
| `<svc>.<ns>.<domain>:18081` | the primary port, over HTTP (the explicit HTTP spelling) |
| `<svc>.<ns>.<domain>:18082` | the primary port, as raw TCP (the explicit L4 spelling) |
| `<svc>.<ns>.<domain>:18082/udp` | the primary port, as plaintext UDP (the same L4 spelling; needs a `UDPRoute`) |
| `<svc>.<ns>.<domain>:<p>` | port `p`, in whatever class `p` declares |

HTTP demuxes on the **authority header**, which carries its own port as a
string — so `http://<svc>/` is unambiguous without one. Raw TCP has no
authority; its only demux key is the 5-tuple. That asymmetry is intrinsic to
the protocols, not to this mesh, and it is why TCP gets a well-known port of
its own (`18082`) rather than the bare name changing meaning depending on
which protocol a service's primary port happens to be.

**`:18082` does not require redirect-all.** The scoped capture rule diverts
it alongside `:18081` — on both transports — and the generated mesh Service
exposes it. A dial to a service's **own** application port (`<svc>:9000`) is
captured only under redirect-all, which is the managed-pod default and TCP-only
— the same property per-port HTTP already has. UDP is captured on `:18082`
only; there is no any-port UDP capture.

**A port nobody registered** is refused rather than silently forwarded: the
generated mesh Service exposes only its known ports, and kube-proxy REJECTs the
rest, so the caller gets `ECONNREFUSED` instead of a hang.

**Traffic shaping** (canary weights, header routing, timeouts, gRPC method
routing, L4 splits/SNI) is standard Gateway API routes parented to the
*Service* (GAMMA) — see the getting-started guide §10.

### Declaring upstreams

The mesh distributes a service's clusters/endpoints/routes only to nodes that
need them (demand-scoped distribution, proposal 004). Declare what a pod
calls:

```yaml
metadata:
  annotations:
    config.aether.io/upstreams: "svc-payments,svc-ledger,svc-audit"
```

- **Declared upstreams are warm before first use** — the node's proxy carries
  them the moment the pod lands. Declare everything latency- or
  correctness-critical. The list is also reviewable architecture
  documentation, exactly like `minReadySeconds`/`preStop` above.
- **Undeclared upstreams still work** (cold path): the first request pauses
  ~one node-local xDS round-trip while the cluster is fetched on demand
  (ODCDS), then stays warm while used (1h idle TTL). Cold-path calls use the
  same FQDN authority as everything else. Requests to nonexistent services
  *under the mesh domain* fail after the on-demand timeout (`onDemandClusterTimeout`, 2s — `agent/internal/xds/proxy/httpfilter.go`); anything
  outside the domain 404s immediately at the route table.
- Every miss increments `aether.agent.upstreams.miss` (and is logged with the
  service name) — the signal to promote an undeclared dependency to the
  annotation.
- A pod's **own** service is always in scope; it never needs declaring.

**Use keepalive (or HTTP/2) connections to the outbound listener.** The mesh
pools upstream mTLS connections *per downstream connection* (this is what
keeps one pod's certificate from ever being reused for another pod's
traffic). A long-lived client connection — an HTTP/1.1 keepalive connection
or an HTTP/2/gRPC channel, whose multiplexed streams all share one upstream —
reuses its mTLS connection across requests. Connection-per-request clients
pay a fresh mTLS handshake per request and each abandoned upstream lingers
until the 30s idle timeout reclaims it: it works, but it is the expensive
traffic shape.

## Subset routing and locality

Requests choose *which endpoints* of a service they may land on via headers;
the mesh prefers *closer* endpoints automatically.

### Pinning (always available)

| Header | Meaning |
|---|---|
| `x-aether-ip: 10.42.1.11` | route to exactly that endpoint |
| `x-aether-pod: my-svc-7f9c4-xv2qp` | route to exactly that pod |

Pin-or-fail: if the target is gone (drained, ejected, never existed) the
request gets a 503 — it never silently lands on a different pod.

### Provider-defined subsets

Endpoints publish routing dimensions via metadata annotations:

```yaml
metadata:
  annotations:
    metadata.endpoint.aether.io/version: "v2"
```

Consumers select with `x-aether-subset-<key>` (here
`x-aether-subset-version: v2`). The vocabulary travels via the control
plane — consumers declare nothing; any key published by an in-scope service
is routable from every pod on the node. Selection is strict (NO_FALLBACK):
asking for a subset that has no endpoints fails rather than spilling onto
the rest of the service. Keys must be lowercase DNS-label shaped
(`[a-z0-9-]`); `ip`, `pod`, `cluster`, `namespace` are reserved.

**Multiple subset headers intersect**: a request carrying
`x-aether-subset-version: v2` and `x-aether-subset-shard: s1` routes only to
endpoints matching both, or fails. Up to 4 keys per service combine; beyond
that, extra keys select individually only. **Pin headers are exclusive**:
`x-aether-ip`/`x-aether-pod` identify a single endpoint by design and never
combine — mixing a pin with subset headers matches no selector and falls
back to normal balancing.

Requests without subset headers are balanced across all healthy endpoints,
unchanged. Note: a *cold* (ODCDS) first request to an undeclared upstream
routes before that service's vocabulary lands (~ms); declare upstreams whose
subset routing is correctness-critical.

### Locality-aware failover

Endpoints carry their node's `topology.kubernetes.io/region`/`zone`. Each
node's proxy routes to same-zone endpoints first (EDS priority 0), spilling
to same-region (1) and then anywhere (2) only as closer endpoints become
unhealthy or drain — a zonal roll automatically shifts traffic to the
region and back. Nodes without topology labels express no preference.

## Hitless rolling restarts

The mesh handles most of the work automatically — endpoints are marked
draining the instant pod deletion is *requested* (before SIGTERM), new
endpoints enter clients pre-vetted, and client routes retry connection-level
failures on another endpoint. Two workload-side settings close the remaining
windows; **without them rolls outrun the mesh and drop requests**:

1. **`minReadySeconds: 10`** — Kubernetes considers a new pod Ready seconds
   before the mesh has vetted and propagated its endpoint (~5–10s: local
   health-check pass → liveness promotion → registrar → every client's EDS).
   `minReadySeconds` paces the roll so the previous endpoint is only retired
   after the replacement is mesh-routable.
2. **`preStop: { sleep: { seconds: 10 } }`** (native sleep action, k8s ≥ 1.30 —
   no shell needed in the image) — delays SIGTERM so the app keeps serving
   through the mesh's two-phase drain. The sleep **sizes the in-flight
   completion window**: at deletion-requested the endpoint goes DRAINING (no
   new requests after ~1s), and the mesh closes client connection pools 1s
   before SIGTERM — established requests have `sleep − 1s` to finish, and the
   pools close while idle, ahead of the app's exit.

   Measured under full load (2026-06-12): `sleep 10` (9s window) → **0 failed
   requests per roll**; `sleep 3` (2s window, the supported minimum) → ~1 blip
   per pod for requests still in flight when the window ends. Use ≥ 10 for
   zero-loss rolls; longer if requests can run longer than ~9s (the window is
   capped 2s short of `terminationGracePeriodSeconds`).

Also keep `maxUnavailable: 0` (the mesh never has fewer vetted endpoints than
replicas) and a real `readinessProbe` (the agent gates endpoint promotion on
the app actually answering).

## What the mesh retries for you

Client routes retry on a **different endpoint** (2 retries, 25–250ms backoff):

| What happened | Retried? |
|---|---|
| The caller's proxy could not send the request: `connect-failure`, `refused-stream`, `reset-before-request` | yes, every method |
| The destination's proxy could not reach your application and answered `503`: connection refused or timed out, no healthy host | yes, every method |
| Your application answered `503` itself (the standard "try another endpoint" signal) | yes, every method |
| The destination's proxy had sent the request to your application, and the application closed or reset the connection before answering (`503`) | **only** `GET`, `HEAD`, `OPTIONS`, `TRACE`, `PUT`, `DELETE` |
| Your HTTP/2 application refused the stream (`RST_STREAM` with `REFUSED_STREAM`: not processed, by the protocol) | yes, every method |
| Any other application error (other 5xx), a timeout, a gRPC status your application returned | no |

A gRPC call follows the same rows. The destination's proxy reports its own
failures to a gRPC caller as `UNAVAILABLE`, and the mesh retries those it
could not deliver and not those your application had received.

The fourth row is the one to design for. A request your application has
received may have been run, so the mesh replays it only when the method is
idempotent by definition (RFC 9110). What that means for you:

- **A `POST` or `PATCH` your application received is never run again by the
  mesh.** If the application exits, crashes or closes the connection with one
  in flight, the caller gets a `503` and decides for itself. A caller that
  retries needs the endpoint to be safe to retry (an idempotency key, or a
  natural one).
- **A `GET`, `HEAD`, `OPTIONS`, `TRACE`, `PUT` or `DELETE` may be run twice**,
  on two different pods: once by the pod that failed to answer, once by the
  one the retry reached. Keep them idempotent, as HTTP requires. A `GET` with
  a side effect will see that side effect repeated.
- **Keep your server's keep-alive (idle) timeout long**, or unset. The node
  proxy reuses its connections to your application (an idle HTTP/1.1
  connection for up to an hour, an idle HTTP/2 one for 30 seconds). A server
  that closes an idle connection at the instant the proxy sends a request on
  it looks exactly like a server that died with the request in hand, so a
  `POST` that loses that race is answered `503` and not replayed. Idempotent
  methods are retried and never notice.
- An application that answers `503` asks for another endpoint, for every
  method, `POST` included. Answer `503` only for a request you did not run.
- A gRPC `UNAVAILABLE` your application returns is yours: the mesh does not
  retry it.

The destination's proxy tells the caller's which row applies in the response
header `x-aether-outcome`. It is removed before a response reaches a client
application, anything your application puts under that name is overwritten,
and a request header of that name is removed before it reaches you. One header of Envoy's own still
applies: a response that carries `x-envoy-ratelimited` is never retried.

While the mesh itself is being upgraded across the release that introduced
this, a caller on an upgraded node does not retry anything a destination on a
not-yet-upgraded node answers (`docs/runbook.md`, "Upgrading across #1641").
Do not roll workloads during that upgrade.

## Termination sequence (what actually happens)

```
kubectl delete pod / rollout step
  └─ apiserver sets deletionTimestamp          (pod still Running)
       └─ agent marks endpoint DRAINING        (~1s to every client's EDS:
          new requests stop arriving; established connections keep going)
       └─ 1s before SIGTERM: agent re-marks UNHEALTHY — clients close their
          now-idle pools ahead of the app's exit (drain phase 2)
  └─ kubelet runs preStop sleep, then SIGTERM
       └─ app finishes any post-SIGTERM work through the grace period
  └─ containers exit; CNI DEL fires
       └─ endpoint removed from the registry; local xDS torn down;
          netns pin released after the drain tail (60s, detached)
```

Force deletes (`--grace-period=0`) skip the draining phase; clients then rely
on retries and health checking, so brief errors are possible — avoid force
deletes for serving workloads.
