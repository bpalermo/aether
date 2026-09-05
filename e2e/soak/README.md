# 8-hour soak harness

Validates a deployed build against sustained mesh load plus continuous rollout churn.
Used on the `talos-main` cluster before promoting a release.

Three components run together:

| Component | What it proves |
|---|---|
| **External prober** (`//prober`, DaemonSet, already deployed) | the availability SLI — **authoritative for PASS/FAIL** |
| **k6 runners** (`k6-runner.yaml`) | mesh load by NAME (~300/s) so DNS + cross-node paths are exercised |
| **Churn driver** (`churn.sh`) | 31 rolling restarts incl. mesh-dns/agent/proxy/edge + a concurrent triple, then a 90-minute no-roll window and a demand-set shrink |

The prober is external **on purpose**: the mesh's own self-reported metrics are blind to
the very churn being tested. Never grade a soak on mesh self-SLI alone.

## Run

```bash
# 0. The mesh_dns SLI target. Only needed once (and after any change to it), but
#    CHECK IT before a run: the prober resolves echo.<ns>.aether.internal on every
#    node, so if this is a single replica the mesh_dns tier measures that one pod's
#    node instead of the mesh. See the gotcha below.
kubectl apply -n aether-test -f e2e/soak/echo.yaml
kubectl -n aether-test get pods -l app=echo -o wide   # expect 3, on 3 different nodes

# 1. Load the k6 script as a ConfigMap (source of truth is the .js file here).
kubectl create configmap k6-soak-script -n aether-test \
  --from-file=test.js=e2e/soak/k6-mesh-soak.js \
  --dry-run=client -o yaml | kubectl apply -f -

# 2. Pre-flight: every component Ready, 0 restarts, prober SLI live at 25/s with 0 errors.
kubectl get pods -n aether-system
# prober baseline (Grafana/Prometheus):
#   sum by (tier) (rate(aether_probe_requests_total{result="success"}[3m]))   -> ~25/s
#   sum by (tier) (rate(aether_probe_requests_total{result!="success"}[5m]))  -> 0

# 3. Start load, then churn. Detached — churn sleeps ~7h32m, and `nohup setsid` is
#    mandatory: on 2026-09-03 the harness reaped a plain `&` job at T0+75m.
kubectl apply -f e2e/soak/k6-runner.yaml
nohup setsid bash e2e/soak/churn.sh "rev192/0.92.0" >/dev/null 2>&1 &

# 4. Age-matched proxy RSS baseline at T0+30m (churn.sh takes the rest itself).
bash e2e/soak/sample-proxy-rss.sh --at-age 1800

# 5. Teardown at T0+8h.
kubectl delete -f e2e/soak/k6-runner.yaml
grep -c ROLLED /tmp/soak-churn.log      # expect 31
grep -E "no-roll window|SHRINK" /tmp/soak-churn.log
column -t /tmp/soak-proxy-rss.tsv       # the #628 age-matched series
```

## The churn schedule

29 schedule entries; the TRIPLE fires three rolls at once, so `grep -c ROLLED` is
**31** (6 proxy, 2 agent, 3 mesh-dns, 2 edge, 18 svc). The older footer said 30 — it
forgot the TRIPLE's proxy. The set of rolls has not changed, only the tally.

