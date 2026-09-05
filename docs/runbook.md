# Developer Runbook

A practical guide for **building, testing, and running Aether from a clone** —
the day-to-day developer loop and the local multi-cluster end-to-end harness.

For **installing and operating** Aether on a real cluster (workload onboarding,
routing, observability), see [`getting-started.md`](./getting-started.md). For the
chart values and CLI/annotation reference, see
[`configuration.md`](./configuration.md).

---

## 1. Prerequisites

| Tool | Why | Notes |
|---|---|---|
| **Bazel** via [Bazelisk](https://github.com/bazelbuild/bazelisk) | build system | The pinned version (`9.2.0`) is read from `.bazelversion`; just run `bazel …` and Bazelisk fetches it. |
| **Go** 1.26.5 | language toolchain | Managed by `rules_go`; you rarely invoke `go` directly (use `bazel run @rules_go//go …`). |
| **Docker** (or **Colima** on macOS) | container images + integration tests | Integration tests spin up real etcd / DynamoDB Local via testcontainers-go. |
| **kubectl**, **Helm 3** (OCI) | deploy / e2e | |
| **kind** | local multi-cluster e2e | Only needed for `e2e/multicluster_config.sh`. |

### macOS + Colima one-time setup

If you use Colima for Docker, generate the Bazel Docker-socket config once:

```bash
./bazel/configure_colima.sh
```

This writes `.bazelrc.colima` (gitignored); it is auto-enabled on macOS via
`--config=colima`, so sandboxed integration tests can reach the Docker socket.

---

## 2. Build

All build/test entry points are in the [`Makefile`](../Makefile) (thin wrappers
over Bazel).

```bash
make build              # bazel build //...  (everything)

make build-agent        # //agent/cmd/agent/...        (node agent + edge + supervisor)
make build-mesh-dns     # //agent/cmd/mesh-dns/...     (slim standalone mesh-DNS daemon)
make build-registrar    # //registrar/cmd/registrar/...
make build-cni-install  # //cni/cmd/cni-install/...
```

There is no `make build-controller`; build it directly with
`bazel build //controller/cmd/controller/...`.

> **The `aether-proxy` (custom Envoy) is NOT built here.** It lives in a separate
> sibling Bazel workspace under `proxy/` (its own `.bazelversion` = 8.7.0) and
> compiles Envoy from source (multi-hour; use a warm cache / CI). Build/load it
> with `make load-proxy-image` only when you need a fresh proxy image. See
> [`proxy/README.md`](../proxy/README.md) and proposal 010.

---

## 3. Test

```bash
make test               # bazel test //...              (unit + integration; needs Docker)
make test-unit          # --test_tag_filters=-integration  (no Docker)
make test-integration   # --test_tag_filters=integration   (needs Docker)
make test-race          # all tests with the Go race detector
```

Run a single target directly:

```bash
bazel test //agent/internal/xds/cache:cache_test
```

Integration tests are tagged `integration` and sized `medium`; many are also
guarded by `testing.Short()`, so you can force unit-only behavior on a specific
target with:

```bash
bazel test //... --test_arg=-test.short
```

---

## 4. Format & lint

```bash
make format             # bazel run //:format        — gofumpt, buildifier, shfmt, buf (in place)
make format-check       # bazel run //:format.check  — CI-friendly, fails on drift
make lint               # bazel build --config=lint //...  — buf, buildifier, shellcheck aspects
```

After changing Go imports or adding/removing Go files, regenerate BUILD files:

```bash
make gazelle            # bazel run //:gazelle
```

To add a Go dependency (never edit `go.mod` by hand, and never run `go mod tidy`
directly):

```bash
bazel run @rules_go//go get <package>
make gazelle
make tidy               # bazel mod tidy
```

---

## 5. Container images

In-repo images (agent, mesh-dns, cni-install, registrar) build with `rules_img`:

```bash
make load-all           # load agent + mesh-dns + cni-install + registrar into local Docker
make load-agent-image   # a single image (…-mesh-dns-image, …-registrar-image, …-cni-install-image likewise)
make push-all           # push all to the registry
```

`mesh-dns` is its own image (#583): the `aether-mesh-dns` DaemonSet must NOT ship
the full agent, so it gets the slim `/mesh-dns` binary alone (~7Mi vs the agent's
~20Mi) and the chart pins it separately via `agent.meshDnsDaemon.image`.

The `controller` image is not in `load-all`; load it with
`bazel run //controller/cmd/controller:image_load`.

---

## 6. Local multi-cluster end-to-end (proposal 026)

[`e2e/multicluster_config.sh`](../e2e/multicluster_config.sh) stands up **two kind
clusters** (`a` = exporter, `b` = importer) that share **one etcd** (a Docker
container on kind's network) and drives the cross-cluster GAMMA config loop:

```
cluster a: HTTPRoute + ServiceExport ──(registrar config-export)──▶ shared etcd
                                                                       │
cluster b: agent --import-config  ◀──(registrar ListAllConfig reads same etcd)─┘
```

### Commands

```bash
e2e/multicluster_config.sh up      # build+load images, create clusters + etcd, install aether on both
e2e/multicluster_config.sh test    # apply the Service+ServiceExport+HTTPRoute on 'a', assert propagation
e2e/multicluster_config.sh verify  # re-run the assertions only
e2e/multicluster_config.sh down    # delete both clusters + the shared etcd
e2e/multicluster_config.sh         # up + test (full run)
```

It builds images via `make load-all`, installs the Gateway API (experimental
channel) + MCS-API CRDs, then installs both aether charts per cluster with
`spire.enabled=false`, `agent.gamma=true`, `registrar.registryBackend=etcd`, and
`agent.importConfig=true` on cluster `b`.

### The `fs.inotify` gotcha

Two kind clusters each run an inotify-heavy agent DaemonSet; the host's default
limits are easily exhausted, leaving cluster `b`'s agent stuck in `Init:Error`.
`up` raises the limits (needs sudo):

```bash
sudo sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=524288
```

Without it, the **control-plane** half of the loop (export → shared etcd,
readable by `b`'s registrar) is still proven; only the agent-side materialization
on `b` is unobservable. See the script header and `e2e/kind-cluster.yaml` (which
also assigns non-overlapping pod/service CIDRs per cluster so cross-cluster
endpoints in the shared registry never collide).

### The other multi-cluster harnesses

Two sibling harnesses reuse the same two-kind pattern (same `up`/`test`/
`verify`/`down` verbs, same inotify gotcha); both run SPIRE on each cluster
under a **shared trust domain** (shared upstream CA in `e2e/certs/`):

- [`e2e/multicluster_waypoint.sh`](../e2e/multicluster_waypoint.sh) — the
  **019 east/west waypoint data path**: two clusters + ONE shared etcd,
  `agent.eastWestWaypoint=true`; asserts client(a) → echo(b) returns 200 over
  the node tunnel with mTLS end-to-end (nightly CI: `e2e.yaml` waypoint job).
- [`e2e/multicluster_replicator.sh`](../e2e/multicluster_replicator.sh) — the
  **006 two-region replicator failover**: one etcd PER cluster,
  `registrar.peerEtcd` cross-wired; asserts mirror visibility, the data path
  over the mirror, lease-lapse failover when a region's registrar dies, and
  recovery (nightly CI: `e2e.yaml` replicator job).

---

## 7. Installing on a real cluster

Aether ships **two** charts, and **install order matters**:

```
charts/crds     — the CRDs: MeshConfig + HTTPFilter + EdgeConfig + EndpointPolicy   ← install FIRST
charts/aether    — the whole system (agent DaemonSet + proxy + mesh-dns + registrar + controller)
```

Install the CRDs before the system chart. **The agent crashes if it starts before
the `HTTPFilter` CRD exists** (it watches that type at startup); installing the
`crds` chart first avoids the race (see #453).

```bash
# 1) CRDs first
helm upgrade --install aether-crds oci://ghcr.io/bpalermo/aether/charts/crds \
  --version "$VERSION"

# 2) then the system
helm upgrade --install aether oci://ghcr.io/bpalermo/aether/charts/aether \
  --version "$VERSION" -n aether-system --create-namespace \
  --set clusterName=my-cluster --set meshDomain=aether.internal
```

From a checkout, the Bazel install targets do the same in order:

```bash
bazel run //charts/crds:crds.install
bazel run //charts/aether:aether.install
```

> Always pass the **full** values on every `helm upgrade` of the `aether` chart —
> do **not** use `--reuse-values` (it keeps the stale digest-pinned image). Bump
> the chart's `version:` on any change to its templates/values (CI enforces this).

There are also two standalone charts, installed independently: **`prober`**
(`charts/prober`) — the external mesh-availability prober (proposal 013) — and
**`udsecho`** (`charts/udsecho`) — the UDS validation workloads (proposal 034)
that exercise both socket-delivery paths (annotation and `EndpointPolicy`) under
continuous mesh traffic.

See [`charts/README.md`](../charts/README.md) for chart layout, image mirroring,
and the `--stamp` versioning scheme, and [`getting-started.md`](./getting-started.md)
for the full install + onboarding walkthrough.

> Moving or renaming a binary **inside** an image — the Bazel image rules, a `tars_layer`
> entry (`/proxy-ready`, `/mesh-dns-ready`, `/opt/cni/bin/aether-cni`), or `proxy/` —
> breaks profile symbolisation, silently: flame graphs decay to hex addresses with no
> error and no failing test. **Adding** a binary to an image is the same trap; it profiles
> as hex until it is listed. See
> [`observability/profiling-symbols.md`](./observability/profiling-symbols.md) for the
> path table that has to be updated alongside it.

## 8. Troubleshooting

### Forwarded DNS keeps failing after a kube-dns roll

The mesh-DNS forward path keeps a small pool of **connected** UDP sockets per upstream
(issue #674) rather than dialling one per query. The upstream is a ClusterIP, so
connecting pins a conntrack entry to **one** kube-dns backend pod; when that pod rolls
the entry survives pointing at a corpse and datagrams are black-holed with **no ICMP** —
the socket only ever sees a read timeout.

This self-heals: any exchange error retires the socket, and every socket also expires on
its own budget (a jittered ~30s, or 1000 queries). Symptoms are therefore a burst of
`aether_mesh_dns_forward_conn_recycles_total{reason="error"}`, not a sustained outage.
If it does NOT settle:

```bash
# Dials per forwarded query -- should be well under 0.01, and is 1.0 when pooling is off.
sum(rate(aether_mesh_dns_forward_conn_dials_total[5m]))
  / sum(rate(aether_mesh_dns_queries_total{result="forwarded"}[5m]))

# Open pooled sockets -- flat at pool size x upstreams at steady state.
aether_mesh_dns_forward_conn_pool_open
```

To rule the pool out entirely, disable it — `--forward-pool-size=0` restores the exact
pre-#674 dial-per-query behaviour:

```bash
helm upgrade ... --set agent.meshDnsDaemon.forwardPoolSize=0
```

### The agent reports an unrepairable conflist

Symptom: `AetherCNIConflistUnchained` fires for a node, the agent there is NotReady and
the startup taint is held (proposal 033, #667), and the agent logs

```
aether is not chained in the active CNI conflist and no known-good entry could be
recovered; cni-install must run
```

Aether is a **chained** plugin in another CNI's conflist, so this node issues no CNI ADD
to the agent at all: every pod that starts on it comes up unmeshed. The re-assert loop
re-appends the entry it last **observed**, and this message means it has none — the
strip landed before its first check (the priming window, #680).

Check the durable entry `cni-install` leaves beside the conflist; the loop primes from it
and repairs within a check (~2.5s) when it is present and valid:

```bash
# On the node (talosctl, or a debug pod with /etc/cni/net.d mounted):
ls -la /etc/cni/net.d/                     # .aether-cni-entry must be there
cat /etc/cni/net.d/.aether-cni-entry       # one JSON object, "type": "aether-cni"
grep -c aether-cni /etc/cni/net.d/*.conflist
```

- **Present and valid, agent still refusing** — look for
  `primed known-good entry from durable file` in the agent log. Its absence with the
  file present means the agent is reading a different directory: compare its
  `--mounted-cni-net-dir` (log line `CNI conflist re-assert loop started dir=…`) with
  where `cni-install` wrote.
- **Missing** — the node predates #680, or `cni-install` failed to write it (search the
  init container's log for `failed to write the durable aether CNI entry`). Recreate the
  agent **pod** on that node (`kubectl -n aether delete pod aether-agent-…`): only the
  init container renders the entry, so restarting the container is not enough.
- **Present but garbage** — the agent logs `the durable aether entry is unusable` and
  ignores it, by design; recreate the agent pod to have `cni-install` rewrite it.

Never hand-write either file: `cni-install` is the durable entry's only writer, and the
conflist belongs to the primary CNI plus the re-assert loop.
### Grepping the outbound identity bindings during a soak (issue #638)

`ssl_fail_verify_san` bursts a few tens of seconds into a fresh proxy generation point at
the node's **netns → SPIFFE ID index** (`localWorkloads`), which is the whole of the
(source pod → outbound cluster → SDS client-cert secret) binding: every mTLS-injected
outbound cluster carries one transport-socket match per local identity, *named by* the
SPIFFE ID whose SDS secret it fetches, plus one matcher mapping each source pod's netns
to one of those names. The agent logs that binding at **INFO**, `outbound identity
binding`, only when a `(source pod, cluster)` pair re-binds or is seen for the first time
— so steady state is silent and a startup re-bind is exactly the handful of lines you
want. A binding whose bound secret is not the owning pod's own SPIFFE ID additionally
logs **WARN** `outbound cluster bound to a foreign identity` and increments
`aether_agent_identity_outbound_binding_mismatch_total`; an identity mapping no pod owns
any more (a missed CNI DEL) logs WARN `outbound identity mapping has no owning pod`.
Above 200 changed pairs in one snapshot a single `outbound identity bindings changed`
summary replaces the per-pair lines, keeping the distinct source→identity transitions.

```bash
# Every re-bind on one node, around the roll.
kubectl -n aether-system logs ds/aether-agent --since=10m \
  | grep -E 'outbound identity (binding|bindings changed|mapping)|foreign identity'

# The alarm, in VictoriaLogs (field syntax; never `| stats`, it false-zeroes).
_stream:{service.name="aether-agent"} AND "outbound cluster bound to a foreign identity"

Note: the stream labels on these logs are the dotted OTel resource fields (`service.name`,
`k8s.pod.name`, `k8s.namespace.name`). A selector on `k8s_container_name` matches nothing and
returns a FALSE ZERO — control-test any negative by dropping the selector.

# Same, as a counter: zero at steady state, any increase is the #638 defect.
# increase(), never a raw read — the raw value is per-process (an agent restart
# resets it) and an instant query lands between samples and false-zeroes.
sum by (k8s_node_name) (increase(aether_agent_identity_outbound_binding_mismatch_total[1h]))
```

Join the `snapshot_version` on the WARN with the first
`envoy_cluster_ssl_fail_verify_san_total` sample of the new proxy generation: a WARN at
(or just before) that timestamp proves the source-side binding was wrong; the absence of
one over a window that *did* produce SAN failures rules the agent-side index out and
moves the search to the destination proxy's inbound chain selection.

### Attributing an `ssl_fail_verify_san` event to the TERMINATING node (issue #638, inbound side)

**The inversion.** Envoy's `default_validator.cc:332` message
`verify cert failed: SAN matcher, certificate SANs are [...]` prints the SANs of the
certificate being *validated* — the one the **peer** presented. So
`envoy_cluster_ssl_fail_verify_san_total` is a **client-side** counter about the
**server's** certificate, and every #638 ledger join that read it as "the restarting
proxy presented X as its client identity" had it backwards. The wrong identity belongs to
a **server** certificate: the inbound filter chain → SDS server-secret binding of
whichever proxy **terminated** the connection. For same-node traffic that is the
restarting proxy itself, which is the observed "node-wide constant per time slice" shape.
The outbound discriminator above watches the client side and structurally cannot fire for
this.

**The inbound discriminator.** On every snapshot the agent reads each local pod's inbound
listener back out of the snapshot it just handed Envoy, extracts each filter chain's
`DownstreamTlsContext.tls_certificate_sds_secret_configs[0].name`, and compares it with
the SPIFFE ID of the pod that listener entry belongs to:

- **INFO `inbound identity binding`** — `chain` (`<listener>/<chain>`), `pod`,
  `pod_spiffe_id`, `secret` (the server certificate the chain will present),
  `previous_secret` (empty on a first bind), `secret_served`, `snapshot_version`. Emitted
  only on a first bind or a change; steady state is silent.
- **WARN `inbound chain bound to a foreign identity`** + counter
  `aether_agent_identity_inbound_binding_mismatch_total` — the chain would terminate mesh
  mTLS for its pod while presenting **another workload's** SVID.
- **WARN `inbound chain references a secret absent from the snapshot`** — the chain
  reached Envoy before its own SVID did (the other candidate mechanism). Reported, not
  counted.
- Above 200 changed chains in one snapshot a single `inbound identity bindings changed`
  summary replaces the per-chain lines.

The binding holds **by construction** inside `proxy.NewInboundListener` (the chain's
secret name and the chain's own name both come from the same `CNIPod`). That is the
point: **if the counter stays 0 through a #638 event while the INFO lines show the chains
re-binding correctly, the mis-binding is not in the agent's snapshot — it is in Envoy's
SDS/secret lifecycle across the hot restart**, and the agent-side line of enquiry closes.
A non-zero counter names the pod, both identities and the snapshot version.

```bash
kubectl -n aether-system logs ds/aether-agent --since=10m \
  | grep -E 'inbound identity (binding|bindings changed)|inbound chain '
```

```
# VictoriaLogs (field syntax; never `| stats`, it false-zeroes).
_stream:{service.name="aether-agent"} AND "inbound chain bound to a foreign identity"
```

```promql
# Zero at steady state; any increase is an agent-side inbound mis-binding.
# increase(), never a raw read: the value is per-process and an instant query
# lands between samples and false-zeroes.
sum by (k8s_node_name) (increase(aether_agent_identity_inbound_binding_mismatch_total[1h]))
```

#### The ledger join, with the terminating-node column

The join that has been missing: each failing request must be attributed to the node whose
proxy **served** it, then compared with the node whose proxy was restarting.

1. **Pull the events.** Field syntax only — a bare phrase search or `| stats` returns a
   documented FALSE ZERO. Control-test every negative by dropping the reason field (normal
   200s must come back).

   ```
   _stream:{service.name="aether-proxy"}
     AND log_name:aether_access_logs
     AND upstream_transport_failure_reason:"CERTIFICATE_VERIFY_FAILED"
   ```

   The identity in `certificate SANs are [spiffe://…]` on these lines is the **server's**.
   Keep `upstream_host`, `upstream_cluster`, `pod_name`/`pod_namespace` (the local pod this
   hop serves — the *client* side here), `response_flags` and the timestamp.

2. **`upstream_host` → pod → node.** `upstream_host` is `<pod IP>:18008`. Resolve the IP
   to its pod and that pod's node:

   ```bash
   kubectl get pods -A -o wide --field-selector status.phase=Running \
     | awk '$7=="<upstream-ip>" {print $1, $2, $8}'   # ns name node
   ```

   For an IP that is already gone, use the agent-side record on each node
   (`aether_agent_storage_pods` is the per-node count; the entries themselves are the
   agent's protojson store) or the pod-IP index in the run record. **That node is the
   terminating node** — its proxy holds the inbound listener whose chain presented the
   certificate the client rejected.

3. **Compare with the restarting node.** The proxy generation boundary per node:

   ```promql
   max_over_time(envoy_server_hot_restart_epoch[8h])          # step, per node
   max_over_time(envoy_cluster_ssl_fail_verify_san_total[8h]) # never an instant query
   ```

   `max_over_time` is mandatory — the series ages out with the proxy generation and an
   instant query at grade time returns **no series at all** (that trap has produced two
   premature "zero" readings on this issue).

4. **Read the verdict.**
   - Terminating node **==** the restarting node → same-node termination by the restarting
     proxy. Grep that node's agent for the WARNs above in the same window; a hit localises
     the defect to the agent's snapshot, a miss to Envoy's SDS lifecycle.
   - Terminating node **!=** the restarting node → the server was a remote proxy that
     itself shows no counter (the counter is client-side). Grep *that* node's agent log.
   - Terminating nodes **scattered across many nodes** for one presented identity → the
     identity was not bound per-server, and `upstream_host` is not the TLS-terminating
     peer; record it and re-open the transport path.
