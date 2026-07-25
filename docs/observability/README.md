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