| T0+ (min) | Roll | | T0+ (min) | Roll |
|---|---|---|---|---|
| 12 | svc-1 | | 24 | svc-2 |
| 36 | **proxy** \* | | 48 | svc-3 |
| 60 | mesh-dns | | 72 | **agent** |
| 84 | svc-5 | | 96 | **proxy** \* |
| 108 | svc-1 | | 120 | edge |
| 132 | svc-2 | | 144 | mesh-dns |
| 156 | svc-3 | | 168 | svc-4 |
| 180 | svc-5 | | 192 | svc-1 |
| 204 | edge | | 216 | **proxy** \* |
| 228 | svc-2 | | 240 | svc-4 |
| 252 | **proxy** \* | | 264 | svc-3 |
| 276 | svc-1 | | 300 | **TRIPLE** \* — agent + proxy + svc-3, the stress peak **and the last agent roll** |
| 312 | mesh-dns | | 324 | svc-4 |
| 336 | **proxy** \* | | 348 | svc-2 |
| 360 | svc-1 — the last roll of any kind | | | |
| **360 → 450** | **NO-ROLL WINDOW** — nothing is rolled for 90 minutes | | | |
| 450 | **SHRINK** — `svc-5` scaled to 0 for 90s, then restored | | | |
| ~452 | `churn driver complete` (k6 runs 7h40m, so both land under load) | | | |

\* = 30 minutes after that proxy roll an age-matched RSS sample is taken in the
background (`sample-proxy-rss.sh --at-age 1800`), for #628. Six samples per run.

### The no-roll window (#682)

