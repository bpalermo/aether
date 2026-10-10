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
| one label that differs per replica of the registrar, the controller and the edge control plane, under any name | resource attribute `service.instance.id` (the pod name; chart 2.5.3 and later) | Prometheus's OTLP ingestion maps `service.instance.id` onto `instance` by itself. Another backend may need it done. **Your pipeline must not drop it**: see below |

**Do not drop `instance` from the registrar's, the controller's and the edge's series.**
A Collector processor that removes `service.instance.id`, a relabelling rule that drops
`instance`, or a remote-write stage that aggregates it away puts every replica of those
back into one series, and `AetherRegistrarSnapshotDiverged` can then never fire
([What you see when a label is missing](#what-you-see-when-a-label-is-missing)). With a
chart before 2.5.3 the attribute is not set, and the per-replica label is your
pipeline's to make, from `k8s.pod.name`.

The label has to be called `node`. Promoting `k8s.node.name` under its translated name
(`k8s_node_name`) keeps the series apart but the rules do not select on it: see
[What you see when a label is missing](#what-you-see-when-a-label-is-missing).

## What each component sets

Every Go component builds its resource from a fixed `service.name`, its build version,
and the `OTEL_RESOURCE_ATTRIBUTES` environment variable the chart sets from the downward
API. The edge proxy builds its resource from the environment variable alone. The node
proxy builds its from the environment variable and then from its bootstrap, which names
the service (see below for why it is not in the variable).

| Component | `service.name` | From the chart's `OTEL_RESOURCE_ATTRIBUTES` |
|---|---|---|
| node agent (`aether_agent_*`) | `aether-agent` | `k8s.node.name`, `k8s.pod.name`, `k8s.namespace.name` |
| mesh-dns (`aether_mesh_dns_*`) | `aether-mesh-dns` | `k8s.node.name`, `k8s.pod.name`, `k8s.namespace.name` |
| proxy supervisor (`aether_supervisor_*`) | `aether-proxy-supervisor` | `k8s.node.name`, `k8s.namespace.name`, `k8s.pod.name`, `k8s.pod.uid` |
| node proxy, Envoy stats (`envoy_*`, `aether_requests_total`) | `aether-proxy` (chart 2.4.26 and later; none before) | `k8s.node.name`, `k8s.namespace.name`, `k8s.pod.name`, `k8s.pod.uid` |
| edge control plane (`agent edge`) | `aether-edge` | `service.instance.id` (the pod name; chart 2.5.3 and later), `k8s.node.name`, `k8s.namespace.name`, `k8s.pod.name`, `k8s.pod.uid`, `k8s.deployment.name` |
| edge proxy, Envoy stats | `aether-edge-proxy` | the same (`service.instance.id` in every chart version) |
| registrar (`aether_registrar_*`) | `aether-registrar` | `service.instance.id` (the pod name; chart 2.5.3 and later), `k8s.pod.name`, `k8s.namespace.name` |
| controller | `aether-controller` | `service.instance.id` (the pod name; chart 2.5.3 and later), `k8s.pod.name`, `k8s.namespace.name` |
| prober (`aether_probe_*`) | `aether-prober` | `k8s.node.name`, `k8s.namespace.name`, `k8s.pod.name` |

Where this comes from: the `OTEL_RESOURCE_ATTRIBUTES` entries in
`charts/aether/templates/` and `charts/prober/templates/daemonset.yaml`; the resource
builders in `common/telemetry/setup/setup.go` (agent, edge control plane, registrar,
controller), `agent/internal/meshdns/telemetry.go`,
`agent/internal/proxy/hotrestart/telemetry.go` and `prober/internal/prober/prober.go`;
and the `resource_detectors` entries of the Envoy stats sink in
`agent-proxy-configmap.yaml` and `edge-configmap.yaml`.

Five things follow from the table.

- **The registrar and the controller set no `k8s.node.name`.** They are Deployments, and
  the node is not what tells their replicas apart. Only the pod name does, which they
  set twice: as `service.instance.id` and as `k8s.pod.name`.
- **The Deployments set `service.instance.id`; the per-node DaemonSets do not.** Since
  chart 2.5.3 the registrar, the controller and both containers of the edge set it to
  the pod name (#1560; the edge proxy always did). Prometheus stores it as `instance`,
  so their replicas are separate series with no pipeline configuration. The node agent,
  mesh-dns, the proxy supervisor and the node proxy do not set it, on purpose: what
  tells two of those apart is the node, which keeps its value across a roll, where a
  pod name starts a new set of series per node at every roll. Without a promoted `node`
  label every instance of those writes the same label set. (mesh-dns also has a
  datapoint attribute of its own called `instance`, on its exit counter, which a
  resource-made label would collide with.) What a pod name in a label asks of a rule
  or a panel is under [A label that holds the pod name](#a-label-that-holds-the-pod-name).
- **The edge control plane's replicas are told apart by `instance`, not by `node`.** The
  chart runs two `agent edge` replicas by default and spreads them across nodes only
  softly (`edge.replicaCount`, `edge.nodeSpread`), so two can share a node. Both report
  the pin gauges, and with `job` and `node` alone (a chart before 2.5.3, or a pipeline
  that drops `instance`) two co-located replicas write the same series.
- **The node proxy's Envoy stats set `service.name=aether-proxy`** since chart 2.4.26,
  so they are `job="aether-proxy"` wherever `service.name` is mapped to `job` (the table
  under [What you must do](#what-you-must-do)). Before
  that they had no `service.name` and arrived without a `job`; the edge proxy exports the
  same metric names with `job="aether-edge-proxy"`, and a query could only keep the two
  apart by negation (`{job!="aether-edge-proxy"}`, which still selects the node proxy's
  series from both chart versions). The name is in the proxy's bootstrap
  (`aether-proxy-config`), not in the container's environment, because the supervisor
  runs in the same container and exports `aether_supervisor_*` from that environment as
  `aether-proxy-supervisor`. **Do not add `OTEL_SERVICE_NAME` to the proxy container**:
  it would rename the supervisor's metrics. A `service.name` in the container's
  `OTEL_RESOURCE_ATTRIBUTES` changes neither name.
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
| [`agent-pin-alerts.yml`](./agent-pin-alerts.yml) | `job`, `node` | all four rules aggregate `by (job, node, …)`, over the node agent's series and over each edge replica's (`instance`). Every selector reads samples of the last 150 s only |
| [`agent-xds-alerts.yml`](./agent-xds-alerts.yml) | `job`, `node` | `AetherProxyRejectedListenerUpdate` aggregates `by (job, node)` |
| [`registrar-alerts.yml`](./registrar-alerts.yml) | `job`, and one series per replica | `count by (job, revision)` over each replica's hash. The expression keeps every label (`without ()`), so the replica label may have any name: `instance` by default. Every selector reads samples of the last 150 s only |

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
| gives the registrar replicas no label that differs (drops `instance`; or, with a chart before 2.5.3, promotes no `k8s.pod.name`) | `AetherRegistrarSnapshotDiverged` **can never fire**. The replicas write one series, the rule counts distinct hashes across replica series, and one series is never more than one hash |
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
          # Nothing for the Deployments (registrar, controller, edge): since
          # chart 2.5.3 they set service.instance.id, which Prometheus stores
          # as `instance`. Leave that attribute on the resource. With an older
          # chart, make a per-replica label for those three here instead:
          #   - set(attributes["pod"], resource.attributes["k8s.pod.name"])
          #     where resource.attributes["service.name"] == "aether-registrar" ...
          # The prober sets its own `pod`.

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

**Cardinality.** A label that holds the pod name starts a new set of series every time
the pod is replaced, which is why the chart sets `service.instance.id` only on the three
Deployments and not on the per-node DaemonSets, where `node` is stable across a roll.
Do not add a `pod` label for the DaemonSets either. For the registrar, the controller
and the edge that is a handful of replicas, and the series of a replaced pod receive no
more samples.

A `node` label on the node proxy's per-cluster Envoy stats multiplies
those series by the number of nodes. The chart's comments
(`agent-proxy-daemonset.yaml`) describe a pipeline that promotes `node` only on the
proxy's two static clusters, `agent_xds` and `otel_collector`, which exist once per node
and collide without it, and leaves the per-service clusters collapsed fleet-wide. That
is a choice your pipeline makes. A per-node query over a per-service cluster stat needs
the label on those series too.

## A label that holds the pod name

`instance` on the registrar, the controller and the edge is the pod name (and so is a
`pod` label your pipeline makes from `k8s.pod.name`). It keeps replicas apart, and it
also makes every roll of those Deployments end one set of series and start another. A
pushed series has no staleness marker: nothing tells the store that the old pod is
gone, so **a bare selector returns its last sample for five minutes, beside the new
pod's**. Anything that aggregates over replicas, or compares them, reads a pod that no
longer exists for those five minutes.

The rule files in this directory therefore read fresh samples only. Each selector in
`agent-pin-alerts.yml` and `registrar-alerts.yml` is wrapped:

```promql
last_over_time(aether_agent_snapshot_tls_clusters{pin="unpinned"}[150s])
```

which is the newest sample of each series, if it is at most 150 seconds old. Why that
number: these components export every 60 seconds (the OpenTelemetry SDK's periodic
reader at its default interval; `common/telemetry/setup` sets none, and the chart sets
no `OTEL_METRIC_EXPORT_INTERVAL`). The window is above two intervals, so a live pod that
loses one export is still read until its next one lands, with 30 seconds for the
export's 10-second timeout and the pipeline's delay. It is below three, the shortest
`for:` of those rules, so one last sample of a pod that is gone is never an alert by
itself. If you export less often, or scrape these components at a longer interval,
raise the window to two and a half intervals and each `for:` with it.

What a roll does, run through `promtool test rules` with an old pod's series ending at
minute 9 and its replacement's starting at minute 10 (the cases are in
`agent-pin-alerts_test.yml` and `registrar-alerts_test.yml`, and each "bare selector"
entry is how the same case failed before the window):

| Rule | With a bare selector | With the 150 s window |
|---|---|---|
| `AetherMeshClusterUnpinned` (`for: 3m`), old pod's **last** export reported one unpinned cluster, the new pod none | **fired** at minutes 12 and 13 | does not fire |
| the same rule, firing on the old pod, the new pod reports none | still firing at minute 13 | ends at minute 12 |
| the same rule, firing, and the new pod reports the cluster too | fires throughout, figure 2 until minute 13 | fires throughout, figure 2 at minutes 10 and 11, then 1 |
| `AetherMeshClusterPinPending` (`for: 5m`), firing on the old pod | still firing at minute 13 | ends at minute 12 |
| `AetherProxyHoldsUnpinnedClusters`, `AetherProxyPinStateUnknown` (`for: 15m`), firing on the old pod, last sample at minute 25 | still firing at minute 28 | end at minute 28 |
| `AetherRegistrarSnapshotDiverged` (`for: 3m`), an old registrar's last export fell between an agent RPC and its sync (a hash that is not its revision's) | **fired** at minutes 12 and 13 against the other replicas' hash | does not fire |
| the same rule, the two **live** replicas diverge from minute 10 and a replaced registrar last reported a write-behind depth of 3 | quiet until minute 13, fires at minute 17 | quiet at minutes 10 and 11, fires at minute 15 |
| the same rule, firing, and the odd replica is replaced by one that agrees | still firing at minute 13 | ends at minute 12 |

Two things the window does not remove, both in the test files as a `limit` case or in
the row above. A pod whose last **two** exports reported an unpinned cluster is read for
three and a half minutes, so `AetherMeshClusterUnpinned` can fire for one evaluation
after the pod is gone. And a replaced registrar's last write-behind depth still delays
a real divergence, by the window.

`AetherProxyRejectedListenerUpdate` (`agent-xds-alerts.yml`) has no window: it reports
an event of the last hour from a counter, `increase()` over a series that has ended
still counts what it counted, and a window would only end a true alert sooner. The
rules in `mesh-dns-alerts.yml` and `agent-cni-alerts.yml` read per-node DaemonSets,
whose series carry no pod name and keep their identity across a roll.

**Your own rules and panels** over the registrar's, the controller's or the edge's
series need the same care:

- an aggregation over a gauge (`sum`, `count`, `max`, a comparison between replicas):
  wrap the selector in `last_over_time(...[150s])`, or it counts replaced pods for five
  minutes after every roll;
- a panel that does not aggregate shows one line per pod, and after a roll the old
  pod's line ends and a new one starts. Aggregate away `instance`
  (`max without (instance) (...)`, `sum without (instance) (...)`) for a line per
  component;
- `rate()` and `increase()` are computed per series, which is right for a counter per
  pod; sum them after, not before.

The node proxy's `job` label is the same effect, once: see the runbook's "Chart 2.4.26".
The upgrade that crosses chart 2.5.3 is the runbook's "Chart 2.5.3".

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

As written it counts a replaced pod for five minutes after a registrar roll. To count
the replicas that report now, and to see their names:

```promql
count(last_over_time(aether_registrar_snapshot_content_hash[150s]))
count by (instance) (last_over_time(aether_registrar_snapshot_content_hash[150s]))
```

The second has to return one row per registrar pod, named after it. One row with no
`instance` means the label is dropped on the way (or the chart is older than 2.5.3). The
same check for the edge control plane, where two replicas may share a node:

```promql
count by (node, instance) (last_over_time(aether_agent_snapshot_tls_clusters{job="aether-edge", pin="pinned"}[150s]))
```

The node proxy and the edge proxy export the same Envoy metric names. This returns one
result per proxy kind: `aether-proxy`, and `aether-edge-proxy` when the edge is on. A
result with no `job` is a node proxy on a chart older than 2.4.26, one whose Envoy has
not yet restarted on the new bootstrap, or, for five minutes after that restart, the
last sample of its old series.

```promql
count by (job) (envoy_server_live)
```

Then prove a rule fires, as [`README.md`](./README.md) says to: break one node and
confirm that the alert names that node.
