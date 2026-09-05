# Collector-pressure harness (issue #662)

Deliberately drives the shared `otel-collector` into `memory_limiter` shedding, restarts
**one** node's `aether-agent` under that pressure, and asserts the agent still starts,
attests to SPIRE and serves. It is the on-demand test for the branch #668 fixed.

**Never run this during a soak.** See [Risk](#risk-and-blast-radius).

```bash
bash e2e/pressure/run.sh --node main-worker-03

# Resolve GOMEMLIMIT, the pod limits, the abort thresholds and the metrics source,
# print them, and exit. Applies no Job and touches no agent — safe any time.
bash e2e/pressure/run.sh --node main-worker-03 --dry-run
```

## Why a soak cannot test this

#662 was a data-plane outage caused by an observability component: with the collector
refusing exports (`data refused due to high memory usage`), the agent's startup context
was cancelled, SPIRE source creation failed, and 3 of 5 agents crash-looped — worker-02
at 5 restarts and still `CrashLoopBackOff`, i.e. a node with no xDS control plane.

#668 decoupled startup from export. Since then the fix has been **"not disproven", never
exercised**. Measured over the 09-03 and 09-04 soaks:
`max_over_time(otelcol_process_memory_rss_bytes[2d])` = **245 MiB, 21.7% of the 1126 MiB
soft limit**, and `increase(otelcol_receiver_refused_metric_points_total[2d])` = **0**. The
soak's job is to hold the mesh under *load and churn*, and it succeeds — but its load is
mesh traffic, not telemetry volume, and the o11y plane was resized (2Gi, GitOps #35/#36)
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
`otel-collector.o11y.svc.cluster.local:4317` until the process crosses `memory_limiter`'s
soft limit and starts refusing. Every agent, proxy, prober and controller in the cluster
exports to that collector, so the shed hits the real agent's real export path — the exact
#662 condition, with no code, image or chart change anywhere in the mesh.

The cost is honest: the SLI goes blind while the collector is shedding (prober counters
are exported through it). That is acceptable *outside* a soak and is the reason the
pre-flight refuses to run when a soak is up.

### B — a dedicated throwaway collector + one node pointed at it (rejected)

Deploy a second collector with a small memory limit into a scratch namespace and point one
node's agent at it. It sounds safer — nothing shared is touched — but **there is no
node-scoped mechanism to point one agent at a different endpoint**:

- `--otlp-endpoint` (chart value `telemetry.otlpEndpoint`) is a DaemonSet-wide flag. Changing
  it is a `helm upgrade` that rolls **all five** agents, and leaves the whole fleet exporting
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
plane with the pressure signal would be self-defeating; the garbage lands in VictoriaLogs
under `service.name=aether-collector-pressure` instead (NUL-filled payloads, so it compresses
to nearly nothing).

Sizing, from the deployed config (`clusters/talos-main/otel-collector/values.yaml`, 2Gi limit,
`check_interval: 5s`, `limit_percentage: 80`, `spike_limit_percentage: 25`):

| | |
|---|---|
| hard limit (refuse + forced GC) | 80% of 2Gi = **1638 MiB** |
| soft limit (shedding begins) | (80−25)% of 2Gi = **1126 MiB** |
| spike budget per 5s check | 512 MiB ⇒ ~**102 MiB/s per replica** is the OOM-risk frontier |
| this harness | 3 pods × 16 workers × 3 records/s × 1 MiB ≈ **144 MiB/s**, ~72 MiB/s per replica |

About 70% of the frontier (2 pods × rate 2, a third of it, never reached pressure in 300s — twice on 2026-09-05), so the collector *sheds* rather than OOM-kills — shedding is
`memory_limiter` working correctly, and 09-02 showed it sheds under real saturation too.

## Measured behaviour (four runs, 2026-09-05) — do not re-derive these

`memory_limiter` triggers on the **Go heap**, not on RSS. What the four runs actually
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

| ceiling | rule | on today's collector |
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

**Today's deployed collector does not serve `:8888`.** Its `service.telemetry.metrics`
has only a `periodic` OTLP reader, no pull reader, so nothing listens on 8888 and
`run.sh` logs a warning and falls back to Prometheus (poll 15 s instead of 5 s). To get
the lag-free path, add a pull reader in the GitOps values for `o11y/otel-collector`:

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
`otelcol_processor_memory_limiter_refused_*` (what the deployed 0.159.0 emits),
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

1. `kubectl` context is `talos-main` (it gets silently stolen by `kind-kind`; set
   `EXPECT_CONTEXT` to override deliberately).
2. **No soak is running**: no `k6-soak-loader` DaemonSet in `aether-test`, no `churn.sh` on
   this workstation. Shedding the collector mid-soak makes the prober's cumulative counters
   non-monotonic and the run ungradeable (soak README gotchas 3 and 4).
3. `otel-collector` is at full readiness (2/2). Do not pressure an already-degraded o11y plane.
4. The target node's agent pod is Ready with `restartCount: 0`.
5. The external prober is reporting successes — the availability signal must be alive going in.
6. `aether_agent_storage_pods{node="<node>"}` has samples — the agent's export works going in.
7. Baseline collector RSS is **under 35% of the soft limit** (≈394 MiB). Idle is ~250 MiB
   (22%), which is why the gate is 35% and not the 20% one might reach for: a 20% gate can
   never pass at rest. Anything above 35% means the collector is already loaded and the run
   would not be attributable.

Not fatal, but printed loudly: the metrics source it settled on, and — when it fell back to
Prometheus — that shedding onset will be seen 30–60 s late. Run `--dry-run` first and read
the resolved plan; it is the cheapest way to catch a resized o11y plane or a stolen context.

Also confirm by eye that nothing else important is mid-flight (a release, a conformance run,
a profiling pass) — for the shedding window, cluster telemetry is unreliable by design.

## Procedure

`run.sh` does all of it, in order, and prints every number it reads:

1. Resolves the collector's `GOMEMLIMIT` and memory limit off the live pod, derives the
   abort ceilings from them, picks the metrics source, port-forwards `prometheus-server`
   (the LB address is not workstation-reachable, and `kubectl exec` into Prometheus is not
   permitted) for the prober and agent-freshness signals, and records the baselines:
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
  node under test          main-worker-03
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
  telemetry recovery       aether_agent_storage_pods{node="main-worker-03"} fresh again
```

Peak RSS **above** the 1126 MiB soft limit is expected and correct — that is the limiter
holding the process at its ceiling, not a problem. Peak RSS ≤ 1,766 MiB in all five runs.

Exit codes: **0** PASS · **1** FAIL (the agent misbehaved — #662 is back, keep the logs) ·
**2** INCONCLUSIVE (pressure never reached, lapsed mid-test, or the safety ceiling aborted the
run; nothing was proven and, on a ceiling abort, no agent was touched).

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
- **VictoriaLogs ingests the flood** under `service.name=aether-collector-pressure`. Payloads
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
   workstation dies, the port-forward drops, or the script is `SIGKILL`ed. `backoffLimit: 0`
   means a failed generator pod ends the Job rather than retrying at an unknown pressure.
3. The manual switch, safe at any instant:

   ```bash
   kubectl -n aether-test delete job aether-collector-pressure
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

- `collector-pressure-job.yaml` — the `telemetrygen` Job (namespace `aether-test`, explicit
  `aether.io/managed: "false"` mesh opt-out, no tolerations, priority 0, capped resources,
  `activeDeadlineSeconds: 600`).
- `run.sh` — pre-flight, pressure, one-node agent restart, assertions, teardown, verdict.
  Useful flags: `--dry-run` (resolve and print the plan, change nothing), `--metrics-source
  auto|collector|prometheus`, `--pressure-timeout`, `--ready-timeout`. Env overrides for
  everything else, including `ABORT_HEAP_PCT` / `ABORT_RSS_PCT` and `EXPECT_CONTEXT`.