The node agent's demand-scoped dependency set holds each observed upstream for **1h**,
and rolling `aether-agent` rebuilds that set with a fresh TTL. The old schedule rolled
the agent at T0+90m and again at T0+300m, so **the TTL could never expire inside a
soak**: for its entire history this harness was *structurally blind* to the whole
TTL-expiry → ODCDS-stall class, which surfaced only in the quiet hours between runs
(#682). The window sits after the last agent roll and outlasts the TTL, so the expiry
now happens under load, inside the graded window, on the nodes that have no local
replica of the upstream (w01/w03 in the current `echo` placement).

Opt out with `SOAK_NO_ROLL_WINDOW=0` (default on). `SOAK_NO_ROLL_END_MIN` moves the end.

### The demand-set shrink (#682)

Grading the 2026-09-05 run found a much cheaper trigger: **any** demand-set shrink —
not the 1h TTL specifically — drops the cluster on a node with no local replica and
exposes the same ODCDS stall, in seconds instead of an hour. So after the window the
driver scales `deploy/svc-5` to 0 for 90s and restores it to **whatever replica count
it read first** (never a hard-coded number; an EXIT/INT/TERM trap restores it even if
the driver is killed mid-shrink).

`svc-5` is the target on purpose: it is the one service the k6 script declares as an
upstream but never actually drives, so bouncing it cannot pollute the k6 error rate or
the prober SLI. The observable is the log pair — `service left dependency set` in the
agent log, `cm odcds: ... timed out` in the proxy log.

Opt out with `SOAK_SHRINK=0` (default on); `SOAK_SHRINK_TARGET` / `SOAK_SHRINK_SECONDS`
retarget it.

**Authorization:** rolling these shared `talos-main` workloads for soak validation is
standing-authorized, and scaling `svc-5` down and back up for 90s is the same class of
action on the same harness namespace.

## Proxy RSS sampling (#628)

`sample-proxy-rss.sh` prints one row per `aether-proxy` pod:

```
timestamp             node            pod                 age_seconds  working_set_mi
2026-09-05T01:35:04Z  main-worker-01  aether-proxy-abcde  1803         241
```

Rows are appended to `/tmp/soak-proxy-rss.tsv` (header written once) and echoed to
stdout. `--at-age SECONDS` polls every 15s (max 20 min) until the **youngest** proxy
pod reaches that age and then samples once, so every checkpoint reads the same
generation age.

**Run it at T0+30m, and 30 minutes after each proxy roll** (`churn.sh` logs
`ROLLED aether-system/daemonset/aether-proxy`, and queues those samples itself).

Why the age match: #628 has never had two age-matched samples. Churn recycles the proxy
every ~30-90 minutes, so a "higher" node is usually just an older incarnation, and
ramp-then-plateau (born-hot) cannot be told from a leak without holding age fixed.

## Grading

Compute prober deltas over the churn window and compare against the last known-good run:

```promql
sum by (tier, result) (increase(aether_probe_requests_total[8h]))
```

- **liveness** tier (local, no DNS) — data-path SLI. Target **0.000%**.
- **mesh_dns** tier (resolves a real FQDN) — DNS + cross-node SLI. Target: `dns_error`,
  `dns_nxdomain`, `dns_timeout` **all zero**. A residual `http_error` (~0.02%) is the
  known cross-node drain path, tracked separately.
- **#682 episodes during the no-roll window or SHRINK are the harness working, not a
  regression** — attribute via the agent log line `service left dependency set`.

## Hard-won gotchas

Each of these invalidated a real run:

1. **Runner pods MUST carry `aether.io/managed: "true"`.** Without it there's no ndots
   injection and no per-pod CNI `:53` DNAT, so mesh DNS never resolves — 100% failure
   that looks like a mesh outage but is a harness bug.
2. **Mesh DNS names are namespace-qualified: `<svc>.<ns>.aether.internal`.** The flat
   `<svc>.aether.internal` form **never** resolves and returns NXDOMAIN.
3. **Never restart the otel-collector mid-soak.** It causes Prometheus series churn that
   makes the prober's cumulative counters non-monotonic — they become unusable for
   grading. If the SLI breaks mid-run, grade from the clean window *before* the break
   plus `kubectl` evidence; do **not** trust `increase[8h]` spanning a gap.
4. **Watch collector memory.** If the collector saturates it silently sheds prober
   exports and blinds the SLI (it is the same collector the mesh exports to). Sample
   `kubectl top pods -n o11y` alongside the prober rate.
5. **Never `helm --reuse-values`** on aether charts — it silently pins a stale image
   digest. Use `helm get values <rel> -n <ns> -o yaml > /tmp/v.yaml` then `-f /tmp/v.yaml`.
6. **k6 needs 1Gi.** At 256Mi runners OOM-restart ~3-5h into the 7h40m run, fragmenting
   the summary.
7. **`echo` must be multi-replica and spread, or the mesh_dns tier is not a mesh
   signal.** It is the *only* target of that tier, so its own health is
   indistinguishable from the mesh's. On 2026-09-02 it was a single replica that
   happened to sit on the node hosting the entire o11y stack, with a 10m CPU request;
   when that node saturated, echo starved and mesh_dns reported ~30% errors fleet-wide
   (66% on the co-located prober) while the mesh itself was fine — and the run was
   ungradeable. It had no repo-owned definition at all, so there was nowhere to fix
   it; `e2e/soak/echo.yaml` now owns it with 3 replicas, a soft hostname spread, and
   honest requests. It is deliberately NOT `test/e2e/testdata/echo.yaml`, which the
   CNI e2e tests apply on single-node kind and must stay minimal.
8. **A churn schedule can hide a whole defect class.** Until 2026-09-05 the driver
   rolled `aether-agent` every ~15 minutes for the entire run, and every agent roll
   rebuilds the demand-scoped dependency set with a fresh 1h TTL. The soak therefore
   *could not* observe a TTL expiry, and #682 — 5-minute per-node outages on nodes
   with no local replica — lived undetected in the quiet hours between runs for the
   harness's whole history while every soak reported PASS. The no-roll window and the
   shrink exist to close that hole; a repeat of this shape (a churn step that resets
   the very state a defect needs to age) is worth looking for whenever a bug is only
   ever seen *between* soaks.

## Files

- `echo.yaml` — the mesh_dns SLI target (3 replicas, soft hostname spread). Apply before
  a run; it is the workload the mesh_dns tier actually measures.
- `k6-mesh-soak.js` — load script (constant-arrival-rate, qualified mesh names, no OTLP).
- `k6-runner.yaml` — the 5-node runner DaemonSet.
- `churn.sh` — the 31-roll churn driver plus the no-roll window and the demand-set
  shrink; takes a build label for the log header.
- `sample-proxy-rss.sh` — age-matched `aether-proxy` working-set sampler for #628.
  Standalone, and queued automatically by `churn.sh` after each proxy roll.
