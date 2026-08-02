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
| `endpoint.aether.io/port` | `8080` | Application port the mesh routes to |
| `endpoint.aether.io/weight` | `1024` | Load-balancing weight |
| `endpoint.aether.io/health-path` | `/` | Path the node-local agent health-checks (delegated liveness) |
| `endpoint.aether.io/health-check-mode` | `eds` | `eds`: node-local agent vets the endpoint once and publishes health over EDS (endpoints enter clients pre-warmed). `active`: every client proxy probes the endpoint itself |
| `metadata.endpoint.aether.io/<key>` | — | Free-form endpoint metadata (subset keys) |
| `endpoint.aether.io/uds-socket` | — | Deliver to a Unix socket instead of a TCP port (see "Serving on a Unix domain socket"); overrides an `EndpointPolicy` on the service |
| `config.aether.io/upstreams` | — | Comma-separated services this pod **calls** (see "Declaring upstreams") |

## Serving on a Unix domain socket

An app that serves on a Unix socket instead of a TCP port joins the mesh with
`endpoint.aether.io/uds-socket: <volume>/<socket-file>`. The node proxy then
delivers inbound requests to that socket. Nothing changes for callers: the pod
is still reached at its pod IP over mTLS and is indistinguishable from a
TCP-serving pod.

```yaml
    metadata:
      labels:
        aether.io/managed: "true"
      annotations:
        endpoint.aether.io/port: "8080"
        endpoint.aether.io/uds-socket: "uds/app.sock"
    spec:
      containers:
        - name: app
          volumeMounts:
            - name: uds                # NO subPath
              mountPath: /run/app
      volumes:
        - name: uds
          emptyDir: {}                 # emptyDir only
```

Requirements:

- **`emptyDir` volume, mounted without `subPath`.** The proxy reaches the
  socket through kubelet's pod-volumes directory on the host
  (`/var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~empty-dir/<volume>/`);
  CSI, projected and `hostPath` volumes, and `subPath` mounts, all have a
  different (or no) host path and are rejected.
- **The annotation is `<volume>/<socket-file>`** — the volume *name* from the
  pod spec (not its mount path) and a file directly inside it. Anything else
  (extra path segments, `..`, absolute paths) is rejected.
- **Keep both names short.** The full host path must fit an `AF_UNIX` address
  (107 bytes), and the fixed kubelet prefix consumes ~91 of them, leaving about
  **16 characters** for `<volume>/<socket-file>` together. A resolution that
  does not fit is refused (and logged) rather than sent to Envoy, which would
  reject the cluster.
- **Ports are still required and still meaningful.** `endpoint.aether.io/port`
  (and `endpoint.aether.io/ports` for multi-port) name the *service* ports
  clients dial and drive inbound demux and endpoint registration; the
  annotation only changes what the proxy dials on delivery. A pod declares one
  socket and every declared port is delivered to it — multiplexing protocols on
  the one socket is the app's affair (normal for gRPC).
- **The app creates the socket**, ideally `0600`–`0660`. The proxy runs as root
  and is never blocked by the mode; restrictive modes keep other containers in
  the pod out.

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

Its advantage is *when* mistakes are caught: the admission webhook rejects a
malformed value — including one that overflows the 107-byte budget — at
`kubectl apply`, instead of leaving the agent to log an error and fall back.

- **The pod annotation wins.** A pod carrying
  `endpoint.aether.io/uds-socket` uses its own value; the policy is the
  service-level default for the pods that do not.
- **The pod spec still has to match.** The CR cannot see the workload, so a
  policy naming a volume the pods do not mount is not rejected at apply time —
  it degrades exactly like a bad annotation (below).
- **One policy per service.** A second policy targeting the same Service is
  ignored (the lexicographically smallest policy name wins) and the conflict is
  logged by the agent.
- The CRD ships in the `crds` chart, and the agent only reads it when
  `proxy.udsWorkloads.enabled` is true.

Failure semantics match TCP delivery: until the socket file exists and accepts,
the delegated-liveness probe fails and the endpoint stays **unpromoted** — no
traffic is sent to it, exactly as for an app that has not yet bound its port.
The same applies if the mesh cannot use the socket at all (the annotation does
not resolve, or the operator disabled `proxy.udsWorkloads`): delivery falls back
to the TCP port, where nothing is listening, so the endpoint stays unpromoted
instead of blackholing traffic. The agent logs the reason.

## Calling other services

With transparent capture + mesh DNS (both on by default), apps dial the
destination by name — `http://<service>.<namespace>.<meshDomain>:18081` (mesh
DNS) or the generated Kubernetes Service
`<service>.<namespace>.svc.cluster.local:18081` — and the CNI-programmed
capture listener routes it. Apps that prefer zero interception assumptions
can instead address the outbound listener explicitly: `http://127.0.0.1:18081`
with the mesh FQDN in the `Host` header. Either way every hop is mTLS between
workload identities; the callee sees the caller's SPIFFE ID in
`x-forwarded-client-cert`.

**Authorities are FQDN-only, namespace-qualified, and deterministic.**
`<service>.<namespace>.<mesh-domain>` (default domain `aether.internal`,
agent `--mesh-domain` / chart `meshDomain`; proposal 020) is the accepted
mesh form — it is simultaneously the vhost domain, the data-plane cluster
name, and the on-demand (ODCDS) lookup key, declared or not. The capture path
also honors the standard `<service>.<namespace>.svc.cluster.local` name. A
`:port` on the authority is stripped before routing. Anything else — bare
names (`Host: my-svc`), foreign domains, nested labels — matches no route and
404s immediately; only authorities under the mesh domain can reach the cold
path. The SPIFFE trust domain is resolved from each component's own SVID and
matches the mesh domain by design, so addressing and identity share one
domain.

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
  *under the mesh domain* fail after the 5s on-demand timeout; anything
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

Client routes retry, on a **different endpoint** (2 attempts, 25–250ms
backoff): `connect-failure`, `refused-stream`, `reset-before-request`, and
`503`. All of these fail before a request reaches an application (or are the
standard "try another endpoint" signal), so retries are safe for
non-idempotent traffic. Application errors (other 5xx) and timeouts are
deliberately **not** retried.

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
