# Proposal 042: sortie as the soak's load driver — k6 retired

**Status:** Draft, harness built and proven on kind 2026-10-06 (#1339) against
sortie b71b37e, and moved to sortie 94cf103 on 2026-10-07, which fixed every
defect the first version had to work around ("What sortie 94cf103 retired").
One new sortie defect was found on the way: it waits without limit for an engine
whose node has gone silent ("Risks", 5).
Phase 1, the short e2e on talos-main, ran twice on 2026-10-07 ("Verified on
talos-main"): the driver works there, and it needs a client queue, which the
plan now carries by default ("The client queue").
The harness then moved to sortie 9fcbb81 (2026-10-07, "What sortie 9fcbb81
changed"): that defect is fixed, a request still in flight when a run ends is
counted, a stage that was not run is marked as such, the report carries
latency and timestamps, and each execution's result is also written as it
finishes. With 9fcbb81 the short e2e passed on talos-main (run 4, 2026-10-08,
8 of 8) and the **first 8-hour soak** ran the same day: the sortie gate passed
with 9,180,000 of 9,180,000 requests answered `2xx` under 35 rollouts
("Verified on talos-main").
The harness then moved to sortie f0750ec (2026-10-08, "What sortie f0750ec
changed"): a stage refused at the execution cap is marked by a field, and a
driver that wakes past a backend's deadline takes the answer that is waiting.
Proven on kind only.
The harness then moved to sortie 96e6bfb (2026-10-08, "What sortie 96e6bfb
changed"): nothing the harness reads or passes changed; the engine has a fix
for a data race between executions that start together, and an image no longer
gets a new digest from a commit that did not change it.
Proven on kind; **neither f0750ec nor 96e6bfb has run on talos-main yet**.
**Author:** Bruno Palermo
**Date:** 2026-10-06
**Related:** #1323 (the kickoff lived outside the repository and had two
defects; closed by this), #1093 / #1320 (nodes are CPU-tight at proxy handoffs,
which is what the engine's CPU cost collides with), #846 / #887 (the k6
per-class and per-target failure breakdown this has to replace), #1108 (the UDS
share), #1009 (the benign k6 `DC` artefact; one row with its signature was seen under
sortie, unexplained),
#1086 (first-use failures at loader start), #1242 / #1265 (the restart
watchdog), 013 (the external prober, still the authority for PASS/FAIL).

## Problem

