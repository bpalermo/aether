# Proposal 042: sortie as the soak's load driver — k6 retired

**Status:** Draft, harness built and proven on kind 2026-10-06 (this PR). Not yet
run on talos-main: phase 1, the short e2e there, is the next step and is where
the items under "Unverified until the talos run" get their first reading.
**Author:** Bruno Palermo
**Date:** 2026-10-06
**Related:** #1323 (the kickoff lived outside the repository and had two
defects; closed by this), #1093 / #1320 (nodes are CPU-tight at proxy handoffs,
which is what the engine's CPU cost collides with), #846 / #887 (the k6
per-class and per-target failure breakdown this has to replace), #1108 (the UDS
share), #1009 (the benign k6 `DC` artefact, which does not exist under sortie),
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
  victim when a surging `aether-proxy` pod needs room. The chart only
  references it; `run.sh` applies it.
- **Two-step install.** A `dns:` pool is resolved once, when the run starts,
  and takes the first non-empty answer. `run.sh` installs the engines alone
  (`job.enabled=false`), waits for the DaemonSet and for the headless Service to
  list every engine, and only then upgrades the release with the plan.
- **Pinned by digest**, both images and the chart, signatures verified with
  cosign 3.0.6 against sortie's publish workflow identity (index and both
  per-arch manifests of each image). The chart itself is not signed; it is
  pulled by its OCI digest.

## Mapping: the load shape

| | k6 (`k6-mesh-soak.js`) | sortie (`sortie-plan.sh`) |
|---|---|---|
| arrival | `constant-arrival-rate`, 60/s per runner | `constant-rate`, `rate: 60`, `per_backend: true`, `open_loop: true` |
| duration | 8h30m | `30600s` (profile `soak`); `900s` (profile `e2e`) |
| protocol | HTTP/1.1 cleartext to `:18081` (k6 speaks h2 only over TLS) | `protocol: http1`, same URLs |
| targets | one random draw per request: 95 % uniform over six mesh targets, 5 % uniform over two UDS targets | eight concurrent executions, each paced on its own |
| concurrency | 60–180 VUs, one keep-alive connection each | one worker thread per target; connections as the rate needs them (a handful) |
| request timeout | 60 s (k6 default) | none: see "What is lost" |
| DNS | a lookup per new connection | once per execution, at start |
| verdict | `http_req_failed`, printed at exit | thresholds, per target, in a JSON report |

### What `http_req_failed == 0` and the failure classes become

The k6 gate was "every runner's `http_req_failed` is 0, or every failure is
attributed", read from eight class counters and a by-target table. Under sortie
each target is its own execution, and the gate is the **zero-failure set**: these
counters `== 0`, per target, summed over the nodes.

| k6 class (`aether_fail_*`) | sortie counter | notes |
|---|---|---|
| `http_4xx`, `http_5xx` | `benchmark.http_4xx`, `benchmark.http_5xx` | a complete response with that status |
| `conn` | `benchmark.pool_connection_failure` (+ `pool_failure_local_…` / `pool_failure_remote_connection_failure`) | never got a connection |
| `timeout` | `benchmark.pool_failure_timeout` | **connect** timeout only; there is no response timeout |
| `proto`, and any mid-response cut | `benchmark.stream_resets` (+ `_before_headers`, `_incomplete_body`, `_<reason>`) | the reason counters name Envoy's `StreamResetReason` |
| — | `benchmark.pool_overflow` | the client refused its own request: load-generator saturation, never the mesh's failure. No k6 equivalent (k6 reports `dropped_iterations`) |
| `dns` | — | no per-request class: a name that does not resolve fails the whole execution with an `error`, before any request |
| `tls` | — | the soak drives cleartext to the local proxy |
| `other` | — | nothing is unclassified: a request is counted under a status or under a reset or pool counter |

