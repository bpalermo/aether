# Observability: alerting rules

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
```

`prometheus.yml`'s `rule_files` **already** lists `/etc/config/alerting_rules.yml` — the
wiring exists, the file was just empty — so populating that key is the only change needed
to make the rules evaluate.

Do **not** `helm upgrade` by hand: those values are reconciled by Flux (see below).

## Alert delivery (Alertmanager -> GitHub issue)

**This file is the source of truth for the rules; it is not where they are deployed
from.** talos-main is GitOps-managed by Flux, so nothing here is applied by hand — the
rules and the delivery path both live in
[`bpalermo/k8s-talos-main`](https://github.com/bpalermo/k8s-talos-main):

| what | where |
|---|---|
| rule group `aether-mesh-dns` | `clusters/talos-main/prometheus/values.yaml` → `serverFiles.alerting_rules.yml` |
| Alertmanager routing + receiver | same file → `alertmanager.config` |
| receiver Deployment | `clusters/talos-main/alertmanager-github-receiver/` |
| GitHub PAT | SOPS-encrypted `secret.sops.yaml` in that dir (AWS KMS + PGP) |

A firing alert opens an issue on this repo labelled `alert`, and closes it on resolve.
Issues are keyed on `GroupKey`, and `group_by: [alertname]` makes that stable — so one
issue per condition listing every firing node, and a flapping alert **reopens** its issue
rather than opening a new one.

Two things that are easy to get wrong, both already handled there:

- **`Watchdog` must never reach the GitHub receiver.** It is a dead-man's switch
  (`expr: vector(1)`) that fires permanently *by design*. Since `github` is now the
  default receiver, its `→ "null"` route is load-bearing — without it you get one
  immortal issue that can never be closed, and you learn to ignore the label.
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
