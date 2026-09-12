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
| **Go** 1.27.1 | language toolchain | Managed by `rules_go`; you rarely invoke `go` directly (use `bazel run @rules_go//go …`). |
| **Docker** (or **Colima** on macOS) | container images + integration tests | Integration tests spin up a real etcd via testcontainers-go. |
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
# The commit you intend to deploy (the `publish` run that built it), plus each
# chart's X.Y.Z from its charts/<name>/Chart.yaml at that commit.
COMMIT=<full 40-char git sha>
CRDS_VERSION=<X.Y.Z>-$COMMIT
AETHER_VERSION=<X.Y.Z>-$COMMIT

# 1) CRDs first
helm upgrade --install aether-crds oci://ghcr.io/bpalermo/aether/charts/crds \
  --version "$CRDS_VERSION"

# 2) then the system. Prefer this commit-pinned chart tag over the bare
#    `--version <X.Y.Z>`: the bare tag is mutable and re-pushed by every publish,
#    the commit tag never is (#692).
helm upgrade --install aether oci://ghcr.io/bpalermo/aether/charts/aether \
  --version "$AETHER_VERSION" -n aether-system --create-namespace \
  --set clusterName=my-cluster --set meshDomain=aether.internal

# 3) ALWAYS assert what actually landed. The chart's appVersion is the commit
#    that built it, so this is the only check that catches a chart tag serving
#    a different build than you asked for — which happened on 2026-09-05, when
#    two racing publishes wrote the same bare tag and the older build won,
#    silently deploying a release without #689 in it (#692).
got="$(helm get metadata -n aether-system aether -o json | jq -r .appVersion)"
case "$got" in
  *"$COMMIT"*) echo "deployed $got — OK" ;;
  *) echo "DEPLOY MISMATCH: appVersion=$got, expected $COMMIT" >&2; exit 1 ;;
esac
```

`appVersion` is the chart's `{STABLE_GIT_VERSION}` — `git describe --tags --always
--long --dirty --abbrev=40` — so the full sha is embedded in it but may carry a
`v1.2.3-N-g` prefix or a `-dirty` suffix; hence the substring match rather than
string equality. If it fails, the chart tag you pulled was built from a different
commit: re-run that commit's `publish` workflow and upgrade again.

**After a proxy release, normally there is nothing to do.** `proxy-release`
publishes the image, pins `charts/aether/values.yaml` (`tag:` *and* `digest:`),
pushes `chore/proxy-image-bump-<short sha>`, opens the pin PR with the
`RELEASE_PAT` fine-grained token and arms `--auto --squash` (#727). GitHub merges
it the moment `ci` + `proxy` are green — the `main` ruleset is still the only
gate, and it has no bypass actors. Any superseded `chore/proxy-image-bump-*` PR
is closed and its branch deleted by the same run; `main-post-merge` updates the
pin PR's branch whenever another merge leaves it `BEHIND` (auto-merge does not do
that by itself). Watch the rolling **proxy releases pending chart pin** issue
(#724): every release comments there with the PR, the digest, the chart version,
the run link and the token's remaining life. The **deploy stays manual** — after
the pin merges, take that commit's `publish` run and use the commit-suffixed
chart tag (`<X.Y.Z>-<full sha>`, above) with the `appVersion` assert.

**Manual fallback.** When #724 says the token is missing, expired or rejected,
the run degrades to #703's hand-off instead of failing: the branch is pushed with
`GITHUB_TOKEN` and both the run summary and the #724 comment carry the exact
`gh pr create` one-liner (its `| release token |` row says which of the three it
was). Run it from a checkout, let `ci` + `proxy` go green, squash-merge, close
any superseded `chore/proxy-image-bump-*` PR. A PR opened by `GITHUB_TOKEN`
itself is never an option: GitHub's recursion guard means it fires no
`pull_request` events, so the required checks would never report.

**Rotating `RELEASE_PAT`.** The workflow warns 30 days ahead — a `::warning::` on
the release run and the `| release token |` row on #724 — and, once expired,
falls back to the manual hand-off. To rotate: Settings → Developer settings →
Fine-grained tokens → new token, resource owner `bpalermo`, **only** the
`bpalermo/aether` repository, repository permissions **Contents: Read and write**
+ **Pull requests: Read and write** and nothing else (no Workflows, no
Administration, no Actions; `Metadata: Read` is implied), maximum expiration.
Then Settings → Environments → `release` → replace the **environment** secret
`RELEASE_PAT` with it. It must stay an environment secret of `release`, whose
deployment-branch policy names `main` only: that is what keeps a `pull_request`
run, or a run of a workflow edited on a branch, from ever reading it. The next
`proxy-release` run reports the new expiry on #724.

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

### Grading the mesh-DNS lame-duck handoff across a roll

On SIGTERM the resolver keeps serving until it has PROVEN a successor is answering on
the shared SO_REUSEPORT address, and only then closes its sockets (issue #729). Every
window ends with exactly one exit, counted by `reason`:

| `reason` | meaning |
| --- | --- |
| `successor` | a DIFFERENT instance answered on our listen address — the handoff drained. **This is what a healthy DaemonSet roll must show on every node.** |
| `deadline` | `--lame-duck-max` elapsed with no successor. Expected on a scale-down or node drain; on a roll it means the successor never came up, and the queued datagrams were dropped as they were before #729. |
| `signal` | a second termination signal cut the window short. |

The exit is a **once-per-process event**, so the counter carries an `instance` attribute
— this process's 64-bit lame-duck identity stamp, exactly one value per mesh-DNS
generation (issue #736). Without it every successor restarts the same `{node,reason}`
series at 1, Prometheus never sees a reset, and `increase()` over a window spanning
several rolls reads ~0 (the #729 validation had to count 50 exits from raw per-roll
snapshots). With it each generation is its own series and a genuine 0 → 1 rise:

```promql
# Exits per node over the last 30m. After two rolls this is 2 per node -- it counts
# every generation, which the pre-#736 series could not.
sum by (node) (increase(aether_mesh_dns_lame_duck_exits_total[30m]))

