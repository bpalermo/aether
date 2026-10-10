# Collector-pressure harness (issue #662)

Deliberately drives a cluster's shared OpenTelemetry collector into `memory_limiter` shedding, restarts
**one** node's `aether-agent` under that pressure, and asserts the agent still starts,
attests to SPIRE and serves. It is the on-demand test for the branch #668 fixed.

**Never run this during a soak.** See [Risk](#risk-and-blast-radius). The script cannot
detect a soak, so a real run needs the operator to say there is none
(`--no-soak-running`); see [The soak guard](#the-soak-guard-what-it-can-and-cannot-know).

```bash
# Three inputs have no default: each is your cluster's own (see "Inputs").
export EXPECT_CONTEXT=<kube-context>             # the context this run is meant for
export COLLECTOR_NS=<collector-namespace>        # where the collector Deployment runs
export PROM_NS=<prometheus-namespace>            # where the Prometheus Service runs

# Resolve GOMEMLIMIT, the pod limits, the abort thresholds and the metrics source,
# print them, and exit. Applies no Job and touches no agent — safe any time.
bash e2e/pressure/run.sh --node <node> --dry-run

# The real run. --no-soak-running is your statement, not something the script checked.
bash e2e/pressure/run.sh --node <node> --no-soak-running
```

## Inputs

`run.sh` describes no particular cluster. What it cannot know has no default, and a run that
lacks one aborts before it asks the cluster anything, naming each missing variable:

| Variable | What it is | Default |
|---|---|---|
| `EXPECT_CONTEXT` | The kube context the run is meant for. The run aborts when `kubectl config current-context` is another one (a `kind` cluster made in the meantime takes the current context). | none, required |
| `COLLECTOR_NS` | Namespace of the collector Deployment. | none, required |
| `PROM_NS` | Namespace of the Prometheus Service. | none, required |
| `COLLECTOR_DEPLOY` | Name of the collector Deployment. Its `.spec.selector` finds the replicas, and its name the collector's own series in Prometheus (`COLLECTOR_SEL`, default `instance=~"<COLLECTOR_DEPLOY>-.*"`). | `otel-collector` |
| `COLLECTOR_CONTAINER`, `COLLECTOR_METRICS_PORT` | The collector's container and its self-telemetry port. | `opentelemetry-collector`, `8888` |
| `COLLECTOR_OTLP_ENDPOINT` | The OTLP gRPC endpoint the Job floods. | `<COLLECTOR_DEPLOY>.<COLLECTOR_NS>.svc.cluster.local:4317` |
| `PROM_SVC` | Name of the Prometheus Service (port 80). | `prometheus-server` |
| `AGENT_NS`, `AGENT_SELECTOR`, `AGENT_CONTAINER` | The node agent, as the `aether` chart installs it. | `aether-system`, `app.kubernetes.io/name=aether-agent`, `agent` |
| `JOB_NS`, `JOB_NAME`, `--job-manifest` | The pressure Job. The shipped manifest takes its namespace and name from the two variables. A manifest of your own must have the one shape `run.sh` accepts (an allow-list, see below); anything else is refused before anything is applied. | `aether-test`, `aether-collector-pressure`, `collector-pressure-job.yaml` |
| `MIN_POLL_OK_PCT` | The share of a wait loop's polls that must have got an answer before its deadline may be read as a FAIL. A whole number from 0 to 100. | `50` |
| `SOAK_POD_SELECTOR` | See [The soak guard](#the-soak-guard-what-it-can-and-cannot-know). | unset |

**The shape of a `--job-manifest`.** The Job that is applied must be the Job that is watched
and deleted, and `kubectl apply` creates the objects of a stream in order, so `run.sh` accepts
one shape and refuses everything else (`manifest_defines_job`):

- one YAML document in block style: no second document behind `---` or `...`, no JSON, no
  flow-style `{...}` at the top, no tags, directives or anchors there, no tab, carriage return
  or byte-order mark;
- top-level keys exactly `apiVersion: batch/v1`, `kind: Job`, `metadata:` and `spec:`, each
  once, in any order;
- under `metadata:`, at two spaces, the lines `  name: <JOB_NAME>` and `  namespace: <JOB_NS>`
  (unquoted), once each, no `generateName`; labels and annotations are free;
- under `spec:`, one `  activeDeadlineSeconds: <positive integer>`: it is the stop that works
  when a delete cannot be confirmed.

It reads lines and does not parse YAML: `kubectl --dry-run=client` asks the API server
(measured with kubectl 1.35), and the script requires no YAML tool. A valid Job written
another way is refused, which is the safe side.

The measured numbers further down (a 2Gi collector, two replicas, five nodes) are those of the
cluster the runs of 2026-09-05 were made on. They are a worked example, not a requirement:
`run.sh` derives every threshold from the live pod.

## Why a soak cannot test this

#662 was a data-plane outage caused by an observability component: with the collector
refusing exports (`data refused due to high memory usage`), the agent's startup context
was cancelled, SPIRE source creation failed, and 3 of 5 agents crash-looped — one of them
at 5 restarts and still `CrashLoopBackOff`, i.e. a node with no xDS control plane.

#668 decoupled startup from export. Since then the fix has been **"not disproven", never
exercised**. Measured over the 09-03 and 09-04 soaks:
`max_over_time(otelcol_process_memory_rss_bytes[2d])` = **245 MiB, 21.7% of the 1126 MiB
soft limit**, and `increase(otelcol_receiver_refused_metric_points_total[2d])` = **0**. The
soak's job is to hold the mesh under *load and churn*, and it succeeds — but its load is
mesh traffic, not telemetry volume, and the collector was resized (to 2Gi) after #662
precisely so it would never saturate again. A soak therefore cannot reach the branch:

| | soak | this harness |
|---|---|---|
| collector RSS | ~245 MiB flat (21.7% of the 1126 MiB soft limit) | driven past 1126 MiB on purpose |
| `otelcol_receiver_refused_*` | 0 | > 0, verified before the agent is touched |
| agent restart | rolling, DaemonSet-wide, collector healthy | one pod, one node, collector shedding |

Waiting for the next organic saturation is not a test strategy — the previous one cost a
soak and an unplanned outage.

## The two options, and why this implements A

### A — flood the shared collector (implemented)

An OTLP load generator (`telemetrygen`, a `Job` in `aether-test`) pushes synthetic data at
the collector's OTLP endpoint (`COLLECTOR_OTLP_ENDPOINT`, by default
`<COLLECTOR_DEPLOY>.<COLLECTOR_NS>.svc.cluster.local:4317`) until the process crosses `memory_limiter`'s
soft limit and starts refusing. Every agent, proxy, prober and controller in the cluster
exports to that collector, so the shed hits the real agent's real export path — the exact
#662 condition, with no code, image or chart change anywhere in the mesh.

The cost is honest: the SLI goes blind while the collector is shedding (prober counters
are exported through it). That is acceptable *outside* a soak and is the reason the
pre-flight refuses to run until the operator has said that no soak is up.

### B — a dedicated throwaway collector + one node pointed at it (rejected)

Deploy a second collector with a small memory limit into a scratch namespace and point one
node's agent at it. It sounds safer — nothing shared is touched — but **there is no
node-scoped mechanism to point one agent at a different endpoint**:

- `--otlp-endpoint` (chart value `telemetry.otlpEndpoint`) is a DaemonSet-wide flag. Changing
  it is a `helm upgrade` that rolls **every** agent, and leaves the whole fleet exporting
  to a deliberately undersized collector for the duration.
- `kubectl set env` / `kubectl patch` on the DaemonSet is equally fleet-wide. Patching the
  *pod* is rejected — container env and args are immutable on a running pod, and a DaemonSet
  pod recreated by hand is reconciled back.
- A node-local override (per-node ConfigMap, downward-API-selected endpoint) is a **product
  change to ship a test**, and it would add a config surface whose only consumer is this
  harness.

B also tests less: it proves an agent survives *a* shedding collector, not that it survives
*the* shedding collector every other component is queued behind. Rejected — unless the chart
ever grows a legitimate per-node telemetry override, in which case B becomes strictly safer
and this harness should switch.

## How the pressure is made

The generator floods the **logs** pipeline, not the metrics pipeline. `memory_limiter` is a
single process-wide component shared by every pipeline in the collector's config, so pressure
applied anywhere makes *every* pipeline refuse — including the metrics pipeline the agents
export to. Flooding logs therefore reproduces #662's trigger while keeping the flood itself
out of Prometheus, which is the plane `run.sh` measures from. Contaminating the measurement
plane with the pressure signal would be self-defeating; the garbage lands in the logs backend
under `service.name=aether-collector-pressure` instead (NUL-filled payloads, so it compresses
to nearly nothing).

Sizing, for a collector deployed with a 2Gi memory limit, two replicas and this
`memory_limiter` (`check_interval: 5s`, `limit_percentage: 80`, `spike_limit_percentage: 25`;
redo the arithmetic for another one):

| | |
|---|---|
| hard limit (refuse + forced GC) | 80% of 2Gi = **1638 MiB** |
| soft limit (shedding begins) | (80−25)% of 2Gi = **1126 MiB** |
| spike budget per 5s check | 512 MiB ⇒ ~**102 MiB/s per replica** is the OOM-risk frontier |
| this harness | 3 pods × 16 workers × 3 records/s × 1 MiB ≈ **144 MiB/s**, ~72 MiB/s per replica |

About 70% of the frontier (2 pods × rate 2, a third of it, never reached pressure in 300s — twice on 2026-09-05), so the collector *sheds* rather than OOM-kills — shedding is
`memory_limiter` working correctly, and 09-02 showed it sheds under real saturation too.

## Measured behaviour (five runs, 2026-09-05) — do not re-derive these

`memory_limiter` triggers on the **Go heap**, not on RSS. What the five runs actually
measured, and what the thresholds in `run.sh` are now built from (#699):

| | observed |
|---|---|
| heap at refusal onset | **1,077–1,486 MiB** (≈ the 1,126 MiB soft limit, as designed; the 1,486 run was 90% of `GOMEMLIMIT`, 4% under the abort ceiling) |
| RSS at that same instant | **1,250–1,766 MiB** — GC slack runs RSS **1.15–1.4×** ahead of heap |
| peak RSS, limiter engaged | **≤ 1,766 MiB** on the 2 Gi pod — the limiter caps it, no OOM |
| collector restarts | **0**, all five runs |
| time from Job apply to shedding | 46 s – 4 min |

The consequence, and the reason runs 2 and 3 died on a false abort: a **fixed RSS ceiling
of 1,500 MiB sits inside the 1,250–1,650 MiB band in which shedding is already engaged**.
Both runs aborted *after* the collector had started refusing but before the poll saw it.
Only run 4, with the ceiling lifted to 1,850 MiB, observed shedding and proceeded.

So `run.sh` aborts on **heap vs `GOMEMLIMIT`** — the point past which the Go runtime, not
the limiter, is what is at risk — and keeps RSS only as a cgroup-OOM backstop. Both are
derived from the live pod at run time, never hard-coded; `--dry-run` prints them:

| ceiling | rule | on that 2Gi collector |
|---|---|---|
| heap (primary) | 95% of `GOMEMLIMIT`, read from the pod env | **1,556 MiB** (of `GOMEMLIMIT=1638MiB`) |
| RSS (backstop) | 90% of `resources.limits.memory` | **1,843 MiB** (of 2 Gi) |

If `GOMEMLIMIT` is absent or derived (`valueFrom: resourceFieldRef`), `run.sh` falls back
to the container memory limit and says so.

### Where the numbers are read from

`run.sh` prefers the collector's **own** `:8888/metrics`, one `kubectl port-forward` per
replica: Prometheus lags, and that lag is why runs 2 and 3 missed the onset — the
collector *pushes* its self-telemetry OTLP every 30 s, so `otelcol_*` in Prometheus is
30–60 s stale, while shedding starts and the abort fires within a single 15 s poll.

**A collector that only pushes its self-telemetry does not serve `:8888`.** When its
`service.telemetry.metrics` has only a `periodic` OTLP reader and no pull reader, nothing
listens on 8888, and `run.sh` logs a warning with the reason (no port-forward, a fetch that
failed, or an answer without the heap or the RSS gauge the safety ceilings read) and falls back to Prometheus (poll 15 s
instead of 5 s). To get the lag-free path, add a pull reader to the collector's config:

```yaml
config:
  service:
    telemetry:
      metrics:
        readers:
          - pull:
              exporter:
                prometheus:
                  host: 0.0.0.0
                  port: 8888
```

`--metrics-source collector|prometheus|auto` (default `auto`) forces the choice; forcing
`collector` while 8888 is closed is a clean INCONCLUSIVE rather than a silent fallback.

Metric names are read tolerantly, with and without `_total`, across
`otelcol_processor_memory_limiter_refused_*` (what collector 0.159.0 emits),
`otelcol_processor_refused_*` and `otelcol_receiver_refused_*`. heap and RSS are taken as
the **max** across replicas (each is a per-process limit); the refused counters as the
**sum** (any replica shedding is shedding).

Three flags in the Job are load-bearing and must not be dropped:

- `--allow-export-failures` — without it a worker calls `Fatal()` on the first refused export,
  i.e. the generator kills itself at the instant shedding begins.
- `--timeout=5s` — the OTLP client retries `Unavailable` (what a shedding collector returns)
  for up to a minute by default; without a short timeout the flood collapses to ~1 record per
  worker per minute exactly when the pressure has to hold.
- `--batch=false` — the default 100 records/request against `--size=1` would build ~100 MiB
  requests, which the receiver rejects at the gRPC layer (`max_recv_msg_size_mib: 64`) *before*
  `memory_limiter` ever sees them: a refusal that proves nothing.

## Pre-flight (all enforced by `run.sh`, all fatal)

0. `EXPECT_CONTEXT`, `COLLECTOR_NS` and `PROM_NS` are set ([Inputs](#inputs)).
1. The current `kubectl` context is `EXPECT_CONTEXT` (making a `kind` cluster silently takes
   the current context), and the node exists.
2. **The soak guard**: the operator's acknowledgement (`--no-soak-running` or
   `NO_SOAK_RUNNING=1`; a `--dry-run` only warns without it), no pod carrying
   `SOAK_POD_SELECTOR` when that is set, and no DaemonSet of `aether-system` mid-roll.
   Shedding the collector mid-soak leaves a hole in the prober's cumulative counters and
   makes the run ungradeable. This guard does not detect a soak: see
   [the next section](#the-soak-guard-what-it-can-and-cannot-know).
3. The collector Deployment is at full readiness (every replica Ready). Do not pressure an
   already-degraded telemetry plane.
4. The target node's agent pod is Ready with `restartCount: 0`.
5. The external prober is reporting successes — the availability signal must be alive going in.
6. `aether_agent_storage_pods{node="<node>"}` has samples — the agent's export works going in.
7. Baseline collector RSS is **under 35% of the soft limit** (≈394 MiB of a 2Gi pod). Idle was ~250 MiB
   (22%), which is why the gate is 35% and not the 20% one might reach for: a 20% gate can
   never pass at rest. Anything above 35% means the collector is already loaded and the run
   would not be attributable.

Not fatal, but printed loudly: the metrics source it settled on, and — when it fell back to
Prometheus — that shedding onset will be seen 30–60 s late. Run `--dry-run` first and read
the resolved plan; it is the cheapest way to catch a resized collector or a stolen context.

Also confirm by eye that nothing else important is mid-flight (a release, a conformance run,
a profiling pass) — for the shedding window, cluster telemetry is unreliable by design.

### The soak guard: what it can and cannot know

A soak is run by an external soak harness, maintained outside this repository. What such a
harness may read from a mesh is written down in
[`test/harnesscontract/external-harness.yaml`](../../test/harnesscontract/external-harness.yaml),
and that contract names **no identity for a load driver**: the namespace, the names and the
labels of a harness's workloads are its own (the file's `not_contract` section says so). So
nothing in this repository can tell that a soak is running, and `run.sh` does not claim to.
Before #1555 it looked for a DaemonSet and a local process by names a soak no longer uses,
and passed while one ran.

| Part | What it is | What it proves |
|---|---|---|
| `--no-soak-running` / `NO_SOAK_RUNNING=1` | The operator's acknowledgement. A real run aborts without it, before it asks the cluster anything. | Nothing the script verified. It is your statement that no soak, release validation or other graded run is using the cluster. |
| `SOAK_POD_SELECTOR=<label selector>` | Optional. When set, the run is refused while any pod in any namespace carries the selector. | Only as much as the selector: set it to the label your harness puts on its pods. Unset (the default), no pod is looked for, and the script prints that. There is no default because any default would be a guess. |
| The roll check | The run is refused while a DaemonSet of `aether-system` is mid-roll (spec not yet observed, a pod not updated, or a pod unavailable). | That no roll is in progress **at that instant**. A soak rolls these DaemonSets, and so does an upgrade. A soak between two rolls passes it. |

The two checks can only add a refusal, and each fails closed: a list that could not be read
aborts the run. Neither looks at the processes of the machine the script runs on, because a
harness need not run there (and a `pgrep -f <pattern>` matches the command line of whatever
waits on it). `//e2e/pressure:preflight_test` holds the guard to all of this with a fake
`kubectl`.

### A question that could not be asked

Every `kubectl` and `curl` call either gets an answer or ends the run with **exit 2** and the
tool's own error on the terminal (#1578). A call that failed is never read as an absent object,
and never as a verdict: exit 1 says "#662 is back", and an API server that timed out has not
said that. Where absence is a legitimate answer it is read from a call that **succeeded**
(`--ignore-not-found`, or a list or a jsonpath that came back empty).

| Call | Absent (the call succeeded) | The call failed |
|---|---|---|
| `kubectl config current-context` | n/a | abort |
| `get node <node>` | abort: `node <node> not found` | abort: could not ask |
| `get deploy` of the collector (readiness, selector) | kubectl's `NotFound`: abort | abort |
| `get pods -l <SOAK_POD_SELECTOR>` (when set) | pass: no pod carries it | abort (#1565) |
| `get ds` of `AGENT_NS` | pass: none mid-roll | abort (#1565) |
| `get job` (a Job left by an earlier run) | pass: no Job | abort: could not ask |
| `get pods` of the agent on the node, pre-flight and before the restart | abort: no agent pod. Two pods (a roll's standby, or one still terminating) abort too: the script does not guess which to restart | abort: could not list |
| the agent pod's `ready` and `restartCount`, pre-flight | n/a | abort |
| `get pods` of the collector | abort: none Running | abort: could not list |
| the collector pod's `GOMEMLIMIT` | fall back to the memory limit, said | abort |
| the collector pod's memory limit | abort: no limit, the ceilings cannot be sized | abort |
| `port-forward` to Prometheus | n/a | abort, with the port-forward's output |
| `port-forward` to a collector replica, and the first `curl` of its `/metrics` | `auto`: fall back to Prometheus, with the reason; `collector`: abort | the same: the fallback is the design, and the reason is printed |
| `curl` of a replica's `/metrics`, every poll | abort when the heap or the RSS gauge is not in the answer: a gauge that is not there is not a zero, and its ceiling could not fire | abort |
| a Prometheus query for a required number (prober rate, collector heap, RSS, refused counters) | abort: `no data for …` | abort: `could not query Prometheus for …` (transport error, an error status, or not Prometheus's JSON) |
| the age of the agent's series, pre-flight | abort: the series has no samples | abort: could not query |
| `apply` of the Job | n/a | abort, saying the apply could not be confirmed: the Job may have been created, and the cleanup trap tries to delete it |
| `delete job` at a safety ceiling (the message also says whether the agent pod had already been deleted) | fine (`--ignore-not-found`) | abort, saying the deletion could **not be confirmed** (the API server may have acted before the answer was lost); the cleanup trap tries again |
| `delete pod` of the agent | abort (the pod is gone: nothing was proven) | abort: whether the agent was restarted is not known |
| `get pods` of the agent, waiting for the replacement | keep waiting; **FAIL** at the deadline | ask again; abort at the deadline if the last call failed, or if fewer than `MIN_POLL_OK_PCT` percent of the polls got an answer |
| the replacement's status, waiting for Ready | keep waiting; **FAIL** at the deadline | ask again; abort at the deadline on the same two conditions |
| the replacement's `restartCount` and terminated state, and its log | part of the verdict | abort: unread evidence is not a verdict |
| `delete job`, releasing the pressure | fine | abort (the recovery was not measured); the cleanup trap tries again |
| the age of the agent's series, waiting for it to go fresh | keep waiting; **FAIL** at the deadline | ask again; abort at the deadline on the same two conditions |
| `delete job` in the cleanup trap | fine | a `WARN` that the Job may still be running; the run's exit status stands |

`//e2e/pressure:preflight_test` has a case for the failed call of each row, and one for the
absence where there is one.

**One cluster for the whole run.** The current context is checked against `EXPECT_CONTEXT`
once, and it can change under a run that lasts minutes. Every later call names
`--context "$EXPECT_CONTEXT"`, the cleanup trap's delete included, so no apply or delete can
land on another cluster.

## Procedure

`run.sh` does all of it, in order, and prints every number it reads:

1. Resolves the collector's `GOMEMLIMIT` and memory limit off the live pod, derives the
   abort ceilings from them, picks the metrics source, port-forwards the Prometheus Service
   (so that it needs neither a route to Prometheus from where it runs nor an exec into its
   pod) for the prober and agent-freshness signals, and records the baselines:
   `otelcol_process_runtime_heap_alloc_bytes`, `otelcol_process_memory_rss_bytes` and the
   refused metric-point / log-record counters.
2. Applies the pressure `Job`.
3. Polls every 5s (collector source) or 15s (Prometheus) until **refused metric points
   increase** — that is the agents' own exports being shed, i.e. #662's condition, not merely
   the flood being shed. Aborts if heap crosses 95% of `GOMEMLIMIT` or RSS crosses 90% of the
   pod limit. Gives up after 5 minutes and reports the peak heap and RSS reached (see
   [Tuning](#if-pressure-is-not-reached)).
4. Deletes the agent pod **on `--node` only**. `kubectl rollout restart ds/aether-agent` is
   DaemonSet-wide and would restart the whole fleet under a shedding collector; deleting one
   pod restarts one node.
5. Asserts on the replacement pod — **only #662's signature**: it appears within 90s (the
   DaemonSet only recreates it after the old pod's 30s grace), is Ready within 120s of
   appearing, has `restartCount: 0`, has no terminated previous container, never enters
   `CrashLoopBackOff`, does not log `failed to create SPIRE Workload API source`, and does
   log `resolved workload trust domain from SPIRE`. See
   [What counts as a FAIL](#what-counts-as-a-fail).
6. Re-reads the refused counters and requires them to have moved **during the restart window**.
   If the flood lapsed while the agent was starting, the agent had an easy start and the run is
   INCONCLUSIVE, not a PASS.
7. Deletes the Job, waits for the refusals to go flat, then waits for
   `aether_agent_storage_pods{node="<node>"}` to be fresher than 120s — proof the agent's
   telemetry resumed once the pressure lifted.

## What counts as a FAIL

The verdict asserts on **#662's signature and nothing else** (#699):

| | |
|---|---|
| FAIL | `failed to create SPIRE Workload API source` in the agent log |
| FAIL | the container exited: `restartCount > 0`, a terminated previous container, or `CrashLoopBackOff` |
| FAIL | `resolved workload trust domain from SPIRE` never logged (the source was never established) |
| INFO | **every other `ERROR` line**, listed in the output, not gating the verdict |

Run 4 of 2026-09-05 is why the last row exists. The agent came Ready in 16 s with 0 restarts
and SPIRE resolved — #668 demonstrably held — yet the harness printed FAIL because the new
pod logged one self-recovering client retry (`registrar.go:463 failed to start watch stream,
retrying` … `context canceled`, tracked separately in #700). A harness that fails on log
noise adjacent to the very condition it creates cannot be used to close #662. Other ERRORs
are still worth reading — they are printed, deduplicated to the first 20 lines — but they are
evidence for a different bug, not this one.

## Expected PASS signature

```
==== PASS
  node under test          <node>
  metrics source           collector | prometheus
  agent pod                aether-agent-xxxxx -> aether-agent-yyyyy (restartCount 0, Ready)
  collector heap           baseline 5xMiB -> peak 1[1-4]xxMiB (6x-9x% of the 1638MiB GOMEMLIMIT)
  collector RSS            baseline 24xMiB -> peak 1[2-7]xxMiB (1xx% of the 1126MiB soft limit)
  refused (whole run)      log_records +N, metric_points +M          (both > 0)
  refused (restart window) log_records +N', metric_points +M'        (at least one > 0)
  agent evidence           'resolved workload trust domain from SPIRE' present; no
                           'failed to create SPIRE Workload API source'; restartCount 0;
                           no CrashLoopBackOff; no terminated container
                           (K other ERROR line(s), informational)
  telemetry recovery       aether_agent_storage_pods{node="<node>"} fresh again
```

Peak RSS **above** the 1126 MiB soft limit is expected and correct — that is the limiter
holding the process at its ceiling, not a problem. Peak RSS ≤ 1,766 MiB in all five runs.

Exit codes: **0** PASS · **1** FAIL (the agent misbehaved — #662 is back, keep the logs) ·
**2** INCONCLUSIVE (pressure never reached, lapsed mid-test, the safety ceiling aborted the
run, or a `kubectl` or `curl` call failed; nothing was proven). An INCONCLUSIVE run is not
always a run that changed nothing: the abort message says whether the Job was applied or
deleted and whether the agent pod had already been deleted. The ceiling is read until the
end, so it can be crossed after the restart.

A FAIL is a real regression report: capture `kubectl -n aether-system describe pod` and the
full pod log before re-running, because the next run replaces the pod.

## Risk and blast radius

- **The SLI goes blind while shedding is engaged** (typically 1–4 minutes). The prober exports
  through the same collector; its counters have a hole. This is *the* reason the harness must
  never run during a soak or a release validation.
- **`rate()` over the prober SLI *under-reads* during the window** — 12–18/s against a real
  25/s in the 09-05 runs. That is export lag on cumulative counters, not lost requests: the
  counters are monotonic and self-heal once the shed lifts, and the total is right afterwards.
  Do not read the dip as an availability incident, and do not grade anything off a `rate()`
  that spans the shedding window.
- **Other telemetry is dropped in the same window**: mesh metrics, traces, logs from every
  component. Nothing in the data path depends on them — that is precisely what this test is
  asserting.
- **The collector must not restart.** Two guards: the flood runs at ~70% of the rate the
  limiter's spike budget tolerates, and `run.sh` deletes the Job the moment heap crosses 95%
  of `GOMEMLIMIT` (1,556 MiB) or RSS crosses 90% of the pod limit (1,843 MiB). Shedding is the
  designed behaviour; OOM is not, and a restart would also break the measurement (Prometheus
  series churn). Observed across five runs: peak RSS ≤ 1,766 MiB, **0 restarts**.
- **The logs backend ingests the flood** under `service.name=aether-collector-pressure`. Payloads
  are NUL-filled and compress to nearly nothing; the stream is trivially excluded from queries.
- **One node's agent restarts.** During its ~10–20s startup that node serves no xDS updates —
  the same exposure as any agent roll in a soak, on one node.
- **Prometheus is untouched by the flood** by design (see above), so the measurement plane
  stays trustworthy throughout.

## Cleanup guarantees

Three independent layers, because a harness that can leave a flood running is worse than no
harness:

1. `run.sh` traps `EXIT`/`INT`/`TERM` and deletes the Job on **every** exit path, including
   failures, aborts and Ctrl-C.
2. The Job carries `activeDeadlineSeconds: 600` — it self-terminates at 10 minutes even if the
   machine running it dies, the port-forward drops, or the script is `SIGKILL`ed. `backoffLimit: 0`
   means a failed generator pod ends the Job rather than retrying at an unknown pressure.
3. The manual switch, safe at any instant:

   ```bash
   # the EXPECT_CONTEXT, JOB_NS and JOB_NAME the run was given
   # (defaults: aether-test, aether-collector-pressure)
   kubectl --context <kube-context> -n <JOB_NS> delete job <JOB_NAME>
   ```

Nothing else is mutated: no helm release, no chart value, no DaemonSet, no collector config.
The only cluster changes are the Job (deleted) and one agent pod (recreated by its DaemonSet).

## If pressure is not reached

`run.sh` exits 2 with the peak heap (as a percentage of `GOMEMLIMIT`) and peak RSS it saw.
Escalate one knob at a time in `collector-pressure-job.yaml`, re-checking the arithmetic above each time — the
aggregate must stay well under ~102 MiB/s **per collector replica**:

1. `--rate=3` → `4` (+33%; ~96 MiB/s per replica — at the frontier, do not combine with 2).
2. `parallelism`/`completions` 3 → 4 with `--rate` back at `3` (~96 MiB/s per replica).
3. `--size=1` → `2` (2 MiB records; halve `--rate` if you do this).

The default is already ~70% of the frontier and puts the Go heap within ~4% of the 95%
abort ceiling, so most "pressure not reached" exits are a collector that got more headroom
(bigger limit, third replica) rather than a rate problem — re-run `--dry-run` first.

If RSS climbs but refusals stay at 0, the exporters are draining as fast as the flood arrives:
raise `--rate` rather than `--size`. If `refused_log_records` moves but `refused_metric_points`
never does, the pressure is intermittent — the collector is dipping back under the soft limit
between agent export intervals (60s); more parallelism, not more size, is the fix.

## Files

- `collector-pressure-job.yaml` — the `telemetrygen` Job, with four tokens `run.sh` fills in
  for the collector's endpoint and namespace and the Job's own (by default `aether-test`, explicit
  `aether.io/managed: "false"` mesh opt-out, no tolerations, priority 0, capped resources,
  `activeDeadlineSeconds: 600`).
- `run.sh` — pre-flight, pressure, one-node agent restart, assertions, teardown, verdict.
  Useful flags: `--dry-run` (resolve and print the plan, change nothing),
  `--no-soak-running` (required for a real run), `--metrics-source
  auto|collector|prometheus`, `--pressure-timeout`, `--ready-timeout`. Environment for
  everything else ([Inputs](#inputs)), including `ABORT_HEAP_PCT` / `ABORT_RSS_PCT` and
  `SOAK_POD_SELECTOR`.
- `preflight_test.sh` — the hermetic test of the soak guard and of what every `kubectl` and
  `curl` call does when it fails (`bazel test //e2e/pressure:preflight_test`).
