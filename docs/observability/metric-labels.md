# Metric labels: what the metrics pipeline has to make

The alert rules in this directory, and the queries in [`../runbook.md`](../runbook.md),
select on the labels `node` and `job`. Aether does not put those labels on its metrics.
It puts the facts on the OpenTelemetry **resource** each component exports with, and the
metrics pipeline between the components and the query engine turns resource attributes
into labels. This page says which attributes each component sets, which labels the
shipped rules need, and what you see when one is missing.

It covers the OTLP push path (chart value `otel.endpoint` in `charts/aether`,
`telemetry.otlpEndpoint` in `charts/prober`). If you scrape a component's
`--metrics-bind-address` endpoint instead, the labels are whatever your scrape
configuration attaches, and the same label names are needed.

## What you must do

| The rules need | Make it from | Who makes it |
|---|---|---|
| `node` | resource attribute `k8s.node.name` | **your pipeline**. Nothing makes it by default |
| `job` | resource attribute `service.name` | Prometheus's OTLP ingestion maps `service.name` onto `job` by itself. Another backend may need it done |
| one label that differs per replica of the registrar, the controller and the edge control plane, under any name | resource attribute `k8s.pod.name` | **your pipeline** |

The label has to be called `node`. Promoting `k8s.node.name` under its translated name
(`k8s_node_name`) keeps the series apart but the rules do not select on it: see
[What you see when a label is missing](#what-you-see-when-a-label-is-missing).

## What each component sets

Every Go component builds its resource from a fixed `service.name`, its build version,
and the `OTEL_RESOURCE_ATTRIBUTES` environment variable the chart sets from the downward
API. The two Envoy proxies build theirs from the environment variable alone.

| Component | `service.name` | From the chart's `OTEL_RESOURCE_ATTRIBUTES` |
|---|---|---|
| node agent (`aether_agent_*`) | `aether-agent` | `k8s.node.name`, `k8s.pod.name`, `k8s.namespace.name` |
| mesh-dns (`aether_mesh_dns_*`) | `aether-mesh-dns` | `k8s.node.name`, `k8s.pod.name`, `k8s.namespace.name` |
| proxy supervisor (`aether_supervisor_*`) | `aether-proxy-supervisor` | `k8s.node.name`, `k8s.namespace.name`, `k8s.pod.name`, `k8s.pod.uid` |
| node proxy, Envoy stats (`envoy_*`, `aether_requests_total`) | **not set** | `k8s.node.name`, `k8s.namespace.name`, `k8s.pod.name`, `k8s.pod.uid` |
| edge control plane (`agent edge`) | `aether-edge` | `k8s.node.name`, `k8s.namespace.name`, `k8s.pod.name`, `k8s.pod.uid`, `k8s.deployment.name` |
| edge proxy, Envoy stats | `aether-edge-proxy` | the same, plus `service.instance.id` (the pod name) |
| registrar (`aether_registrar_*`) | `aether-registrar` | `k8s.pod.name`, `k8s.namespace.name` |
| controller | `aether-controller` | `k8s.pod.name`, `k8s.namespace.name` |
| prober (`aether_probe_*`) | `aether-prober` | `k8s.node.name`, `k8s.namespace.name`, `k8s.pod.name` |

Where this comes from: the `OTEL_RESOURCE_ATTRIBUTES` entries in
`charts/aether/templates/` and `charts/prober/templates/daemonset.yaml`; the resource
builders in `common/telemetry/setup/setup.go` (agent, edge control plane, registrar,
controller), `agent/internal/meshdns/telemetry.go`,
`agent/internal/proxy/hotrestart/telemetry.go` and `prober/internal/prober/prober.go`;
and the `resource_detectors` entry of the Envoy stats sink in
`agent-proxy-configmap.yaml` and `edge-configmap.yaml`.

Five things follow from the table.

- **The registrar and the controller set no `k8s.node.name`.** They are Deployments, and
  the node is not what tells their replicas apart. Only `k8s.pod.name` does.
- **Only the edge proxy sets `service.instance.id`.** No other component does, so
  Prometheus's `instance` label does not separate two agents, two mesh-dns daemons or two
  registrar replicas. Without a promoted `node` (or pod) label, every instance of a
  component writes the same label set.
- **The edge control plane needs a pod label as well as `node`.** The chart runs two
  `agent edge` replicas by default and spreads them across nodes only softly
  (`edge.replicaCount`, `edge.nodeSpread`), so two can share a node. Both report the pin
  gauges, and with `job` and `node` alone two co-located replicas write the same series.
- **The node proxy's Envoy stats have no `service.name`,** so they arrive without a
  `job`. The edge proxy exports the same metric names with `job="aether-edge-proxy"`,
  which is how a query keeps the two apart (`{job!="aether-edge-proxy"}`).
- **Only the `hostNetwork` Go components set `host.name`.** The binary decides this, not
  the chart: the SDK's host detector reads the hostname, and a component keeps the
  attribute only where that hostname is a host's.

  | Component | `hostNetwork` | `host.name` |
  |---|---|---|
  | node agent | yes | the node's hostname |
  | mesh-dns | yes | the node's hostname |
  | proxy supervisor | yes | the node's hostname |
  | edge control plane (`agent edge`) | no | **not set** (#1596) |
  | registrar | no | **not set** (#1596) |
  | controller | no | **not set** (#1596) |
  | prober | no | **not set** (#1041) |

  On the pod network the hostname is the pod name, which `k8s.pod.name` already holds for
  all four, so leaving it out loses nothing. Until #1596 the registrar, the controller
  and the edge control plane did set it, to their pod name. A pipeline that selected or
  grouped their series by a label made from `host.name` uses `k8s.pod.name` instead.

  Do not derive `node` from `host.name` ahead of `k8s.node.name`. A node's hostname need
  not be its Kubernetes node name, and series from a release before #1596 (or from a
  prober before #1041) hold a pod name there, which joins with nothing
  ([`../configuration.md`](../configuration.md), the prober's "Metrics").

Labels that are **not** your pipeline's job: the prober's `pod`, `tier`, `target` and
`result`, and every `reason`, `pin`, `result` or `aether_*` label in the rules, are
datapoint attributes the component sets. They arrive as labels with no promotion. Metric
and label names are stored with dots turned into underscores, and a counter gains
`_total`.

## What the shipped rules select on

| Rule file | Labels it needs | Where |
|---|---|---|
| [`mesh-dns-alerts.yml`](./mesh-dns-alerts.yml) | `node` | every per-node rule aggregates `by (node)`. `MeshDNSResolutionFailing` aggregates the prober's counter `by (node, target)`. `MeshDNSMetricsAbsent` is `absent()` over the bare metric and needs no label |
| [`agent-cni-alerts.yml`](./agent-cni-alerts.yml) | `node`, **equal across two components** | `AetherCNIConflistUnchained` joins mesh-dns's `aether_mesh_dns_ready` against the agent's `aether_agent_cni_conflist_chained` on `node`. `AetherCNIConflistReasserting` sums `by (node)`. `AetherCNIConflistMetricsAbsent` is `absent()` and needs no label |
| [`agent-pin-alerts.yml`](./agent-pin-alerts.yml) | `job`, `node` | all three rules aggregate `by (job, node, …)` |
| [`registrar-alerts.yml`](./registrar-alerts.yml) | `job`, and one series per replica | `count by (job, revision)` over each replica's hash. The expression keeps every label (`without ()`), so the replica label may have any name |

The join in `agent-cni-alerts.yml` is why the agent and mesh-dns DaemonSets both stamp
`k8s.node.name` from `spec.nodeName`: the two `node` labels have to hold the same value.

This repository ships no recording rules and no dashboards. A grader or dashboard of
your own that groups per node or per component needs the same two labels; the queries in
[`../runbook.md`](../runbook.md) are written with them.

## What you see when a label is missing

The first three rows were run through `promtool test rules` against the rule files in
this directory, with two nodes or two replicas and one of them in the failing state.

| Pipeline does this | What happens |
|---|---|
| promotes `k8s.node.name` as `k8s_node_name`, not `node` | `AetherCNIConflistUnchained` **does not fire** for the unchained node. Both sides of its `unless` aggregate to one series with no `node`, and one healthy agent anywhere cancels the alert. It fires only when no agent in the fleet reports `1` |
| the same | `MeshDNSNoRecords` (and the other `by (node)` rules) still fire, as one fleet-wide alert with no `node` label. The summary reads "mesh-DNS is serving zero records on " and names no node |
| gives the registrar replicas no label that differs (no `k8s.pod.name` promotion) | `AetherRegistrarSnapshotDiverged` **can never fire**. The replicas write one series, the rule counts distinct hashes across replica series, and one series is never more than one hash |
| promotes nothing per node at all | every agent (and every mesh-dns, supervisor and node proxy) writes the same label set. The prober does not collapse, because it sets its own `pod` label; its series only lose their `node`. One series then has one writer per node: a gauge holds whichever node exported last, and `rate()` or `increase()` over a counter reads the interleaved cumulative values as resets. The chart's comments record both from before the attributes existed: fabricated resets and connect failures on the node proxy's `agent_xds` counters, and Prometheus rejecting whole batches with "duplicate sample for timestamp" for two edge proxies |
| leaves `job` out | `AetherRegistrarSnapshotDiverged` still works for one registrar Deployment (it was run with no `job`). The pin rules sum the node agent's series together with the edge control plane's on a node that runs both, and their text has an empty `{{ $labels.job }}` |

The last row but one is the reason `absent()` cannot stand in for a missing promotion: a
collapsed series is present. The value is wrong, and the series is there.

## Example pipeline

**An example, not a supported configuration.** It shows the shape of the two promotions
for an OpenTelemetry Collector that forwards to a Prometheus OTLP receiver. It was not
run as part of this repository's tests: check the processor and its statement syntax
against the Collector version you run, and the result against the checks in the next
section.

```yaml
# OpenTelemetry Collector (contrib): metrics pipeline
processors:
  transform/aether_labels:
    error_mode: ignore
    metric_statements:
      - context: datapoint
        statements:
          # node: from k8s.node.name only. Never from host.name.
          - set(attributes["node"], resource.attributes["k8s.node.name"])
            where resource.attributes["k8s.node.name"] != nil
          # A per-replica label for the Deployments, whose replicas nothing
          # else tells apart: the registrar and the controller (no node), and
          # the edge control plane (two replicas may share a node). The prober
          # already sets its own `pod`; the edge proxy has service.instance.id.
          - set(attributes["pod"], resource.attributes["k8s.pod.name"])
            where resource.attributes["k8s.pod.name"] != nil
            and (resource.attributes["service.name"] == "aether-registrar"
            or resource.attributes["service.name"] == "aether-controller"
            or resource.attributes["service.name"] == "aether-edge")

service:
  pipelines:
    metrics:
      receivers: [otlp]
      processors: [transform/aether_labels, batch]
      exporters: [otlphttp/prometheus]
```

Prometheus's own OTLP setting does the promotion without a Collector, under the
attribute's translated name:

```yaml
# prometheus.yml
otlp:
  promote_resource_attributes:
    - k8s.node.name   # stored as k8s_node_name
    - k8s.pod.name    # stored as k8s_pod_name
```

That keeps the series apart, which is the first half. The rules here say `node`, so with
this alone you also replace `node` with `k8s_node_name` in every rule file you install,
or you rename in a Collector as above.

**Cardinality.** A `pod` label starts a new set of series every time the pod is
replaced, which is why the example adds it only for the three Deployments and not for
the per-node DaemonSets, where `node` is stable across a roll. For the registrar, the
controller and the edge control plane that is a handful of replicas, and the series of
a replaced pod go stale.

A `node` label on the node proxy's per-cluster Envoy stats multiplies
those series by the number of nodes. The chart's comments
(`agent-proxy-daemonset.yaml`) describe a pipeline that promotes `node` only on the
proxy's two static clusters, `agent_xds` and `otel_collector`, which exist once per node
and collide without it, and leaves the per-service clusters collapsed fleet-wide. That
is a choice your pipeline makes. A per-node query over a per-service cluster stat needs
the label on those series too.

## Checking a pipeline

Each of these should return one result per node. A single result with no `node` label
means the promotion is missing.

```promql
count by (node) (aether_mesh_dns_ready)                # one per node, node = the Kubernetes node name
count by (node) (aether_agent_cni_conflist_chained)    # the same set of node values
count by (job, node) (aether_agent_snapshot_tls_clusters)
count by (node) (aether_probe_requests_total)          # node is a node name, not a prober pod name
```

The registrar check reads differently. This always returns one result, and its **value**
has to equal the number of registrar replicas. A value of `1` with more than one replica
running means the replicas write one series.

```promql
count(aether_registrar_snapshot_content_hash)
```

Then prove a rule fires, as [`README.md`](./README.md) says to: break one node and
confirm that the alert names that node.