# The only breakdown that matters: everything must be reason="successor".
sum by (node, reason) (increase(aether_mesh_dns_lame_duck_exits_total[30m]))

# How long each generation held its sockets open, p95 (10s is --lame-duck-max,
# i.e. a window that ran to its deadline).
histogram_quantile(0.95,
  sum by (le) (rate(aether_mesh_dns_lame_duck_duration_seconds_bucket[30m])))
```

> **Sample count.** A generation records its exit and then dies, so its series is
> exported by the shutdown flush and usually carries a **single** sample. `rate()` and
> `increase()` need two samples in the range to produce anything for a series, so if a
> window you know had rolls still comes back empty, count the series instead — each one
> tops out at exactly 1, so this is exact regardless of how many samples landed:
>
> ```promql
> # Rolls per node, sample-count independent.
> sum by (node) (max_over_time(aether_mesh_dns_lame_duck_exits_total[30m]))
> sum by (node, reason) (max_over_time(aether_mesh_dns_lame_duck_exits_total[30m]))
> ```
>
> `max_over_time` is also what survives the series ageing out with its generation — the
> same trap that produced two premature "zero" readings on #638.

`instance` is also the join key back to the logs: the same stamp appears as `instance=`
on that generation's `lame duck started` and `successor observed` lines (and as
`successor=` on the *predecessor's* line, naming the generation that replaced it), so a
suspicious series resolves to one pod's window.

```
_stream:{service.name="aether-mesh-dns"} AND instance:"<stamp>"
```

Cardinality is one series per generation per node — bounded by how often the DaemonSet
rolls, and the series age out with the generation. That ageing-out is why an **instant**
query is never the right read here: minutes after a roll the generation's series is
gone, and the instant read returns nothing at all rather than the exit it recorded.

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
### The agent is stuck waiting for SPIRE

Symptom: the agent logs, once per retry,

```
waiting for the SPIRE Workload API to issue this workload's SVID socket=/run/secrets/workload-spiffe-uds/socket attempt=7 elapsed=41.2s warnAfter=2m0s error=...
```

**That line is the fix working, not the failure** (issue #740). Startup no longer dies
when the Workload API is not serving yet: the agent programs everything that does not
need identity, keeps `/healthz` and `/readyz` answering, and folds the SVID in when it
arrives. The signature of a healthy wait is the waiting lines plus **0 restarts** and
**no** `failed to create SPIRE Workload API source` anywhere. Before #740 that error was
followed by `exit 1`, which is how a cold boot produced 2-4 restarts per node.

What to check, in order:

```bash
# 1. Is it still waiting, or did it resolve? (arrival is one INFO line)
kubectl -n aether logs ds/aether-agent | grep -E "waiting for the SPIRE Workload API|obtained this workload's SVID|resolved workload trust domain"

