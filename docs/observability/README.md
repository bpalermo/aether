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

Then `helm upgrade` the Prometheus release. Verify with:

```bash
kubectl get cm prometheus-server -n prometheus \
  -o go-template='{{index .data "alerting_rules.yml"}}' | head
```

Rules appear under **Alerts** in the Prometheus UI once loaded.

> **Note — no notification delivery.** There is currently no Alertmanager deployed, so
> firing alerts are visible in the Prometheus UI but are **not routed anywhere**.
> Deploying Alertmanager (or moving these to Grafana-managed alert rules with a contact
> point) is required before they page anyone.

## Alert delivery (Alertmanager -> GitHub issue)

Rules that fire nowhere are decoration. These two values changes turn the rules above
into a tracked GitHub issue. Deploy `alertmanager-github-receiver.yaml` first (it
documents the fine-grained-PAT prerequisite), then patch the Prometheus helm values:

```yaml
# 1. Load the rules. NOTE: prometheus.yml's `rule_files` ALREADY lists
#    /etc/config/alerting_rules.yml -- the wiring exists, the file is just empty.
#    So this is the only change needed to make alerts evaluate.
serverFiles:
  alerting_rules.yml:
    groups:
      # contents of mesh-dns-alerts.yml
      - name: aether-mesh-dns
        rules: [...]

# 2. Enable Alertmanager. The chart wires prometheus.yml's `alerting.alertmanagers`
#    automatically when the subchart is enabled -- no prometheus.yml edit needed.
alertmanager:
  enabled: true
  config:
    route:
      receiver: github
      # Group by alertname ONLY, not [alertname, node]: the receiver titles issues
      # from .GroupLabels.alertname, so grouping by node would open one issue PER
      # NODE per alert. One issue per condition, listing every firing instance, is
      # the artifact you actually want to triage.
      group_by: [alertname]
      group_wait: 30s
      group_interval: 5m
      # Long, because the issue persists. A short repeat just re-comments on an
      # issue that is already open and already being read.
      repeat_interval: 12h
    receivers:
      - name: github
        webhook_configs:
          - url: http://alertmanager-github-receiver.o11y.svc.cluster.local:9393/v1/receiver
            send_resolved: true   # required for -enable-auto-close to ever fire
    inhibit_rules:
      # When the whole fleet stops reporting, the per-node gauge alerts are not
      # independent findings -- they are the same telemetry gap restated five times.
      # Suppress them so the page says "metrics absent", not a wall of noise.
      - source_matchers: [alertname = "MeshDNSMetricsAbsent"]
        target_matchers: [alertname =~ "MeshDNSNoRecords|MeshDNSNoUpstreams|MeshDNSWatcherInactive"]
        equal: []
```

Then `helm upgrade` the Prometheus release and verify:

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
