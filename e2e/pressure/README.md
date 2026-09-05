# Collector-pressure harness (issue #662)

Deliberately drives the shared `otel-collector` into `memory_limiter` shedding, restarts
**one** node's `aether-agent` under that pressure, and asserts the agent still starts,
attests to SPIRE and serves. It is the on-demand test for the branch #668 fixed.

**Never run this during a soak.** See [Risk](#risk-and-blast-radius).

```bash
bash e2e/pressure/run.sh --node main-worker-03
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
| this harness | 2 pods × 16 workers × 2 records/s × 1 MiB ≈ **64 MiB/s**, ~32 MiB/s per replica |

A third of the frontier, so the collector *sheds* rather than OOM-kills — shedding is
`memory_limiter` working correctly, and 09-02 showed it sheds under real saturation too.
`run.sh` additionally aborts and tears down if any replica's RSS crosses **1500 MiB**.

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

Also confirm by eye that nothing else important is mid-flight (a release, a conformance run,
a profiling pass) — for the shedding window, cluster telemetry is unreliable by design.

## Procedure

`run.sh` does all of it, in order, and prints every number it reads:

1. Port-forwards `prometheus-server` (the LB address is not workstation-reachable, and
   `kubectl exec` into Prometheus is not permitted) and records the baselines:
   `otelcol_process_memory_rss_bytes`, `otelcol_receiver_refused_metric_points_total`,
   `otelcol_receiver_refused_log_records_total`.
2. Applies the pressure `Job`.
3. Polls every 15s until **`refused_metric_points_total` increases** — that is the agents' own
   exports being shed, i.e. #662's condition, not merely the flood being shed. Gives up after
   5 minutes and reports the max RSS reached (see [Tuning](#if-pressure-is-not-reached)).
4. Deletes the agent pod **on `--node` only**. `kubectl rollout restart ds/aether-agent` is
   DaemonSet-wide and would restart the whole fleet under a shedding collector; deleting one
   pod restarts one node.
5. Asserts on the replacement pod: it appears within 90s (the DaemonSet only recreates it
   after the old pod's 30s grace), is Ready within 120s of appearing, has `restartCount: 0`,
   never enters `CrashLoopBackOff`, and its log contains `resolved workload trust domain from
   SPIRE` and contains neither `failed to create SPIRE Workload API source` nor any
   `ERROR`-level `context canceled`.
6. Re-reads the refused counters and requires them to have moved **during the restart window**.
   If the flood lapsed while the agent was starting, the agent had an easy start and the run is
   INCONCLUSIVE, not a PASS.
7. Deletes the Job, waits for the refusals to go flat, then waits for
   `aether_agent_storage_pods{node="<node>"}` to be fresher than 120s — proof the agent's
   telemetry resumed once the pressure lifted.

## Expected PASS signature

```
==== PASS
  node under test          main-worker-03
  agent pod                aether-agent-xxxxx -> aether-agent-yyyyy (restartCount 0, Ready)
  collector RSS            baseline 251MiB -> peak 11xxMiB (10x% of the 1126MiB soft limit)
  refused (whole run)      log_records +N, metric_points +M          (both > 0)
  refused (restart window) log_records +N', metric_points +M'        (at least one > 0)
  agent evidence           'resolved workload trust domain from SPIRE' present;
                           no 'context canceled' ERROR; no CrashLoopBackOff
  telemetry recovery       aether_agent_storage_pods{node="main-worker-03"} fresh again
```

Exit codes: **0** PASS · **1** FAIL (the agent misbehaved — #662 is back, keep the logs) ·
**2** INCONCLUSIVE (pressure never reached, lapsed mid-test, or the safety ceiling aborted the
run; nothing was proven and, on a ceiling abort, no agent was touched).

A FAIL is a real regression report: capture `kubectl -n aether-system describe pod` and the
full pod log before re-running, because the next run replaces the pod.

## Risk and blast radius

- **The SLI goes blind while shedding is engaged** (typically 1–4 minutes). The prober exports
  through the same collector; its counters have a hole. This is *the* reason the harness must
  never run during a soak or a release validation.
- **Other telemetry is dropped in the same window**: mesh metrics, traces, logs from every
  component. Nothing in the data path depends on them — that is precisely what this test is
  asserting.
- **The collector must not restart.** Two guards: the flood runs at a third of the rate the
  limiter's spike budget tolerates, and `run.sh` deletes the Job if a replica's RSS crosses
  1500 MiB. Shedding is the designed behaviour; OOM is not, and a restart would also break the
  measurement (Prometheus series churn).
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

`run.sh` exits 2 with the max RSS it saw, as a percentage of the soft limit. Escalate one knob
at a time in `collector-pressure-job.yaml`, re-checking the arithmetic above each time — the
aggregate must stay well under ~102 MiB/s **per collector replica**:

1. `parallelism`/`completions` 2 → 3 (+50% throughput, better spread over both replicas).
2. `--rate=2` → `3` (+50%).
3. `--size=1` → `2` (2 MiB records; halve `--rate` if you do both).

If RSS climbs but refusals stay at 0, the exporters are draining as fast as the flood arrives:
raise `--rate` rather than `--size`. If `refused_log_records` moves but `refused_metric_points`
never does, the pressure is intermittent — the collector is dipping back under the soft limit
between agent export intervals (60s); more parallelism, not more size, is the fix.

## Files

- `collector-pressure-job.yaml` — the `telemetrygen` Job (namespace `aether-test`, explicit
  `aether.io/managed: "false"` mesh opt-out, no tolerations, priority 0, capped resources,
  `activeDeadlineSeconds: 600`).
- `run.sh` — pre-flight, pressure, one-node agent restart, assertions, teardown, verdict.