# 2. Restarts must be zero — a restarting agent is a DIFFERENT problem
kubectl -n aether get pods -l app.kubernetes.io/name=aether-agent

# 3. The readiness gate and its dwell
kubectl -n aether exec ds/aether-agent -c agent -- wget -qO- localhost:8082/readyz?verbose
```

#### The readiness semantics are NOT the same on every component

This is the first thing to get straight, because the same `spire-svid` check
deliberately answers differently on the agent and on the three Deployments:

| Component | Workload | Dwell before NotReady | What NotReady does |
| --- | --- | --- | --- |
| `aether-agent` | DaemonSet | **2 m** (`spire.NotReadyDwell`) | The controller's node-taint guard re-arms `aether.io/agent-not-ready:NoSchedule` on this node |
| `aether-registrar` | Deployment | **0** (`spire.ServiceNotReadyDwell`) | This replica leaves the registrar Service's endpoints |
| `aether-controller` | Deployment | **0** | This replica leaves the webhook Service's endpoints |
| `aether-edge` | Deployment | **0** | This replica leaves the LoadBalancer's endpoints |

The agent's dwell is **not** a general tolerance for slow SPIRE. It exists solely
because NotReady on the agent DaemonSet is the signal the node-taint guard consumes, and
a taint is a fleet-level action a 40s SPIRE hiccup must not trigger. Nothing behind a
Service has that coupling: there, NotReady removes one replica from one endpoint set,
which is precisely the right handling of a pod that cannot complete an mTLS handshake,
so those three go NotReady the instant they are known to lack an identity.

That asymmetry is a fix, not an inconsistency. On the rev210 upgrade roll
(2026-09-07 20:03:45Z) the registrar carried the agent's 2 m dwell, so a registrar Pod
was Ready — and in its Service's endpoints — before it had an SVID, and an agent on
`main-worker-01` that dialled it logged
`transport: authentication handshake failed: x509svid: could not get X509 bundle`.

The `--spire-wait-warn-after` flag is a **logging** threshold only; since #740 PR 4 it no
longer drives readiness on any component. On the agent the two values coincide at 2 m.

`readyz?verbose` reads `spire-svid ok` for the first **2 minutes** of an agent's wait
(the dwell above) and then fails. It prints
**`spire-svid failed: reason withheld`** — controller-runtime redacts checker errors, so
the endpoint never shows the reason. The reason is in the **log**, once per transition:

```bash
kubectl -n aether logs ds/aether-agent | grep -E "readiness (failing|passing)"
# spire-svid readiness failing   reason="no SPIRE SVID after 2m10s (socket /run/secrets/…)"
# spire-svid readiness passing
```

The dwell is deliberate: the controller's node-taint guard re-arms
`aether.io/agent-not-ready:NoSchedule` after 30s of NotReady, so a gate that failed
immediately would turn a brief SPIRE hiccup into a fleet-wide taint. Past the dwell the
NotReady is the point: stop scheduling pods onto a node that cannot give them an
identity. Since #740 the agent's own taint remover **holds** that taint while any
readiness gate is failing, instead of clearing it 50ms after the guard arms it — so
**spire-server, spire-agent and the SPIFFE CSI driver must tolerate the taint**
(`spire.waitWarnAfter`'s note in `values.yaml`; applied on talos-main in
k8s-talos-main #45). Without those tolerations the outage fences out its own cure.

**The node keeps serving.** While the agent has no SVID it does NOT open the xDS socket
— it logs `holding xDS until this agent has an SVID; Envoy keeps its current
configuration` every 15s with a rising `elapsed`, and Envoy goes on serving the last
config it was given. This matters because the alternative is worse than the crash loop
#740 replaced: a live agent that cannot reach the registrar publishes a **local-only**
snapshot, which REPLACES a complete one and strips every cross-node endpoint. On
2026-09-07 that failed ~95% of one node's mesh probes for a whole 6m41s outage. If you
see `registry unavailable for initial snapshot; starting with local-only config` while
SPIRE is down, that is the bug — the hold is missing.

Recovery is announced only when **both** halves of the identity exist (the SVID and the
bundle for its trust domain), and the registrar client is kicked out of its gRPC
backoff the moment they do:

```bash
kubectl -n aether logs ds/aether-agent | grep -E "identity acquired"
# identity acquired; generating the initial snapshot                      held=6m41.2s
# identity acquired; reconnecting the registrar client immediately …
```

**Acquiring the SVID is not the same as being able to use it.** Every handshake made
before the SVID landed failed, so the registrar client's gRPC connection is still
carrying that failure for the second or so it takes to redial — and a snapshot built in
that window has no cross-node endpoints, which is the same local-only outage as above by
another route. Since #740 PR 5 the initial snapshot therefore waits for a registry that
actually **answers**, not merely for its readiness latch, and says so:

```bash
kubectl -n aether logs ds/aether-agent | grep -E "registry connected|not yet re-established|local-only"
# registry connected; generating the initial snapshot                     waited=1.106s
# registrar connection not yet re-established after identity; retrying    reconnecting=true …
```

`registry connected` is the healthy end of every agent start; `waited` is normally a few
milliseconds and rises to about a second when the start raced identity. The
`not yet re-established` INFO is that race being named correctly — it is **our own**
connection catching up, not the registrar, so do not go reading registrar logs for it.
Reserve that for `registrar has no identity yet; retrying`, which is only ever logged
once this agent's identity has been settled for more than a few seconds. On the rev211
deploy roll (2026-09-07 20:47Z) both lines read `registrar has no identity yet`, and
`main-worker-02` published an endpoint-less snapshot and logged **316** prober
`http_error` in ~30s while both registrar replicas had been Ready for 43 seconds. The
wait is bounded by the same 15s budget as before, so a registrar that is genuinely
unreachable still ends at `registry unavailable for initial snapshot; starting with
local-only config` rather than stalling the node.

The upstream cause is almost always spire-server or the node's spire-agent, not aether:

```bash
kubectl -n spire-server get pods                  # spire-server-0 Running and 1/1?
kubectl -n spire-system get pods -o wide          # this node's spire-agent Running?
kubectl -n spire-server logs spire-server-0 | tail -50
```

Metrics for the same question, fleet-wide:

```promql
# nodes without an SVID right now (0 = waiting)
min by (k8s_node_name) (aether_agent_spire_svid_ready)
# how long the wait took on the last boot
histogram_quantile(0.99, sum by (le) (rate(aether_agent_spire_wait_seconds_bucket[15m])))
# must stay flat at 0: a source that connected but could not serve an SVID
sum(increase(aether_agent_spire_source_restarts_total[1h]))
```

While the wait is on, the registrar watch stream logs
`watch stream deferred until this agent has an SVID` at INFO and counts no
`watch_errors` — the agent cannot handshake without a certificate, and that is not a
registrar fault. Envoy keeps serving on the certificates and the configuration it
already holds. A **new** pod on that node is still registered and still gets its CNI
redirect, but nothing is pushed to Envoy until the SVID lands (and it could not have
meshed without an identity anyway) — which is why the node reports NotReady and stays
tainted for the duration.

#### The controller, registrar or edge is stuck waiting for SPIRE

All four binaries wait the same way, log the same two lines, and register the same
`spire-svid` readiness gate, so the commands above work verbatim against
`deploy/aether-controller`, `deploy/aether-registrar` and `deploy/aether-edge`.

Two things differ, and both matter. The **dwell is 0** on all three Deployments (see the
table above): they go NotReady the moment they are known to have no identity, and leave
their Service's endpoints, rather than sitting Ready for 2 minutes absorbing dials they
cannot handshake. And the **metric namespace** differs:
`aether_controller_spire_*`, `aether_registrar_spire_*`, `aether_edge_spire_*`
(**not** `aether_agent_spire_*` -- a false zero if you query the wrong one).

What each component does while it waits:

- **Controller** -- the admission webhooks cannot complete a TLS handshake, so the
  apiserver **fails open**: both webhook configurations are `failurePolicy: Ignore`,
  so `MeshConfig`/`HTTPFilter`/`EdgeConfig`/`EndpointPolicy`/`HTTPRoute` creations are
  admitted **unvalidated** and pods are **not** mutated (no mesh-domain `ndots`
  injection) for the duration. The caBundle injector logs
  `webhook caBundle injection deferred until this workload has an SVID` at INFO and
  injects on the first SVID. Symptom of the wait having ended: one
  `injected SPIRE trust bundle into webhook caBundle`. The replica is NotReady for the
  whole wait and leaves the webhook Service's endpoints, which is what makes the
  fail-open immediate instead of a 10s webhook timeout per request.
- **Registrar** -- agents cannot handshake, so their watch streams retry (they log
  `watch stream deferred until this agent has an SVID`, not errors). While the SVID is
  pending the registrar authorizes **nothing**; the trust domain it authorizes against
  is read from its own SVID at handshake time and announced once, as
  `resolved workload trust domain from SPIRE`. The replica is NotReady for the whole
  wait and leaves the registrar Service's endpoints, so agents dial one that can serve
  instead of one that will reject the handshake. An agent that dials it anyway (a
  single-replica registrar, or the endpoint update racing the dial) logs
  `registrar has no identity yet; retrying` at **INFO** and counts no `watch_errors`;
  the cluster-refresh path logs
  `cluster refresh deferred: the registrar has no identity yet, keeping the current
  snapshot` at **WARN** and counts no `refresh_errors`. Both are transients of the
  server's startup, and both used to be ERRORs (#740 PR 4).
- **Edge** -- ingress keeps serving on the certificates its Envoy already holds. The
  control plane starts with seeds (mesh domain, empty SPIFFE ID), so newly loaded
  clusters carry **no upstream mTLS** until the SVID lands; the arrival logs
  `resolved edge identity from SPIRE` and pushes a new snapshot, so no restart is
  needed. The replica is NotReady for the whole wait and leaves the LoadBalancer's
  endpoints; a second replica goes on serving ingress.

```bash
for d in aether-controller aether-registrar aether-edge; do
  echo "== $d"
  kubectl -n aether logs deploy/$d | grep -E "waiting for the SPIRE Workload API|obtained this workload's SVID"