The counters **overlap** — a reset is in `stream_resets`, in one phase counter
and in one reason counter — so the gate lists them and never adds them up. A
counter that never incremented is absent from the report and reads zero; the
`pool_overflow` control for that is real (see "CPU and memory": it was seen
non-zero on kind).

**An execution must not stop at its first failure, and by default it does.**
Nighthawk carries four default *failure predicates* — `benchmark.http_4xx`,
`http_5xx`, `pool_connection_failure` and `stream_resets`, each with a limit of
0 — and ends the execution the moment one is exceeded. sortie then reports the
execution with an `error` ("nighthawk reported failure …") and **no counters and
no thresholds at all**. On kind, scaling one target to zero for 90 s ended that
target's execution on both nodes one second later, 65 s into a 240 s run, and
the report could not say which class had failed. For a soak that is fatal twice
over: the first 503 of the first roll would stop that target's load on that
node for the remaining hours, and hide what it was. The plan therefore lifts
all four predicates (`nighthawk_template.failure_predicates`, limits that are
never reached) and leaves the verdict to the thresholds. With that, the same
experiment reads `FAIL uds-cr-echo … http_5xx=554 [node-a=277 node-b=277]`, the
target keeps being driven through and after the outage, and the other seven
targets pass.

Two things the plan adds that k6's `rate<0.01` never had:

