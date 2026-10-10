# Observability: alerting rules

Every `.yml` in this directory is a Prometheus rule file, except `<name>_test.yml`,
which is a `promtool` unit test for `<name>.yml`. The build holds both to a pinned
`promtool`: `bazel test //:observability_rules_test` runs `promtool check rules` on
every rule file and `promtool test rules` on every rule test, and it is part of the
unit tests CI runs. A file added here is picked up with no other edit. To run
`promtool` by hand with the same version: `bazel run //bazel/promtool -- test rules
docs/observability/<name>_test.yml`.

## mesh-DNS (`mesh-dns-alerts.yml`)

Covers the `aether-mesh-dns` DaemonSet, which is on the critical path for **every**
managed pod's DNS (the CNI DNATs all UDP+TCP `:53` to `HOST_IP:18054`).

Since #578 the resolver runs in its own process and answers from a snapshot file the
node **agent** writes. That cross-process dependency is the reason most of these rules
exist — and why the external prober alone is not sufficient.

| Alert | Severity | Catches |
|---|---|---|
| `MeshDNSSnapshotStale` | critical | agent stopped writing → daemon serves frozen records |
| `MeshDNSNoRecords` | critical | empty snapshot → every mesh name NXDOMAINs |
| `MeshDNSNoUpstreams` | critical | no forward upstream → all non-mesh DNS fails |
| `MeshDNSWatcherInactive` | warning | fsnotify watcher died → updates silently stop |
| `MeshDNSReloadFailing` | warning | corrupt/unreadable snapshot |
| `MeshDNSForwardConnsRecyclingOnError` | info | pooled upstream sockets retired on exchange errors (#674) — resolution still works, it just costs an extra dial |
| `MeshDNSResolutionFailing` | critical | external prober can't resolve (per path) |
| `MeshDNSMetricsAbsent` | critical | daemons down fleet-wide, or the OTLP path is broken |

### Why staleness is the important one

If the agent dies, no fsnotify event ever fires, so the daemon keeps serving its **last
known table indefinitely**. Because `ready` is true, misses are answered as
*authoritative* NXDOMAINs, which clients negatively cache. New services never resolve;
re-IP'd services resolve to dead ClusterIPs.

**The external prober stays 100% green through all of it** — its long-lived target keeps
resolving from the stale snapshot. `MeshDNSSnapshotStale` is the only signal for a
failure that is silently *wrong* rather than loudly *broken*.

The agent re-stamps the snapshot every 60s (`capture.MeshDNSHeartbeat`) even when
records are unchanged, precisely so that age is meaningful on a quiet cluster.
`snapshot_generation` advances only on a real content change, so the two are
distinguishable.

### Probe targets cover different paths

The `mesh_dns` prober tier carries two targets, separable by the `target` label:

- `*.aether.internal` → answered authoritatively from the snapshot
- `*.svc.cluster.local` → **forwarded** to kube-dns

Both matter: the forward path carries the majority of a real workload's lookups but
almost no organic traffic here, so without the second target a kube-dns or
upstream-failover regression would be invisible.

## Node agent CNI conflist chaining (`agent-cni-alerts.yml`)

Covers the node agent's presence in the node's **active CNI conflist**. Aether is a
*chained* plugin inside another CNI's conflist, so any competing writer that rewrites
that file from its own template silently unchains us — on Talos, kube-flannel's init
container `cp -f`s its ConfigMap template over `10-flannel.conflist` on every flannel
pod recreation, which a bootstrap-manifest re-sync triggers. That was the 2026-08-29
fleet-wide ~2h outage (#645).

| Alert | Severity | Catches |
|---|---|---|
| `AetherCNIConflistUnchained` | critical | per node: entry gone and unrepairable, **or the agent reporting nothing at all** |
| `AetherCNIConflistReasserting` | warning | strips the agent self-healed — invisible to the gauge, visible only in the counter |
| `AetherCNIConflistMetricsAbsent` | critical | metric renamed, OTLP path broken, or the re-assert loop disabled fleet-wide |

### Why `absent()` is not the per-node answer

The gauge is **pushed by the agent**, so a down agent produces *no series* rather than a
`0`. `absent()` over the bare selector is **fleet-wide**: it drops the `node` label and
fires only when *every* agent is silent, so one node's agent going away produces neither
the `== 0` arm nor the `absent()` arm — vacuous for exactly the outage it would be
written for.

Per-node absence needs an inventory joined with `unless`. The inventory is
`aether_mesh_dns_ready`, from the **separate** mesh-dns DaemonSet (#583) carrying the
same `node` label — which is why it survives the agent outage the rule exists to catch.
Residual gap, stated plainly: a node where *both* the agent and mesh-dns are silent falls
out of the inventory and is covered only by the fleet-wide meta-rule.

### Why the counter is a separate rule

The re-assert loop repairs in ~2.5s and the OTLP `PeriodicReader` exports every 60s, so
a *healed* strip is exported as `1` and **never observed as `0`**. The gauge under-reports
by construction; `aether_agent_cni_conflist_reasserts_total` is the only evidence the
strip happened at all.

These expressions have **promtool unit tests** in the GitOps repo
(`clusters/talos-main/prometheus/rules_test.yaml`, run by CI), including a node whose
`chained` series is omitted entirely — the case `absent()` misses.

## Registrar snapshot divergence (`registrar-alerts.yml`)

Every registrar replica serves an endpoint snapshot listed from etcd at one store
revision, and the listing is a pure function of that revision. Two replicas at the same
`aether_registrar_snapshot_revision` must therefore report the same value of
`aether_registrar_snapshot_content_hash` (#1193, #1329).

| Alert | Severity | Catches |
|---|---|---|
| `AetherRegistrarSnapshotDiverged` | warning | two replicas at one revision serving different endpoint sets, with no write-behind intent pending |

### The hash is the gauge's value

`aether_registrar_snapshot_content_hash` is the first 13 hex digits (52 bits) of the
content hash in the snapshot version, as an integer; a float64 holds it exactly. One
series per replica and no label that changes, so a content change moves the value and
leaves nothing behind in Prometheus's lookback. The rule is then a plain count of
distinct values per `(job, revision)`: no `timestamp()` filter, and no dependence on the
two metrics' timestamps (the file's comments say how they arrive). 52 bits is a
divergence check between a few replicas (two different endpoint sets collide with
probability 2^-52), not an identifier.

### Deprecated: `aether_registrar_snapshot_content{content_hash}`

The hash used to be a label on an info gauge. A superseded hash stayed visible for the
5-minute lookback because OTLP has no staleness marker, and a count over the bare
selector read that as a divergence (#1322: 108 of 1,920 samples on a soak with none);
#1328 filtered on `timestamp()` to cope. The labelled metric is still exported for one
release and is removed in the next. While registrar replicas of both images can run,
keep the #1328 expression as a second `or` arm (`docs/runbook.md`, "Stored vs in place
vs applied", has the text and the order of work): a rule on the new metric alone cannot
see a replica on the older image.

Its promtool tests are in the GitOps repo: same revision and same hash (quiet), same
revision and different hash (fires after 3m), both replicas changing hash together
(quiet at every step), a replica one revision behind (quiet), a pending write-behind
intent (quiet), and replicas that export only the labelled metric (still caught by the
transitional arm).

etcd backend only: the kubernetes backend reports no revision, so the rule returns
nothing there. The gauge itself is reported on both backends.

## Node agent SAN-pin state (`agent-pin-alerts.yml`)

Whether the mesh clusters a node proxy dials check the server identity they are handed
(#832, #1424, #1425, #1481, #1482). `docs/runbook.md`, "The unpinned-cluster signal",
says what each `reason` means and what to do about it. Reported by the node agent (`job`
of the agent) and by the edge control plane.

| Alert | Severity | `for:` | Catches |
|---|---|---|---|
| `AetherMeshClusterUnpinned` | critical | 3m | per node and reason: clusters published **with TLS and without a server-identity pin** (`no_namespace_metadata`, `pin_not_rendered`). The mTLS validation gap |
| `AetherMeshClusterPinPending` | warning | 5m | per node and reason: an agent still publishing clusters with **no TLS at all** five minutes on (`trust_domain_unknown`, `tls_not_published`): it never learned its trust domain or never got its SVID |
| `AetherProxyHoldsUnpinnedClusters` | warning | 15m | per node: the proxy has accepted more unpinned clusters than the agent now publishes, so it has not accepted the update that pinned or removed them. A rejected cluster stays counted at the version the proxy last accepted through later ACKs of other clusters (#1508) |
| `AetherProxyPinStateUnknown` | warning | 15m | per node: the proxy holds mesh clusters whose pin state the agent cannot determine, so the acknowledged gauge is not written and the rule above cannot fire there (#1509). It does not say the proxy holds an unpinned cluster or that it rejects updates: a proxy rejecting a cluster update across an agent restart gets there, and so does one that accepted a version the agent no longer had on record |

| Metric (as Prometheus stores it) | Type | Labels | Meaning |
|---|---|---|---|
| `aether_agent_snapshot_tls_clusters` | gauge | `pin`, `reason` | mesh cluster entries in the agent's current snapshot that are meant to be mTLS: `pin="pinned"` (no `reason`), and `pin="unpinned"` once per `reason`. Written on every snapshot, zeros included |
| `aether_agent_xds_acked_tls_clusters` | gauge | `pin`, `reason` | the same count for the clusters the proxy has accepted, kept cluster by cluster: each is counted at the version the proxy last acknowledged or stated when it opened its stream (#1508). An entry with no cluster in the snapshot is in it only while the proxy still holds that cluster from an earlier snapshot (it rejected the removal); one whose cluster was never published, or whose removal the proxy accepted, is not. Absent until a proxy answers a cluster response of this agent process, which a reconnecting proxy normally does within a second or so of an agent restart (#1483); not written while the proxy holds a mesh cluster this agent process has no pin class for: a version it never published, or a cluster it no longer publishes and has no record of |
| `aether_agent_xds_acked_tls_clusters_unknown` | gauge | none | how many mesh clusters the proxy holds whose pin state the agent cannot determine (#1509). Zero while the gauge above is written, the number of such clusters while it is not written for that reason, and absent, like it, until a proxy answers a cluster response of this agent process |
| `aether_agent_identity_cluster_unpinned_total` | counter | `reason` | grows by the number of unpinned clusters on every snapshot that has any. Seeded at zero per reason. Before #1424 it had no `reason` label |
| `aether_agent_xds_nacks_total` | counter | `aether_xds_type_url` | delta-xDS responses the proxy rejected. Seeded at zero for each of the six resource types the agent serves, and `other` (#1480) |
| `aether_agent_xds_ack_wait_failures_total` | counter | `aether_xds_wait`, `aether_xds_reason` | ACK waits for a pod's listener that failed (`present`/`absent` by `nack`/`timeout`). Seeded at zero, four series (#1480) |

`reason` is a closed set of four: `trust_domain_unknown`, `tls_not_published`,
`no_namespace_metadata`, `pin_not_rendered`. No series carries a cluster name, so each
gauge is five series per agent and the counter four, whatever the size of the mesh; the
names are in the agent's WARN line.

### Severity follows what is on the wire

The four reasons are two kinds of fact, and the rules split on that:

- Under `no_namespace_metadata` (and `pin_not_rendered`, which no code path produces) the
  cluster is published with TLS and its handshake accepts any workload of the trust
  domain. That is an authentication gap being served: **critical**.
- Under `trust_domain_unknown` and `tls_not_published` the agent publishes **no TLS** for
  those clusters. Nothing is authenticated wrongly; a peer's mesh inbound refuses the
  connection. It is the normal state of an agent for the moment before it has its
  identity, and a fault only when it lasts: **warning**, after a longer `for:`.
  `tls_not_published` is also the reason of a TCP service with no namespace metadata
  whose floor cluster is not in the snapshot (the service is not captured; on the edge,
  no route references it). That one lasts with a healthy agent, and the warning is then
  about the registry data: the entry is the critical rule's the moment its floor is
  published.

`tls_not_published` exists since #1482. Before it, an entry whose endpoints carry no
namespace was `no_namespace_metadata` whether or not TLS was published, so a critical
rule on that reason would have fired for an agent that was merely waiting for its SVID.
An entry moves from `tls_not_published` to `no_namespace_metadata` in the snapshot that
first publishes TLS for it, so the critical rule's clock starts when the gap does. The
agent settles the reason against what each snapshot publishes, not only against its own
state: a snapshot able to publish TLS never leaves an entry under `tls_not_published`
unless that entry's floor cluster is absent from it.

### Why `for: 5m` rides out an agent start

- Measured on a five-node test cluster: all five running agents had their SVID within
  0.5 s of asking (`aether_agent_spire_wait_seconds`, every observation in the lowest
  bucket). The agent exports every 60 s, so a normal start is shorter than one export and
  usually in no gauge sample at all. The counter still records it.
- From the code: an agent still without an SVID after 2 minutes
  (`--spire-wait-warn-after`) reports NotReady. That is the agent's own bound on a
  healthy start.
- 5 minutes is that bound, two more exports so the state is seen twice past it, and one
  for evaluation jitter.

`AetherMeshClusterUnpinned` has no start-up window to ride out (the same entries are
`tls_not_published` until TLS exists), so its 3 minutes are only "three exports in a row
are a state, one is a snapshot".

A mesh run **without SPIRE** never has a node SVID: every entry with no namespace
metadata stays `tls_not_published`, by design. Drop `AetherMeshClusterPinPending` there.

### The divergence rule and an absent gauge

`aether_agent_xds_acked_tls_clusters` has no series until a proxy answers a cluster
response of that agent process: the ACK of one that added or removed a cluster, or the
answer to the first one of a stream. It is kept per cluster (#1508): an ACK moves the
clusters its response carried and no other, so a cluster the proxy rejected stays where
it was through later ACKs. Since #1483 an agent that restarts against a proxy
already in sync has its sample as soon as the proxy reconnects (the proxy states the
clusters it holds and acknowledges the agent's empty answer). The gauge is still absent
until a proxy has answered that agent process at all (a standby agent, a proxy that has
not connected since the agent started), and after a restart
for as long as the proxy rejects a cluster it holds an older version of (the new agent
process cannot count a version it never published, and writes nothing rather than a
count without it), or rejects the removal of a mesh cluster the agent no longer
publishes and has no record of: the runbook's "Published is not held" has them. Whenever
the gauge is absent because the agent cannot classify a cluster the proxy holds, those two
cases among them, `aether_agent_xds_acked_tls_clusters_unknown` is above zero, and
`AetherProxyPinStateUnknown` speaks for the node after 15 minutes (#1509). Once the gauge has
samples, a proxy stream that drops does not withdraw them: the last accepted state
stays exported until the proxy answers again, so a sample is not proof of a live xDS
connection.
`AetherProxyHoldsUnpinnedClusters` compares two vectors, and a comparison
returns nothing for a `(job, node)` that one side lacks, so that rule is silent there: it
cannot fire on absence, and it does not need `absent()` or `or vector(0)` (either
would make it fire there). It is one-directional on purpose: acknowledged *below*
published is an unpinned cluster not yet acknowledged, or an entry with no cluster in
the snapshot that the proxy does not hold (never published, or its removal accepted),
which the first rule already covers. Its residue is stated in the file: a late ACK from
the proxy generation that is leaving during a hot restart, for a cluster that does not
change afterwards.

Four more things to know. The figure in an annotation (`{{ $value }}`) is per `job` and
`node`, added up over every series stored for them: for two edge control-plane replicas
that share a node, in a pipeline that keeps them apart by `pod` (see "Labels the rules
need from your pipeline"), it counts a cluster once for each replica that reports it
(#1616). The gauges are pushed by the agent, so a down agent is no
series, not a zero (the same trap as the conflist gauge above): these rules are silent
for a node whose agent does not report, and `AetherCNIConflistUnchained` is the rule for
that. Anything that sums or compares the *counter* across an upgrade must not select on
`reason`: an agent from before #1424 exports the one label-less series, and one from
before #1482 has no `tls_not_published` series. And the rules have a promtool unit test
beside them, `agent-pin-alerts_test.yml` (an agent start, an agent stuck without its
SVID, the gap, an absent acknowledged gauge, an agent restart with the proxy in sync and
with the proxy rejecting, a proxy that keeps what the agent has pinned, also through
later ACKs of other clusters, a pin state the agent cannot determine, two edge replicas
on one node, a silent agent):

```bash
bazel test //:observability_rules_test
```

## Rejected xDS updates (`agent-xds-alerts.yml`)

The agent, and the edge control plane, count every delta-xDS response their proxy
rejects in `aether_agent_xds_nacks_total`, by resource type, seeded at zero (#1480). A
rejection is always a defect of the agent that built the response.

| Alert | Severity | `for:` | Catches |
|---|---|---|---|
| `AetherProxyRejectedListenerUpdate` | warning | 0m | per `job` and node: the proxy rejected **a Listener response** in the last hour. One rejection fires it |

Listener rejections have a rule of their own because of what the proxy does with one
(#1633): it applies the rest of the response, its removals and the listeners it could
build, and a listener kept that way is not in what the proxy states when it reconnects.
After an agent restart the agent does not know of it, and nothing removes it when its
pod goes. The agent does not hunt for such listeners; the rule makes the precondition
loud, and `docs/runbook.md`, "The proxy rejected a Listener update", has how to find the
listener and the recovery (fix the cause, then replace the proxy pod on that node).
Rejected Cluster responses are read by the pin rules above.

Three things to know, each a case in `agent-xds-alerts_test.yml`. The expression has two
arms: `increase(...[1h]) > 0`, and one for a series whose **first sample is already above
zero** (the proxy rejected the first response of an agent before the agent's first
export, on a node nothing reported from in the two hours before), where `increase()` is
0. The alert **ends an hour after the last rejection whatever was done**, because a
counter cannot tell that the proxy was replaced: it is an event, not a state. And it
misses one case and repeats in another: a restarted agent whose first export carries
exactly the count its predecessor ended on is one unbroken series and is not seen; and
the two agents of a surge roll, in a pipeline that keeps agents apart by `node` alone,
share one series for the overlap, so an old count next to the successor's zero reads as
an increase and the alert fires again with no new rejection.

## Labels the rules need from your pipeline

Every rule here selects on `node`, `job`, or both, except the two fleet-wide `absent()`
meta-rules (`MeshDNSMetricsAbsent`, `AetherCNIConflistMetricsAbsent`), which select on
neither. The components do not set those labels: they set the resource attributes `k8s.node.name` and `service.name`, and the
metrics pipeline has to turn `k8s.node.name` into a label called `node`. Where it does
not, `AetherCNIConflistUnchained` cannot fire for a single node and the per-node rules
fire without naming one. [`metric-labels.md`](./metric-labels.md) has the attributes
each component sets, what each rule file needs, what a missing promotion looks like, and
an example pipeline. Read it before installing the rules.

## Installing

There is **no Prometheus operator** on `talos-main` (no `PrometheusRule` CRD) and the
Grafana install has **no alerting sidecar** — only dashboard and datasource sidecars.
So these rules are delivered through the Prometheus helm values:

```yaml
# prometheus helm values
serverFiles:
  alerting_rules.yml:
    groups:
      # contents of mesh-dns-alerts.yml
      # contents of agent-cni-alerts.yml
      # contents of registrar-alerts.yml
      # contents of agent-pin-alerts.yml
      # contents of agent-xds-alerts.yml
```

`prometheus.yml`'s `rule_files` **already** lists `/etc/config/alerting_rules.yml` — the
wiring exists, the file was just empty — so populating that key is the only change needed
to make the rules evaluate.

Do **not** `helm upgrade` by hand: those values are reconciled by Flux (see below).

## Alert delivery (Alertmanager -> Slack + GitHub issue)

**This file is the source of truth for the rules; it is not where they are deployed
from.** talos-main is GitOps-managed by Flux, so nothing here is applied by hand — the
rules and the delivery path both live in
[`bpalermo/k8s-talos-main`](https://github.com/bpalermo/k8s-talos-main):

| what | where |
|---|---|
| alert rule groups | `clusters/talos-main/prometheus/values.yaml` → `serverFiles.alerting_rules.yml` |
| Alertmanager routing + `github-slack` receiver | same file → `alertmanager.config` |
| Slack webhook URL | SOPS Secret `alertmanager-slack` (ns `prometheus`), mounted as a **file** via `extraSecretMounts` → `global.slack_api_url_file` (Alertmanager cannot interpolate env vars, and `alertmanager.config` renders into a plaintext ConfigMap) |
| GitHub receiver Deployment | `clusters/talos-main/alertmanager-github-receiver/` |
| GitHub PAT | SOPS-encrypted `secret.sops.yaml` in that dir (AWS KMS + PGP) |

One receiver, two legs. **Slack (`#alerts`) is the pager**: GitHub issues created with
your own PAT are self-authored, and GitHub does not notify you about your own actions —
issues alone reach nobody. **The GitHub issue is the durable record**: a firing alert
opens an issue on this repo labelled `alert`, and closes it on resolve. Issues are keyed
on `GroupKey`, and `group_by: [alertname]` makes that stable — so one issue per
condition listing every firing node, and a flapping alert **reopens** its issue rather
than opening a new one.

Things that are easy to get wrong, already handled there:

- **There is no `Watchdog` rule, on purpose.** The stock always-firing dead-man's switch
  was removed together with its `→ "null"` route: nothing *outside* the cluster consumed
  the heartbeat, so it proved nothing and would file an immortal issue if it ever leaked
  to a real receiver. (If you re-introduce an always-firing rule, remove rule before
  route on teardown — the reverse ordering once briefly filed one.) A real dead-man's
  switch needs an external sink and is still an open item.
- **`measurementlab/alertmanager-github-receiver` cannot run on talos-main.** It is the
  receiver everyone cites, but it is published **amd64-only** (neither `latest` nor
  `v0.11` is a multi-arch index) while every talos-main node is **arm64**. We use
  `ghcr.io/pfnet-research/alertmanager-to-github`, which ships a genuine multi-arch
  index, pinned by digest.

Verify what is actually loaded:

```bash
kubectl get cm prometheus-server -n prometheus \
  -o go-template='{{index .data "alerting_rules.yml"}}' | head
```

Rules appear under **Alerts** in the Prometheus UI, and `ALERTS{alertstate="firing"}`
becomes queryable via the Grafana Prometheus datasource.

### Do the collector bump BEFORE enabling these

Prometheus here is **OTLP-receive-only** (all scrape configs disabled,
`web.enable-otlp-receiver`), so series arrive by push. Under memory pressure the
otel-collector **sheds gauge exports** -- during the 2026-07-25/26 soak one node
vanished from `aether_mesh_dns_records` and `snapshot_age_seconds` for a stretch while
its resolver was provably healthy.

Four of the seven rules key on exactly those gauges (`MeshDNSNoRecords`,
`MeshDNSNoUpstreams`, `MeshDNSWatcherInactive`, `MeshDNSMetricsAbsent`). With a shedding
collector a healthy node whose export was dropped is indistinguishable from a broken
one, and `absent()` fires on the telemetry gap rather than a resolver failure. Enabling
these before the collector has headroom trains everyone to ignore them within a week.

`MeshDNSSnapshotStale` and `MeshDNSResolutionFailing` are safer -- the latter is
counter-based, and counters self-heal across a refused export.

### Prove each rule fires before trusting it

Break something deliberately and confirm the expected rule -- and only that rule --
fires. `MeshDNSResolutionFailing` most of all: its exclusion of `http_error` and
inclusion of generic `timeout` is reasoned (a hung resolver surfaces as a context
deadline, while `http_error` is the cross-node backend path) but has never been
observed firing.