done
```

Before #740 each of these exited with `failed to create SPIRE Workload API source`
(the controller: `failed to open SPIRE Workload API source`) and crash-looped. Seeing
that error at all now means an OLD image.

**Expected side effects while an agent holds xDS (#743).** The hold keeps the xDS socket
closed so Envoy keeps its last configuration; from Prometheus that is
`envoy_control_plane_connected_state == 0` with `envoy_server_live == 1` on that node, and
`EnvoyControlPlaneDisconnected` fires for the duration. It resolves by itself when the
identity lands (`identity acquired; generating the initial snapshot held=…`). A firing
`EnvoyControlPlaneDisconnected` with `aether_agent_spire_svid_ready == 0` on the same node
is this case, not a broken agent. Attestation-to-Ready is bounded by the wait loop's 30 s
backoff cap; the node taint is armed ~30 s after NotReady and stays until readiness returns.

**Reproducing the SPIRE-outage path on purpose.** Scaling `spire-server` to 0 alone does
not reproduce it (the node's spire-agent serves from cache; measured: 0 prober delta).
Delete that node's spire-agent pod and then its aether-agent pod. For a fleet test, do not
use `kubectl rollout restart ds/aether-agent`: the DaemonSet is `maxUnavailable: 1`, so the
rollout stalls as soon as the first replaced agent goes NotReady — delete the agent pods
instead. Keep each window under 10 minutes (SVID TTL 4 h) and recover with
`kubectl -n spire-server scale statefulset spire-server --replicas=1`.

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
sum by (node) (increase(aether_agent_identity_outbound_binding_mismatch_total[1h]))
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
sum by (node) (increase(aether_agent_identity_inbound_binding_mismatch_total[1h]))
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