- **A rate floor per target.** `rate:benchmark.http_2xx` must be at least 99 %
  of the target's planned rate. Zero failures with nothing sent is otherwise a
  pass. The plan can carry one floor for the whole scenario (the smallest
  share's, 3 rps × nodes); `sortie-gate.sh` applies each target's own.
- **The pool is checked.** The floor is written for the number of engines that
  were ready at launch, and the gate requires every execution to have run on
  exactly that many backends. A node missing from the pool fails the run
  instead of shrinking it.

`sortie-gate.sh` prints, per target, `PASS`/`FAIL`, the achieved and planned
rate, the 2xx total, and for a failure the class, the count and the **node**
(the report names backends by pod IP; `run.sh` records the IP → node map at
launch). That replaces the k6 "by target" table and adds the node, which k6
could only give per runner log.

A `FAIL` line is what k6's non-zero `http_req_failed` was: the list of what has
to be attributed from the access logs and the roll brackets, not by itself a
failed soak. The 2026-09-30 run had 147 k6 failures, 141 of them before T0.

### Every README gate that reads k6 output

| gate / step (README) | reads from k6 today | under sortie |
|---|---|---|
| Run step 3: "all five runners report 60.00 iters/s" before churn | runner logs | the sortie pod `Running` after the settle time, and live `…benchmark_http_2xx` if statsd is on; `run.sh` refuses to start churn otherwise |
| Run step 5: teardown only after k6 exits; summary in `logs --previous` | pod log | `sortie-save.sh` (armed at load start) copies the report and the log; `sortie-teardown.sh` refuses without its `SAVED` marker. The Job pod does not restart, so nothing is in `--previous` |
| The restart gate: "the watchdog stops at T0+8h because `k6-soak-loader` restarts once by design" | — | a finished Job pod has no restart; the 8 h window no longer has a trap at its end. The watchdog also now sees an engine OOM (it did, on kind) |
| The UDS gates, item 1: "the `--- by target` table must show no `uds-echo` / `uds-cr-echo` row" | k6 summary | `sortie-gate.sh`: `PASS uds-echo`, `PASS uds-cr-echo`; a `FAIL` row must match an access-log row, as before. Control counts double (see "Rate shares") |
| The UDS gates, control query: `client_sa default` ≈ 216,000 per authority | access logs | `client_sa soak-sortie` ≈ 432,000 per authority |
| Benign `DC` (#1009): k6 lines must sit in a source-node proxy roll bracket; excluded from the k6 reconciliation | access logs + k6 | **expected to disappear for the loader**: see below. If `DC` + 200 + full bytes rows appear for `client_sa soak-sortie`, that is a finding, not a baseline |
| New-SA step text: "visible only because the k6 loaders happened to start … 58 s before T0" (#1086) | history | the engines are a new identity on every node at install; `run.sh` waits `SOAK_ENGINE_SETTLE` (30 s) after they are Ready before the first request |
| Gate 3 (twin count = observed pairs): "k6 loaders + prober + new-SA steps" | Prometheus | same query; the loader's pairs are now `…@aether-test/soak-sortie` |
| QUIC per-request cost gate (#1021): "at the soak's load shape", 300 rps | Pyroscope / Prometheus | the fleet rate is unchanged (300 rps), the per-target mix is not: re-baseline h3/h2 on the first sortie soak rather than comparing against 1.18× across drivers |
| Gotcha 6, "k6 needs 1Gi" | — | the engine's own numbers, below |
| Files: `AETHER_FAIL` sample lines with timestamps, the only instrument that placed a burst against a roll | k6 log | **lost** as a client-side instrument. Placement comes from statsd (10 s resolution, fleet-wide only — see "Telemetry") and from the access logs, which carry the user agent |

### What is lost

- **Per-request random target mixing.** k6 drew a target per iteration, so the
  targets' requests interleaved randomly and any one second could hold any mix.
  sortie paces each target on its own limiter: a fixed 9 or 3 rps each, every
  second. Bursts across targets never coincide by chance, and never fail to.
- **The k6 `DC` class** — and that is a gain. k6 closed the connection after a
  full body while the h3 upstream FIN was still in flight, which the source
  proxy logged as `DC` (#1009). Envoy's client does not treat a disconnect
  after a complete response as a failure, and it does not close first.
- **Per-request DNS.** A target is resolved once per execution, by the engine,
  at the start. Mesh DNS is exercised 8 times per node per run by the loader,
  not continuously. The prober's `mesh_dns` tier (which resolves on every probe)
  is and stays the DNS SLI; the loader was never the authority for it.
- **A response timeout.** A request that never gets an answer is not a failure
  under sortie: it waits until the run and its drain end. k6 failed it at 60 s.
  The rate floor catches a target that stops answering wholesale; a single hung
  request is invisible to the loader. The prober's 2 s budget is the instrument
  for that, as it already was.
- **The verbatim, timestamped failure sample** (`AETHER_FAIL`).
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

This is the part of the cut-over that is not free, and the reason the engine
runs under a CPU limit.

**An idle Nighthawk worker thread is not idle.** Between requests the sequencer
either spins (`SPIN`, Nighthawk's and sortie's default), polls a 25 µs timer
(`POLL`), or sleeps 50 µs at a time (`SLEEP`). None of them blocks until the
next request is due, so the cost is per worker thread and independent of the
rate: a 3 rps target costs what a 9 rps one does. Eight targets are eight
worker threads per node.

Measured on kind (two worker nodes, 20-core workstation, engine cgroup
`cpu.stat`, the plan's 60 rps per node over eight executions):

| engine, per node | CPU | notes |
|---|---|---|
| no execution | 0.003 cores | |
| `SPIN` (default) | **8.0 cores** | 1.00 core per worker thread |
| `POLL` | 2.85 cores | 0.36 per thread |
| `SLEEP`, no limit | **1.7 cores** | 0.19–0.21 per thread; rate exactly 18.00 / 6.00 rps over two nodes, 0 failures |
| `SLEEP`, limit 400m, engine's default pending queue | 0.40 cores | `pool_overflow` on **every** target (5–29 per target in 150 s), 2xx rate 1.1 % short |
| `SLEEP`, limit 400m, `max_pending_requests: 16` | 0.40–0.41 cores | **what ships.** 15 min: 16,182–16,186 2xx per mesh target (18.00 rps over two nodes), 5,394–5,395 per UDS target (6.00 rps), every failure class 0 |

k6's budget was 100m request / 400m limit. talos-main's workers have 3.95
allocatable cores and sit at 1.3–1.65 in use before any load; #1093 and #1320
are both about nodes starved at proxy handoffs. 1.7 more cores per node would
not be a load generator, it would be the dominant workload on the node.

**So the plan sets `sequencer_idle_strategy: SLEEP`, and the engine gets k6's
own budget: 100m request, 400m limit.** Under the limit a throttled worker
releases the requests that came due while it was off CPU together; with the
engine's default queue those were refused as `pool_overflow`, which is why the
plan also sets `max_pending_requests: 16`. The requests are then sent a
throttle window late instead of dropped.

What the limit costs, stated plainly:

- **sortie's latency numbers include the throttle** (p99 of 20–100 ms per
  target on kind, tracking the CFS period rather than the mesh). The soak does
  not gate on the loader's latency and must not start to while this limit is in
  place.
- **Pacing jitter** of up to a CFS period (100 ms) per target. The arrival
  process is 9 rps with ±100 ms jitter, not a clean 111 ms tick.
- **`pool_overflow` is the canary.** A non-zero `pool_overflow` means the
  engine fell further behind than its queue: raise the limit before believing
  anything else in that report. It is in the zero-failure set for that reason.

The fix belongs in sortie: an idle strategy that blocks until the next
request is due (or a configurable sleep) would make an 8-thread, 60 rps engine
cost a few millicores, and the limit and the queue could both go. Reported to
the sortie session; "Open questions" tracks it.

**Memory** (kind, same plan): ~300 MiB with eight executions running (~37 MiB
per execution), flat for 15 minutes (300.4 → 300.7 MiB); assembling the final
report peaks at ~600 MiB, and ~447 MiB stays resident in the idle engine
afterwards. With `sortie run --progress 60s` the first snapshot takes a running
engine to 550–585 MiB and the second to ~810 MiB, where it stays. A 512 MiB limit
OOM-killed both engines 60 s into the first run, at the first snapshot. The
values file therefore requests 384 MiB with a 1 GiB limit (k6: 256 MiB / 1 GiB)
and **does not pass `--progress`**: a snapshot is also a CPU burst inside the
engine's own quota, and under the 400m limit the first one cost one
`pool_overflow` on each of two targets in an otherwise clean 15-minute run
(16,183 of 16,200 requests per mesh target, everything else zero).

Whether memory grows over 8 h is **unmeasured** (the histograms are
fixed-size, which argues no; nothing has run longer than 15 minutes here).

## Telemetry

sortie has no OTLP sink (Envoy's aborts the engine on its first flush there);
the plan's `stats` block makes each engine flush its Envoy stats store to a
statsd server over UDP, and the address must be an IP literal.

**talos-main already has the receiver.** `o11y/otel-scraper` (GitOps:
`clusters/talos-main/otel-scraper/values.yaml`) exposes `8125/UDP`, cluster IP
`10.106.234.157` on 2026-10-06, with a statsd receiver (10 s aggregation,
monotonic counters, `|ms` timers as explicit-bucket histograms), a
`deltatocumulative` processor and `job="statsd"`. It was added for Nighthawk
in `clj-grpc-soak`. `run.sh --statsd auto` (the default) resolves that
Service's cluster IP and UDP port at launch and templates it into the plan; if
the Service or the port is absent the run goes ahead without live metrics and
says so. `--statsd off` disables it; `--statsd IP:PORT` overrides. No GitOps
change is needed to receive.

Names, as they arrive in Prometheus (verified on kind against a collector with
the same receiver and processor settings):

```
sortie_mesh_<target>_cluster_0_benchmark_http_2xx_total
sortie_mesh_<target>_cluster_0_benchmark_pool_overflow_total     (exists once non-zero)
sortie_mesh_<target>_cluster_0_benchmark_http_client_latency_2xx_{bucket,sum,count}
sortie_mesh_<target>_cluster_0_upstream_rq_total   … and Envoy's other cluster counters
```

(`<target>` with `-` as `_`; `cluster_0` is worker 0.) About 34 series per
target, 272 for the plan, plus histogram buckets.

**The live series are not per node, and not the fleet sum either.** The statsd
names carry no backend, the receiver exposes no sender address, and every
engine sends the same names — so five engines write one series. On kind, with
two engines, `…_benchmark_http_2xx_total` read one engine's worth, not two:
8,085–8,101 per mesh target at the end of a 15-minute run in which each engine
sent 8,091–8,093 and the report's total was 16,182–16,186. Until sortie can put the
backend into the prefix (or a DogStatsD tag), the live series answer "is the
load flowing, and did a failure class just move" and nothing more; they cannot
place a failure on a node and their absolute values are not totals. **The
grade comes from the JSON report**, which has per-node counters.

**Dashboard.** `aether-k6` (read 2026-10-06) is built on `k6_*` series keyed by
`testrun_name` — `k6_http_reqs_total`, `k6_http_req_failed_total`,
`k6_http_req_duration_milliseconds_bucket`, `k6_vus`, by `endpoint` and
`status` — which k6-operator test runs export. The soak runner exports none of
them (it runs without an output sink on purpose), so the dashboard has never
shown a soak. Its replacement for the soak is a small one over the
`sortie_mesh_*` series above: 2xx rate per target (`…_benchmark_http_2xx_total`),
one stat per zero-failure class (`{__name__=~"sortie_mesh_.*_benchmark_(http_[45]xx|stream_resets|pool_.*)_total"}`,
which has no series on a clean run), and p95 per target from the latency
histogram, labelled as throttle-inclusive. With the one-series-for-all-nodes
caveat above it is a liveness view, not a grading one. It is a GitOps change
(sidecar ConfigMaps in `k8s-talos-main`), made in phase 2, once the first talos
run has shown what the series look like there. Retiring `aether-k6` and
`k6-operator` is phase 4, and only if nothing else still uses them.

## Report durability

- The sortie pod writes the JSON report to
  `/var/run/sortie/<run tag>.json` on PVC `sortie-soak-reports` (`run.sh`
  creates it; `openebs-hostpath` on talos-main, which has no default class, so
  `run.sh` takes the cluster's only class, or `--storage-class`). One small
  file per run; the PVC is kept across runs.
- `sortie-save.sh` is armed, detached, at load start. When the Job finishes it
  copies into the run directory: the report (through a short-lived reader pod
  that prints the file — `kubectl exec` is denied on talos-main), the sortie
  pod's log (the readable summary), the Job and pod objects, every engine's
  log, and the engine pod list. It writes `SAVED` last, only if the report
  parsed.
- `sortie-teardown.sh` refuses to uninstall without `SAVED` (`--force`
  overrides), and keeps the PVC unless `--purge`.
- The Job has no TTL, so the pod and its log stay until teardown.

## What is in the repository now (#1323)

| file | role |
|---|---|
| `e2e/soak/run.sh` | the one kickoff: pre-flight → engines → load → churn (soak) → watchdog at T0 → saver |
| `e2e/soak/sortie-values.yaml` | chart values: DaemonSet, mesh label, PriorityClass, resources, digests |
| `e2e/soak/sortie-targets.txt`, `sortie-plan.sh` | the eight targets and weights; the plan template for both profiles and the share arithmetic |
| `e2e/soak/sortie-save.sh`, `sortie-gate.sh`, `sortie-teardown.sh` | save before teardown; PASS/FAIL per target; uninstall |
| `e2e/soak/pods-not-ready.awk` | the numeric readiness check |
| `e2e/soak/harness_test.sh` + `testdata/{sortie,preflight}/` | offline tests of the gate, the plan and the readiness check (`//e2e/soak:harness_test`) |

The two #1323 defects: the watchdog is started by `run.sh` right after the
churn driver (soak) or the load (e2e), never after the kickoff returns; the
readiness check compares ready and total as numbers, and the test suite keeps
the old expression as a red reading (it flags all eight healthy pods of the
fixture).

Deploying aether is deliberately not in `run.sh`. The nightly wrapper's
"wait for main to publish, deploy, check surge/debug/limits" half stays a
separately authorized `helm upgrade`.

## Evidence on kind (2026-10-06/07)

A three-node kind cluster (pinned kind v0.33.0, one control plane and two
workers), aether from this tree as `e2e/uds.sh up` installs it (SPIRE off,
`uds-echo`, `uds-cr-echo`, `tcp-echo`), a collector with talos-main's statsd
receiver settings as `o11y/otel-scraper`, and the committed values and plan
with a stand-in targets file of the same shape (six 9 rps targets on
`tcp-echo`, the two UDS targets at 3 rps). Every run went through `run.sh e2e`.

| run | result |
|---|---|
| SHORT profile, 15 min | pool resolved to 2 backends, one per worker node; 16,184–16,185 2xx per 9 rps target (16,200 planned, 18.00 rps), 5,395–5,396 per UDS target (5,400, 6.00 rps); every failure class 0; `VERDICT PASS targets=8 passed=8`; watchdog `verdict=PASS new_restarts=0`; report saved off the PVC by the saver; engine 0.407 cores and 300 MiB per node |
| the same with one target's Deployment scaled to 0 for 90 s | `FAIL uds-cr-echo … http_5xx=554 [sortie-worker=277 sortie-worker2=277] \| rate 3.68 rps < 99% of 6`; the seven other targets `PASS`; `VERDICT FAIL targets=8 passed=7 failed=1`; gate exit 1 |
| 11 min with `--rolls` (agent roll at T+3m, proxy roll at T+8m) | both `ROLLED`; `VERDICT PASS`, every failure class 0 on all eight targets |
| engine memory limit 512Mi, `--progress 60s` (the first attempt) | both engines `OOMKilled` at T+60 s; every target `error: … EOF`; the watchdog named both restarts; the saver still saved the report |

The clean roll result is a kind result: one replica per target, SPIRE off, a
20-core host. It shows the driver does not manufacture failures at a hot
restart; it says nothing about what a roll costs on talos-main.

## Risks

1. **Concurrent executions are tested upstream over HTTP/1 only.** The soak
   drives HTTP/1.1 only, so this is inside what sortie tests. It rules out, for
   now, moving the multi-protocol or UDP dialer legs (`mp-dialer`,
   `udp-dialer`) onto sortie's TCP/UDP modes alongside the HTTP targets.
2. **An 8 h unattended run is unproven**: memory over ~9 M requests per node,
   pacing at soak length, the final report's size and assembly time. The first
   sortie soak (phase 3) is the measurement; the k6 files stay until it has
   passed.
3. **The CPU limit can make the harness fail the gate** (`pool_overflow`) on a
   node slower or busier than kind. It is in the zero-failure set, it is named
   per node, and the control for "is this the harness" is that it appears with
   no `http_5xx` / `stream_resets` beside it.
4. **One sortie pod is a single point of failure for the run.** If it is
   evicted or its node drains, every execution is cancelled and reported
   `cancelled`, not evaluated: 8 h gone. k6 runners failed independently. It
   has `backoffLimit: 0` on purpose (a retry would start a second, partial
   run); it carries no PriorityClass today (see "Open questions").
5. **An engine that dies takes its node's eight executions with it**, and the
   report then holds an `error` for every target (one dead backend fails the
   execution for the pool). The other nodes' counters for that run are lost
   with it.
6. **A node that joins or a pod that is replaced mid-run gets no load.** The
   pool is fixed at start. An engine pod that is deleted and recreated (a node
   drain) is a new IP the run never drives.
7. **No response timeout** (above): a class of failure k6 counted is now
   invisible to the loader.

## Cut-over

1. **Short e2e on talos-main** (15 min, `run.sh e2e`), k6 not running. Reads:
   the pool is five backends on five nodes; per-target rate holds under the
   400m limit; `pool_overflow` is zero; what the engine really costs on those
   CPUs; that the `sortie_mesh_*` series arrive; that a run under one agent
   roll and one proxy roll (`--rolls`) produces failures the access logs can
   attribute. Exit: `sortie-gate.sh` PASS on a no-roll run, and every number in
   "Unverified" replaced by a reading.
2. **Side by side.** One short run with the k6 runner up as well
   (`run.sh` notes it; the node carries both loads, so this is a comparison of
   instruments, not a capacity test): per target, sortie's failures against
   k6's by-target table and against the access logs for both identities.
   The dashboard change lands here.
3. **First sortie soak** (`run.sh soak`), k6 not running. It is graded as a
   soak and as the 8 h proof of the driver. Exit: prober gates as always, the
   loader ran 8h30m on five backends, its failures reconcile against access
   logs the way k6's did.
4. **Retire k6**: delete `k6-mesh-soak.js` and `k6-runner.yaml`, the k6
   sections of the README, the `aether-k6` dashboard and `k6-operator` (GitOps),
   and rewrite the README passages that still say "k6" (the churn script's
   comments too). Not before phase 3 has passed.

## Unverified until the talos run

- Engine CPU per worker thread on talos-main's CPUs, and therefore how hard the
  400m limit throttles there and whether a queue of 16 is enough.
- That `aether-test/soak-sortie` gets its SVID and twins without first-use
  failures after a 30 s settle (SPIRE is off in the kind harness used here).
- The `openebs-hostpath` PVC: provisioning time, and that uid 65532 can write
  to it.
- The statsd path end to end on the real `otel-scraper` and Prometheus, and
  what five engines writing one series does to it there (on kind: one engine's
  worth).
- Behaviour under a proxy hot restart: whether the engine's HTTP/1.1
  connections drain cleanly on `Connection: close`, and what a roll costs in
  `stream_resets`.
- Anything past 15 minutes.

## Open questions

- **sortie: an idle strategy that blocks until the next request.** Removes the
  CPU limit, the pending queue and the latency caveat. Until then the limit is
  load-bearing.
- **sortie: the backend in the statsd prefix or as a tag.** Without it live
  series cannot be per node.
- **sortie: failure predicates.** A plan with thresholds wants the run to
  finish and be judged; today that takes four `nighthawk_template` lines that
  are easy not to know about, and an execution ended by a predicate reports no
  counters. Either default them off when a plan has thresholds, or keep the
  counters in the report of an execution that ended early.
- **sortie: `--progress` cost.** A snapshot more than doubles a running
  engine's memory and keeps it; the README calls progress "advisory".
- **sortie: per-backend request totals in the JSON report.** The text summary
  has them; the JSON has only thresholds and non-zero failure counters, which is
  why the plan carries a `counter:benchmark.http_2xx > 0` threshold as a
  carrier for the 2xx total.
- **Should the sortie pod get `aether-soak-loader` too?** It is the run's
  single point of failure and it can move, so the class would protect it from
  preemption without the DaemonSet argument. Probably yes; left out of this PR
  to keep the first talos run identical to what was tested on kind.
- **Should the loader keep `default` as its identity** (`serviceAccount.create:
  false`) so old access-log queries keep working? Proposed: no — a named
  identity is the better instrument, and the side-by-side needs the two loads
  to be distinguishable.
- **The e2e profile's rolls.** `run.sh e2e --rolls` does one agent roll and one
  proxy roll inside 15 minutes; the old workstation e2e did one agent roll and
  three proxy rolls over 46 minutes. Whether the short profile should grow to
  that is a decision for after phase 1.