The soak's load has been k6 since the first run: a DaemonSet of `grafana/k6`
pods, one per worker node, each at 60 iterations per second for 8h30m against
six mesh services and two UDS-delivered ones (`e2e/soak/k6-mesh-soak.js`,
`k6-runner.yaml`). On 2026-10-06 the decision was taken that `nightly-1006` was
the last k6-driven soak and that validation from now on runs on
[sortie](https://github.com/bpalermo/sortie): declarative plans over Nighthawk,
whose engine is the same Envoy the mesh runs.

Three things about the k6 setup are worth not carrying over:

- **Its verdict exists only in a pod's stdout.** `http_req_failed` and the
  per-target table are printed once, when k6 exits; the DaemonSet then restarts
  the container, so the summary is in `logs --previous`, and a teardown at the
  wrong minute loses it. The harness has lost a soak's summary to that more than
  once.
- **Its per-target breakdown is hand-built.** k6's summary aggregates across
  tags, so the script declares 297 threshold submetrics to make "which target,
  which class" visible at all, and a classifier in the script maps k6's error
  numbering onto classes (it was wrong from #846 to #887).
- **Launching it is not in the repository.** The kickoff, the nightly wrapper
  and the saver are workstation scripts (`~/aether-soak-logs/<date>/…`). #1323
  found two defects in them on the last run: the restart watchdog started at
  T0+30 min instead of at T0, and the pre-flight's readiness check used an awk
  back-reference, which awk does not have, so it printed `NOT READY` for every
  healthy pod.

## What changes, in one paragraph

One sortie engine (`nighthawk_service`) per worker node, as a mesh-managed
DaemonSet in `aether-test`; one sortie Job whose plan finds the engines through
a headless Service and drives each of them at 60 rps, split over the same eight
targets as eight concurrent executions with their own rate limiters, counters
and histograms. The verdict is a JSON report written to a PersistentVolume and
copied to the run directory before anything is torn down; `sortie-gate.sh`
turns it into one PASS/FAIL line per target. Everything that launches a run is
`e2e/soak/run.sh`. The prober remains the authority for PASS/FAIL; nothing about
the churn schedule, the watchdog or the access-log gates changes.

## Topology

```
 aether-test
 ┌──────────────────────────────┐        ┌─────────────────────────────────────────┐
 │ Job soak-sortie-<hash>       │  gRPC  │ DaemonSet soak-sortie-engine  (per node) │
 │  1 pod, NOT mesh-managed     ├───────▶│  pod label aether.io/managed: "true"     │
 │  resolves the headless       │ :8443  │  8 executions = 8 worker threads         │
 │  Service ONCE, at start      │ pod IP │  HTTP/1.1 ──▶ node proxy :18081 ──▶ mesh │
 │  writes report ▶ PVC         │        │  statsd (UDP) ──▶ o11y/otel-scraper:8125 │
 └──────────────────────────────┘        └─────────────────────────────────────────┘
        Service soak-sortie-engine-nodes (headless): one A record per ready engine
```

- **The engine is the mesh client.** It opens the connections, so it carries
  exactly what the k6 runner pod carried: the `aether.io/managed: "true"` label
  (ndots injection, the CNI capture divert, the `:53` DNAT to mesh DNS) and the
  `config.aether.io/upstreams` pre-warm list, unchanged. The load enters through
  the node's own proxy, by qualified mesh name, on `:18081`.
- **The sortie pod is not a mesh client.** It has no `aether.io/managed` label.
  It resolves `soak-sortie-engine-nodes.aether-test.svc.cluster.local` through
  cluster DNS and talks gRPC to each engine's pod IP on `:8443`; inbound to a
  managed pod is not captured, and the engines' replies are the reply direction
  of a tracked flow, which the capture table exempts.
- **Identity.** The chart gives both pods the ServiceAccount `soak-sortie`, so
  the load's source identity is `aether-test/soak-sortie`, not `default` as
  under k6. Access-log queries that select the loader by
  `client_sa` change accordingly (and can now tell the loader from everything
  else that runs as `default`). Requests also carry `User-Agent:
  aether-soak-sortie`.
- **PriorityClass `aether-soak-loader`**, reused, for the reason it exists: a
  DaemonSet pod cannot move, and without it the loader is the scheduler's
  victim when a surging `aether-proxy` pod needs room. The sortie pod carries
  it too (the chart's top-level `priorityClassName`): it can move, but it is the
  run's single point of failure and a run cannot. The chart only references the
  class; `run.sh` applies it and reads it back, before the engines and again
  before the Job, because `kubectl delete -f k6-runner.yaml` deletes it and a
  pod naming a class that does not exist is refused at admission.
- **Two-step install.** A `dns:` pool is resolved once, when the run starts,
  and takes the first non-empty answer. `run.sh` installs the engines alone
  (`job.enabled=false`), waits for the DaemonSet and for the headless Service to
  list every engine, and only then upgrades the release with the plan.
- **Pinned by digest and verified**, both images and the chart. All three are
  signed by sortie's publish workflow (cosign, keyless, the signature an OCI
  referrer), and the chart defaults both image references to digests. They were
  verified by hand with the repository's pinned cosign (v3.1.2): the chart, and
  the index and both per-arch manifests of each image, for 96e6bfb (and before
  it f0750ec, 9fcbb81 and 94cf103, each bound to its commit; the f0750ec chart
  digest bound to the 9fcbb81 commit is refused, which is the check doing its
  work). Up to f0750ec every digest changed with every sortie commit, because
  the commit was a label in each image's config. Since sortie 6d801c0 the
  chart's and the driver's still do and the engine's changes only when the
  engine does; a digest is signed once, by the commit that first published it,
  so the 96e6bfb engine is bound to 6a0a866 ("What sortie 96e6bfb changed").
  `run.sh` repeats the check in its pre-flight on every run, against what the
  release actually renders (`--verify` makes a missing cosign an abort).

## Mapping: the load shape

| | k6 (`k6-mesh-soak.js`) | sortie (`sortie-plan.sh`) |
|---|---|---|
| arrival | `constant-arrival-rate`, 60/s per runner | `constant-rate`, `rate: 60`, `per_backend: true`, `open_loop: true` |
| duration | 8h30m | `30600s` (profile `soak`); `900s` (profile `e2e`) |
| protocol | HTTP/1.1 cleartext to `:18081` (k6 speaks h2 only over TLS) | `protocol: http1`, same URLs |
| targets | one random draw per request: 95 % uniform over six mesh targets, 5 % uniform over two UDS targets | eight concurrent executions, each paced on its own |
| concurrency | 60–180 VUs, one keep-alive connection each | one worker thread per target, which waits for its next request (`WAIT`); connections as the rate needs them (a handful) |
| request timeout | 60 s (k6 default) | none per request; a request still unanswered 30 s after the run ends is counted (`http_inflight_lost`, since sortie 1178261): see "What is lost" |
| DNS | a lookup per new connection | once per execution, at start |
| verdict | `http_req_failed`, printed at exit | thresholds, per target, in a JSON report |

### What `http_req_failed == 0` and the failure classes become

The k6 gate was "every runner's `http_req_failed` is 0, or every failure is
attributed", read from eight class counters and a by-target table. Under sortie
each target is its own execution, and the gate is the **zero-failure set**: these
counters `== 0`, per target, on every node.

| k6 class (`aether_fail_*`) | sortie counter | notes |
|---|---|---|
| `http_4xx`, `http_5xx` | `benchmark.http_4xx`, `benchmark.http_5xx` | a complete response with that status |
| `conn` | `benchmark.pool_connection_failure` (+ `pool_failure_local_…` / `pool_failure_remote_connection_failure`) | never got a connection |
| `timeout` | `benchmark.pool_failure_timeout` | **connect** timeout only; there is no per-request response timeout |
| — (k6 failed it at 60 s) | `benchmark.http_inflight_lost` | sent, or queued for a connection, and with **no outcome** when the run was over and the request timeout (30 s, the engine's default) had passed after it. Not a reset, not a status, in no other counter (sortie 1178261; before it such a request was counted nowhere). The gate words it `in flight at the end: N request(s) with no outcome` |
| `proto`, and any mid-response cut | `benchmark.stream_resets` (+ `_before_headers`, `_incomplete_body`, `_<reason>`) | the reason counters name Envoy's `StreamResetReason` |
| — | `benchmark.pool_overflow` | the client refused its own request, which was never sent: the driver saturated beyond its client queue ("The client queue"), never the mesh's failure. The gate words it `driver saturated: N request(s) not sent`. No k6 equivalent (k6 reports `dropped_iterations`) |
| `dns` | — | no per-request class: a name that does not resolve fails the whole execution with an `error`, before any request |
| `tls` | — | the soak drives cleartext to the local proxy |
| `other` | — | nothing is unclassified: a request is counted under a status or under a reset or pool counter |

The counters **overlap** — a reset is in `stream_resets`, in one phase counter
and in one reason counter — so the gate lists them and never adds them up. A
counter that never incremented is absent from the report and reads zero; the
`pool_overflow` control for that is real (it was seen non-zero on kind, under
the CPU limit the engine used to need, and on talos-main with no client queue).

**An execution goes the distance.** Nighthawk carries four default *failure
predicates* — `benchmark.http_4xx`, `http_5xx`, `pool_connection_failure` and
`stream_resets`, each with a limit of 0 — and by itself ends an execution the
moment one is exceeded. Against sortie b71b37e that was fatal twice over: the
first 503 of the first roll would have stopped that target's load on that node
for the remaining hours, and the report then carried an `error` and no counters,
so it could not say which class had failed. The first version of the plan
lifted all four predicates by hand (`nighthawk_template.failure_predicates`,
limits of 10¹²). Since 94cf103 sortie turns them off itself and an execution
lasts its duration, so the plan no longer mentions them. Re-run on kind without
the workaround (one target's Deployment scaled to zero for 90 s of a 240 s run):
the execution ran 240.06 s on both nodes and the gate read `FAIL uds-cr-echo
rate=3.7/6rps http_2xx=888/1440 … http_5xx=552 [sortie-worker=276
sortie-worker2=276]`, with the seven other targets `PASS`.

Four things the gate adds that k6's `rate<0.01` never had:

- **A count floor per target, per node.** Each backend's `http_2xx` must be
  within 99 % and 101 % of what the plan asked of it: the target's share × the
  **configured** duration. Zero failures with nothing sent is otherwise a pass.
  The plan can carry only one floor for the whole scenario (the smallest
  share's, 3 rps × nodes) and sortie judges it against the pool's total;
  `sortie-gate.sh` applies each target's own floor to each node from the
  report's per-backend `results`, so one slow node is named instead of being
  averaged away. Until 9fcbb81 this was a *rate* over the backend's own elapsed
  time. That time is fixed when the run stops and can read a hair under the
  duration (899,999 ms of 900,000 on talos-main), which only flatters a rate —
  and a backend that stopped at 96 % of the run, at exactly its share, had a
  perfect rate. A count against the plan has neither problem, and it is a range
  because a late-woken `WAIT` worker ends a run one request short — sortie's
  own README reports 999 of 1,000 in about one short run in four on a busy
  machine and asks for "a range with some slack, not an equality". The second
  talos run read 40,497–40,500 of 40,500 per 9 rps target with every failure
  class at zero ("Verified on talos-main"): that, or requests still in flight
  when the run stopped, which 94cf103 counted nowhere — its reports cannot say
  which. 1 % is 81 requests of a 9 rps target's 15 minutes, 2,754 of its
  8h30m.
- **A stage that was not run fails.** sortie marks an execution it never
  attempted `"not_run": true` (a stage after one that an engine refused at its
  execution cap); it has no counters, so every failure class reads zero. The
  gate fails it by name, counts it in the verdict, and cross-checks the sortie
  pod's own log for `SKIP` lines and a `K not run` summary. This plan is a
  single stage, so the case is a guard, not an expectation.
- **The pool is checked.** The gate requires every execution to have been
  dispatched to exactly the number of engines that were ready at launch. A node
  missing from the pool fails the run instead of shrinking it.
- **A lost backend fails its node's share, not the run.** Since 94cf103 an
  engine that goes away mid-run is named in the execution's `backend_errors`
  and the others are reported whole. The gate fails every target for the lost
  node, names it, and still judges each survivor on its own counters. The plan's
  own floor cannot do this: with one of five nodes gone, a 9 rps target's pool
  still runs at 36 rps, far above "99 % of 3 rps × 5".

`sortie-gate.sh` prints, per target, `PASS`/`FAIL`, the achieved and planned
rate, the 2xx total against the planned count, the worst node's p50 and p99,
and for a failure the class, the count and the **node** (the report names
backends by pod IP; `run.sh` records the IP → node map at launch). `--per-node`
adds a line per target and node. That replaces the k6 "by target" table and adds
the node, which k6 could only give per runner log. It reads `results` (with
each backend's `statistics` for the latency), `totals`, `backend_errors`,
`not_run` and `started_at` / `ended_at`, and refuses a report without `results`
(a sortie older than 94cf103) or without `started_at` (older than 9fcbb81,
which did not count requests in flight) rather than reading counters that were
never counted as zero.

A `FAIL` line is what k6's non-zero `http_req_failed` was: the list of what has
to be attributed from the access logs and the roll brackets, not by itself a
failed soak. The 2026-09-30 run had 147 k6 failures, 141 of them before T0.

### Every README gate that reads k6 output

| gate / step (README) | reads from k6 today | under sortie |
|---|---|---|
| Run step 3: "all five runners report 60.00 iters/s" before churn | runner logs | the sortie pod `Running` after the settle time, its progress lines in `kubectl logs`, one a minute (`http_2xx` per target and node), and the live per-node `…benchmark_http_2xx` series if statsd is on; `run.sh` refuses to start churn if the pod is not running |
| Run step 5: teardown only after k6 exits; summary in `logs --previous` | pod log | `sortie-save.sh` (armed at load start) copies the report and the log; `sortie-teardown.sh` refuses without its `SAVED` marker. The Job pod does not restart, so nothing is in `--previous` |
| The restart gate: "the watchdog stops at T0+8h because `k6-soak-loader` restarts once by design" | — | a finished Job pod has no restart; the 8 h window no longer has a trap at its end. The watchdog also now sees an engine OOM (it did, on kind) |
| The UDS gates, item 1: "the `--- by target` table must show no `uds-echo` / `uds-cr-echo` row" | k6 summary | `sortie-gate.sh`: `PASS uds-echo`, `PASS uds-cr-echo`; a `FAIL` row must match an access-log row, as before. Control counts double (see "Rate shares") |
| The UDS gates, control query: `client_sa default` ≈ 216,000 per authority | access logs | `client_sa soak-sortie` ≈ 432,000 per authority |
| Benign `DC` (#1009): k6 lines must sit in a source-node proxy roll bracket; excluded from the k6 reconciliation | access logs + k6 | **expected to disappear for the loader**: see below. If `DC` + 200 + full bytes rows appear for `client_sa soak-sortie`, that is a finding, not a baseline. **One did**, in the second talos run ("Verified on talos-main") |
| New-SA step text: "visible only because the k6 loaders happened to start … 58 s before T0" (#1086) | history | the engines are a new identity on every node at install; `run.sh` waits `SOAK_ENGINE_SETTLE` (30 s) after they are Ready before the first request |
| Gate 3 (twin count = observed pairs): "k6 loaders + prober + new-SA steps" | Prometheus | same query; the loader's pairs are now `…@aether-test/soak-sortie` |
| QUIC per-request cost gate (#1021): "at the soak's load shape", 300 rps | Pyroscope / Prometheus | the fleet rate is unchanged (300 rps), the per-target mix is not: re-baseline h3/h2 on the first sortie soak rather than comparing against 1.18× across drivers |
| Gotcha 6, "k6 needs 1Gi" | — | the engine's own numbers, below |
| Files: `AETHER_FAIL` sample lines with timestamps, the only instrument that placed a burst against a roll | k6 log | the verbatim per-failure line is **lost**. Placement in time comes from three coarser instruments: the progress lines in `sortie.log` (every 60 s, cumulative counters and latency `mean`/`max` per target and node), statsd (10 s resolution, per target and node — see "Telemetry"), and the access logs, which carry the user agent |

### What is lost

- **Per-request random target mixing.** k6 drew a target per iteration, so the
  targets' requests interleaved randomly and any one second could hold any mix.
  sortie paces each target on its own limiter: a fixed 9 or 3 rps each, every
  second. Bursts across targets never coincide by chance, and never fail to.
- **The k6 `DC` class** — and that is a gain. k6 closed the connection after a
  full body while the h3 upstream FIN was still in flight, which the source
  proxy logged as `DC` (#1009). Envoy's client does not treat a disconnect
  after a complete response as a failure, and it does not close first.
  (Expected, and nearly so: the second talos run logged one such row in
  269,992, still not a failure to the engine. "Verified on talos-main".)
- **Per-request DNS.** A target is resolved once per execution, by the engine,
  at the start. Mesh DNS is exercised 8 times per node per run by the loader,
  not continuously. The prober's `mesh_dns` tier (which resolves on every probe)
  is and stays the DNS SLI; the loader was never the authority for it.
- **A per-request response timeout.** A request that never gets an answer
  waits until the run ends and for the request timeout (30 s) after it. k6
  failed it at 60 s. Since sortie 1178261 it is then counted, in
  `http_inflight_lost`, and the gate fails the target on it; until then it was
  in no counter at all and a single hung request was invisible to the loader.
  What is still lost is *when*: it is one number at the end of the run, not a
  failure 60 s after the request. The count floor catches a target that stops
  answering wholesale, and the prober's 2 s budget remains the instrument for
  the moment it happens.
- **The verbatim, timestamped failure sample** (`AETHER_FAIL`). What is left is
  a one-minute timeline per target and node (the progress lines), not a line
  per failure.
- **`default` as the loader's identity**, with every query that assumed it.

### What is gained

- **Per-target rate limiters.** One slow target cannot starve the others of
  their slots, which under k6's shared VU pool it could.
- **Per-target latency histograms and counters**, native, with no declared key
  space to maintain and nothing to classify by hand.
- **Reset reasons from the same codec the mesh runs** (`connection_termination`,
  `remote_reset`, `protocol_error`, …) and a before-headers / mid-body split.
- **A verdict that is a file**, per target and per node.
- **A loader that does not restart** inside or after the window.

## Rate shares

k6: 60 iterations/s per runner, 95 % uniform over six mesh targets (15.8 % =
9.5 rps each) and 5 % over two UDS targets (2.5 % = 1.5 rps each).

sortie needs a whole number of requests per second per target, each a multiple
of `concurrency`, and rounds a share that is not whole — `sortie-plan.sh`
refuses one instead, so the shares always add up to the rate. With six equal
mesh shares `m` and two equal UDS shares `u`, `6m + 2u = 60` has three
whole-number solutions near the k6 mix: (10, 0), (9, 3) and (8, 6).

**Proposed: 9 rps × 6 mesh + 3 rps × 2 UDS = 60 rps per node** (weights 3 : 1).

| per node | k6 | sortie | change |
|---|---|---|---|
| each mesh target | 9.5 rps (15.8 %) | 9 rps (15 %) | −5 % |
| each UDS target | 1.5 rps (2.5 %) | 3 rps (5 %) | ×2 |
| total | 60 | 60 | — |

What that changes, on five nodes over the graded 8 h:

- **UDS gate control counts double**: ~432,000 clean 200s per UDS authority
  from the loader (was ~216,000); `uds-client`'s ~28,800 is unchanged. The gate
  itself is still "0 non-benign lines"; only the size of the control moves.
- **Mesh target counts**: 1,296,000 per target (was ~1,368,000).
- **Fleet rate**: 300 rps, unchanged, so fleet CPU stays comparable — except
  that 15 rps per node moved from TCP delivery to UDS delivery.
- **The uds-csi roll steps see twice the traffic** through each roll: a
  delivery failure there is twice as likely to be caught client-side.
- A 5 % UDS share exactly would need 1.5 rps per target, which neither sortie
  nor `concurrency: 1` can express. 3 rps is the smallest whole share that
  keeps the six mesh targets equal.

## CPU and memory

The engine's CPU cost was the part of the cut-over that was not free. Since
sortie 94cf103 it nearly is: **about 0.11 core per node, a CPU request, no CPU
limit.**

**An idle Nighthawk worker thread is not idle unless told to wait.** Between
requests the sequencer spins (`SPIN`, Nighthawk's and sortie's default), polls a
25 µs timer (`POLL`), or sleeps 50 µs at a time (`SLEEP`). None of those blocks
until the next request is due, so the cost is per worker thread and independent
of the rate: a 3 rps target costs what a 9 rps one does. Eight targets are
eight worker threads per node. sortie 94cf103 adds a fourth strategy, `WAIT`,
which blocks until the rate limiter says the next request is due and wakes at
least every 5 ms to see whether the run should end. That wake-up is the floor
that is left: about 14 millicores per worker here. In 94cf103 `WAIT` was
opt-in, through `nighthawk_template`, and the plan asked for it; since sortie
7f338df it is sortie's default and the plan no longer says anything
(`sortie-plan.sh --idle-strategy` still can, for an experiment).

Measured on kind (two worker nodes, 20-core workstation, the engine
container's cgroup `cpu.stat` and `memory.*`, the plan's 60 rps per node over
eight executions, 2 minutes each unless noted; `nr_throttled` is the number of
CFS periods in which the container was throttled):

| idle strategy, limit | CPU per engine | `nr_throttled` | 2xx sent, of planned | latency p50 / p99 (worst node) |
|---|---|---|---|---|
| no execution | 0.004 cores | — | — | — |
| `SPIN` (default), none — b71b37e | **8.0 cores** | — | — | — |
| `POLL`, none — b71b37e | 2.85 cores | — | — | — |
| `SLEEP`, none | **1.68–1.81 cores** | 0 | 100.0 % | 2.2–2.7 ms / 2.8–3.3 ms |
| `SLEEP`, 400m, `max_pending_requests: 16` — **what shipped before** | 0.41–0.44 (at the limit) | **every period** (249–272 of the 255–273 in a 25 s window; 51–105 s of thread time throttled in each) | 99.3 % | 2.5–2.7 ms / **21–78 ms** |
| `WAIT`, 200m | 0.10 cores | 38–43 at start, then 5 more (1.7 s) in one 25 s window on one node | 98.1–98.3 % | 2.0–2.5 ms / 2.5–3.0 ms |
| `WAIT`, 400m | 0.10 cores | 17–20, all at start | 99.3 % | 2.0–2.7 ms / 2.4–3.5 ms |
| **`WAIT`, none — what ships** | **0.11–0.12 cores** | 0 | **100.0 %** | 2.0–2.8 ms / 2.5–3.6 ms |
| the same over 15 min, with `--progress 60s` | 0.125–0.14 cores (0.115–0.13 between snapshots) | 0 | 100.0 % (108,000 of 108,000) | 1.8–2.4 ms / 2.2–2.9 ms |

sortie's own measurement is 26 millicores for one worker at 60 rps and 11 per
idle worker; eight workers at 3–9 rps each landing on 110–130 agrees with it.

**The limit is removed, not kept as a guard rail.** Three readings decide it:

- `WAIT` makes the engine cheap enough not to need one: 0.11 core against the
  1.7 the limit was there to contain, and against k6's own 100m request.
- A limit is not free even when the steady state fits under it four times over.
  At 400m the engine was throttled in 17–20 periods **at the start of every
  run**, while eight executions and their threads come up, and each execution
  then ran about 0.8 s short: 2,144–2,145 of 2,160 requests per mesh target in
  two minutes, where the unlimited engine sent 2,160. At 200m it was throttled
  mid-run as well. "`nr_throttled` stays 0" is the condition under which a
  limit could have stayed, and it does not hold.
- A throttled load generator reports its own scheduling delay as the target's
  latency, and refuses its own requests (`pool_overflow`) when it falls behind.
  Both read as the mesh. An engine that is unexpectedly expensive (a plan
  rendered with `SPIN`) is better found in the node's CPU, where it is looked
  for, than in the report.

With the limit gone, `max_pending_requests` went too — and came back after the
first talos run, for a reason that has nothing to do with CPU limits. It had
existed so that a throttled worker could queue the requests that came due while
it was off CPU; on kind an unthrottled engine never needed it. On talos-main it
does: see "The client queue", next.

The request is **150m**: the measured 0.125–0.14 core with the progress
snapshots in it, and a little room. talos-main's workers have 3.95 allocatable
cores and sit at 1.3–1.65 in use before any load. Read there on 2026-10-07 (two
15-minute runs): **127–135m and 302–321 MiB per engine pod**, so the request
stands.

## The client queue

**The plan carries `max_pending_requests`, sized as the largest share per
worker × a stall budget of 2 s: 18 for this plan.** `connections` is left at
the engine's default.

What the two fields are, read from the engine source at sortie 94cf103
(`engine/source/client/options_impl.h`, `process_bootstrap.cc`,
`benchmark_client_impl.cc`) rather than assumed, and unchanged at 9fcbb81 (the
two defaults in `options_impl.h` were read again):

| plan field | Nighthawk | default | what it caps |
|---|---|---|---|
| `connections` | `--connections` | **100** per worker thread (HTTP/1) | concurrent connections: the pool's `max_connections` circuit breaker. sortie passes it only when the plan sets it |
| `max_pending_requests` | `--max-pending-requests` | **0**, "no client side queuing" | requests waiting for a connection: the pool's `max_pending_requests` circuit breaker, which the engine sets to **1** for a value of 0 |

A worker thread is one target on one engine. A request that falls due and finds
no idle connection makes the pool open another one (up to 100) and waits,
*pending*, until it is up. So a slow **answer** costs a connection and nothing
else: at 9 rps every request would have to hang for 11 s to reach the
connection cap, and raising `connections` would change nothing. What overflows
is the pending queue: with the default, one request may wait for a connection
and a second that falls due meanwhile is refused — `pool_overflow`, a request
that is never sent. (In open loop the engine's own in-flight backpressure is
off; the circuit breaker is the only thing that refuses.) That takes several
requests due at once with no connection ready for them: the node not completing
new connections, or not running the worker thread, for longer than the gap
between two requests (111 ms at 9 rps), after which the backlog arrives
together. Which of the two it was on talos has not been established.

On kind (20 idle cores) that never happened in any run. On talos-main, without
a queue, it happened 6 times in 270,000 requests (run 1 below), on two nodes,
in executions whose progress lines show a ~1 s response-time maximum; with
`max_pending_requests: 16` it did not happen in a run with an agent roll and a
proxy roll in it (run 2). k6 absorbed the same moments without anyone
noticing: 60 preallocated VUs per runner, 180 at most. An open-loop driver
needs somewhere to put a request whose connection is not ready yet; that is
what a client queue is, and removing it was reading a kind result as a general
one.

**Why 18 and not 16.** A worker at `R` rps has at most `R × S` requests due
after `S` seconds in which none could start, so a queue of `R × S` absorbs a
stall of `S`. 16 at 9 rps is a budget of 1.8 s, which is what run 2 proved
enough; the longest response the mesh logged in the two runs was 1.455 s. A
fixed 16 would silently become a 0.9 s budget at 18 rps per target, so the
plan derives it instead: `ceil(largest share ÷ concurrency × stall budget)`,
budget 2 s — the first whole second above everything the mesh logged. sortie
carries the field on the scenario, not on the target, so one number serves all
eight targets and is written for the largest share; the 3 rps UDS targets get
the same 18 and therefore a 6 s budget. That is uneven and harmless: the queue
does not turn a refused request into a passed one, it lets a late one be sent.
The bound is a worst case. A stall in which answers are slow but new
connections still come up uses none of the queue, which is why run 1 has
executions with 2.1–2.3 s response-time maxima and no overflow. What has not
been run is 18 itself (run 2 used 16) and anything longer than 15 minutes.

**`pool_overflow` stays in the zero-failure set, with a narrower meaning.** It
is now: *the driver was saturated beyond the stall budget on that node* — a
stall longer than 2 s, at 9 rps. It is a failure of the **run's validity for
that target on that node** (requests planned and not sent), attributable to the
node and the moment, and it is not a mesh error: the request never reached the
proxy and has no access-log row. `sortie-gate.sh` says so in words — `driver
saturated: N request(s) not sent [<node>=N]` — apart from the `failures` list
of the mesh's classes, so that a grader does not count it as a data-plane
failure. `--max-pending N` / `SOAK_SORTIE_MAX_PENDING` still fix the number, 0
turns the queue off for an experiment, and `--stall-budget` /
`SOAK_SORTIE_STALL_BUDGET` change the budget.

**Pacing under `WAIT`** (the 15-minute SHORT profile, run twice, per target
and node, achieved against planned): 8,100 of 8,100 for every 9 rps target on
both nodes, 2,700 of 2,700 for both 3 rps targets on both nodes — 108,000 of
108,000 requests in each run, none short, none refused. The throttled engine of
the first version sent 16,182–16,186 of 16,200 per mesh target over the same 15
minutes (8,091–8,094 per node).

**Latency under `WAIT`** (the same two runs, per target and node): p50
1.7–2.7 ms, p99 2.1–3.3 ms. The unthrottled `SLEEP` engine, which polls every 50 µs, reads
the same (p50 2.2–2.7 ms, p99 2.8–3.3 ms), so blocking until the next request
adds nothing visible to the measurement. The first version's 15-minute run,
`SLEEP` under the 400m limit, read p50 ≈ 3 ms and p99 ≈ 90 ms for a mesh target
(from the statsd histogram: 16 % of its requests took longer than 10 ms, and
its mean was 9.4 ms against 2.0–2.1 ms now) — the CFS period, not the mesh.

**So sortie's latency is now usable as a soft signal**, and the report carries
it: since sortie 1b3d404 each backend's entry in `results` has its
`statistics` (`benchmark_http_client.latency_2xx` with count, mean, min, max,
p50, p90, p99 and p99.9, in nanoseconds). `sortie-gate.sh` prints the worst
node's p50, p99 and max per target from there and judges nothing by them.
Until then the plan carried two thresholds, `latency_2xx.p50 < 60s` and
`latency_2xx.p99 < 60s`, only because a statistic could reach the report no
other way than as a threshold's `actual`; they could not fail on a mesh that
answers at all, and they are **removed, not replaced**. Latency is not a gate
until talos-main has shown what the numbers look like there, under rolls, on
nodes that are busy: on kind p99 is flat at 2–4 ms with nothing else running,
and the first talos runs read 115–207 ms. When there is a number to defend,
the place for it is a real threshold in the plan (`latency_2xx.p99 < …`),
which sortie judges per backend.

**Memory** is the engine's latency histograms, about 37 MiB per worker per
execution. On kind, same plan: 300 MiB with eight executions running, flat for
15 minutes (299.9 → 301.8 MiB, and 300.0 → 301.9 in the second run); a peak of
597–603 MiB for the instant the final reports are assembled; 270–290 MiB still
resident in the idle engine afterwards. sortie's own figures for eight
executions are 421 MiB running, 722 MiB peak and 340–400 MiB held, on an
unoptimized build. The values file requests 384 MiB with a 1 GiB limit (k6:
256 MiB / 1 GiB): a limit below the peak kills the engine as the run ends, and
512 MiB killed both engines of the first version sooner still, at their first
progress snapshot.

**`--progress` is passed, at 60 s.** Against b71b37e a progress snapshot copied
every histogram: the first one took a running engine from 300 to 550–585 MiB
and the second to ~810 MiB, so the first version passed none. Since 94cf103 a
snapshot carries summaries only, and the 15-minute runs above ran with
`--progress 30s` and `60s` at the same 300 MiB as a run without. What a
snapshot still costs is CPU: sampled in 10 s windows, an engine reads
115–128 millicores between snapshots and 173–186 in a window that holds one,
so one snapshot of eight executions is a burst of about 0.6 CPU-second — +19
millicores averaged at 30 s (131–133 measured over that run), +10 at 60 s.
Pacing and latency did not notice either. A line per target and node every
minute is the run's
liveness in the pod log and, saved, a one-minute timeline of every counter with
the latency `mean` and `max` — enough to place a failure against a 12-minute
roll schedule, and the statsd series resolve 10 s for anything finer.

Whether memory grows over 8 h is **unmeasured** (the histograms are
fixed-size, which argues no; nothing has run longer than 15 minutes here).

## Telemetry

sortie has no OTLP sink (Envoy's aborts the engine on its first flush there);
the plan's `stats` block makes each engine flush its Envoy stats store to a
statsd server over UDP, and the address must be an IP literal.

**talos-main already has the receiver.** `o11y/otel-scraper` (GitOps:
`clusters/talos-main/otel-scraper/values.yaml`) exposes `8125/UDP`, cluster IP
`10.106.234.157` on 2026-10-07, with a statsd receiver (10 s aggregation,
monotonic counters, `|ms` timers as explicit-bucket histograms), a
`deltatocumulative` processor and `job="statsd"`. It was added for Nighthawk
in `clj-grpc-soak`. `run.sh --statsd auto` (the default) resolves that
Service's cluster IP and UDP port at launch and templates it into the plan; if
the Service or the port is absent the run goes ahead without live metrics and
says so. `--statsd off` disables it; `--statsd IP:PORT` overrides. No GitOps
change is needed to receive.

Names. On the wire, since sortie 94cf103:

```
sortie.<scenario>.<target>.<backend>.cluster.<worker>.benchmark.http_2xx                  counter
sortie.<scenario>.<target>.<backend>.cluster.<worker>.benchmark_http_client.latency_2xx  timer, ms
```

and as they arrive in Prometheus (the path was verified on kind with 94cf103
against a collector with talos-main's receiver and processor settings, read
from the GitOps values; with 9fcbb81 the names were read off the wire only):

```
sortie_mesh_<target>_<backend>_cluster_0_benchmark_http_2xx_total
sortie_mesh_<target>_<backend>_cluster_0_benchmark_pool_overflow_total     (exists once non-zero)
sortie_mesh_<target>_<backend>_cluster_0_benchmark_http_client_latency_2xx_{bucket,sum,count}
sortie_mesh_<target>_<backend>_cluster_0_upstream_rq_total   … and Envoy's other cluster counters
sortie_mesh_svc_1_main_worker_03_cluster_0_benchmark_http_2xx_total        (an example)
```

`<scenario>` is `mesh`; `<target>` has `-` as `_`; `cluster_0` is worker 0, the
only one per target. **`<backend>` is the engine's node, by name**, lowercased
and reduced to `[a-z0-9_]` (`main-worker-03` → `main_worker_03`). That is
sortie 0fc5746: the plan's stats block says `backend: name`, sortie sends a
placeholder where the name goes, and each engine puts in the name it was
started with (`--backend-name`), which the chart takes from the node
(`engine.backendNameFrom: node`, set in `sortie-values.yaml`). Up to 94cf103
`<backend>` was the engine's pod IP (`10_244_3_17`), the map from it to a node
was `engines.tsv`, and every install of the engines wrote a new set of names.
The receiver attaches no labels and does not expose the sender, so the node is
still in the metric *name* and nowhere else — but it is a name that means
something and does not change. Two consequences of sortie not knowing the
names: an engine started without one refuses the execution (so the two
settings are pinned together by the harness test), and nothing checks that two
nodes' names stay distinct once reduced to `[a-z0-9_]` (talos-main's
`main-worker-01` … `-05` do). The **report** is unchanged: it names a backend
by pod IP and port, and the gate maps that through `engines.tsv`.

**The per-node series sum to the report.** With sortie b71b37e the names
carried no backend, every engine wrote the same series, and two engines read as
one engine's worth (8,085–8,101 where the report said 16,182–16,186). With
94cf103 each engine has its own. On kind, at the end of the 15-minute run:

| target | series | sum over the two backends | report `totals` |
|---|---|---|---|
| each of the six 9 rps targets | 2 (8,100 + 8,100) | 16,200 | 16,200 |
| each of the two 3 rps targets | 2 (2,700 + 2,700) | 5,400 | 5,400 |

and each series equals that backend's `results[].counters` in the report. So
the live view can now place a failure class on a node. That equality held
because it was a first run on series nobody had written before; **an absolute
value on these series is not, in general, one run's count.** The engine sends
no totals: every flush carries what each counter gained since the last one
(read off the wire on kind with f0750ec: `40|c 45|c 45|c … 44|c 5|c` for a
9 rps target on one node, which add up to the report's 540; the next run sends
`40|c 45|c …` again under the same name), and the collector's
`deltatocumulative` adds them up. Since the names stopped changing with every
install (0fc5746), a series the collector still holds carries on from the
previous run's total; it starts from zero only if the collector had expired it
or was restarted, and that can happen inside a run. On talos-main, 2026-10-08,
`sortie_mesh_svc_1_main_worker_03_cluster_0_benchmark_http_2xx_total`: 7,739
after run 4 (the report: 8,097); from zero again when the soak started 21
minutes later; **241,808** at the soak's end where the report has **275,400**,
with one reset in between; `increase()` over the soak's window: 274,864. So:
`rate()` and `increase()` across runs, never a total read as a run's count
(sortie's README says the same since f0750ec). **The grade comes from the JSON
report**: the series are UDP and a collector's memory, and the report is a
file. Nothing in `sortie-gate.sh` or in the README's grading recipe reads them.

What it costs: 33 metric families per target and backend (two of them
histograms of 16 buckets), 528 for the kind run, and so about **1,320 families,
some 2,800 series, on five nodes** — once. While the backend was a pod IP every
install of the engines wrote that many *new* names, which the previous run
stopped updating; named by node, a run writes the names the last one wrote.
(Counted with 94cf103; `http_inflight_lost` adds a family when it is non-zero.)

**Dashboard.** `aether-k6` (read 2026-10-06) is built on `k6_*` series keyed by
`testrun_name` — `k6_http_reqs_total`, `k6_http_req_failed_total`,
`k6_http_req_duration_milliseconds_bucket`, `k6_vus`, by `endpoint` and
`status` — which k6-operator test runs export. The soak runner exports none of
them (it runs without an output sink on purpose), so the dashboard has never
shown a soak. Its replacement for the soak is a small one over the
`sortie_mesh_*` series above: 2xx rate per target and per node
(`…_benchmark_http_2xx_total`, the target and the backend taken out of the name
with `label_replace`), one stat per zero-failure class
(`increase({__name__=~"sortie_mesh_.*_benchmark_(http_[45]xx|http_inflight_lost|stream_resets.*|pool_.*)_total"}[$__range])`,
the selector of the README's "Live view",
with the dashboard's range set to the run, PLUS the series born inside the
range, which `increase()` reads as 0: for each class the stat is
`(sum(increase(m[$__range]) and m offset $__range) or vector(0)) + (sum(m unless m offset $__range) or vector(0))`
(each sum defaults to zero, or an empty arm would make the whole stat "no data"):
`increase()` only for a series that already existed at the start of the
range, the current value only for one born inside it, so a first failure
cannot look clean and a new series is not counted twice), and p95 per target from the latency
histogram, which is the mesh's latency now that the engine is not throttled.
Every panel and any alert on these series is a `rate()` or an `increase()`,
never the series' value (#1420): named by node, a failure series outlives the
run that made it, so its bare value is not zero on a later clean run, and a
series born inside the window reads as 0 under `increase()`, which is why
the stat above and any alert carry the "present now, absent before" arm
(`m unless m offset <window>`, each arm with `or vector(0)`) next to `increase()`; the same rule is in the
runbook for the ext_authz counters and under "Live view" in
`e2e/soak/README.md`. The dashboards and rules that existed on
2026-10-08 were checked for this: `aether-k6` puts every `k6_*` counter under
`rate()` and reads only the `k6_vus` gauges bare, and no dashboard or rule reads
a `sortie_*` series yet. It
is a GitOps change (sidecar ConfigMaps in `k8s-talos-main`), made in phase 2,
once the first talos run has shown what the series look like there. Retiring
`aether-k6` and `k6-operator` is phase 4, and only if nothing else still uses
them.

## Report durability

- The sortie pod writes the JSON report to
  `/var/run/sortie/<run tag>.json` on PVC `sortie-soak-reports` (`run.sh`
  creates it; `openebs-hostpath` on talos-main, which has no default class, so
  `run.sh` takes the cluster's only class, or `--storage-class`). One small
  file per run; the PVC is kept across runs.
- `sortie-save.sh` is armed, detached, as soon as the Job exists and before
  `run.sh` first looks at its pod (#1387: armed after that look, it was never
  armed for a Job that had already ended, and such a run's report is the one
  thing that says why it ended). When the Job finishes it
  copies into the run directory: the report (through a short-lived reader pod
  that prints the file — `kubectl exec` is denied on talos-main), the sortie
  pod's log (the progress lines and the readable summary), the Job and pod
  objects, every engine's log, and the engine pod list. It writes `SAVED` last, only if the report
  parsed.
- `sortie-teardown.sh` refuses to uninstall without `SAVED` (`--force`
  overrides), and keeps the PVC unless `--purge`.
- The Job has no TTL, so the pod and its log stay until teardown.
- **The results stream** (sortie 9fcbb81): beside the report, on the same PVC,
  sortie appends one line of JSON per execution as it finishes
  (`/var/run/sortie/<run tag>.jsonl`; `--results-stream`, the chart's
  `report.stream`), synced to disk line by line. It is what exists if the
  sortie pod dies before the report is written — risk 4's eight lost hours, in
  the case where the executions had finished. The saver copies it out
  (`results.jsonl`) whether or not there is a report, and `sortie-gate.sh
  --stream` grades it. sortie appends to that file and never truncates it, so
  `run.sh` names it by run tag (a file of the run's own) and the gate fails a
  label that occurs twice; a run that died mid-write leaves a last line that
  does not parse, which the saver counts and the gate skips — the execution it
  was about is then missing, which is a FAIL. With this plan every execution
  ends at the same moment, so the stream is written seconds before the report;
  it protects that window, and any later plan with stages.
- **When the run was.** Every execution in the report has `started_at` and
  `ended_at` (sortie's clock) and every backend its own `started_at` (the
  engine's clock, when its first worker started). The saver writes them as
  `times.tsv` and the gate prints one `WINDOW` line, so a failure can be set
  beside the `ROLLED` lines of `churn.log` without reconstructing the start
  from `T_LOAD`.

## What is in the repository now (#1323)

| file | role |
|---|---|
| `e2e/soak/run.sh` | the one kickoff: pre-flight (with the provenance check) → PriorityClass → engines → load → churn (soak) → watchdog at T0 → saver |
| `e2e/soak/sortie-values.yaml` | chart values: DaemonSet, mesh label, PriorityClass (engines and the sortie pod), resources, `--progress`, digests |
| `e2e/soak/sortie-targets.txt`, `sortie-plan.sh` | the eight targets and weights; the plan template for both profiles and the share arithmetic |
| `e2e/soak/sortie-save.sh`, `sortie-gate.sh`, `sortie-teardown.sh` | save before teardown; PASS/FAIL per target with per-node attribution; uninstall |
| `e2e/soak/pods-not-ready.awk` | the numeric readiness check |
| `e2e/soak/harness_test.sh` + `testdata/{sortie,preflight}/` | offline tests of the gate, the plan, the pins and the readiness check (`//e2e/soak:harness_test`) |

The two #1323 defects: the watchdog is started by `run.sh` right after the
churn driver (soak) or the load (e2e), never after the kickoff returns; the
readiness check compares ready and total as numbers, and the test suite keeps
the old expression as a red reading (it flags all eight healthy pods of the
fixture).

Deploying aether is deliberately not in `run.sh`. The nightly wrapper's
"wait for main to publish, deploy, check surge/debug/limits" half stays a
separately authorized `helm upgrade`.

## What sortie 94cf103 retired

The first version of this harness (#1339) was built against sortie b71b37e and
worked around seven things. Each was reported to the sortie session; 94cf103
(main, 2026-10-07) fixed all of them, and the harness dropped each workaround
after re-running the case it had been written for.

| sortie b71b37e | the harness's workaround | sortie 94cf103 | the harness now |
|---|---|---|---|
| an execution ended at its first 4xx / 5xx / reset / connection failure, and then reported no counters | `nighthawk_template.failure_predicates`, four limits of 10¹² | sortie turns the default predicates off; a stopped execution still reports its counters | no `failure_predicates` in the plan. Deliberate failure re-run: 240 s of 240 s on both nodes, `http_5xx=552`, only that target fails |
| a worker thread never blocked: 8.0 / 2.85 / 1.7 cores per engine (`SPIN` / `POLL` / `SLEEP`) | `SLEEP`, a 400m CPU limit, and `max_pending_requests: 16` so the throttled engine queued instead of refusing | the `WAIT` idle strategy (opt-in, through the template) | `WAIT`, a 150m request, **no CPU limit**: 0.11–0.14 core, every request sent, p99 2–4 ms instead of 20–100 ms. The pending queue went with the limit and **came back after the first talos run** as a sized client queue ("The client queue"): open-loop load needs one on nodes that stall, limit or no limit |
| a `--progress` snapshot copied every histogram (300 → 550–810 MiB, OOM at 512 MiB) | no `--progress` | snapshots carry summaries; memory is the same with and without | `--progress 60s`: 300 MiB flat |
| statsd names carried no backend: every engine wrote one series | "the live series are one engine's worth; grade from the report" | `<backend>` (the engine's address, sanitised) in the prefix | per-node series that sum to the report; the caveat is gone, the IP → node map is documented |
| the JSON report held thresholds and non-zero failure counters only | a `counter:benchmark.http_2xx > 0` threshold as a carrier for the 2xx total | per execution: `results` (per backend), `totals`, `backend_errors` | the carrier is dropped; `sortie-gate.sh` reads the three, judges rate and failures **per node**, and refuses an old-format report |
| one dead engine failed every execution with an `error`, and the other nodes' counters were lost with it | none possible; the run was void | a lost backend is named in `backend_errors`, the others are reported whole | the gate fails each target for the lost node, names it, and still judges the survivors (kind: one engine pod deleted at T+60 s) |
| the chart was unsigned and named the engine by tag | both images pinned by digest in the values file, the chart by digest in `run.sh` | the chart is signed and defaults both images to digests | all three still pinned here, and **verified on every run** by `run.sh`'s pre-flight |

Progress lines also carry the execution label now (`mesh/svc-1  10.0.0.11:8443
…`), which is what makes them usable as the per-target, per-node timeline the
k6 `AETHER_FAIL` lines used to be the only source of.

## What sortie 9fcbb81 changed

The second bump, 94cf103 → 9fcbb81 (eight commits on sortie main, all of
2026-10-07). Every name below was read in the sortie source at that commit
(`internal/report/report.go`, `internal/run/run.go`, `internal/compile/`,
`api/sortie/plan/v1/plan.proto`, the chart) before the harness used it.

**What the harness dropped, because sortie now does it:**

| the harness did | why | sortie commit | the harness now |
|---|---|---|---|
| `nighthawk_template.sequencer_idle_strategy: WAIT` in every plan | `WAIT` was opt-in and the engine's default costs a core per worker | 7f338df: `WAIT` is sortie's default; a strategy named in the template is left alone | nothing is templated. `--idle-strategy` / `SOAK_SORTIE_IDLE_STRATEGY` still write the template, for an experiment |
| two thresholds that could not fail, `latency_2xx.p50 < 60s` and `latency_2xx.p99 < 60s` (and `sortie-plan.sh --no-latency` to drop them) | the report held no statistic; a threshold's `actual` was the only way in | 1b3d404: `results[].statistics` | both removed, the flag with them; the gate reads the statistics and prints p50, p99 and max. No latency threshold replaces them yet |
| statsd series named by pod IP, with `engines.tsv` as the only map to a node, and ~2,800 new series per install of the engines | sortie knew a backend only by its address | 0fc5746: `stats.backend: name` and the chart's `engine.backendNameFrom` | the plan asks for `backend: name`, the values file for `backendNameFrom: node`: the series carry the node's name and are the same on every run |
| the saver's `SORTIE_OVERDUE` line at two minutes, the only notice that sortie was waiting for a silent node without limit; the gate's `ran off plan` as the only thing that failed a thawed one | sortie had no deadline on a backend | 76f699b: a backend that has not answered `duration + timeout + drain + 2 min` after dispatch is given up on and reported; one whose own account runs far past the plan is failed, its counters kept | `SORTIE_OVERDUE` moves to five minutes and means sortie itself is stuck. The gate's own 5 % check stays: it is tighter than sortie's two minutes |

**What stays, and why:**

- **`max_pending_requests`.** The engine's default is still 0 ("no client side
  queuing") and sortie still passes the field only when the plan sets it
  (`options_impl.h`, `compile.go` at 9fcbb81). Nothing better to default to;
  the sized queue stays.
- **No CPU limit, a 150m request.** These were never workarounds for sortie;
  the chart sets no resources and the values file's are the harness's.
- **`engine.maxConcurrentExecutions: 16`.** It is the chart's own default and
  was at 94cf103 too, so it is redundant — and kept, because
  `sortie-plan.sh` refuses a targets file longer than that number and the two
  should be read in one place.

**What the harness gained:**

| sortie 9fcbb81 | commit | the harness |
|---|---|---|
| an HTTP run waits for its in-flight requests, up to the request timeout, before it reads the counters; what is still open then is `benchmark.http_inflight_lost` (not a reset, not a default failure predicate). The engine's elapsed time is fixed at stop | 1178261 | `counter:benchmark.http_inflight_lost == 0` in the plan; in the gate's zero-failure set, worded apart (`in flight at the end: N request(s) with no outcome`); the count is judged as a 99–101 % range against the configured duration |
| a start refused at an engine's execution cap (`RESOURCE_EXHAUSTED`) stops the whole stage on every backend; later stages are `SKIP` in the text and `"not_run": true` in the JSON; the summary reads `N/M executions passed, K not run` | 1374999 | the gate fails a `not_run` execution by name, counts it (`not_run=K`), reads the pod's log for `SKIP` and `K not run` as a second witness, and words a refused stage `REFUSED AT THE EXECUTION CAP` instead of calling the refusing engines lost |
| `started_at` / `ended_at` per execution, `results[].started_at` per backend | 9fcbb81 | `times.tsv` in the run directory, a `WINDOW` line from the gate; a report without `started_at` is refused as old |
| a `PASS`/`FAIL`/`SKIP` line on stderr per finished execution | 9fcbb81 | in `sortie.log`, with or without `--progress` |
| `sortie run --results-stream FILE`, the chart's `report.stream`: one JSON line per finished execution, appended, never truncated | 9fcbb81 | `run.sh` sets it to a file of the run's own; the saver copies it out; `sortie-gate.sh --stream` grades it, skipping lines that do not parse and failing a label listed twice ("Report durability") |

**Not used:** the TCP work of 83d7d47 (reconnect with backoff,
`tcp.max_messages_per_connection`, `benchmark.tcp_reconnects` /
`tcp_connections_rotated` / `tcp_connections_opened` / `tcp_connect_failures` /
`tcp_unavailable` / `tcp_inflight_lost` / `tcp_echo_mismatch`,
`benchmark_tcp.connect_latency`). The `mp-dialer` and `udp-dialer` legs stay
as they are until sortie can do what they do. What is still missing there, by
sortie's own account: an upstream idle-timeout option, telling a remote close
from a local one, UDP socket rotation, and per-target overrides in a scenario
that mixes schemes — the last is what would let a raw-TCP target sit beside the
eight HTTP ones in the one `mesh` scenario. A separate TCP *scenario* against
`mixed-svc`'s raw port is now straightforward to write (an exact-echo target,
`expect_echo: true`, a reconnect that survives a roll, rotation to keep
connection setup in the measurement) and is left as a follow-up: scenarios run
one after another, not side by side, so it would be a second run, not a ninth
target.

## What sortie f0750ec changed

The third bump, 9fcbb81 → f0750ec (one commit on sortie main, 2026-10-08,
answering findings of the previous bump). It touches the driver only:
`internal/nh`, `internal/report`, `internal/run` and the README. Each item
below was read in the sortie source at that commit before the harness used it.

**The pins, and a digest that had to be looked up.** Chart
`sha256:b1f0a6d5…`, driver `sha256:3d9a9b67…`, engine `sha256:d0fbc860…`, all
resolved from the registry and verified with the pinned cosign (v3.1.2): the
chart, and the index and both per-arch manifests of each image, bound to the
commit. The engine "is unchanged from 9fcbb81", and its digest is new all the
same (9fcbb81's index was `sha256:99d5058c…`). Both are true: on each
architecture the two images have the same 22 layers, digest for digest, and
configs that differ in one label, `org.opencontainers.image.revision`, which
is the commit. A new config is a new manifest and a new index. So every digest
moves with every sortie commit, a bump always changes all three pins, and "the
engine did not change" has to be read off the layers. What is pinned is what
the f0750ec chart names; the 9fcbb81 engine index does not verify against the
f0750ec commit.

| sortie f0750ec | the harness |
|---|---|
| an execution of a stage refused at an engine's execution cap has `"refused": "execution_cap"` in the report and in the results stream; so does each `backend_errors` entry that is an engine which refused. The stages after it keep `"not_run": true` (and no `refused`) | `sortie-gate.sh` knows a refusal by the field. An entry of `backend_errors` without it is a lost backend, also inside a refused stage, which the text match could not tell. The error text is still read for one shape only: an execution with no `refused` field at all, which is a 9fcbb81 report, and those are still graded with this gate (run 4 and the first soak are 9fcbb81 reports) |
| a backend's answer that is already there when the driver notices the deadline has passed, or arrives within 10 s of that, is taken as the result. The deadline is wall time, and a driver that was itself stopped can wake past it with every result waiting; 9fcbb81 then picked between "answered" and "overdue" at random | nothing to change. It is what the frozen-driver run of the 9fcbb81 evidence showed (the healthy node's engine reported silent on four of eight executions); re-run below |
| the README says what a receiver shows across runs on series named by backend name: the engine sends per-flush deltas, so a kept series carries on from the last run | "Telemetry" and the README's "Live view" now say it, with what talos-main's Prometheus showed. Nothing in the harness read a total as a count |

Not in f0750ec, and still done by the harness: `run.sh` used to abort on a
Job that had already ended **before** it armed the saver (the 9fcbb81 evidence
below saved that run by hand). The saver is now armed first and the abort
waits for it (#1387).

## What sortie 96e6bfb changed

The fourth bump, f0750ec → 96e6bfb (three commits on sortie main, all
2026-10-08: 6d801c0, 6a0a866, 96e6bfb). Read in the sortie source: the diff
between the two commits touches no file under `internal/`, `api/`, `charts/`,
`main.go` or `go.mod`. So **no flag, no field of the JSON report or of the
results stream, no exit code and no chart value changed**, and the chart pulled
by its two digests differs in three lines: its version, and the two default
image references. The harness's scripts and fixtures are unchanged by this
bump; only the pins and what the documents say about them moved.

| sortie | what it is | the harness |
|---|---|---|
| 6d801c0 | no image carries the commit any more (`org.opencontainers.image.revision` is gone from both images, and so is the stamping). An image whose content did not change keeps its digest. The publish workflow verifies a digest before it signs and signs only what is not signed yet | the README and `sortie-values.yaml` said "all three digests change with every sortie commit". That was true up to f0750ec and is corrected to what the registry shows (below) |
| 6a0a866 | **the engine**: two executions that start at once each construct Envoy's options, which writes a process-wide delimiter in the argument parser (TCLAP); the construction is now serialized. Found by sortie's ThreadSanitizer run. The rest of the commit is tests and the sanitizer job | nothing to change, and nothing to remove: the harness never worked around it. It is the harness's own case, eight executions started together on every engine, so the path ran on every run so far. Every writer stored the same character (sortie's commit message), which would make the race harmless in practice; that is sortie's reading, not something a run here can show |
| 96e6bfb | an engine **test** only (a progress snapshot may go out without statistics on a slow sanitizer host) | nothing. It is the commit that shows the first row working: its engine index is 6a0a866's |

**The digests, measured.** From the registry, each commit's
`dev-<commit>` tag for the two images and the chart's version tag, with every
per-arch manifest and config read:

| sortie commit | chart | driver index | engine index | engine layers (amd64 / arm64) | `revision` label |
|---|---|---|---|---|---|
| f0750ec | `sha256:b1f0a6d5…` | `sha256:3d9a9b67…` | `sha256:d0fbc860…` | 22 / 22 | yes |
| 6d801c0 | `sha256:446caa71…` | `sha256:d1ff3f59…` | `sha256:94bde035…` | the same 22 / 22, digest for digest | no |
| 6a0a866 | `sha256:a4dbd82d…` | `sha256:f4ababe0…` | `sha256:65392801…` | 22 / 22, a different set | no |
| 96e6bfb | `sha256:b30a4266…` | `sha256:490dfbd6…` | `sha256:65392801…` | as 6a0a866, and the same two per-arch manifests | no |

So the chart and the driver have a digest per commit (the chart's version is
the commit; the driver's binary reports its own version) and the engine has a
digest per engine. One commit shows it, 6a0a866 → 96e6bfb; 6d801c0 is the
counter-example that is not one, since removing the label was a change of the
image's config. At a bump all three are still taken from the new chart.

**The signatures, and what binding to a commit now means.** With the pinned
cosign (v3.1.2), signer `…/bpalermo/sortie/.github/workflows/publish.yml@refs/heads/main`,
issuer `https://token.actions.githubusercontent.com`, on the chart and on the
index and both per-arch manifests of each image, seven references:

| check | chart, driver ×3 | engine ×3 |
|---|---|---|
| the signer alone (what `run.sh` checks) | verified, one signature each | verified, one signature each |
| `--certificate-github-workflow-sha 96e6bfb…` | verified | **refused**: `expected GithubWorkflowSHA to be "96e6bfb…", got "6a0a866…"` |
| `--certificate-github-workflow-sha 6a0a866…` | refused (`got "96e6bfb…"`) | verified |
| `--certificate-github-workflow-sha f0750ec…` | refused | refused |

A digest is signed once, by the commit that first published it, so the engine
of this pin is bound to the commit that last changed it and not to the pin.
The earlier bumps recorded "bound to the commit" for all three; from here that
holds for the chart and the driver only. `run.sh` never bound to a commit: its
pre-flight verified the 96e6bfb chart and both images with no change
(`verified=cosign-v3.1.2`). The pinned cosign prints no commit for a signature
it accepts (`"optional": {}`), so the commit that signed a digest is read off
the refusal.

`//e2e/soak:harness_test` already held `run.sh` and `sortie-values.yaml` to
one commit. It now holds the README to it too: the sentence that states the
pin, and the chart digest in the cosign command a reader would copy (each seen
red with the previous pin's text).

## Evidence on kind (2026-10-08, sortie 96e6bfb)

The same cluster shape, aether install (from the tree this bump was made on),
stand-in targets and UDP listener as for f0750ec below; every run through
`run.sh e2e --verify` (`verified=cosign-v3.1.2`, chart `sha256:b30a4266…`).
Run directories: `~/aether-soak-logs/1008-sortie-96e6bfb-kind/`.

| run | result |
|---|---|
| 5 min, the committed values (the f0750ec run, like for like) | 2,700 of 2,700 per 9 rps target on each node, 900 of 900 per 3 rps target: **36,000 of 36,000**; every failure class 0; each backend's elapsed time 300,000–300,004 ms; p50 1.7–2.5 ms, p99 2.3–3.5 ms; `VERDICT PASS targets=8 passed=8 failed=0 not_run=0 backends=2 lost_backends=0`; watchdog `verdict=PASS new_restarts=0`; `SORTIE_SAVED … not_run=0 … stream_executions=8`, the stream's eight lines the report's eight executions, object for object |
| the e2e profile as it is, 15 min, nothing overridden | 8,100 of 8,100 per 9 rps target on each node, 2,700 of 2,700 per 3 rps target: **108,000 of 108,000**; every failure class 0; each backend's elapsed time 900,001–900,002 ms; p50 1.9–2.6 ms, p99 2.5–3.5 ms; `VERDICT PASS … 8 of 8`; watchdog `verdict=PASS new_restarts=0` over 30 samples; stream and report identical |
| the engines capped below the plan (`engine.maxConcurrentExecutions: 4`, eight targets) | as with f0750ec: the Job failed at once (window 50 ms), `saver armed` before the abort, the abort 46 s later with `SORTIE_SAVED … job=failed executions=8 … stream_executions=8`. All eight executions carry `"refused": "execution_cap"`, every `backend_errors` entry is marked, and the report has the same set of fields as the committed fixture `kind-capped.json` (every path of the two documents compared). Gate: eight `REFUSED AT THE EXECUTION CAP`, `lost_backends=0` |

**What differs from the f0750ec runs: nothing the harness reads.** The 5-minute
report has the same keys at every level, the sortie pod's log the same 201
lines (column padding aside), the Job the same arguments, each engine's log the
same 698 lines with no warning or error, and the counts are the same to the
request.

The engine, a new binary at this bump (risk 3), read on each node from its
pod's cgroup: **0.099 core** per engine over three minutes of the 5-minute run
and **0.107** over ten minutes of the 15-minute run, `nr_throttled` 0, 300.6 →
302.1 MiB. The previous engine read 0.09–0.11 core and 301 MiB on the same
shape.

**Not run against 96e6bfb:** any cluster but kind; anything longer than
fifteen minutes; `--rolls`; the frozen-driver and staircase runs
(the driver's code did not change between the two pins); an engine pod deleted
mid-run. Nothing here can show the race that 6a0a866 fixes, or its absence:
the eight executions of every run above start together, which is the path, and
a clean run is what f0750ec gave too.

While comparing: the two earlier 5-minute rows below said 43,200 of 43,200.
Their saved reports sum to 36,000 (60 rps on two nodes for 300 s), and the
rows now say so.

## Evidence on kind (2026-10-08, sortie f0750ec)

The same cluster shape, aether install, stand-in targets and UDP listener as
for 9fcbb81 below; every run through `run.sh e2e --verify`
(`verified=cosign-v3.1.2`, chart `sha256:b1f0a6d5…`). Run directories:
`~/aether-soak-logs/1008-sortie-f0750ec-kind/`.

| run | result |
|---|---|
| 5 min, the committed values | 2,700 of 2,700 per 9 rps target on each node, 900 of 900 per 3 rps target: **36,000 of 36,000**; every failure class 0; each backend's elapsed time 300,000–300,003 ms; p50 1.9–3.0 ms, p99 2.4–3.8 ms; `VERDICT PASS targets=8 passed=8 failed=0 not_run=0 backends=2 lost_backends=0`; watchdog `verdict=PASS new_restarts=0`; `SORTIE_SAVED … stream_executions=8` |
| the engines capped below the plan (`engine.maxConcurrentExecutions: 4`, eight targets) | the Job failed at once (the report's window is 50 ms). **`run.sh` had armed the saver before it looked**: `saver armed`, then 45 s later `ABORT the sortie pod … is 'Failed', not Running … what it wrote is saved … [saver: SORTIE_SAVED … job=failed executions=8 pass=false not_run=0 … stream_executions=8]` (the 45 s are the saver's one 30 s poll for a Job not yet marked failed, and two reader pods). All eight executions carry `"refused": "execution_cap"`; five of them list one or both engines in `backend_errors`, each entry marked, and three list none. Gate: eight `REFUSED AT THE EXECUTION CAP … [refused by: …]`, `lost_backends=0`. This report and its stream are the fixture `kind-capped.*` |
| the three-stage staircase of five targets against the same capped engines | stage 1 refused on all five targets (`refused` on each; `backend_errors` on one of them, both engines, both marked); the ten executions of stages 2 and 3 are `"not_run": true` with no `refused`; the stream has the same fifteen objects; the log ends `FAIL  0/5 executions passed, 10 not run`. Four of the executions and the log's lines about them are the fixture `kind-refused.*` |
| 2 min, and **the driver's node** frozen (`docker pause`) from T+46 s to T+300 s, which is 29 s past the 271 s backend deadline and before anything is evicted | the report was written 2 s after the thaw. All eight executions have results from **both** backends. The healthy node's engine is in no `backend_errors`: its complete results, waiting since T+120 s, were taken. The frozen node's own engine is failed on all eight, as it should be: `reported an execution of 5m1.011s for one planned to last 2m0s: it stalled, and its results are not those of the plan`, counters kept. Gate: `lost_backends=1`; the healthy node reads whole for the two UDS targets (360 of 360) and `http_5xx` for the six whose only target pod was on the frozen node |
| the same, frozen for ten minutes (the 9fcbb81 case, like for like) | `SORTIE_OVERDUE` at five minutes; the pod was evicted with its node and its log lost, as before; on the thaw sortie wrote the report and the stream before it was killed. Again results from both backends on all eight executions and only the frozen node's own engine failed (`reported an execution of 10m46.876s …`): `lost_backends=1`, where 9fcbb81 read 2. Sixteen of sixteen executions over the two runs took the healthy backend's waiting answer; with 9fcbb81 four of eight did not |
| two 60 s runs one after the other, the listener on the control-plane node | on the wire, `sortie.mesh.tcp_a.sortie_worker.cluster.0.benchmark.http_2xx`: `40\|c 45\|c 45\|c 45\|c 45\|c 45\|c 45\|c 46\|c 45\|c 45\|c 45\|c 44\|c 5\|c` in the first run, which is the report's 540, then `40\|c 45\|c …` again under the same name in the second. Deltas, as sortie's README says; what a receiver makes of them is in "Telemetry" |

**Not run against f0750ec:** talos-main; anything longer than five minutes;
`--rolls`; an engine pod deleted mid-run; a backend that answers *within the
ten seconds after* a missed deadline rather than before it (both frozen runs
had the answer waiting). The sortie session could not reproduce the frozen
driver by freezing a node; the two runs above did, by pausing the kind node
that holds the report PV, and therefore the sortie pod.

## Evidence on kind (2026-10-07, sortie 9fcbb81)

The same three-node kind cluster shape as below (pinned kind v0.33.0, one
control plane and two workers, on a kubeconfig of its own), aether from this
tree as `e2e/uds.sh up` installs it (SPIRE off, `uds-echo`, `uds-cr-echo`,
`tcp-echo`), and the committed values and plan with the same stand-in targets
file (six 9 rps targets on `tcp-echo`, the two UDS targets at 3 rps). In place
of a collector, a UDP listener as `o11y/otel-scraper` that prints what arrives:
it shows the names on the wire and nothing about Prometheus. Every run went
through `run.sh e2e --verify` (`verified=cosign-v3.1.2`).

| run | result |
|---|---|
| 5 min, the committed values | pool resolved to 2 backends; 2,700 of 2,700 2xx per 9 rps target on each node, 900 of 900 per 3 rps target: **36,000 of 36,000**; every failure class 0, `http_inflight_lost` among them (the threshold is in the report, `actual 0`); each backend's elapsed time 300,001–300,002 ms and each execution's 300.77–300.80 s, so nothing was in flight long enough to show; latency from `statistics`: p50 1.8–2.5 ms, p99 2.2–3.0 ms, max 6.6–79.7 ms; `VERDICT PASS targets=8 passed=8 failed=0 not_run=0 backends=2 lost_backends=0`; watchdog `verdict=PASS new_restarts=0`; `SORTIE_SAVED … not_run=0 window=…23:26:33.393Z..…23:31:34.192Z stream_executions=8`. The Job ran `--results-stream /var/run/sortie/<run tag>.jsonl`; the stream's eight lines are the report's eight executions, object for object (both compacted with `jq -c` and compared: identical). The engines ran `--backend-name $(SORTIE_BACKEND_NAME)` from `spec.nodeName`, and the datagrams on the wire were named `sortie.mesh.tcp_d.sortie_worker.cluster.0.…` |
| the engines capped below the plan (`engine.maxConcurrentExecutions: 4`, eight targets) | sortie 1374999: the Job failed five seconds after it started, every execution with `error: backend … refused a start because the engine is at its cap of 4 concurrent executions, and this scenario starts 8 at once on every backend … Raise the engine's --max-concurrent-executions … to at least 8`; nothing ran (the report's window is 49 ms). `run.sh` aborted (`the sortie pod … is 'Failed', not Running`) before arming the saver, which then saved the report and the stream by hand; the gate fails all eight. This run is why the gate now words a refused stage `REFUSED AT THE EXECUTION CAP` (it first listed the refusing engines as lost backends) and why the abort says how to read the report |
| a three-stage staircase of five targets against the same capped engines (a plan written by hand; `run.sh` cannot render one) | the real `not_run`: stage 1 refused as above on all five targets; the ten executions of stages 2 and 3 are `"not_run": true` with `elapsed_ms: 0`, no `started_at`, no counters, and `error: not run: mesh/stage-1 was refused …`; the pod's log has a `SKIP` line for each, twice (as each "finishes" and in the summary), and ends `FAIL  0/5 executions passed, 10 not run`. The saver: `SORTIE_SAVED … executions=15 pass=false not_run=10 … stream_executions=15`. One target's three stages and the log's lines about them are the test fixture `kind-not-run.*` |
| 11 min with `--rolls` (agent roll at T+3m, proxy roll at T+8m) | both `ROLLED` (23:41:50Z and 23:47:27Z, inside `WINDOW dispatched=…23:38:41.939Z ended=…23:49:42.701Z`, which is what the line is for); 5,940 of 5,940 per 9 rps target and 1,980 of 1,980 per 3 rps target on both nodes, 79,200 of 79,200; every failure class 0, `http_inflight_lost` 0; p99 ≤ 3.8 ms, max ≤ 80 ms; `VERDICT PASS`; watchdog `verdict=PASS new_restarts=0` |
| 2 min, and the node that holds **the sortie pod** frozen (`docker pause`) from T+46 s for ten minutes — a mistake in the test (the report PV pins the sortie pod to one worker, and that was the one paused), kept because of what it showed | the driver froze with its node, so nothing could end the run; the saver said `SORTIE_OVERDUE` at five minutes; the pod was evicted with the node and its log lost (`sortie.log`: nothing). On the thaw sortie applied its backend deadline at once (`no result 4m31s after dispatch, for an execution planned to last 2m0s: the backend went silent or is far behind`) — to the frozen node's engine on all eight targets, and on four of the eight **also to the healthy node's engine**, whose complete results (1,080 of 1,080) were waiting unread — and wrote the report and the stream before it was killed: `SORTIE_SAVED … job=failed executions=8 … stream_executions=8`, gate `VERDICT FAIL … lost_backends=2`. Risk 4 in a new light: a frozen driver node costs the run, but no longer everything it recorded |
| 2 min, and the **other** worker frozen from T+45 s — the node with an engine and, as it happened, every target pod, but not the sortie pod; never thawed until the Job had ended | sortie 76f699b: the Job ended at T+307 s **with the node still frozen**, the report written 301.08 s after dispatch (181 s past the planned end: 151 s of deadline, 30 s for a cancellation the frozen engine could not answer). The frozen engine is in `backend_errors` on all eight executions (`no result 4m31s after dispatch, for an execution planned to last 2m0s: the backend went silent or is far behind (… the service did not answer the cancellation within 30s)`) and returned nothing; the other node is reported whole and judged: `FAIL tcp-a rate=3.44/18rps http_2xx=413/2160 backends=1/2 -- LOST BACKEND sortie-worker (10.10.1.17:8443) [returned nothing] \| survivors sortie-worker2=FAIL \| failures http_5xx=667 [sortie-worker2=667] \| http_2xx below 99% of the planned 1080 on [sortie-worker2=413]` — 413 + 667 = 1,080: every request that node was asked for has an outcome (the targets were on the frozen node, and the mesh answered for them with 5xx), `http_inflight_lost` 0. `VERDICT FAIL targets=8 passed=0 failed=8 not_run=0 backends=2 lost_backends=1`; no `SORTIE_OVERDUE`. With 94cf103 this run had no end and no report |

The engine with `WAIT` as sortie's default and nothing in the plan: **0.09–0.11
core per engine** (cgroup `cpu.stat`, a 40 s and a 60 s window with a progress
snapshot and a proxy roll in it), `nr_throttled` 0, 301 MiB while running. That
is at or under what 94cf103 read with `WAIT` templated (0.11–0.14).

**Not run against 9fcbb81:** a 15-minute run (5 and 11 minutes here); the
deliberate-failure run (a Deployment scaled to zero); an engine pod deleted
mid-run; the collector with talos-main's statsd receiver settings, and so the
series' names in Prometheus and their sum against the report; a real
`http_inflight_lost` (it needs a target that holds a request open across the
end of a run for more than 30 s; the fixture for it is canned from the schema).

## Evidence on kind (2026-10-07, sortie 94cf103)

A three-node kind cluster (pinned kind v0.33.0, one control plane and two
workers), aether from this tree as `e2e/uds.sh up` installs it (SPIRE off,
`uds-echo`, `uds-cr-echo`, `tcp-echo`), a collector with talos-main's statsd
receiver settings as `o11y/otel-scraper`, and the committed values and plan
with a stand-in targets file of the same shape (six 9 rps targets on
`tcp-echo`, the two UDS targets at 3 rps). Every run went through `run.sh e2e`,
with the provenance check on (`verified=cosign-v3.1.2`).

| run | result |
|---|---|
| SHORT profile, 15 min, the committed values | pool resolved to 2 backends, one per worker node; 8,100 of 8,100 2xx per 9 rps target on each node, 2,700 of 2,700 per 3 rps target: 108,000 of 108,000; every failure class 0; p50 1.8–2.4 ms, p99 2.1–2.9 ms; `VERDICT PASS targets=8 passed=8 failed=0 backends=2 lost_backends=0`; watchdog `verdict=PASS new_restarts=0`; engine 0.125–0.14 core with no limit, `nr_throttled` 0, 300 → 302 MiB; the per-node statsd series sum to the report for all eight targets; both pods `priority=1000`. An earlier 15 min with `--progress 30s`: the same counts, 0.13 core, 300.0 → 301.9 MiB |
| one target's Deployment scaled to 0 for 90 s of 240 s, **no `failure_predicates` in the plan** | every execution ran its 240 s. `FAIL uds-cr-echo rate=3.7/6rps http_2xx=888/1440 backends=2/2 -- failures http_5xx=552 [sortie-worker=276 sortie-worker2=276] \| rate below 99% of 3 rps on [sortie-worker=1.85 sortie-worker2=1.85]`; the seven other targets `PASS` at 4,319–4,320 of 4,320 and 1,440 of 1,440; `VERDICT FAIL targets=8 passed=7 failed=1`; gate exit 1. The progress lines place it: `http_5xx` 0 → 84 → 174 → 264 → 276 at 30 s steps (this run passed `--progress 30s`), then `http_2xx` climbing again |
| one engine pod deleted at T+60 s of 240 s | `backend_errors` names `10.10.2.23:8443` on all eight executions (`awaiting execution response: … Unavailable … "Server shutdown"`) and the report has no execution-level `error`. Gate: `FAIL tcp-a rate=9/18rps http_2xx=2160/4320 backends=1/2 -- LOST BACKEND sortie-worker2 (10.10.2.23:8443) [returned nothing] \| survivors sortie-worker=ok`, for all eight targets; the surviving node's counts are whole (2,160 of 2,160, 720 of 720, every class 0); `VERDICT FAIL … lost_backends=1`. sortie's own rate threshold **passed** for the six 9 rps targets (one node at 9 rps clears the plan-wide 5.94), which is why the gate reads `backend_errors`. The restart watchdog says `PASS`: a replaced pod is not a restart |
| 11 min with `--rolls` (agent roll at T+3m, proxy roll at T+8m) | both `ROLLED`; 5,940 of 5,940 per 9 rps target and 1,980 of 1,980 per 3 rps target on both nodes; every failure class 0; p99 ≤ 3.6 ms; `VERDICT PASS`; watchdog `verdict=PASS new_restarts=0` |
| the PriorityClass deleted before a run, and again between the engines and the Job | the pre-flight said `absent; the run creates it`; both times `run.sh` applied it and read it back, and the Job's pod came up with `priorityClassName=aether-soak-loader priority=1000` |
| every engine pod deleted at T+45 s of 120 s | the Job fails at T+77 s; each execution has an `error` and both backends in `backend_errors`, no `results`. Gate: `FAIL tcp-a rate=?/18rps backends=0/2 -- LOST BACKEND sortie-worker (…) [returned nothing] \| LOST BACKEND sortie-worker2 (…) [returned nothing] \| no backend survived`, `lost_backends=2`; the saver still saved the report |
| one node frozen (`docker pause`, so no FIN and no RST) from T+45 s of 120 s | **a sortie defect, not a pass.** sortie did not treat the silent engine as lost: the Job was still running 4 minutes later in one run and **20 minutes later** in another (19 minutes past the plan's end), and finished only when the node was thawed. The engine then answered, and its result was accepted with `elapsed_ms` 289,510 and 1,263,237 for a 120,000 ms execution, with no `backend_errors`. The gate fails it (`ran off plan (120s) on [sortie-worker=289.51s] \| rate below 99% …`), and the saver now logs `SORTIE_OVERDUE` two minutes past the planned end; neither can produce the report sortie has not written. See "Risks" |
| pre-flight provenance, negative controls | no cosign: `provenance NOT VERIFIED`, run allowed; the same with `--verify`: abort; a cosign 2 on `PATH`: abort ("older than 3"); the b71b37e chart digest (unsigned): `no signatures found`, abort; an engine image by tag: abort before cosign is asked |

The first version's evidence, against b71b37e on 2026-10-06, is in #1339: a
clean 15-minute run under the 400m limit (16,184–16,185 of 16,200 per mesh
target), the same deliberate failure with the predicates lifted by hand, the
rolls, and both engines OOM-killed at 512 MiB by the first progress snapshot.

The clean roll result is a kind result: one replica per target, SPIRE off, a
20-core host. It shows the driver does not manufacture failures at a hot
restart; it says nothing about what a roll costs on talos-main.

## Risks

1. **Concurrent executions are tested upstream over HTTP/1 only.** The soak
   drives HTTP/1.1 only, so this is inside what sortie tests. It rules out, for
   now, moving the multi-protocol or UDP dialer legs (`mp-dialer`,
   `udp-dialer`) onto sortie's TCP/UDP modes alongside the HTTP targets, and an
   h2 or h3 leg next to the HTTP/1.1 one.
2. ~~**An 8 h unattended run is unproven**~~: memory over ~9 M requests per
   node, pacing at soak length, the final report's size and assembly time, 510
   progress snapshots. **Run once, 2026-10-08, with 9fcbb81** ("Verified on
   talos-main"): 8h30m on five engines, every one of 9,180,000 requests sent
   and answered, no engine restarted, the report and the stream written and
   saved 25 s after the last answer. One run; the k6 files stay until phase 4.
3. **The engine has no CPU limit.** It costs 0.11–0.14 core on kind because a
   worker waits (`WAIT`); nothing else keeps it there. Since sortie 7f338df
   that is sortie's default and no longer the plan's doing, so what keeps it is
   the pin: a plan rendered with another idle strategy (`--idle-strategy`), or
   a sortie bump that changed the default back, would cost up to eight cores on
   a four-core node and starve the proxy under test. The test suite pins that
   the plan templates nothing; **the engine's CPU has to be read again at every
   sortie bump**, and has not been read on talos for 9fcbb81, f0750ec or 96e6bfb
   (94cf103 read 127–135m per engine, 2026-10-07; neither saved run directory
   of 2026-10-08 holds a reading). f0750ec's engine has the layers of
   9fcbb81's, so the two are one reading. 96e6bfb's engine is a new binary
   (one lock around a constructor) and has been read on kind only: 0.10–0.11
   core and 301–302 MiB per engine, as before ("Evidence on kind (2026-10-08,
   sortie 96e6bfb)").
4. **One sortie pod is a single point of failure for the run.** If it is
   evicted or its node drains, every execution is cancelled and reported
   `cancelled`, not evaluated: 8 h gone. k6 runners failed independently. It
   has `backoffLimit: 0` on purpose (a retry would start a second, partial
   run). It now carries the `aether-soak-loader` PriorityClass, so it is no
   longer the first preemption victim; a node drain or a node failure still
   takes it.
5. **An engine that dies loses its node's share of the run**, all eight
   targets of it, from the start: a backend that goes away returns nothing, not
   what it had counted. The other nodes are whole and graded. A node that
   reboots mid-soak therefore fails the loader gate for that node for the whole
   run, not from the reboot on; the access logs carry what that node did send.
   That is the engine that goes away *audibly*: its pod is deleted or its node
   shuts down, the connection closes, and sortie names it. **A node that goes
   silent was worse, and sortie 94cf103 did not handle it; 9fcbb81 does** (the
   paragraph after this one). With 94cf103: a node that
   freezes, loses power or is partitioned sends no FIN and no RST; sortie has no
   deadline on a backend's answer and no keepalive, so it waits. On kind, with
   one node paused, the Job was still running 20 minutes after the pause (19
   minutes past the end of a 2-minute plan) and ended only when the node came
   back. Until then there is **no report for any node**: stopping the sortie pod
   cancels the run, and a cancelled run is not evaluated. For a soak, a node
   that hard-fails and stays down therefore costs the loader's report for all
   five nodes — the b71b37e behaviour for a dead engine, by another road. What
   the harness can do is say so early (the saver's `SORTIE_OVERDUE` line, two
   minutes past the planned end) and keep what exists: the progress lines in the
   sortie pod's log are each target's cumulative counters per node, up to one
   minute old, and the statsd series and the access logs are unaffected. The fix
   is sortie's: give up on a backend some grace after its execution's duration
   (and report it in `backend_errors`), or keep the connection alive. It went
   to the sortie session with that PR.
   **Since sortie 76f699b (in the 9fcbb81 pin) it is fixed, the first way.**
   Every backend has a deadline counted from dispatch — the duration, the
   request timeout (30 s), the engine's drain (1 s) and two minutes of grace:
   151 s past the planned end for this plan. A backend that has not answered
   by then is cancelled, given 30 s to answer the cancellation, and named in
   `backend_errors` as silent, and the report is written with the other nodes
   whole; a backend that answers with an execution far longer than planned is
   failed too, its counters kept. A node that hard-fails mid-soak therefore
   costs that node's share and three minutes, not the report. **Measured on
   kind** ("Evidence on kind (2026-10-07, sortie 9fcbb81)"): with one node
   frozen from T+45 s of a 120 s plan and never thawed, the report was written
   181 s past the planned end. What is left of the risk is the driver's own
   node: the report PV pins the sortie pod to one worker, and if *that* node
   freezes nothing ends the run (the same test, aimed wrong, showed it). It is
   risk 4, and one node in five. That test also showed a driver waking past
   the deadline and calling the *healthy* node silent on four of eight
   executions, its results unread; **sortie f0750ec takes the answer that is
   waiting** (16 of 16 executions over two frozen-driver runs on kind), so a
   driver node that stalls and comes back costs its own engine's share only.
6. **A node that joins or a pod that is replaced mid-run gets no load.** The
   pool is fixed at start. An engine pod that is deleted and recreated (a node
   drain) is a new IP the run never drives.
7. **No per-request response timeout** (above): a request that hangs is
   counted only when the run ends (`http_inflight_lost`, since 9fcbb81), not
   when it should have been answered.
8. **Series churn** — retired with 9fcbb81: the statsd names carry the node's
   name, not the pod IP, so a run writes the series the previous one wrote
   ("Telemetry"). What replaced it as an unknown, how Prometheus shows two
   runs on one series, is known now: the engine sends deltas, so a series the
   collector still holds carries on from the last run, and one it has expired
   (or lost to a restart, also mid-run) starts from zero. A total is not a
   run's count; `rate()` and `increase()` are right either way.

## Cut-over

1. **Short e2e on talos-main** (15 min, `run.sh e2e`), k6 not running; the
   exact commands and what each writes to the cluster are in the README ("The
   first short e2e on talos-main"). Reads: the pool is five backends on five
   nodes; what the engine really costs on those CPUs, against the 150m request;
   pacing (`http_2xx=<counted>/<planned>` per target and node) and
   `pool_overflow` zero with no limit and no queue; the latency the gate
   prints; that the per-node `sortie_mesh_*` series arrive and sum to the
   report; and, in a second run with `--rolls`, that one agent roll and one
   proxy roll produce failures the access logs can attribute. Exit:
   `sortie-gate.sh` PASS on a no-roll run, and every number that was
   unverified replaced by a reading. **Run on 2026-10-07** ("Verified on talos-main"):
   the no-roll run FAILED the gate on 6 `pool_overflow` with no client queue,
   which is the finding; the run with rolls and a queue PASSED. The exit
   condition as written — a PASS on a no-roll run, with the default queue —
   was missed once more by run 3 (one reset in 270,000, an aether defect,
   #1350, since fixed) and **met by run 4 on 2026-10-08**: 8 of 8.
2. **Side by side.** One short run with the k6 runner up as well
   (`run.sh` notes it; the node carries both loads, so this is a comparison of
   instruments, not a capacity test): per target, sortie's failures against
   k6's by-target table and against the access logs for both identities.
   The dashboard change lands here. The k6 runner is taken down with `kubectl
   delete ds/k6-soak-loader`, never `kubectl delete -f k6-runner.yaml`.
3. **First sortie soak** (`run.sh soak`), k6 not running. It is graded as a
   soak and as the 8 h proof of the driver. Exit: prober gates as always, the
   loader ran 8h30m on five backends, its failures reconcile against access
   logs the way k6's did. **Run on 2026-10-08** ("Verified on talos-main"):
   the loader ran its 8h30m on five backends and had no failure to reconcile;
   the prober's liveness tier was clean and its mesh_dns tier was not (37
   timeouts, all labelled one node, under investigation), so the exit is not
   yet called.
4. **Retire k6**: delete `k6-mesh-soak.js` and `k6-runner.yaml`, the k6
   sections of the README, the `aether-k6` dashboard and `k6-operator` (GitOps),
   and rewrite the README passages that still say "k6" (the churn script's
   comments too). Not before phase 3 has passed.

## Verified on talos-main (2026-10-07 and -08), and what is still open

Five runs, k6 not running, five workers at 60 rps per node, `WAIT`, no CPU
limit: four 15-minute `run.sh e2e` runs and one `run.sh soak`. Runs 1 to 3
with sortie 94cf103, run 4 and the soak with 9fcbb81; **none yet with
f0750ec or 96e6bfb**. Run directories under `~/aether-soak-logs/`: `1007-sortie-e2e-1`,
`-2`, `-3`, `1008-sortie-e2e-9fcbb81`, `1008-sortie-soak`. Access-log figures
are the source proxies' rows for `user_agent:"aether-soak-sortie"` (the LogsQL
is in the README, "The mesh's own numbers"). Runs 1 and 2 first, as they were
written up at the time; runs 3 and 4 and the soak follow.

| | run 1 | run 2 |
|---|---|---|
| rolls | none | `aether-agent` at T+3m, `aether-proxy` at T+8m; both `ROLLED` |
| client queue | none (the plan as it then was) | `max_pending_requests: 16` |
| sent, of 270,000 planned | 269,994 | every target 40,497–40,500 of 40,500 (9 rps), 13,499–13,500 of 13,500 (3 rps) |
| `pool_overflow` | **6**: `svc-1` 3 and `svc-4` 2 on main-worker-03, `svc-2` 1 on main-worker-02 | 0 |
| every other failure class | 0 | 0 |
| gate | `VERDICT FAIL targets=8 passed=5 failed=3` | `VERDICT PASS targets=8 passed=8 failed=0` |
| sortie latency, worst node per target | p50 13–17 ms, p99 115–141 ms | p50 12–16 ms, p99 153–207 ms |
| access logs: rows, whole run | 269,994, all `200`, no flag | 269,992: 269,991 `200` with no flag, one `200` `DC` |
| access logs: `duration_ms` | p50 7–9 ms, p99 66–78 ms, p99.9 259–332 ms, max 0.85–1.2 s (per node); 89 rows over 500 ms | p50 7 ms, p99 106 ms, p99.9 458 ms, max 1.455 s; 168 rows over 500 ms |
| engine | 127–135m CPU, 302–321 MiB per pod; 0 restarts | watchdog PASS, 0 restarts |

**Now verified:**

- **Engine CPU under `WAIT`**: 127–135m per pod against the 150m request; the
  request stands. Memory 302–321 MiB.
- **Five backends on five nodes**, each driven for the whole run and reported
  per node; every rate floor held on every node in both runs.
- **The `soak-sortie` identity with SPIRE on**: the rows carry
  `spiffe://aether.internal/ns/aether-test/sa/soak-sortie`, and run 1's first
  requests after the 30 s settle were answered like the rest — 269,994 rows,
  every one `200`. No first-use failure.
- **The report PVC**: provisioned, written by the sortie pod, read back by the
  saver in both runs.
- **Signature verification on talos** (`VERIFIED=cosign-v3.1.2` in both
  `run.env`s) and the PriorityClass, which the first run created.
- **One agent roll and one proxy roll under load** (run 2): no reset, no
  connection failure, no 5xx, on any target or node. One data point, not a
  characterisation of hot restart.

**Found:**

- **The client-queue requirement** ("The client queue"). The mesh answered
  every request of run 1 with `200`; the six that failed the gate were never
  sent. The overflows line up with ~1 s response-time maxima on the same
  executions (sortie's progress lines: 1.0–1.1 s).
- **sortie's latency reads above the mesh's own.** Run 1, per node: p50 8–17 ms
  where Envoy's source-side `duration_ms` is 7–9 ms, p99 80–140 ms against
  66–78 ms — a few ms at the median, tens of ms at the tail. It includes the
  client's connection wait and scheduling. It stays a soft signal; the
  access-log quantiles are the mesh's number.
- **One `200` / `DC` row** in run 2: main-worker-04, to `echo`, at T+10m08s,
  16 s before the proxy roll finished. A complete response (799 bytes, like
  every `echo` response; 78 ms), `downstream_remote_disconnect`, the single
  request on a second connection the engine opened while its usual one was
  busy and then closed. The engine counted nothing for it. Why Envoy saw the
  close first is **unexplained**; it has the signature of k6's benign `DC`
  (#1009), which this proposal expected not to see from an Envoy-based client.
  One occurrence in 540,000 requests is not a baseline.

### Runs 3 and 4: the short e2e with the default queue

Both without rolls, both with the plan's default `max_pending_requests: 18`.
What is in each row is read from that run's saved `report.json`,
`save.log`, `engines.tsv` and `restart-watch.log`.

| | run 3 | run 4 |
|---|---|---|
| when | 2026-10-07, load from 11:43:08Z | 2026-10-08, 01:25:51Z to 01:40:54Z |
| sortie, aether | 94cf103 | 9fcbb81, aether 2.4.16 |
| sent, of 270,000 planned | 269,999 | 269,981 |
| failure classes | **1**: `mesh/svc-3` on main-worker-03, `stream_resets` 1 = `stream_resets_before_headers` 1 = `stream_resets_connection_termination` 1 (one reset, counted in its class, its phase and its reason). Every other class 0, `pool_overflow` among them | every class 0, `http_inflight_lost` (new in 9fcbb81) among them |
| sortie's own verdict | `FAIL  7/8 executions passed` (the plan's `stream_resets == 0`) | `PASS  8/8 executions passed` |
| gate | not graded by today's gate, which refuses a 94cf103 report as old-format; by the report: 7 targets clean, `svc-3` failed | `VERDICT PASS targets=8 passed=8 failed=0 not_run=0 backends=5 lost_backends=0 report_pass=true` |
| short of the plan | 1, the reset | 19, **all on main-worker-03**: 3 on each of five 9 rps targets, 2 on `echo`, 1 on each UDS target; the four other nodes sent 54,000 of 54,000 each |
| sortie latency, worst node per target | p50 12–16 ms, p99 178–264 ms | p50 12–15 ms, p99 170–228 ms, max 1.1–2.0 s |
| watchdog, engines | `verdict=PASS new_restarts=0`, 30 samples; no engine restart | the same |
| saver | `SORTIE_SAVED … job=failed executions=8 pass=false` | `SORTIE_SAVED … job=complete executions=8 pass=true not_run=0 window=…01:25:51.582Z..…01:40:54.014Z stream_executions=8` |

- **Run 3's one reset is aether's, and it is fixed.** A reset before any
  response header, reason connection termination, on a request to `svc-3`: the
  proxy's 5-minute idle close racing the reuse of an HTTP/1.1 keep-alive
  connection (#1350). Fixed in #1358, deployed with chart 2.4.14. **It did not
  recur** in run 4 or in the soak's 9,180,000 requests, both on 2.4.16. It is
  the first failure this driver found that the mesh had caused, and the gate
  placed it: one target, one node, one class with its phase and reason.
- **Run 4 is phase 1's exit**: a no-roll run that passes, with the default
  queue. No `pool_overflow` in either run (the queue of 18; run 1 had 6 with
  none).
- **Run 4's 19 short requests are one node's late start, by the timestamps.**
  Every worker of main-worker-03's engine started between 01:25:52.537Z and
  .573Z, 0.31–0.35 s after the first worker of the pool (.225Z); on the other
  four nodes the workers started between .225Z and .556Z. And that engine's
  own elapsed time is short by the same amount: 899.68–899.75 s on its eight
  executions, 899.98–900.06 s on every other node. It started late and
  stopped with the rest, and 0.3 s at 9 rps is the 2–3 requests each of its
  targets is short. Nothing failed and nothing was in flight at the end
  (`http_inflight_lost` 0); 19 in 270,000 is 0.007 %, inside the gate's 1 %
  range, which is what the range is for. Why that one engine started late is
  not known.
- **First on talos-main with 9fcbb81 in run 4:** the results stream on
  `openebs-hostpath` (eight lines, the report's eight executions), the
  timestamps and the `WINDOW` line, latency from `statistics`, the count judged
  as a range, and a run that ended 2.4 s after its 900 s with nothing in
  flight.

### The first 8-hour soak (2026-10-08)

`run.sh soak`, sortie 9fcbb81, aether 2.4.16-8f0ff40
(`~/aether-soak-logs/1008-sortie-soak`). Load from 02:01:51Z to 10:31:53Z; the
churn driver's T0 is **02:06:04Z**.

| | |
|---|---|
| sortie gate | `VERDICT PASS targets=8 passed=8 failed=0 not_run=0 backends=5 lost_backends=0 report_pass=true` |
| sent, of 9,180,000 planned | **9,180,000**: 1,377,000 of 1,377,000 on each 9 rps target, 459,000 of 459,000 on each UDS target, 1,836,000 on each of the five nodes. Not one request short |
| failure classes | every class 0 on every target and node; `http_inflight_lost` 0; nothing not run |
| churn | 35 `ROLLED` lines and no `FAILED` or `ABORTED`: the 33 rolls (6 proxy, 2 agent, 2 edge, 3 mesh-dns, 2 uds-csi, 18 `svc-*`) and the 2 new-ServiceAccount steps; the SHRINK done and restored |
| restart watchdog | `verdict=PASS new_restarts=0 containers=0 samples=240 error_samples=0 baseline=0 ended=complete`, 02:06:10Z to 10:06:11Z |
| engines | the five pods of the start are the five of the end, 0 restarts |
| sortie latency, worst node per target | p50 12–15 ms, p99 388–473 ms, max 2.2–3.5 s. The worst p99 is main-worker-04's on all eight targets (388–473 ms there, 136–253 ms on the other four nodes) |
| report and stream | `SORTIE_SAVED … job=complete executions=8 pass=true not_run=0 window=…02:01:51.550Z..…10:31:53.620Z stream_executions=8`, 25 s after the last answer |
| prober, liveness tier | 0 non-success |
| prober, mesh_dns tier | **37 timeouts, all labelled `node="main-worker-04"`**; attribution under investigation at the time of writing |

- **The loader gate passed an 8-hour soak with nothing to attribute**: no
  failure class moved on any target or node across 35 rollouts, so there was
  no `FAIL` line to reconcile against the access logs. The k6 soaks it
  replaces ended at 0 of 9.18 M as well; this is the same reading from the new
  instrument, per target and per node.
- **The soak's verdict is the prober's, as always**, and the prober's mesh_dns
  tier is not clean. Those 37 timeouts are not in the loader's report (it
  drives the eight targets, not the prober's mesh_dns probes) and their cause
  is not known here.
- **Risk 2 has its first measurement**: pacing held for 8h30m (each
  execution's elapsed time is 30,601.8–30,602.1 s for 30,600 s), every engine
  lived, and the report was assembled, written and saved.
- **The live series did not survive the night whole**: one of them read
  241,808 at the end where the report has 275,400 ("Telemetry"). The grade
  never depended on them.

**Still open:**

- **sortie f0750ec or 96e6bfb on talos-main at all.** Runs 4 and the soak were
  made with 9fcbb81. f0750ec changes the driver only, and what it changes (the
  `refused` field, the answer taken after a missed deadline) is not something
  a clean run exercises. 96e6bfb changes the engine's binary (a lock around
  one constructor, "What sortie 96e6bfb changed"), so its first run there is
  also the first reading of that engine's CPU on real nodes (risk 3).
- **The mesh_dns timeouts of the soak** (37, all labelled
  `node="main-worker-04"`): under investigation.
- **The engine's CPU and memory on talos-main with 9fcbb81, f0750ec or
  96e6bfb**, and over eight hours: not in the saved run directories (risk 3).
- **The access-log cross-check for run 3, run 4 and the soak** is not recorded
  here (rows, codes and flags, `duration_ms` quantiles, whether a `200` / `DC`
  row recurred).
- **h2 and h3**: the soak drives HTTP/1.1 only.
- **A node that goes silent** (risk 5), and which of the two a talos reboot or
  upgrade is to sortie: a clean close, or silence.
- ~~Anything past 15 minutes, and the 8 h run.~~ Run once (above).
- ~~The default queue itself, and a no-roll run that passes.~~ Runs 3 and 4
  used 18; run 4 passed.
- ~~The statsd path end to end on the real `otel-scraper` and Prometheus.~~
  The node-named series arrive (`sortie_mesh_svc_1_main_worker_03_…` was read
  for run 4 and for the soak); they are not a count ("Telemetry"). Their sum
  against a report was not taken on talos-main and, after that reading, is
  not worth taking.
- ~~Behaviour under more than one proxy hot restart.~~ The soak: six proxy
  rolls and two agent rolls among 35, no loader failure. What the handoffs of
  #1093 / #1320 cost shows in the prober, not in this report.

## Open questions

- ~~Should `WAIT` be sortie's default?~~ **Answered by sortie 7f338df: it
  is.** The last template line is gone from the plan.
- ~~sortie: a deadline on a backend.~~ **Answered by sortie 76f699b** (risk 5).
- ~~Latency in the JSON report.~~ **Answered by sortie 1b3d404**; the two
  always-true carrier thresholds are removed.
- **A latency threshold that can fail.** The carriers are gone and nothing
  judges latency. The 9fcbb81 runs on talos now give numbers: worst-node p99
  per target 170–228 ms over 15 minutes without rolls, 388–473 ms over the
  soak's 35 rollouts (136–253 ms on four of the five nodes). Whether a real
  `latency_2xx.p99 < …` is set from them, or the access logs stay the only
  latency gate, is still to decide; one node sets the soak's number.
- ~~A structured marker for a stage refused at the execution cap.~~
  **Answered by sortie f0750ec**: `"refused": "execution_cap"`, on the
  execution and on the refusing engines' `backend_errors` entries. The gate
  reads it; the text is read for 9fcbb81 reports only.
- **Why bash can lose a TERM.** Found while testing the watchdog's stop
  (#1386): bash 5.2.21 does not run a trap that arrives while it is expanding
  a `$(…)`; it prints `trap: line 2: unexpected EOF while looking for matching
  ')'` and carries on (2 of 1,680 timed TERMs). Nothing is orphaned by it, and
  the watchdog spends its time in a wait where it cannot happen, but every
  script here that traps a signal has the exposure while it works.
- **h2 and h3 under concurrent executions are untested** — by sortie
  ("concurrent executions are tested over HTTP/1; the other protocols and modes
  have not been run concurrently") and by this harness, which drives HTTP/1.1
  as k6 did. A QUIC leg on sortie needs that first.
- ~~8 h is unproven (risk 2).~~ **Run once, 2026-10-08**: the loader held for
  8h30m and its gate passed. Whether phase 3 is passed is the prober's call
  and waits on the mesh_dns timeouts.
- **OTLP is absent.** The live path is statsd over UDP into the collector's
  statsd receiver, names without labels. An OTLP sink (Envoy's aborts the
  engine) or DogStatsD tags for scenario / target / backend would give labels
  instead of 2,800 name-encoded series.
- **What a progress snapshot costs.** About 0.6 CPU-second per engine for
  eight executions ("CPU and memory"). sortie's README says a snapshot allocates
  no histogram and costs no memory, which holds here; it says nothing about
  CPU. At one a minute it is +10 millicores per node, and a short burst rather
  than a level. Whether it can be cheaper is a question for sortie; whether a
  0.6 s burst matters at a proxy handoff on a four-core node is one for talos.
- **Should the loader keep `default` as its identity** (`serviceAccount.create:
  false`) so old access-log queries keep working? Proposed: no — a named
  identity is the better instrument, and the side-by-side needs the two loads
  to be distinguishable.
- **The e2e profile's rolls.** `run.sh e2e --rolls` does one agent roll and one
  proxy roll inside 15 minutes; the old workstation e2e did one agent roll and
  three proxy rolls over 46 minutes. Whether the short profile should grow to
  that is a decision for after phase 1.
