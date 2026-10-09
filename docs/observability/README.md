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
| `AetherProxyHoldsUnpinnedClusters` | warning | 15m | per node: the proxy last acknowledged more unpinned clusters than the agent now publishes, so it has not taken the update that pinned them |

| Metric (as Prometheus stores it) | Type | Labels | Meaning |
|---|---|---|---|
| `aether_agent_snapshot_tls_clusters` | gauge | `pin`, `reason` | mesh cluster entries in the agent's current snapshot that are meant to be mTLS: `pin="pinned"` (no `reason`), and `pin="unpinned"` once per `reason`. Written on every snapshot, zeros included |
| `aether_agent_xds_acked_tls_clusters` | gauge | `pin`, `reason` | the same count for the last snapshot whose cluster update the proxy acknowledged. Absent until the first cluster ACK the agent process sees |
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

`aether_agent_xds_acked_tls_clusters` has no series until the first cluster ACK an agent
process sees, and an agent that restarts against a proxy already in sync is owed none
(#1483). `AetherProxyHoldsUnpinnedClusters` compares two vectors, and a comparison
returns nothing for a `(job, node)` that one side lacks, so that state is silent: the
rule cannot fire on absence, and it does not need `absent()` or `or vector(0)` (either
would make it fire there). It is one-directional on purpose: acknowledged *below*
published is an unpinned snapshot not yet acknowledged, which the first rule already
covers. Its residue is stated in the file: a late ACK from the proxy generation that is
leaving during a hot restart, on a node where no cluster changes afterwards.

Three more things to know. The gauges are pushed by the agent, so a down agent is no
series, not a zero (the same trap as the conflist gauge above): these rules are silent
for a node whose agent does not report, and `AetherCNIConflistUnchained` is the rule for
that. Anything that sums or compares the *counter* across an upgrade must not select on
`reason`: an agent from before #1424 exports the one label-less series, and one from
before #1482 has no `tls_not_published` series. And the rules have a promtool unit test
beside them, `agent-pin-alerts_test.yml` (an agent start, an agent stuck without its
SVID, the gap, an absent acknowledged gauge, a proxy that keeps what the agent has
pinned, a silent agent):

```bash
promtool test rules docs/observability/agent-pin-alerts_test.yml
```

This repository has no promtool in its build, so CI does not run it here.

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
