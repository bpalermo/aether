# 8-hour soak harness

Validates a deployed build against sustained mesh load plus continuous rollout churn.
Used on the `talos-main` cluster before promoting a release.

Three components run together:

| Component | What it proves |
|---|---|
| **External prober** (`//prober`, DaemonSet, already deployed) | the availability SLI — **authoritative for PASS/FAIL** |
| **k6 runners** (`k6-runner.yaml`) | mesh load by NAME (~300/s) so DNS + cross-node paths are exercised, 5% of it on the two UDS-delivered services (#1108) |
| **Churn driver** (`churn.sh`) | 33 rolling restarts incl. mesh-dns/agent/proxy/edge/uds-csi + a concurrent triple, two mid-run pods under a brand-new ServiceAccount (#1014), then a 90-minute no-roll window and a demand-set shrink |
| **Restart watchdog** (`restart-watch.sh`) | 0 new container restarts in the soak's namespaces, with the last-terminated reason of any that happen (#1242) |
| **Multi-protocol leg** (`multiprotocol.yaml`) | proposal 037's per-port TCP chains under load, and the evidence for its Phase 4 gate |
| **UDP leg** (`udp.yaml`) | proposal 038's transparent UDP capture under load: the divert, the transparent socket, and the VIP-sourced reply, through every roll |

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

# 0b. The proposal-037 leg: a multi-protocol workload (HTTP :8080 primary + raw
#     TCP :9000) plus the per-node dialer that keeps its new chains busy. Same
#     "only needed once" status as echo.yaml, and the same reason to CHECK it:
#     without it the whole 037 data path sits idle for eight hours.
kubectl apply -n aether-test -f e2e/soak/multiprotocol.yaml
kubectl -n aether-test get pods -l app=mixed-svc -o wide     # expect 3, spread
kubectl -n aether-test logs -l app.kubernetes.io/name=mp-dialer --tail=1 | grep AETHER_METRIC

# 0b2. The proposal-038 leg: a UDP-primary workload behind a UDPRoute plus the
#      per-node dialer that keeps the transparent UDP capture busy. Same "only
#      needed once" status and the same reason to CHECK it: without it the UDP
#      half of #947 sits idle for eight hours. The dialer must already be
#      reporting fail=0 before T0 -- a leg that is failing at T0 measures nothing
#      about churn.
kubectl apply -n aether-test -f e2e/soak/udp.yaml
kubectl -n aether-test get pods -l app=udp-echo -o wide         # expect 3, spread
kubectl -n aether-test logs -l app.kubernetes.io/name=udp-dialer --tail=1 | grep AETHER_METRIC

# 0c. ONCE, before the run: prove the any-port shim can be seen taking a
#     connection. See "The Phase 4 evidence clock" below -- the probe's L4
#     access-log rows (port 19999) are the control that makes an empty gate
#     evidence (#1095); its counter bump is only a pre-roll sanity check.
bash e2e/soak/anyport-probe.sh

# 0d. ONCE, before the run: one new-ServiceAccount step, now, so the two the
#     driver runs mid-soak are known to work on this cluster (image resident,
#     objects creatable, tally readable). Its own log; exits 1 on a hole. Expect a
#     single `ROLLED newsa/...` line with non2xx=0 and connerr=0 on every
#     destination. See "The new-ServiceAccount step" below.
SOAK_CHURN_LOG=/tmp/soak-newsa-preflight.log \
  bash "$PWD/e2e/soak/churn.sh" --context talos-main --new-sa-once &&
  grep -E 'ROLLED|FAILED' /tmp/soak-newsa-preflight.log

# 0e. The UDS leg (#1108/#1109): charts/udsecho installed in aether-test (uds-echo
#     = annotation delivery, uds-cr-echo = EndpointPolicy delivery, uds-client),
#     on the csi.aether.io carrier. k6 drives both services and churn.sh rolls
#     aether-uds-csi under them, deleting one of their pods mid-roll; its
#     pre-flight refuses to start without them (or set SOAK_UDSCSI=0). Optionally,
#     ONCE before the run, one uds-csi step now. It rolls the plugin and replaces
#     one uds-echo pod. Expect a single `ROLLED aether-system/daemonset/aether-uds-csi`
#     line with window=down, csinode=n/n and failedmount_after=0. See "The UDS leg".
kubectl -n aether-test get deploy uds-echo uds-cr-echo uds-client   # expect 2/2 2/2 1/1
SOAK_CHURN_LOG=/tmp/soak-udscsi-preflight.log \
  bash "$PWD/e2e/soak/churn.sh" --context talos-main --uds-csi-once &&
  grep -E 'ROLLED|FAILED' /tmp/soak-udscsi-preflight.log

# 1. Load the k6 script as a ConfigMap (source of truth is the .js file here).
#    RE-RUN THIS after any edit to k6-mesh-soak.js -- the pod mounts the
#    ConfigMap, so an edited .js that was never re-applied runs the OLD script
#    and the run looks normal while measuring the wrong thing.
kubectl create configmap k6-soak-script -n aether-test \
  --from-file=test.js=e2e/soak/k6-mesh-soak.js \
  --dry-run=client -o yaml | kubectl apply -f -

# 2. Pre-flight: every component Ready, 0 restarts, prober SLI live at 25/s with 0 errors.
kubectl get pods -n aether-system
# (restarts that already exist are recorded by the watchdog's baseline in 3b and
# reported as BASELINE, not graded; still find out why they happened before T0)
# prober baseline (Grafana/Prometheus):
#   sum by (tier) (rate(aether_probe_requests_total{result="success"}[3m]))
#     -> liveness ~25/s, mesh_dns ~50/s (two targets at ~25/s each)
#   sum by (tier) (rate(aether_probe_requests_total{result!="success"}[5m]))  -> 0
# (a rate is fine for this live check; the GRADE uses raw counters, see "Grading")

# 3. Start load, then churn ~4 minutes later, once all five runners report
#    60.00 iters/s and 0 interrupted. T0 is the churn driver's own start line. k6
#    runs 8h30m, so with this offset it outlasts the graded T0+8h window by ~25
#    minutes and EXITS BY ITSELF (~T0+8h26m): the runners cannot self-restart
#    inside the window (the 2026-09-06 artifact), and the end-of-test summary
#    exists to be read. Detached — churn sleeps ~7h32m, and `nohup setsid` from the
#    MAIN session is mandatory: on 2026-09-03 the harness reaped a plain `&` job at
#    T0+75m, and a driver started inside a subagent dies with it.
kubectl apply -f e2e/soak/k6-runner.yaml
# REFUSE to start a second driver. Two of them share /tmp/soak-churn.log and
# interleave their schedules into ~66 rolls instead of 33, which does not fail
# the run -- it silently invalidates it. Match on the ABSOLUTE path, never on
# "soak/churn.sh": see "Stopping the churn driver" below for why.
#
# Pass the context EXPLICITLY (#951). churn.sh never uses the kubeconfig's
# current-context, which `kind delete cluster` clears when it pointed at the kind
# cluster: on 2026-09-26 that made every roll hit localhost:8080 for 58 minutes
# while the soak looked healthy. The foreground --preflight run is the loud one:
# it checks /readyz, that every DaemonSet/Deployment the schedule rolls (and the
# SHRINK target) exists, and that the context may patch them, that the uds-csi
# step's UDS Deployments have a Running pod to delete, and exits non-zero
# with the reason on stderr. The detached launch pre-flights again, but its stderr
# goes to /dev/null -- so never skip the foreground run. A failed pre-flight
# writes NOTHING: /tmp/soak-churn.log is neither archived nor truncated, and no T0
# line exists.
if pgrep -f "bash $PWD/e2e/soak/churn.sh"; then
  echo "a driver is already running"
elif bash "$PWD/e2e/soak/churn.sh" --context talos-main --preflight; then
  nohup setsid bash "$PWD/e2e/soak/churn.sh" --context talos-main "rev192/0.92.0" >/dev/null 2>&1 &
fi
# Confirm the launch: the first line must be TODAY's start line, the second the
# context. A stale T0 means the detached run refused to start.
head -2 /tmp/soak-churn.log
#
# 3b. The restart watchdog (#1242), right after the churn driver so its
#     baseline is T0. Same rules: explicit context, foreground pre-flight
#     (it takes the baseline and prints it), then detached with nohup setsid
#     from the MAIN session. It runs 8h (the graded window) and must not run
#     past ~T0+8h26m, where the k6 runners exit and restart once by design.
#     See "The restart gate" under Grading.
if pgrep -f "bash $PWD/e2e/soak/restart-watch.sh"; then
  echo "a watchdog is already running"
elif bash "$PWD/e2e/soak/restart-watch.sh" --context talos-main --preflight; then
  nohup setsid bash "$PWD/e2e/soak/restart-watch.sh" --context talos-main >/dev/null 2>&1 &
fi
head -2 /tmp/soak-restart-watch.log   # TODAY's start line, then `BASELINE count=<n>`
#
# Fail fast: the FIRST `FAILED` roll (or a SHRINK that cannot scale) ends the
# driver with `CHURN ABORTED ...` and exit 1, restoring the SHRINK target first.
# It no longer carries on with holes in the schedule. An ABORTED line means stop,
# fix, relaunch with a fresh T0 -- that run is not gradeable. The two exceptions
# are `FAILED newsa/...` and `FAILED aether-system/daemonset/aether-uds-csi ...`
# with a tally and no ABORTED after it: that is the new-ServiceAccount or the
# uds-csi gate reading red (a finding), not a hole -- the run goes on.
grep -E "FAILED|CHURN ABORTED" /tmp/soak-churn.log   # expect nothing

# 4. Age-matched proxy RSS baseline at T0+30m (churn.sh takes the rest itself,
#    passing its own --context through).
bash e2e/soak/sample-proxy-rss.sh --context talos-main --at-age 1800

# 5. Teardown AFTER k6 has exited on its own (~T0+8h26m), never at T0+8h sharp:
#    k6 publishes no metrics to Prometheus here, so `http_req_failed` exists only
#    in each runner's end-of-test summary, and deleting the DaemonSet first loses
#    it. The container restarts once after exiting, so the summary is in
#    `logs --previous`.
for p in $(kubectl -n aether-test get pods -o name | grep k6-soak-loader); do
  kubectl -n aether-test logs --previous "$p" | grep -E 'http_req_failed|iterations|dropped'
done
kubectl delete -f e2e/soak/k6-runner.yaml
# Count ROLLED in the CURRENT log only. churn.sh archives the previous run as
# /tmp/soak-churn.log.<ts>.prev, and older runs left /tmp/soak-churn-*.log
# behind, so `grep -c ROLLED /tmp/soak-churn*` inflates the tally across runs.
grep -c ROLLED /tmp/soak-churn.log      # expect 35 (33 rolls incl. 2 uds-csi + 2 new-SA steps) -- this exact path, no glob
grep -E "newsa/" /tmp/soak-churn.log    # expect 2 ROLLED, 0 FAILED -- see "The new-ServiceAccount step"
grep aether-uds-csi /tmp/soak-churn.log # expect 2 ROLLED, 0 FAILED, each window=down -- see "The UDS leg"
grep ' SUMMARY ' /tmp/soak-restart-watch.log  # expect verdict=PASS new_restarts=0 -- see "The restart gate"
grep -E "no-roll window|SHRINK" /tmp/soak-churn.log
column -t /tmp/soak-proxy-rss.tsv       # the #628 age-matched series
```

## The churn schedule

31 roll entries; the TRIPLE fires three rolls at once, so the rolls are **33** (6
proxy, 2 agent, 3 mesh-dns, 2 edge, 2 uds-csi, 18 svc). An older footer said 30 — it
forgot the TRIPLE's proxy. The two new-ServiceAccount steps (#1014) each log one
`ROLLED newsa/…` line too, so `grep -c ROLLED` is **35** by default.

| T0+ (min) | Roll | | T0+ (min) | Roll |
|---|---|---|---|---|
| 12 | svc-1 | | 24 | svc-2 |
| 36 | mesh-dns | | 48 | svc-3 |
| 60 | **proxy** \* — the first one, a full hour after T0 (see below) | | 72 | **agent** |
| 84 | svc-5 | | 96 | **proxy** \* |
| 108 | svc-1 | | 120 | edge |
| 126 | **NEW-SA** #1 — a pod under a brand-new ServiceAccount, 2 min of traffic (#1014) | | | |
| 132 | svc-2 | | 144 | mesh-dns |
| 156 | svc-3 | | 168 | svc-4 |
| 162 | **UDS-CSI** #1 — roll `aether-uds-csi`, delete a `uds-echo` pod mid-roll (#1109) | | | |
| 180 | svc-5 | | 192 | svc-1 |
| 204 | edge | | 216 | **proxy** \* |
| 228 | svc-2 | | 240 | svc-4 |
| 252 | **proxy** \* | | 264 | svc-3 |
| 270 | **UDS-CSI** #2 — the same, deleting a `uds-cr-echo` pod (#1109) | | | |
| 276 | svc-1 | | 288 | **NEW-SA** #2 (#1014) |
| 300 | **TRIPLE** \* — agent + proxy + svc-3, the stress peak **and the last agent roll** | | | |
| 312 | mesh-dns | | 324 | svc-4 |
| 336 | **proxy** \* | | 348 | svc-2 |
| 360 | svc-1 — the last roll of any kind | | | |
| **360 → 450** | **NO-ROLL WINDOW** — nothing is rolled for 90 minutes | | | |
| 450 | **SHRINK** — `svc-5` scaled to 0 for 90s, then restored | | | |
| ~452 | `churn driver complete` (k6 runs 8h30m: both land under load AND the runners cannot self-restart inside the graded T0+8h window) | | | |

\* = 30 minutes after that proxy roll an age-matched RSS sample is taken in the
background (`sample-proxy-rss.sh --at-age 1800`), for #628. Six samples per run.

### The first proxy roll (T0+60)

It used to be at T0+36. On 2026-09-19 that first roll cost **15 mesh_dns timeouts
across all five probers**, while the other five proxy rolls of the same run — identical
`echo` placement — cost 0 / 1 / 0 / 0 / 0. What set it apart is that it replaced the
only Envoy generation born *before* the load started. Either that generation is the
cause (connections that predate k6), or the roll simply landed before the load had
settled. A full hour of steady load before the first roll separates the two: if it still
costs an order of magnitude more than the rest, it is the pre-load generation. Grade the
first proxy roll as its own episode either way, and do not average it into the others.

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
upstream but never actually drives, so bouncing it cannot pollute the k6 error rate.
**It is not free on the prober SLI**, though this file used to say so: on 2026-09-19 the
restore cost **18 mesh_dns timeouts — 43% of that run's total and its largest single
episode**. The chain was: `svc-5` back at 07:03:01Z → agents WARN `inbound chain
references a secret absent from the snapshot` (`…/sa/svc-5`) at :03 → prober requests
expiring client-side at :20–:22 (`DC` / `downstream_remote_disconnect` in the access
log; no `UF`, `UH`, `URX` or `NR`, and upstream hosts on three nodes, so routing and
`echo` were healthy — added latency on the busiest node, not an outage). Grade the SHRINK
as its own episode and keep it out of any "steady-state" error rate. The #682 observable
is the log pair — `expired observed upstreams from node dependency set` in the agent log
(not `service left dependency set`, which no build logs), `cm odcds: ... timed out` in
the proxy log.

Opt out with `SOAK_SHRINK=0` (default on); `SOAK_SHRINK_TARGET` / `SOAK_SHRINK_SECONDS`
retarget it.

### The new-ServiceAccount step (#1008/#1014)

**Why.** Every other step rolls a workload whose ServiceAccount the node has already
seen, so the run never makes a node proxy build clusters for an identity that is *new
to it*. That is exactly where #1008 lived: an east-west QUIC twin
(`<ns>/<svc>@<ns>/<sa>`) added *after* its h2 base was subscribed shared the base's EDS
resource name, Envoy's delta `WatchMap` deduplicated the subscription, and the twin sat
warming for its full 15 s `initial_fetch_timeout` while every request from that
identity to a QUIC destination got **503/NC**. On rev242 that was **1,060** k6 failures
in 11 s, visible only because the k6 loaders happened to start (as a new identity,
`aether-test/default`, on every node) 58 s before T0 — the 8 h of churn after it showed
nothing, because nothing in it introduced a new identity. Fixed in #1012 (each twin
gets its own EDS resource name); this step is what lets a soak see a regression of it.
Gotcha 8's shape again: the schedule never created the state the defect needed.

**Since #1020 it also proves the on-demand path.** Twins are demand-scoped: a new
identity has a selection arm on svc-1's route but no twin until it dials. Its **first**
request to svc-1 resolves to the missing twin. The proxy fetches it over ODCDS, and
the agent admits the pair and publishes the twin together with its load assignment.
That request, and every one after it, must be **200 with no `NC`**. An `NC` in the
step's first ~2 s is the on-demand fetch failing, and ~15 s of them is #1008 back (every
on-demand twin is a late twin). Either way, it is a gate failure.

**What it does.** At T0+126 and T0+288 (`SOAK_NEWSA_OFFSETS`, minutes; nudged off the
T0+120 edge roll and the T0+300 TRIPLE so neither confounds the other) the driver:

1. creates, together, a ServiceAccount, a ConfigMap (`newsa-client.sh`) and a
   1-replica Deployment, all named `sa-new-<epoch>`, in `aether-test`, pinned with a
   `kubernetes.io/hostname` nodeSelector to one Ready worker, round-robin across steps
   (offset by T0, so successive runs start elsewhere);
2. the pod (`curlimages/curl:8.22.0`, already resident for the UDP dialer, so no pull)
   drives **~20 rps** from its first instant for **120 s**, split across
   `svc-1.aether-test.aether.internal:18081` (QUIC-enabled on the proving run) and
   `svc-3…:18081` (h2 on the proving run; since #979 both are QUIC-eligible, so
   each is a first-use twin fetch for the new ServiceAccount), with user agent **`aether-soak-newsa/sa-new-<epoch>`**, and
   prints a per-destination `ok` / `non2xx` / `connerr` tally every 10 s
   (`AETHER_NEWSA …`), then `AETHER_NEWSA_FINAL …` and
   `AETHER_METRIC newsa_tally={…}`;
3. copies those lines into the churn log, deletes all three objects together, and logs

   ```
   ROLLED newsa/sa-new-<epoch> <node> ready=<s>s svc-1:ok=1300,non2xx=0,connerr=0,codes=- svc-3:ok=1300,…
   ```

   or `FAILED newsa/…` with the same tally when any destination saw a non-2xx or a
   connect error, or no 2xx at all.

A `FAILED newsa/` line **without** a following `CHURN ABORTED` is a gate finding, not
a hole in the schedule: the driver carries on and the run stays gradeable. A step
that produced no tally at all (apply refused, pod not Ready in 180 s, no final line)
is a hole and aborts the run like a failed roll. The step's name is the join key
everywhere: it is the SA, the user agent's suffix and the twin's stats key
(`aether-test/svc-1@aether-test/sa-new-<epoch>`).

Knobs: `SOAK_NEWSA=0` (opt out), `SOAK_NEWSA_OFFSETS`, `SOAK_NEWSA_SECONDS`,
`SOAK_NEWSA_RPS`, `SOAK_NEWSA_TARGETS` (`name=url …`), `SOAK_NEWSA_UPSTREAMS`,
`SOAK_NEWSA_IMAGE`. `churn.sh --new-sa-once` runs one step now and exits (step 0d).

**Gate.** Both must hold, at T0+8h (and gates 3, 4 and 5 below):

```promql
# 1. no twin ever waited out its initial fetch (the #1008 signature)
sum(increase(envoy_cluster_init_fetch_timeout_total{aether_cluster=~".*@.*"}[8h])) == 0
# ... AND, because increase() reads 0 for a series BORN at 1 -- which is exactly how
#     the rev242 series appeared (first sample 1, at 09:54:44Z) -- the raw form must
#     be empty too:
max_over_time(envoy_cluster_init_fetch_timeout_total{aether_cluster=~".*@.*"}[8h]) > 0
```

```logsql
# 2. zero 503/NC for the new identities (VictoriaLogs, not Loki)
log_name:aether_access_logs AND reporter:source AND user_agent:~"aether-soak-newsa" AND response_flags:NC
  | stats by (node_name, authority) count()
```

**The hard case is an agent restart immediately before the step (#1049).** On rev248,
the first pods of a ServiceAccount started right after an agent roll waited
6.9-7.4 s for their SVID. A twin published before its source's certificate warms on
SDS for all of that time, so the step's first requests 503 `NC` at the 2 s
`on_demand` timeout (428 k6 failures fleet-wide). To test it, roll the agent
DaemonSet and start `churn.sh --new-sa-once` as soon as the roll finishes. The step
must still read `non2xx=0`. On the step's node the agent logs `east-west QUIC fan-out
… awaiting_client_cert=1`, then `awaiting_client_cert=0` when the SVID lands. Only
after that does the pod's first svc-1 request fetch its twin.
Neither the step's node nor any other may log `not found during on-demand discovery`
for a `quic:` name.

plus the driver's own reading: `grep 'newsa/' /tmp/soak-churn.log` shows two `ROLLED`
lines with `non2xx=0,connerr=0` on both destinations.

**Gate 3 (#1020): twin count = observed pairs.** Every `quic:` twin on a node must be
a (source ServiceAccount, destination) pair that sent traffic, and
`envoy_cluster_manager_active_clusters` must be flat across the run except for +1 per
new pair (each new-SA step adds one on its node when it first dials svc-1, and removes
it when the step's pod is deleted). A step that tracks pod churn rather than new callers
is the pre-#1020 SA × destination fan-out.

```promql
# twins per node == pairs with traffic (k6 loaders + prober + the new-SA steps)
count by (node) (envoy_cluster_upstream_rq_total{aether_cluster=~".+@.+"})
count by (node) (increase(envoy_cluster_upstream_rq_total{aether_cluster=~".+@.+"}[8h]) > 0)
# flat, but for the new-SA steps' +1/-1 and the drift of the rolls. The edge
# proxy exports the same name (job="aether-edge-proxy"); keep it out.
envoy_cluster_manager_active_clusters{job!="aether-edge-proxy"}
# no on-demand fetch timed out; no pair refused by the agent. Both MUST return no
# series. Neither exists on a clean run, so the first event creates the series at 1,
# and increase() reads that as 0 (the counter rule under "Grading").
max_over_time(envoy_cluster_manager_odcds_init_fetch_timeout_total[8h]) > 0
max_over_time(aether_agent_quic_twin_refused_total[8h]) > 0
```

rev242's 118 twins (59 local SAs × 2 destinations) with 2 carrying traffic is the red
reading for the first line. The agent's `east-west QUIC fan-out quic_clusters=N
observed_pairs=P local_identities=I awaiting_client_cert=W` log line gives the same
numbers per node, and must read **`N == P`** and **`P ≪ I × S`**: `P` is the pairs
with traffic, not every local ServiceAccount times every destination (`S` = the
eligible services in the node's dependency set; the line stopped carrying a service
count when #979 dropped the allow-list). On the two-destination proving runs `P` was
single digits per node; with every destination on QUIC it is roughly one per
(caller SA, destination it calls) on the node.

**Red reading for `P ≪ I × S` (#1033).** rev245 (1.0.2-6ad804b, the first deploy of
#1032, 2026-09-28 03:00Z) logged `observed_pairs == local_identities × 2` on all five
nodes — `quic_clusters=24 observed_pairs=24 local_identities=12`, 18/18/9, 28/28/14,
20/20/10, 20/20/10 — because the fresh agent admitted every twin the proxy still held
from the up-front fan-out. On a node carried over from that state, `P` falls to the
traffic pairs one `--east-west-quic-pair-fetch-window` (1 h) after the new agent
starts, with one `pruned persisted east-west QUIC pairs … count=N` line per node
(`with no on-demand fetch since agent start` before #1073, `with no evidence of use`
since); grade gate 3 after that line, not before. On a node with no such legacy state
the line must not appear at all: see gate 5.

```bash
# per agent pod, the latest fan-out line (N == P and P << I*S) and the prune line
for p in $(kubectl -n aether-system get pods -l app.kubernetes.io/name=aether-agent -o name); do
  echo "$p"
  kubectl -n aether-system logs "$p" -c agent | grep 'east-west QUIC fan-out' | tail -1
  kubectl -n aether-system logs "$p" -c agent | grep 'pruned persisted east-west QUIC pairs'
done
```

**Gate 4 (#1036): no stranded twin after a roll.** A svc roll, or a new-SA step's
pod deletion, can take a source ServiceAccount's last pod off a node. The pair's
twin then leaves the snapshot. Envoy keeps its ODCDS subscription for that name for
the life of the process and never re-requests it. So if the agent *forgot* the pair,
the next pod of that ServiceAccount on the node would 503 `NC` at the 2 s
`on_demand` timeout on every request to the QUIC destination, until the proxy
restarts. Since #1036 the agent keeps the pair dormant and republishes the twin the
moment a pod of that ServiceAccount is back. The 18 svc rolls, and the churn of the
two new-SA steps, exercise this every run. After each svc roll there must be **no
`503/NC` on a QUIC destination** from the rolled workload's pods:

```logsql
# the stranded-twin signature: 503/NC after the 2 s on_demand timeout, on a QUIC
# destination (svc-1 on the proving run). upstream_cluster is "-" on an NC line,
# so scope by authority. MUST be empty over T0..T0+8h.
log_name:aether_access_logs AND reporter:source AND authority:~"svc-1" AND response_flags:NC AND duration_ms:>=1900
  | stats by (node_name, pod_namespace, pod_name) count()
```

Also read the agent's side of each roll: a `pruned east-west QUIC pairs … (source_left_node,
dormant)` line when the last pod leaves, and a `republished dormant east-west QUIC pairs`
line when the new pod lands on the same node. The pair only goes dormant if its old
pod was the ServiceAccount's last on the node. With a surge rollout the new pod often
lands first, and then neither line appears, which is fine.

```bash
for p in $(kubectl -n aether-system get pods -l app.kubernetes.io/name=aether-agent -o name); do
  echo "$p"
  kubectl -n aether-system logs "$p" -c agent | grep -E 'dormant east-west QUIC pairs|, dormant\)'
done
```

A stranded-twin hit reads as a burst of `NC` rows for one `pod_name` that does not
stop until that node's proxy is rolled. Each row has `duration_ms` ≈ 2000, and there
is no `observed east-west QUIC pair` or refusal line on the agent for that twin. The
live red reading is `//agent/test/mtlspool`
`TestOnDemandQUICDormantTwinRepublishedWhenSourceReturns/forget_control`: `status=503
… in 2.000099268s`, with no CDS request reaching the control plane. Troubleshoot it
with the runbook, "Stranded twin: 503 NC at 2 s for a source that came back (#1036)".

**Gate 5 (#1073): the fetch-window prune never removes a twin in use.** The first
proxy roll (T0+60) moves every twin onto the wildcard: the new generation receives it
with no fetch and no subscription. The agent rolls that follow (T0+72, and the TRIPLE
at T0+300) re-state those twins on a fresh stream as **held**. Before #1073 the agent
counted only fetches as use, so exactly one `--east-west-quic-pair-fetch-window` (1 h)
after each agent roll every agent pruned the twins it was serving. That produced
about 200 × `503 NC` per burst fleet-wide, and on one node an Envoy SIGBUS (#1074).
That is the red reading: #979 proving soak, 23:37Z and 03:25Z. Since #1073 a held
twin confirms its pair, so for each agent generation, both of these must hold:

- **0 prune lines for pairs in use.** A #1073 build carrying no pre-#1073 state
  logs no `pruned persisted east-west QUIC pairs` line at all. On a run whose store
  predates #1073, a line is allowed only as the one-time drain of pairs the proxy
  neither holds nor fetches. It must name no pair whose twin carried traffic in the
  preceding hour, and no `observed east-west QUIC pair (ODCDS)` re-admission of a
  pruned pair may follow it. A re-admission right after the prune line is the #1073
  signature.
- **0 `503 NC` outside first use.** An `NC` on a QUIC destination is allowed only
  as a pair's first request: an `observed east-west QUIC pair (ODCDS); building its twin`
  line for that twin on that node, within the same second. The windows to read are
  one hour after each agent roll (T0+132 and T0+360, ±2 min). Those minutes must
  read zero.

```bash
# per agent pod: the fresh-stream re-statement (held_served = twins confirmed by
# being held), any confirmation line, and any prune line
for p in $(kubectl -n aether-system get pods -l app.kubernetes.io/name=aether-agent -o name); do
  echo "$p"
  kubectl -n aether-system logs "$p" -c agent | grep -E 'fresh xDS stream re-stated QUIC twins|confirmed east-west QUIC pairs the proxy holds|pruned persisted east-west QUIC pairs|observed east-west QUIC pair \(ODCDS\)'
done
```

```logsql
# 503 NC on the QUIC destinations, in the hour-after-agent-roll windows
log_name:aether_access_logs AND reporter:source AND response_flags:NC
  | stats by (node_name, authority, _time:1m) count()
```

**The gate is not vacuous only if** the agent's fresh-stream line after each roll reads
`held_only=H held_served=S` with `S > 0` on the nodes whose twins carry load. In other
words, the proxy really did hold twins without subscribing to them. If `S` is 0
everywhere, the run never built the #1073 precondition (no proxy roll before the agent
roll), and the gate proves nothing: say so in the grade.

**Negative control — the gate can fail.** rev242 (pre-#1012) *is* the red reading:
`envoy_cluster_init_fetch_timeout_total{aether_cluster="aether-test/svc-{1,2}@aether-test/default"}`
= 1 on all five nodes (and `…@aether-test/anyport-probe` = 1 on w02, the day's other
new identity), and the access logs held **1,060 × 503/NC** from `aether-test/default`
(svc-1 521, svc-2 539), every node, 09:54:32–43Z. Both halves moved; the step exists so
that a run whose T0 comes after every workload already exists can still move them.

**Neither zero is evidence on its own** (see "Zero-reading gates" below). Before
grading, confirm each step actually built its twins and sent traffic:

```promql
# one series per (QUIC destination x step), on the step's node, non-zero
envoy_cluster_upstream_rq_total{aether_cluster=~".*@aether-test/sa-new-.*"}
```

```logsql
log_name:aether_access_logs AND reporter:source AND user_agent:~"aether-soak-newsa"
  | stats by (user_agent, authority, response_flags) count()
```

Since #979 every destination in the node's dependency set is QUIC-eligible, and its
twins exist only for the pairs that dial it (#1020). If `svc-1` has no twin anyway —
a build before #979 without it allow-listed, or an endpoint behind the east/west
waypoint — the `@` half of the gate is vacuous: say so in the grade rather than
reading it as a pass.

**What a FAIL reads like.** The #1008 class looks like this, all on one node, in the
first ~15 s of one step:

- churn log: `FAILED newsa/sa-new-<epoch> <node> ready=…s svc-1:ok=…,non2xx=~150,connerr=0,codes=503x~150 svc-3:ok=…,non2xx=0,…`
  — only the QUIC destination, ~15 s × 10 rps, and the pod's `AETHER_NEWSA` lines
  show `non2xx` climbing in the first two 10-second windows and then flat;
- `envoy_cluster_init_fetch_timeout_total{aether_cluster="aether-test/svc-1@aether-test/sa-new-<epoch>"}`
  = 1 on that node, and the proxy log's `initial fetch timed out for
  …ClusterLoadAssignment` ~15 s after the CDS add;
- the LogsQL above returns `503/NC` rows for `authority` svc-1 on that `node_name`.

Troubleshoot it with the runbook, "QUIC twin never leaves warming / 503 NC on a new
ServiceAccount (#1008)". Other shapes are other classes and are findings in their own
right: non-2xx on the **h2** destination too (a new identity's SVID or inbound secret
not ready — not a twin), or `connerr` with `codes=err6`/`err7` (the new pod's mesh DNS
or capture, i.e. the CNI ADD path, not the proxy).

**Authorization:** rolling these shared `talos-main` workloads for soak validation is
standing-authorized, and scaling `svc-5` down and back up for 90s is the same class of
action on the same harness namespace.

## Stopping the churn driver

**Stop it with `kill -9 <pid>`.** Both of the obvious alternatives are cases
where the safety action causes the harm, which is exactly why they are written
down rather than merely fixed.

```bash
# Find it by ABSOLUTE path, and read the pid before you kill anything.
pgrep -af "bash .*/e2e/soak/churn.sh"
kill -9 <pid>
# The driver leaves nothing behind except an unrestored SHRINK, and that only
# if you kill it inside the 90-second shrink window (T0+450m):
kubectl -n aether-test get deployment/svc-5   # expect the pre-shrink replicas
```

**Do not `pkill -f "soak/churn.sh"`.** The pattern appears in the command line
of the shell that is *running the pkill*, so `pkill` matches and kills your own
session too. `pgrep -cf` has the same flaw in the harmless direction: it counts
one driver too many, so "two drivers are running" is usually one driver and your
own shell. Anchoring the pattern to the absolute script path (`bash
/…/e2e/soak/churn.sh`) fixes both, because the issuing command line does not
contain that.

**Do not expect `kill -TERM` to stop it.** Before #835 the driver had a single
`trap restore_shrink EXIT INT TERM`, and `restore_shrink` *returns* rather than
exiting — so a TERM that reached the in-flight `sleep` ran the handler, the
handler returned, and control fell straight through to **the next roll**. The
signal sent to stop the driver made it fire early: an unscheduled
`ROLLED aether-test/deployment/svc-1` at 23:10:32Z on 2026-09-19, with the
driver still running afterwards. In the log that reads as "the tool ignored my
signal", which sends you debugging the wrong thing — the tool did the opposite,
and the two need different debugging. The traps are now split
(`EXIT` / `INT`→130 / `TERM`→143) so TERM terminates, but `kill -9` remains the
documented way to stop it: a TERM delivered to the driver's pid alone leaves its
`sleep` child running, and bash defers the handler until that `sleep` returns —
up to twelve minutes later.

**Never edit `churn.sh` while a soak is running.** Bash reads a script
incrementally, by byte offset, so rewriting the file under a running driver can
drop it into the middle of a different statement. Patches to this script land
between runs only.

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

> **The counter rule (#1088). Grade a counter gate from RAW cumulative values at T0 and
> at T0+8h, compared per series. Never use a bare `increase()` or `rate()` over the window.**
> An error series is created by its first occurrence, so it is born at 1 (or more)
> inside the window. `increase()` needs two samples of a series, so the first
> occurrence never counts, and a series born at 1 reads **0**. On 2026-10-01 the
> liveness `timeout` series was born at 1 at 02:16:49Z. `increase(...[8h])` read **0**
> and the raw comparison read **1**, the only liveness failure of the run. The
> extrapolation also skews every other value: mesh_dns `timeout` read 6.0/8.0 under
> `increase()` against a raw 7/11. Pair every error delta with the **success** delta
> of the same tier and target as a control. A success delta that is not
> ~25/s × 28,800 s ≈ 720,000 per target means the SLI was blind for part of the
> window, and its error zeros mean nothing.

Compute the prober deltas with two instant queries of the same expression, one at T0
and one at T0+8h. Diff them per series, then compare against the last known-good run:

```promql
# run once at T0 and once at T0+8h (instant queries); diff per row
sum by (tier, target, result) (aether_probe_requests_total)
```

Or in one instant query at T0+8h. `or … * 0` gives a series that did not exist at T0
a baseline of 0 instead of dropping it:

```promql
sum by (tier, target, result) (
  aether_probe_requests_total
  - (aether_probe_requests_total offset 8h or aether_probe_requests_total * 0)
)
# the series born inside the window: every row here is an error class that was
# absent at T0, and its value is its whole in-window count
aether_probe_requests_total unless aether_probe_requests_total offset 8h
```

Both forms are only valid if the **prober pod set is the same at both ends**. A prober
pod that was replaced mid-run takes its counts with it, because its series goes stale
and drops out of the T0+8h instant sum. This query must return nothing:

```promql
(count by (node, pod) (aether_probe_requests_total)
   unless count by (node, pod) (aether_probe_requests_total offset 8h))
or (count by (node, pod) (aether_probe_requests_total offset 8h)
   unless count by (node, pod) (aether_probe_requests_total))
```

If it returns rows, grade each replaced pod from its own last raw value
(`last_over_time(aether_probe_requests_total{pod="<old>"}[8h])`) and add it to the sum.
`node` is the Kubernetes node since #1041 (before it, the prober POD name), and `pod` is
the prober pod. Group by `node, pod` instead of `tier, target, result` to place a burst.

The same rule applies to every other counter gate in this README.

Attribute every non-success burst from the prober's own `AETHER_PROBE_FAIL` lines
(#1040). There is one per failed probe, capped at 20 per `(tier, result)` per minute plus
a `suppressed` summary, and each carries the client-side `t`, `err`, `elapsed_ms`, `pod`
and `node`, plus (#1252) the request `phase` it ended in and per-phase `conn_ms`,
`dns_ms`, `connect_ms`, `tls_ms`, `write_ms`, `ttfb_ms` (`-1` = never started) and
`reused`. The field reference is in `docs/runbook.md`, "Attributing a prober failure".
Pull them from VictoriaLogs for the graded window:

```
_stream:{k8s.namespace.name="aether-test"} AND "k8s.container.name":prober AND "AETHER_PROBE_FAIL"
```

Outside a suppressed minute, the line count must equal the sum of the raw error deltas.
On 2026-10-01 it was 19 lines for 1 liveness + 7 + 11 mesh_dns `timeout`.

> **Access-log queries start at `log_name:`, never at `_stream:`.** The HTTP and L4
> access-log records (`log_name:aether_access_logs`, `log_name:aether_l4_access_logs`)
> carry an empty `_stream` and no `service.name` field. A query that starts
> `_stream:{service.name="aether-proxy"} AND log_name:…` therefore matches nothing.
> Over 2026-10-01 00:40:30Z → 08:40:30Z it returned 0 against 20,289,652 HTTP and
> 70,593 L4 records. The `_stream:{service.name="aether-proxy"} AND "k8s.container.name":proxy`
> form is correct for the supervisor and Envoy stdout, which do carry the stream.

Put each burst's `t` and `node` next to that node's proxy parent-exit and mesh-dns
handoff times. `elapsed_ms` of about 2000 means the probe used its whole budget
(`timeout`), and a few ms means a fast refusal (`connection_error`). `phase` says where
the budget went: `connect` (no accept), `first_byte` (connected and sent, no answer). Runs graded before
#1041 carry the pod name in `node`. Translate it with `kubectl get pods -o wide` while
the pod exists, and after that it cannot be placed.

- **liveness** tier (local, no DNS) — data-path SLI. Target **0.000%**.
- **mesh_dns** tier (resolves a real FQDN) — DNS + cross-node SLI. Target: `dns_error`,
  `dns_nxdomain`, `dns_timeout` **all zero**. A residual `http_error` (~0.02%) is the
  known cross-node drain path, tracked separately.

  **Report these as "no series emitted", not as "verified zero".** The prober creates a
  series only on a class's FIRST occurrence, so `sum(aether_probe_requests_total{result=~"dns_.*"})`
  legitimately returns *no data* on a clean run — which is indistinguishable from the
  metric having been renamed away. A 2026-09-22 grading pass read that empty result as
  "the dns_* classes do not exist" and proposed dropping them from the grade. They do
  exist (`resultDNS*` in `prober/internal/prober/prober.go`, returned by
  `classifyFailure`), and dropping them would have retired the signal #726 was fixed to
  restore: the comment above `classifyErr` records **three separate investigations**
  that read "dns_* is zero" as "DNS is healthy" when it only ever meant "DNS never
  failed FAST". Confirm the classes are still reachable in the source when they read
  empty; do not infer their absence from an empty query.

  **Runs graded before #1252 could not see a resolution stall at all.** Go's transport
  returns the request deadline, not a DNS error, when the deadline fires mid-lookup, so
  a stall that outlived the 2 s budget was counted `timeout`. Since #1252 the prober
  classifies the deadline by the request phase it interrupted (an `httptrace` trace per
  probe): a stall in resolution is `dns_timeout`, and every later stall is still
  `timeout`. To attribute a mesh_dns `timeout`, read the `phase` field of its
  `AETHER_PROBE_FAIL` line (`connect`, `first_byte`, …) and the per-phase `*_ms`
  fields, instead of inferring the phase from access-log source-port order as #1240
  had to.
- **#682 episodes during the no-roll window or SHRINK are the harness working, not a
  regression** — attribute via the agent log line
  `expired observed upstreams from node dependency set`.
- **The first proxy roll and the SHRINK are graded as their own episodes.** Attribute every non-success to its bracketing step from the RAW counter
  series at 30–60s resolution: `x - x offset 8h` silently drops an error series that did
  not exist at the offset, and the unseeded ones are exactly the ones that matter.
- **The fetch-window gate (#1073)** — no `pruned persisted east-west QUIC pairs` line
  removes a pair in use, and zero `503 NC` outside first use in the hour after each
  agent roll. See gate 5 under "The new-ServiceAccount step".
- **The new-ServiceAccount gate (#1014)** — `init_fetch_timeout` on `@` clusters and
  zero `503/NC` for `user_agent:aether-soak-newsa`. See "The new-ServiceAccount step".
- **The restart gate (#1242)** — 0 new container restarts in `aether-system`,
  `aether-ingress` and `aether-test` over T0 → T0+8h: the watchdog's `SUMMARY` line
  reads `verdict=PASS new_restarts=0`. See "The restart gate".
- **The UDS gates (#1108/#1109)** — 0 non-benign access-log lines on `uds-echo` /
  `uds-cr-echo` with a clean-200 control, both uds-csi roll lines `ROLLED` with
  `csinode=n/n` and `failedmount_after=0`, and `aether_agent_uds_resolve_failures_total`
  unchanged T0 → T0+8h. See "The UDS leg".
- **The L4 gates (#1023)** — `ssl_fail_verify_san` on the `tcp_` keys is zero outside
  rolls, and no pod without a raw-TCP primary port takes a TCP-floor connection. See
  "The L4 gates".
- **Benign `DC` on QUIC destinations (#1009)** — `DC` + `downstream_remote_disconnect`
  + 200 + a `bytes_sent` in the authority's clean-size SET (#1089) is the hot-restart
  FIN race, not a failure; every other `DC` is. k6 and new-SA lines must sit in a
  source-node proxy roll bracket. The prober, `uds-client` and `authz-canary` log it
  continuously and are graded as a per-client baseline (#1075). See "Benign `DC` at a
  source-proxy hot restart".
- **The QUIC per-request cost gate (#1021)**, on any run with twins carrying load —
  h3 per-request envoy CPU ≤ 1.5× h2, matched no-roll windows, against the
  same-build 1.18× baseline (rev247, 2026-09-28). See "The QUIC per-request cost gate".
- **SVID rotation** is a bar since the SPIFFE Broker API (proposal 036): with the default
  4h TTL a pod rotates every ~2h, so an 8h run sees four cycles.
  The rotation signal is the agent's counter, summed per node. A restarted agent
  starts a new series, so sum across its generations. `increase()` does this, and it
  is acceptable here because the gate asks whether the counter MOVED on every node
  (tens per node per 8 h), not for an exact count. The label is `node` (#1088); there
  is no `k8s_node_name` on any `aether_*` series, and grouping by it collapses the
  fleet into one label-less row:

  ```promql
  sum by (node) (increase(aether_agent_spire_svid_updates_total{aether_spire_identity="pod",aether_spire_update="rotated"}[8h]))
  ```

  It must return one row per node (2026-10-01: 5 rows, 44.1–48.2).

  It matched the agent's `pod SVID rotated` log lines exactly on 2026-09-19 and again
  on 2026-09-27 (240/240 and 89/89). The prober delta in each rotation minute must be
  zero. `rotated` also counts the fresh SVIDs a restarted SPIRE agent mints (a whole
  node's pods at once), so a churn step that deletes a `spire-agent` pod is NOT a
  rotation cycle — exclude that node's restart minute when counting cycles. Its
  sibling `{aether_spire_update="unchanged"}` fires once per subscribed pod at the
  daily JWT-key prepare (~16:05Z): same certificate redelivered, not a rotation.
  That holds for `aether_spire_identity="node"` (the agent's own SVID) only since
  #993: before it, `node/unchanged` could not move at all, so a zero there from an
  older build is no evidence of anything.

  **Do NOT count `changes()` on `envoy_sds_spiffe_*_version` (#992).** For the
  identities that ORIGINATE mesh connections — `aether_agent`, `prober`, `mp_dialer`,
  `default`, `authz_canary`, `uds_client` — those gauges over-read by ~50×: 258–283
  changes per node over 8 h, in bursts of ~12 at every svc roll, against 5–18 for
  every other identity (its real rotations plus one per proxy roll). The per-connection
  cert selector fetches an originator's secret over its own SotW SDS stream (#865), and
  that stream re-serves the secret on every snapshot bump. Envoy keys SDS stats by
  secret NAME, so the static ADS subscription and the selector's subscription bump the
  same `sds.<name>.version` gauge. It cannot be split without renaming the resources,
  so it is documented rather than changed; the gauges remain usable only as "this
  node's Envoy has received *a* secret push", never as a rotation count.

### The restart gate (#1242)

**Why.** talos-main has no kube-state-metrics or cAdvisor series (`kube_*` /
`container_*` do not exist in Prometheus), so a container restart, its last-terminated
reason and an OOMKill cannot be graded from metrics. Until #1242 the only record was a
one-liner in a workstation wrapper that printed the restarted containers every 10 minutes
and printed the SAME blank line when `kubectl` failed, so its log could never prove
"0 restarts". The `nightly-1005-surge` run had to be graded from VictoriaLogs and Envoy
epoch counts instead. Whether to add kube-state-metrics to the platform is a separate
GitOps decision.

**The watchdog.** `restart-watch.sh` (launched in step 3b) lists every pod in the three
namespaces (the prober DaemonSet, the k6 runners and the soak workloads all live in
`aether-test`) every 120 s for 8 h. It records a baseline at start, so restarts that
predate the run are reported but never counted. Pods are keyed by UID, and init
containers are included. Its log is `/tmp/soak-restart-watch.log`
(`SOAK_RESTART_LOG`), and every line carries a UTC timestamp:

| Line | Meaning |
|---|---|
| `BASELINE count=<n>` + one `BASELINE <ns>/<pod>/<ctr> restarts=… lastReason=…` per container | restarts that existed at start. Not a finding for this run. |
| `ok count=0 sample=<k>` | every namespace was listed and nothing has restarted since the baseline. **The only line that means "none".** |
| `RESTARTS count=<n> sample=<k>` + one `RESTART <ns>/<pod>/<ctr> restarts=<N> new=<M> lastReason=<OOMKilled\|Error\|…> exitCode=… finishedAt=… state=… seen=<live\|gone>` per container | n containers restarted since the baseline. The list is cumulative: a restarted pod that a later roll deleted stays listed as `seen=gone`. |
| `ERROR kubectl failed: ns=<ns> sample=<k> …` | that namespace could not be listed or parsed. The sample proves nothing for it, and no `ok` line is written. |
| `SUMMARY restart-watch verdict=<PASS\|FAIL\|UNPROVEN> new_restarts=<n> containers=<n> samples=<n> error_samples=<n> baseline=<n> ended=<complete\|TERM\|INT> from=… to=…` + one `SUMMARY RESTART …` per container | the end-of-run grade. |

**The gate.**

```bash
grep ' SUMMARY ' /tmp/soak-restart-watch.log
# expect exactly: ... SUMMARY restart-watch verdict=PASS new_restarts=0 containers=0 ... ended=complete
grep -E ' (RESTART|ERROR) ' /tmp/soak-restart-watch.log   # expect nothing
```

- `verdict=PASS`: no new restart, no failed sample, and the run reached its end.
- `verdict=FAIL`: the `SUMMARY RESTART` lines name each container, its last-terminated
  reason and exit code (`OOMKilled`/137 is the memory limit), and when it finished.
  Place `finishedAt` against the churn log's roll brackets.
- `verdict=UNPROVEN`: no restart was seen, but the gate is not proven. Either some
  samples failed (`error_samples>0`; their `ERROR` lines say why), or the watchdog was
  stopped early (`ended=TERM|INT`), or it never got a baseline
  (`reason=no-baseline`, exit 2). restartCount is cumulative for the life of a pod, so
  the only restart a gap can hide is one in a pod that was also deleted inside it.
  Cross-check that window in VictoriaLogs (one `starting aether agent` line per agent
  pod; `k8s.container.restart_count` on its records) before grading the gate.

The watchdog is a workstation script. It cannot see a container that restarts AND whose
pod is deleted within one 120 s interval. Lower `--interval` if a run needs that, and
keep it well under the 12-minute roll spacing. `k6-soak-loader` restarts once after k6
exits at ~T0+8h26m by design, which is why the watchdog stops at T0+8h.

### Zero-reading gates: seeded or vacuous?

Several gates pass on a zero, and a zero from a series that was never created reads
exactly like a zero from a healthy system (aether#853, gotcha 10). Before grading on
one, know which kind it is and what proves it can move:

| gate | series on a clean run | what proves it can move |
|---|---|---|
| prober `dns_*` classes | **no series** (created on first occurrence) | source: `classifyErr` in `prober/internal/prober/prober.go` |
| anyport hits on DECLARED ports in the L4 access log (#1095; see "The Phase 4 evidence clock") | no rows | the `anyport-probe.sh` connections (undeclared port 19999) in the same query's pre-T0 window, and the neighbouring `cap_tcp_*` chains climbing. The `cap_tcp_anyport_*` counter is NOT the proof: its series vanishes at the first proxy roll |
| `aether_cni_operations_total{aether_cni_operation="capture_divert",aether_cni_result="error"}` | **no series** | `…{aether_cni_operation="capture_divert",aether_cni_result="success"}` must exist (the report reaches the agent; exported by the agent since #1166, there is no `add` series any more). The labels are `aether_cni_operation`/`aether_cni_result` (#1088); `operation`/`result` match nothing |
| `envoy_cluster_init_fetch_timeout_total{aether_cluster=~".*@.*"}` (#1014) | **no series**; a failure is BORN at 1, so `increase()` alone reads 0 — use `max_over_time` too | rev242's red reading (above), and each step's own `@…/sa-new-*` twin series existing with traffic |
| `503/NC` for `user_agent:aether-soak-newsa` | no rows | the same query without `response_flags:NC` returns the step's requests |
| non-benign lines on `uds-echo` / `uds-cr-echo` (#1108; see "The UDS leg") | no rows | the clean-200 control query: k6 (`default`) and `uds-client` rows on both authorities |
| `aether_agent_uds_resolve_failures_total` (#1109) | **seeded**: one row per node × reason, at 0 | the T0 query returning every row; an empty result is a broken selector, not a zero |
| **L4 (a)** `envoy_cluster_ssl_fail_verify_san_total{aether_cluster=~"tcp_.*"}` outside roll brackets (#1023; see "The L4 gates") | **no series** (born at 1 on the first rejection — read `max_over_time`, not only `increase`) | rev242: **23** ticks over its soak, and rev243: 1 in 1h47m — both read under the pre-#1023 keys `aether-test/(tcp-echo\|mixed-svc)`, since a pre-#1023 proxy exports no `tcp_` key at all; on a #1023 build, `envoy_cluster_upstream_cx_total{aether_cluster=~"tcp_.*"}` must EXIST and climb with the mp-dialer legs (the keys are exported and the selector is spelled right) |
| **L4 (b)** stray TCP-floor landings `envoy_tcp_in_tcp_<pod>_downstream_cx_total` on pods that serve no raw-TCP primary port (#1007/#1022/#1023; see "The L4 gates") | **no series** once every proxy runs the #1022 thread-self patch | rev243 is the negative control: **6** stray landings in its 1h47m generation, all on `prober` pods (w05 2, w03 3, w04 1); rev242 non-zero on svc-1..5, prober, k6-soak-loader and udp-dialer. On any build, `tcp-echo`'s own `in_tcp_*` and the `*_9000` per-port chains climbing proves the chain family is exported |

### The UDS leg: k6 on the CSI carrier (#1108) and the uds-csi roll (#1109)

**Why.** Since proposal 039 (chart 2.0.0) a UDS-served pod's socket lives on a per-pod
tmpfs that the privileged `aether-uds-csi` DaemonSet mounts. The node proxy reaches it
through a read-only view of `/run/aether/uds`. Before #1108/#1109 the soak only graded
that path from access logs, via `uds-client`'s 1 rps curl loop. No client-side SLI
covered it, and nothing ever rolled the plugin. Two gaps follow from that. A delivery
failure during a roll would only show as a non-200 line that nobody was looking for. And
the three things a plugin roll can break were never exercised under load:

- mounts persisting across the roll;
- `NodeUnpublishVolume` retried for a pod that terminates while its node's plugin is down;
- a new pod mounting right after.

**The load (#1108).** `k6-mesh-soak.js` sends `UDS_SHARE` = 5% of its iterations to
`uds-echo` (annotation delivery) and `uds-cr-echo` (`EndpointPolicy` delivery), split
evenly. The total arrival rate is unchanged, so the TCP targets each drop from 16.7% to
15.8%. At 60 iters/s × 5 runners that is ~7.5 rps per UDS authority, about 216,000
requests each over 8 h. The workloads are `charts/udsecho`, already installed in
`aether-test` (0e above). The k6 runner's `config.aether.io/upstreams` lists both.

**The roll (#1109).** `churn.sh` runs a UDS-CSI step at T0+162 and T0+270. Each step
does the following:

1. Start a `kubectl get pods -w` on the plugin pods of a UDS pod's node, then
   `rollout restart ds/aether-uds-csi`.
2. As soon as the watch shows that node's plugin **down** — the old plugin pod
   terminating or gone and no Ready replacement yet — delete the UDS pod. Step 1 picks
   a `uds-echo` pod, step 2 a `uds-cr-echo` pod. The wait is bounded by
   `SOAK_UDSCSI_WINDOW_TIMEOUT` (300 s); the roll reaches nodes one at a time.
3. Wait for the DaemonSet rollout to finish, the deleted pod to finish Terminating, and
   its replacement to become Ready.
4. Log one line:

```
ROLLED aether-system/daemonset/aether-uds-csi victim=aether-test/uds-echo-…@<node> window=down
  rollout=<s>s terminated=<s>s replacement=uds-echo-… ready=<s>s csinode=5/5 failedmount_during=1 failedmount_after=0
```

The line's fields:

- `window=down`: the pod was deleted while its node's plugin was down, so the kubelet's
  unpublish had to wait for the new plugin. A read of the node's plugin pods taken
  *after* the delete was accepted still showed it down, so this is proven, not
  inferred.
- `window=late`: the plugin was seen down, but it was Ready again by the time the
  delete was accepted. **Treat as "leg not exercised".**
- `window=missed`: the plugin was never seen down within the timeout, or its
  replacement was already Ready when first seen. **Treat as "leg not exercised".**

  `late` and `missed` also log a `UDSCSI window=<w> on <node>: <why> -- the mid-roll
  NodeUnpublishVolume leg was NOT exercised` line, followed by `plugin-watch` lines with
  what the watch saw. The step still counts as one roll and its other gates still
  apply. It is never retried: a retry would roll the DaemonSet again and change the
  roll count. Before #1243 the step polled only the OLD plugin pod once a second and
  called it `missed` as soon as that pod was gone, although the node's plugin stays
  down until the replacement is Ready (`maxSurge: 0`). That logged `window=missed` on
  2026-10-05 (step 1 of `nightly-1005-surge`).
- `failedmount_during`: FailedMount events in `aether-test` between the restart and
  the end of the step. Non-zero is expected. The replacement is usually scheduled while
  its node's plugin is still unregistered, and the kubelet retries. On kind every step
  logged `driver name csi.aether.io not found in the list of registered CSI drivers`
  once, then mounted.
- `failedmount_after`: FailedMount events stamped in the 30 s after the step finished.
  This is a gate and must be 0.
- `csinode=k/n`: nodes running a plugin pod whose CSINode lists `csi.aether.io`. This is
  a gate and must be `n/n`.

The step logs `FAILED … bad=<rollout|terminating|replacement|csinode|failedmount>`
instead of `ROLLED` when any of those breaks. Like `FAILED newsa/…`, that is a gate
finding and the run goes on. Only a refused restart, or no Running UDS pod to delete,
aborts. Opt out with `SOAK_UDSCSI=0`. `SOAK_UDSCSI_OFFSETS` and `SOAK_UDSCSI_VICTIMS`
retarget it. Run one step by hand with `--uds-csi-once` (0e).

**The gates.**

1. **0 non-benign failures on the UDS authorities, with a success control.** Every
   query starts at `log_name:` (#1098), and the benign-`DC` sizes are re-derived from
   this run's own clean lines (#1089). The authority is matched by pattern, so the short
   name and the FQDN spelling both count.

   ```logsql
   # control: clean 200s per UDS authority and calling client. Expect k6
   # (client_sa default) ~216,000 per authority over T0..T0+8h and uds-client
   # ~28,800. A UDS authority missing here means the leg was idle and the
   # zero below means nothing.
   log_name:aether_access_logs AND reporter:source AND authority:~"^uds-(cr-)?echo"
     AND response_code:="200" AND response_flags:="-"
     | extract_regexp "/sa/(?P<client_sa>[^/]+)$" from source_spiffe_id
     | stats by (authority, client_sa) count() clean, uniq_values(bytes_sent) clean_sizes

   # THE GATE: every other line on the UDS authorities. Each row must be benign DC by
   # the four-condition rule (DC alone, downstream_remote_disconnect, 200, bytes_sent
   # in that authority's clean_sizes above), with k6 rows inside a source-node proxy
   # roll bracket and uds-client rows in its per-client baseline. Any other row is a
   # FAIL, and the uds-csi roll brackets in /tmp/soak-churn.log are the first place
   # to look for it.
   log_name:aether_access_logs AND reporter:source AND authority:~"^uds-(cr-)?echo"
     AND NOT (response_code:="200" AND response_flags:="-")
     | extract_regexp "/sa/(?P<client_sa>[^/]+)$" from source_spiffe_id
     | stats by (authority, client_sa, node_name, response_code, response_flags, response_code_details, bytes_sent) count()
   ```

   Cross-check against each runner's k6 summary. The `--- by target` table must show no
   `uds-echo` / `uds-cr-echo` row. A row there is a client-side failure, and it must
   match a gate row above. Benign `DC` is a k6 success, so it never appears in k6.

2. **The roll lines.**

   ```bash
   grep aether-uds-csi /tmp/soak-churn.log
   # expect: 2 ROLLED (no FAILED), each window=down, csinode=n/n where n is the
   # number of nodes running the plugin, and failedmount_after=0
   grep -E 'UDSCSI window=(late|missed)' /tmp/soak-churn.log
   # expect nothing. A line here means that step's mid-roll unpublish leg was
   # NOT exercised: report the UDS roll gate as "1 of 2 legs exercised", not PASS.
   ```

   That `failedmount_after=0` is the record that no FailedMount outlived a roll. The
   API server keeps Events for about 1 h, so a `kubectl get events` at T0+8h cannot
   see a roll at T0+162. For the same reason, read CSINode right after each roll from
   the line's `csinode=` field. To re-check by hand within the hour:

   ```bash
   kubectl -n aether-test get events --field-selector reason=FailedMount \
     -o custom-columns=LAST:.lastTimestamp,POD:.involvedObject.name,MSG:.message
   kubectl get csinode -o custom-columns=NODE:.metadata.name,DRIVERS:.spec.drivers[*].name
   ```

3. **`aether_agent_uds_resolve_failures_total` unchanged.** This uses the counter
   rule: raw values at T0 and at T0+8h, compared per row.

   ```promql
   # instant query at T0 and again at T0+8h; every (node, reason) row must be equal
   sum by (node, reason) (aether_agent_uds_resolve_failures_total)
   # the two agent rolls (T0+72, the TRIPLE) restart the counter, so a generation
   # that climbed and was rolled away is invisible to the T0+8h value. This catches
   # it: every row must equal its T0 value.
   max by (node, reason) (max_over_time(aether_agent_uds_resolve_failures_total[8h]))
   ```

   The counter is seeded at 0 for every reason, so the T0 query must return one row per
   node × reason (5 × 8 on talos-main). An empty result means the selector or the
   export is broken, not that the counter is zero. Any increase means a UDS pod fell
   back to TCP, and the agent logs one ERROR line per pod per reason naming it.

### The L4 gates (#1023)

Two zero-reading gates on the L4 data path. Both are **seeded as vacuous** in the table
above until a #1023 build has been seen red, and on every run they need the existence
proof in its last column before a zero means anything.

**(a) No client-side SAN rejection on an L4 cluster outside a roll.** Since #1023 each
L4 cluster has its own `aether_cluster` key (`tcp_<ns>/<svc>` for the floor,
`tcp_<ns>/<svc>_<port>` per port; `docs/runbook.md`, "L4 stat keys and the L4 access
log"), so this counts L4 clusters only. Before it, `aether-test/mixed-svc` also carried
the HTTP cluster's rejections. `udp_` keys carry no TLS and are left out. There is no
`tls_` key: TLSRoute chains count under their backends' per-port
`tcp_<ns>/<svc>_<port>` keys, never the floor's (#1044).

```promql
# Per minute. A non-zero minute outside a proxy/agent roll bracket is a FAIL.
sum by (node, aether_cluster) (increase(envoy_cluster_ssl_fail_verify_san_total{aether_cluster=~"tcp_.*"}[1m]))
# The born-at-1 half: a series whose first sample is already 1 reads 0 above.
max by (node, aether_cluster) (max_over_time(envoy_cluster_ssl_fail_verify_san_total{aether_cluster=~"tcp_.*"}[8h]))
# Existence proof: the keys are exported and the selector matches them.
sum by (aether_cluster) (increase(envoy_cluster_upstream_cx_total{aether_cluster=~"tcp_.*"}[8h])) > 0
```

**(b) No TCP-floor connection on a pod that serves no raw-TCP primary port.** The
inbound DEFAULT chain (`in_tcp_<pod>`) is where a misdirected L4 connection lands
(#1007). Only `tcp-echo` is TCP-primary in this harness, and per-port chains
(`in_tcp_<pod>_<port>`) are legitimate:

```promql
# One series per landing pod over the window; MUST return no series (or 0).
max by (node, pod) (max_over_time((label_replace(
  {__name__=~"envoy_tcp_in_tcp_.+_downstream_cx_total",
   __name__!~"envoy_tcp_in_tcp_(tcp_echo_.+|.+_[0-9]+)_downstream_cx_total"},
  "pod", "$1", "__name__", "envoy_tcp_in_tcp_(.+)_downstream_cx_total"))[8h:1m]))
```

A landing is not a roll artifact, so (b) is graded over the whole window, rolls
included. Attribute any hit of either gate from the L4 access log, which puts the
source pod, the dialled VIP:port, the chosen L4 cluster (by its `tcp_` stat key), the intended endpoint and
the rejection on one line:

```
log_name:aether_l4_access_logs AND response_flags:!"-"
```

Control-test its zero by dropping `response_flags:!"-"`: the clean connections must
come back (2026-10-01: 0 flagged, 70,593 clean).

`docs/runbook.md`, "Attributing an `ssl_fail_verify_san` event … L4 hops", has the
verdict table.

### The Phase 4 evidence clock (proposal 037)

Phase 4 removes the portless TCP floor chain (`cap_tcp_anyport_<svc>`) once no client
needs it across a full release. **The gate is the L4 access log, not the counter
(#1095).** Every capture TCP connection writes one `aether_l4_access_logs` record that
names the chain that took it (`filter_chain_name`) and the VIP:port the client dialled
(`downstream_local_address`). The gate counts anyport-chain connections to a
**declared** port:

```logsql
# THE GATE. MUST return no rows over T0..T0+8h.
# Declared ports are the ones that have their own per-port chain: for tcp-echo, the
# primary port from the mesh Service's `aether.io/port` annotation (9000) plus the
# TCP mesh port 18082. Re-derive them for any other TCP-primary service with
#   kubectl -n aether-test get svc <svc> -o jsonpath='{.metadata.annotations.aether\.io/port}'
# and the per-port series in the PromQL below.
log_name:aether_l4_access_logs AND filter_chain_name:~"anyport"
  | extract_regexp ":(?P<dport>[0-9]+)$" from downstream_local_address
  | filter dport:~"^(9000|18082)$"
  | stats by (_time:1m, node_name, pod_name, downstream_local_address, filter_chain_name) count() declared_port_cx
```

A row here is a connection the per-port chain should have taken and the floor took
instead. Phase 4 would have dropped it. This is what the gate found on its first run,
2026-10-01. At 05:41:55Z `mp-dialer-fdbk2` on main-worker-03 dialled
`10.96.11.228:9000` and landed on `cap_tcp_anyport_tcp:tcp-echo…`, 12 s after w03's
agent restarted in the TRIPLE. That is **#1094**, and it blocks Phase 4.

The same query without the `filter dport` stage lists every anyport connection by
port. Use it for two more readings:

- **Control (non-vacuous).** Run it over the `anyport-probe.sh` minutes before T0. The
  probe's connections must come back on the undeclared port 19999. On 2026-10-01 that
  was 20 connections on main-worker-05 at 00:33:42–00:34:01Z. Without that row, the
  log is not reaching VictoriaLogs and an empty gate is vacuous.
- **Other undeclared-port rows in the window** from any pod other than `anyport-probe`.
  Such a row is a client that still uses the portless spelling. That is a Phase 4
  finding in its own right, so report it as well.

```logsql
log_name:aether_l4_access_logs AND filter_chain_name:~"anyport"
  | extract_regexp ":(?P<dport>[0-9]+)$" from downstream_local_address
  | stats by (filter_chain_name, dport, node_name, pod_name) count() cx, min(_time) first, max(_time) last
```

The neighbouring chains must carry traffic for the whole run. Without them, no client
dialled any spelling, and an empty gate says nothing. Read them RAW at T0 and at T0+8h
(the counter rule under "Grading"):

```promql
# Every capture TCP chain at once, by pattern. Match these by PATTERN rather than
# by an assembled name: the stat prefix carries the Envoy CLUSTER name, so
# tcp-echo's chains read `cap_tcp_TCP_tcp_echo_…` with a doubled `tcp_` that a
# name derived from the service FQDN quietly drops -- and the resulting "no data"
# is indistinguishable from a legitimate zero.
sum by (__name__) ({__name__=~"envoy_tcp_cap_tcp_.*_downstream_cx_total"})
```

Expect these three, all non-zero and still climbing at the end of the run (2026-10-01:
7,009 / 7,009 / 7,011 at T0+8h):

| series | what it proves |
|---|---|
| `…cap_tcp_tcp_mixed_svc_…_9000_…` | the 037 per-port path: a raw-TCP port on an **HTTP-primary** service |
| `…cap_tcp_tcp_tcp_echo_…_18082_…` | the well-known TCP mesh port |
| `…cap_tcp_tcp_tcp_echo_…_9000_…` | the primary-port spelling |

Before this leg existed the talos fleet had *no* raw-TCP client at all. Over seven days
the only `envoy_tcp_cap_tcp_*` series that had ever existed came from a manual e2e
run, and `tcp-echo`'s own floor chain had never carried one connection, although it
predates 037. A zero in that state says nothing about whether any client still uses
the portless spelling, and that is the only question Phase 4 asks. So the reading is a
conjunction: the neighbouring chains carried traffic for the whole run **and** the
gate query is empty. Neither half alone is evidence.

**The `cap_tcp_anyport_*` counter is a pre-roll sanity check only.** Read it after
`anyport-probe.sh` and before T0: it must show the probe's +N on the probe's node
(2026-10-01: 20 on main-worker-05 at T0). Do not grade on it. Envoy does not carry
that stat across a hot restart, so the series **vanishes at the first proxy roll** and
never comes back unless something dials the floor again. "Flat at its post-probe
value" then reads ABSENCE for the rest of the run, which makes it vacuous after roll
1. On 2026-10-01 the w05 series' last sample was at 01:47:00Z, during the first
proxy roll. The #1094 connection then existed as a w03 series only from 05:42:00 to
05:47:00Z. At T0+8h,
`count({__name__=~"envoy_tcp_cap_tcp_anyport_.*_downstream_cx_total"})` was 0, so
the raw T0/T0+8h comparison misses the hit entirely.

Grade the dialer's own tallies separately from the prober SLI; it is a supplementary
signal for the 037 chains, never authoritative for PASS/FAIL.

```bash
for p in $(kubectl -n aether-test get pods -o name | grep mp-dialer); do
  kubectl -n aether-test logs --tail=1 "$p" | grep AETHER_METRIC
done
```

### The UDP leg (proposal 038)

Since #947 the CNI captures UDP with the same mark-and-divert as TCP, and the UDP
capture listener is a transparent socket on :18082 with one `udp_proxy` matcher arm
per dialled ClusterIP. `udp.yaml` is the only continuous UDP client on the cluster,
so its tallies are the only evidence that path survives churn. Read them the same
way as the 037 dialer's — supplementary, never authoritative:

```bash
for p in $(kubectl -n aether-test get pods -o name | grep udp-dialer); do
  kubectl -n aether-test logs --tail=1 "$p" | grep AETHER_METRIC
done
```

**A `fail` that climbs at a proxy roll is a finding since #967.** Until 2026-09-26 every
proxy hot restart cost 2–3 cycles per node (the child's forwarding registry was
namespace-blind, envoyproxy/envoy#47742); the fix (#47743, carried in the proxy build by
#970) was validated on talos-main with three measured rolls — a mixed unpatched→patched
control lost 3 per node, two patched→patched rolls lost **0**. Grade the leg at 0 per
proxy roll; the only remaining known shape is a `udp-echo` roll (the arm is rebuilt on
the next push). A climb in the no-roll window is a finding too. Two proxy-side series say
which half broke:

```promql
# datagrams the transparent listener received and replied to, per dialer pod (RAW
# counters). The pod is in the metric NAME, so select by __name__ pattern; the
# names end in `_total`, and a bare `envoy_udp_capture_udp_.*_…` is not a selector.
sum by (__name__, node) ({__name__=~"envoy_udp_capture_udp_.+_downstream_sess_rx_datagrams_total"})
sum by (__name__, node) ({__name__=~"envoy_udp_capture_udp_.+_downstream_sess_tx_datagrams_total"})
# an arm whose every backend is unroutable (#937) -- must stay flat
sum by (node) (aether_agent_l4route_udp_no_healthy_backend_total)
```

Both agent counters (`udp_no_healthy_backend_total`, `udp_unsupported_total`) are
**per process**: every agent roll resets them and the new process re-seeds them while it
regenerates each pod's listener (the 09-26 run: 110 → 164 → 108 across three agent
generations). "Must stay flat" means flat *within one agent generation*; the raw sum
across the run is not flat and that is only the reset, not a finding.

And one CNI-side counter that must stay at zero for the whole run. The labels are
`aether_cni_operation` and `aether_cni_result` (#1088). The `operation`/`result`
spelling this section used to carry matches nothing, so both the gate and its
existence proof read empty:

```promql
# MUST return no series. Use max_over_time, not increase(). Since #1166 the AGENT
# exports this counter (the plugin reports each capture divert to it), so it is
# per agent process: it resets at every agent roll, and an error is born at 1,
# which increase() reads as 0 (the counter rule under "Grading").
max_over_time(aether_cni_operations_total{aether_cni_operation="capture_divert",aether_cni_result="error"}[8h])
# the existence proof: one row per node per (operation, result) that occurred
count by (node, aether_cni_operation, aether_cni_result) (last_over_time(aether_cni_operations_total[8h]))
```

A non-zero error series is a pod that started UNCAPTURED (the table was rejected), and
the mesh silently does nothing for it. Each CNI ADD counts it, so a series that
appears during a roll is a real event, not a rate artefact.

Before reading it, prove the report path works at all:
`aether_cni_operations_total{aether_cni_operation="capture_divert",aether_cni_result="success"}`
must have at least one series (every managed-pod ADD increments it). An EMPTY result
is a broken report path, never "zero errors". Before #1166 the PLUGIN exported this
series (with `add` and `del` beside it) over its own OTLP exporter, and talos-main had
none at all until #950 because the plugin, under the host's resolver, could not resolve
the collector's Service name. Since #1166 the plugin sends the outcome to the agent
over the CNI socket (`ReportAddResult`), so the series exists wherever the agent's
own metrics do. If `success` is absent while agent metrics flow, check the agent log
and `/var/log/aether-cni/plugin.log` on a node for `failed to report the ADD outcome`.
A plugin older than the agent sends no report: during an upgrade the series appears
only once cni-install has rolled with the agent.

### The QUIC leg (proposal 038 Phase 4)

East-west QUIC is unconditional: every service in a node's dependency set is dialled
over HTTP/3 by every local ServiceAccount that calls it, so this leg grades **every** service the
k6 loaders and the prober call — no `--set` is needed and there is nothing to list.
(The per-destination allow-list was a proving gate, decision 2026-09-26. The first
proving soak ran on 2026-09-27 with `aether-test/svc-1` and `svc-2` listed and did
**not** pass: prober green, k6 1,154 errors, 1,060 of them 503 NC at first use
(#1008, since closed). #979 dropped the allow-list: merged 2026-09-29 as 33ff5e9,
chart 1.0.12.) The re-soak of the merged build:
2026-09-30, chart 1.0.13-84fb413, T0 10:57:18Z, 33 ROLLED / 0 FAILED: **FAIL on liveness (9 / 719,984)**. Every QUIC gate passed:
- gate 5 (#1073): 0 prune lines, 0 NC in the window, `held_served == held_only` on 30 fresh streams, so non-vacuous
- #1074: 0 crashes, `child_silent` 0 on 5/5 nodes, `starting workers` 6 per node
- new-SA: 2 × 1,200/1,200 on both destinations, `init_fetch_timeout` no series
- gate 3: N == P, 7–10 per node
- #1054: 0 stateless resets, 0 PEER_GOING_AWAY
- L4 (a) and (b): no series, existence proven
- h3/h2: 1.18×

The liveness miss (#1085) happened in the TRIPLE on w05. The agent restart landed inside the proxy fork, and the successor's CDS and LDS initial fetches timed out. It went live with no listeners and the parent drained, so `:18081` refused new connections for 1.6 s. The same gap also caused 44 mesh_dns timeouts and 1 UDP dialer failure. k6: 147 / 9,179,821, of which 141 were before T0 (`503 NC` first use at loader start, #1086) and 6 were `504 UT` in the TRIPLE (#1087). The prober had 61 mesh_dns timeouts, all inside proxy-roll brackets. `dns_*` emitted no series.

```bash
# prerequisite ON TALOS: the SPIRE default ClusterSPIFFEID must already issue the
# <sa>.<ns>.aether.internal + *.<sa>.<ns>.aether.internal DNS SANs (GitOps,
# spire-server.controllerManager.identities.clusterSPIFFEIDs.default.dnsNameTemplates)
# and every node's pods must have ROTATED since (aether_agent_spire_svid_updates_total
# {aether_spire_identity="pod",aether_spire_update="rotated"} moved on every node --
# NOT envoy_sds_*_version, which moves on every svc roll for originator identities,
# #992) -- otherwise, on a proxy pin without envoyproxy/envoy#47740, every HTTP/3
# handshake fails closed (#957) and the leg grades the wrong thing. Then a plain
# upgrade (QUIC is unconditional since #979; there is nothing to --set):
helm upgrade aether ... -f <saved values>
```

The agent logs `east-west QUIC fan-out quic_clusters=N observed_pairs=P
local_identities=I awaiting_client_cert=W` on every node with a caller. Since #1020
`N` is the number of (source SA, destination) pairs that have dialled, not `I × S`
(S = the eligible services in that node's dependency set; on talos-main the ceiling
without the allow-list is 8–14 SAs × ~19 services ≈ 150–270 per node). That is the
budget the run is paying for: the pairs the k6 loaders, the prober and the dialers
actually use -- with every destination now QUIC, expect it to grow from the ~10 of
the two-destination proving runs to roughly one per (caller SA, destination it
calls) per node, and to be flat outside roll brackets. Since #962 the twins have
their own stats key `<ns>/<svc>@<ns>/<sa>`, so the leg grades from Prometheus (the
admin is loopback-only on talos and `kubectl exec` is denied):

```promql
# HTTP/3 requests per (destination, caller ServiceAccount) -- must be non-zero for
# every called destination x caller SA, and must RESUME after each roll of the
# destination and of the proxy (RAW counters across a roll, never increase())
envoy_cluster_upstream_rq_total{aether_cluster=~"aether-test/.*@.*"}
# the same requests must not have fallen back to h2: the h2 series of a destination
# stays FLAT while its twins move (a moving h2 series = on_no_match), except for
# destinations reached through a GAMMA weighted split (h2 by design, #961). Since
# QUIC is unconditional (#979) the h2 kin carry no request at all, so on a clean
# run this returns NO SERIES (2026-10-01: none). A series appearing is the finding.
envoy_cluster_upstream_rq_total{aether_cluster=~"aether-test/[^@]*"}
# every twin connection is HTTP/3 (must climb with the twins, never the h1/h2 kin)
envoy_cluster_upstream_cx_http3_total{aether_cluster=~".*@.*"}
# a twin that cannot connect: 0 outside roll brackets; a step in the no-roll window
# is a finding (the #957 DNS-SAN shape, or UDP:18008 blocked between nodes).
# Both names end in `_total`; without it they match nothing. rq_5xx has no series on a
# clean run (born at 1), so read it with max_over_time, never increase() alone.
envoy_cluster_upstream_cx_connect_fail_total{aether_cluster=~".*@.*"}
max_over_time(envoy_cluster_upstream_rq_5xx_total{aether_cluster=~".*@.*"}[8h])
```

The prober and k6 SLIs grade the run exactly as before: a QUIC destination that
fails still counts against the same error budget, so the leg cannot pass on the
twins' own counters alone. What the twins' counters add is attribution — whether an
error episode on a destination was the QUIC path (its twin's `connect_fail`
/ `rq_5xx` moved) or the h2 path (they did not). Not gradeable from Prometheus: the
destination's per-pod `listener.inbound_<pod>_h3.*` counter (admin only; the kind
harness `e2e/eastwest-quic.sh` reads it).

### Benign `DC` at a source-proxy hot restart (#1009)

A QUIC run leaves a few source-reporter `DC` lines on the QUIC destinations at each
**source-node** proxy roll (74 over the rev242 run, all 200s). They are not failures.
The draining parent stamps `Connection: close` on the 200; the HTTP/1.1 client (k6)
reads the full Content-Length body and closes; the h3 upstream FIN is decoded a moment
later, so `ConnectionManagerImpl::onEvent(RemoteClose)` → `resetAllStreams` flags the
still-open stream `DC` with `response_code_details=downstream_remote_disconnect`. The
client has every byte and k6 counts the request a success. h2 has no such window
(nghttp2 hands DATA and END_STREAM over from one TCP read), so the h2 path never logs it.
No Envoy setting ends a stream on a satisfied Content-Length. Mechanism, read from the
pinned Envoy:
[#1009](https://github.com/bpalermo/aether/issues/1009#issuecomment-5869601110).

**The rule.** A `DC` line is **benign** iff all four hold:

- `response_flags` is exactly `DC` (no other flag beside it),
- `response_code_details` is `downstream_remote_disconnect`,
- `response_code` is `200`,
- `bytes_sent` is in the **set** of clean-line body sizes **for that authority**,
  derived from the same run's own 2xx non-`DC` lines (query 1 below). It is not one
  literal (#1089). A destination's clean bodies come in several sizes that differ by
  a few bytes. On 2026-10-01 svc-3 had six, and the sets were:

  | authority | clean `bytes_sent` set |
  |---|---|
  | `svc-1` | {789, 791, 866, 867, 868} |
  | `svc-2` | {790, 791} |
  | `svc-3` | {790, 791, 865, 866, 867, 868} |
  | `svc-4` | {789, 791} |
  | `echo.aether-test.aether.internal` | {789, 827} |
  | `echo.aether-test.svc.cluster.local` | {829} |
  | `mixed-svc` | {20} |
  | `uds-echo` / `uds-cr-echo` | {73} / {79} |
  | `authz-echo` | {867} |

  Re-derive the sets for every run, and never copy them from this table. A single
  literal per authority (the old wording) would have failed svc-1/2/3/4 and echo.

Anything else stays a failure: `downstream_local_disconnect(...)` (the proxy closed on
the client), a `bytes_sent` outside the set (the body was cut), a non-200, or `DC`
beside another flag. Benign lines are also **excluded from the k6 reconciliation**: k6
counted them as successes, so they have no k6 failure to match.

```logsql
# 1. the clean-size SET per authority, from this run's own 2xx non-DC lines
log_name:aether_access_logs AND reporter:source AND response_code:="200" AND response_flags:="-"
  | stats by (authority) uniq_values(bytes_sent) clean_sizes, count() clean_lines

# 2. the size condition, applied to every candidate line in one pass. It counts
#    clean and DC lines per (authority, bytes_sent). A row with dc > 0 and
#    clean = 0 is a DC size that no clean line of that authority had: NOT benign.
#    Add `_time:[<roll_start>, <roll_end>]` to read one roll bracket.
log_name:aether_access_logs AND reporter:source AND response_code:="200"
  AND (response_flags:="-" OR (response_flags:="DC" AND response_code_details:="downstream_remote_disconnect"))
  | stats by (authority, bytes_sent) count() if (response_flags:="-") clean, count() if (response_flags:="DC") dc
  | filter dc:>0

# 3. DC lines that fail the other three conditions. Every row is a failure to
#    attribute. client_sa is the calling ServiceAccount (see the per-client baseline).
log_name:aether_access_logs AND reporter:source AND response_flags:~"DC"
  AND NOT (response_flags:="DC" AND response_code:="200" AND response_code_details:="downstream_remote_disconnect")
  | extract_regexp "/sa/(?P<client_sa>[^/]+)$" from source_spiffe_id
  | stats by (authority, client_sa, node_name, response_code, response_flags, response_code_details, bytes_sent) count()

# 4. benign DC per calling client: the input to the per-client baseline below
log_name:aether_access_logs AND reporter:source AND response_code:="200" AND response_flags:="DC"
  AND response_code_details:="downstream_remote_disconnect"
  | extract_regexp "/sa/(?P<client_sa>[^/]+)$" from source_spiffe_id
  | stats by (client_sa, authority) count() dc
```

On 2026-10-01 query 2 returned 16 rows, every one with `clean` > 0, so every 200/`DC`
size was in its authority's set. Query 3 returned 7 lines, all
`response_code 0, bytes_sent 0` from the prober on the two echo authorities. Those
were the prober's own 2 s timeouts, which the prober counter already counts. Control-test
a zero from query 3 by dropping its `NOT (...)` clause: the benign lines must come back.

**Per-client baseline: the clients whose benign `DC` is continuous (#1075).** The rule
above was written for the roll-bracketed race, where `DC` lines appear only while a
source node's proxy is restarting. Three harness clients log benign `DC` **all the
time**, at a steady rate unrelated to rolls. Their lines meet all four conditions, and
the client counts them as successes. Identify the client by its source workload,
i.e. the ServiceAccount in `source_spiffe_id` (query 4), and grade them as a baseline.
They are not failures, and they are not roll evidence:

| client (`client_sa`, `user_agent`) | authorities | 2026-10-01 benign DC |
|---|---|---|
| `prober`, `Go-http-client/1.1` | `echo.aether-test.aether.internal`, `echo.aether-test.svc.cluster.local` | 20,088 + 20,821 = 40,909 over 8 h; 4,821–5,259 per full hour (01–07Z), ~2.5k/h per authority |
| `uds-client`, `curl/…` | `uds-echo`, `uds-cr-echo` | 258 + 371 |
| `authz-canary`, `curl/…` | `authz-echo` | 87 |

(`uds-client` and `authz-canary` together: 74–109 per full hour.) How to grade them:

- **Subtract, do not flag.** A grader that reads the prober's ~41k benign lines as roll
  evidence, or as failures, has misread the run. Report the per-client totals, and
  confirm that each full hour is in the same band as the rest of the run. A step in
  the band, i.e. an hour well outside the run's own range, is the finding. A steady
  count is not.
- **The roll-bracketed clients are everything else.** These are the k6 loaders
  (`client_sa` `default`, `Grafana k6/…`) and the new-SA steps (`sa-new-*`). Every one
  of their benign lines must fall inside a source-node proxy roll bracket from
  `/tmp/soak-churn.log`. On 2026-10-01 there were 360 k6 lines plus 1 new-SA line, and
  every k6 minute was one of the six proxy-roll brackets (01:40–42, 02:16–18,
  04:16–18, 04:52–54, 05:40–43, 06:16–18Z).
- The prober is deliberately left unchanged (#1075). Its SLI already counts these
  probes as successes.

Benign lines from a roll-bracketed client belong to the rolled node: their
`node_name` is the source node whose proxy was restarting. The same shape on a node
whose proxy was *not* rolling is not this mechanism, and needs its own attribution.

**The timing fields (#1009).** Since #1009 every HTTP access-log line carries two
durations, both measured from the first upstream response byte:

- `upstream_rx_ms`: `%COMMON_DURATION(US_RX_BEG:US_RX_END:ms)%`, ending when the router
  decoded the upstream end-of-stream.
- `downstream_tx_end_ms`: `%COMMON_DURATION(US_RX_BEG:DS_TX_END:ms)%`, ending when the
  downstream codec finished encoding the response.

`-` means that end point never happened; a clean line carries two numbers (often `0`,
at millisecond precision). The benign race reads **`upstream_rx_ms:"-"` and
`downstream_tx_end_ms:"-"`** alongside a full `bytes_sent`, because the FIN had not been
decoded when `resetAllStreams` destroyed and logged the stream. A `DC` line with
`upstream_rx_ms` set but `downstream_tx_end_ms` `-` is a different case: the upstream
had finished and the response stalled on its way out. Attribute it; it is not the
race. The fields are a cross-check, not a condition of the rule, because lines from
builds before #1009 do not have them.

The kind reproduction attempt is `e2e/eastwest-quic-hotrestart.sh`. It runs HTTP/1.1
keep-alive loops through supervisor hot restarts (SIGHUP) to two h3 twins and an h2
control, then grades the collector stand-in's records with this rule. On a single-node
kind cluster the race did not reproduce: 0 `DC` lines in about 100 drain-closed
connections per h3 destination. That run passes H2 vacuously and says so, and
`HR_REQUIRE_DC=1` turns a zero into a failure.

### The QUIC per-request cost gate (#1021)

**Acceptance for dropping the allow-list (#979, merged 2026-09-29 after this gate
passed at 1.18×): an HTTP/3 mesh request costs at most 1.5× the proxy CPU of an h2
mesh request, at the soak's load shape.** It stays the regression gate for every
QUIC soak. The
prober and k6 SLIs cannot see this: a QUIC run can be error-free and still cost far
more proxy CPU once every destination is on it (#1006 projected 2.3× the fleet's,
from the since-superseded cross-revision 3.3× reading).

**Baseline (grade the next QUIC soak against this): h3/h2 = 1.18× per request —
the gate is passed.** A same-build A/B on 2026-09-28 (rev247, 300 rps, matched
80-min no-roll windows, Pyroscope fleet envoy cores) measured **8.9 ms** per h2
request against **10.5 ms** per h3 request: [#1021, "Same-revision measurement,
2026-09-28"](https://github.com/bpalermo/aether/issues/1021). A QUIC run that grades above 1.5× against it is a
regression.

The earlier reading below is **superseded**: it compared two different revisions
(rev239 h2 vs rev242 QUIC), so build drift and the QUIC path were not separable.
Kept for the record only:

| | run | window | envoy cores (fleet) | mesh rps | per request |
|---|---|---|---|---|---|
| h2 reference | rev239, no QUIC | 2026-09-26 19:20:42–20:40:42Z | 2.092 (idle 0.923) | 350 | 3.3 ms = (2.092 − 0.923) / 350 |
| QUIC | rev242, svc-1/2 on QUIC (100 of 350 rps) | 2026-09-27 16:00–17:20Z | 2.872 | 350 | h3 = 3.3 + (2.872 − 2.092) / 100 = 11.1 ms → 3.3× (superseded) |

**Method (#1006, reproduce it exactly or the numbers do not compare):**

1. **Windows.** The T0-matched no-roll window, 80 min, the same churn offset in
   both runs: **T0 + 6h05m → T0 + 7h25m**. No proxy, agent or service roll may
   fall inside it (check `/tmp/soak-churn.log`). Drop the first point of each
   window; Pyroscope points are end-labelled.
2. **Load matched.** Per mesh destination, `increase(envoy_cluster_upstream_rq_total
   {aether_cluster=~"aether-test/<svc>(@.*)?"}[80m]) / 4800` must agree between the
   two runs to within ~2 % (rev239 vs rev242: echo 100.1/99.9, mixed-svc 50.8/50.8,
   svc-1..4 49.9–50.1). The QUIC share is the same query over `@` series only.
3. **CPU.** Pyroscope `process_cpu`, `service_name="aether-proxy"`, split by
   `process_executable_name`; grade on **`envoy`** only (the supervisor moved
   −0.036 cores between these runs for reasons unrelated to QUIC). Average cores
   over the window, fleet sum.
4. **Idle reference.** Envoy cores with no k6 load but the same background traffic
   (~150 rps of prober and dialers), on the same build: 0.923 on rev239.
5. **Arithmetic.** `per_h2 = (envoy_ref_loaded − envoy_ref_idle) / mesh_rps` on a
   run with no QUIC; `per_h3 = per_h2 + (envoy_quic − envoy_ref_loaded) / quic_rps`
   on a run where `quic_rps` of the same load rides twins. **Gate: `per_h3 / per_h2
   ≤ 1.5`.** An unconditional-QUIC run (no reference half) grades as
   `envoy_quic_loaded − envoy_idle` over mesh rps against the latest h2 reference.
6. **Build drift.** If the Envoy pin moved between the reference and the QUIC run,
   say so and bound it with the idle reading (rev240 vs rev239: −1.3 %); it is not
   separable at load.

Attribute a miss with the frames #1006 used (source-side
`EnvoyQuicClientConnection` read events, `UdpListenerImpl::handleReadCallback`,
`quic::QuicConnection::OnAckAlarm`, `ScopedPacketFlusher`, kernel `udp_sendmsg` /
`udp_recvmsg`) and the connection density: `sum(envoy_cluster_upstream_cx_active
{aether_cluster=~".*@.*"})` against the QUIC rps. Connections are **one per (source
node, source ServiceAccount, Envoy worker the SA's app connections landed on,
destination endpoint)**, not one per app connection (see the runbook, "HTTP/3
per-request cost"); a count that tracks k6 VUs is a regression of #1021.

Do **not** measure this against talos-main with synthetic load outside a soak: the
harness form of the same comparison is `//agent/test/mtlspool` `TestQUICRequestCPU`
(`--test_env=AETHER_QUIC_COST=1`), which prints loaded-minus-idle CPU per request
for h2 and for each inbound UDP option.

### The hot-restart wedge gates (#1050)

On rev248 one proxy roll in ~60 wedged: the successor's last line was `all
dependencies initialized. starting workers`, the draining parent's last was `closing
and draining listeners`, both main threads then sat in blocking hot-restart socket
calls on each other (parent `sendmsg` forwarding QUIC/UDP to the child, child
`recvmsg` waiting for the parent's stats) until the supervisor's liveness watchdog
killed both epochs 38 s later. Every new connection to or from that node was dead for
the whole window, and the prober took 56 errors on it. Mechanism:
[#1050](https://github.com/bpalermo/aether/issues/1050#issuecomment-5875727632). The
carried Envoy patch in the aether-proxy image (#1060) removes the deadlock, so the
chart's workaround, Envoy `--skip-hot-restart-parent-stats`
(`proxy.hotRestart.skipParentStats`), is off by default again and stays as an emergency
switch. These gates are what show the deadlock stayed removed with the child merging
the parent's stats again. Grade them **per roll**, over the proxy DaemonSet's roll
windows from `/tmp/soak-churn.log`.

All of these lines are the supervisor's and Envoy's stdout, i.e. the `proxy`
container's stream (`service.name` `aether-proxy`, `k8s.container.name` `proxy`, #1059).
The container filter keeps the access logs, which share the service name, out.

**(a) No watchdog kill.** Zero of each, per roll, fleet-wide:

```
_stream:{service.name="aether-proxy"} AND "k8s.container.name":proxy AND _msg:"liveness watchdog fired"
_stream:{service.name="aether-proxy"} AND "k8s.container.name":proxy AND _msg:"both hot-restart epochs silent"
_stream:{service.name="aether-proxy"} AND "k8s.container.name":proxy AND _msg:"drain deadline elapsed, killing envoy epoch"
```

Since #1058 a watchdog that finds both epochs of the pair silent SIGKILLs at once and
logs the second line (with `epochs=` and `silentFor=`) instead of the third. The third
line is only a finding when the epoch it kills still had a live child, i.e. when it
follows a watchdog line in the same pod. A pod that is deleted with no successor (see
the runbook, "What the proxy supervisor does on SIGTERM") can log it on its own.
Control-test the zero: `_msg:"starting workers"` over the same window must return one
line per node per roll. If it does not, the container logs did not reach VictoriaLogs
and the zero is vacuous.

**(b) No QUIC blackhole toward the rolling node.** Per roll, count the source-side
QUIC failures whose upstream is on the node being rolled:

```
log_name:aether_access_logs AND reporter:source
  AND response_flags:UC
  AND response_code_details:~"QUIC_TOO_MANY_RTOS|Network_blackhole_detected"
```

Control-test its zero with the same matcher widened to
`…|response_timeout` and without `response_flags:UC`. Over 2026-10-01 that returned
the run's 5 `504 UT` lines.

Resolve `upstream_host` to its node as in the runbook ("upstream_host → pod → node").
The baseline is the count on the rolls that did not wedge: 0 on rolls 1–3 of the
rev248 run. The wedged roll logged ~550 in two minutes. A handful of
`QUIC_PUBLIC_RESET|FROM_PEER|Received_stateless_reset` at a roll is the milder,
separate case (a parent-owned QUIC connection's packets reaching the child after the
parent exited, 2 on roll 4). Report it, but it is not this gate.

**(c) The successor keeps talking after `starting workers`.** A healthy successor logs
its next line (listener warming, xDS updates, the supervisor's `pod ready`) within a few
seconds of `all dependencies initialized. starting workers`. A child that is silent for
**10 s** after that line is the wedge signature, whether or not the watchdog later
fires. Since #1058 the supervisor measures it: a hot-restart child whose admin stops
answering for 10 s after its first observed LIVE (the step that logs `starting workers`)
is logged once per epoch and counted. The gate is **0 per roll** of each:

```
_stream:{service.name="aether-proxy"} AND "k8s.container.name":proxy AND _msg:"hot-restart child silent"
```

```promql
sum by (node) (increase(aether_supervisor_child_silent_total[30m]))
```

The label is `node` (#1088). Grouping by `k8s_node_name` returns ONE label-less row,
and then the "node with no series" check cannot be made. The counter is seeded at
zero, so it must return one row per node (2026-10-01, over `[8h]`: 5 rows, all 0). A
node with no row means its supervisor metrics never arrived, and its zero is vacuous. The log-gap reading (the gap between
`starting workers` and the next line from that pod) stays as the cross-check. It also
catches a child that goes quiet in its log while its admin still answers, which the
metric does not.

**The kind form.** `e2e/hotrestart-wedge.sh` forces the same deadlock by freezing the
child inside the parent's forwarding window. It is the red-then-green check for any
change that touches hot restart or the Envoy pin:

```bash
# red: the knob off. Each wedge costs a container restart, and kubelet's
# crash-loop backoff grows with each one (~2-4 min apiece by the third).
WEDGE_SKIP_PARENT_STATS=false e2e/hotrestart-wedge.sh up
WEDGE_FREEZE_S=6 WEDGE_RESTARTS=4 WEDGE_STOP_ON_FIRST=0 e2e/hotrestart-wedge.sh run
#   -> WEDGES=4 RESTARTS=4 FREEZE_S=6 SKIP_PARENT_STATS=no     (2026-09-28)
# green: the chart default. `up` again only upgrades the release (rolls the proxy).
EWQ_SKIP_BUILD=1 e2e/hotrestart-wedge.sh up
WEDGE_FREEZE_S=6 WEDGE_RESTARTS=6 WEDGE_STOP_ON_FIRST=0 e2e/hotrestart-wedge.sh run
#   -> WEDGES=0 RESTARTS=6 FREEZE_S=6 SKIP_PARENT_STATS=yes    (2026-09-28)
e2e/hotrestart-wedge.sh down
```

The `SKIP_PARENT_STATS=` field is read from the live Envoy's argv, not from the
setting, so a red run that says `yes` was not a red run. The red run above predates the
carried patch: against the chart's current proxy pin, `WEDGE_SKIP_PARENT_STATS=false`
(now the default) is the green arm.

### The h3 stateless-reset gate (#1054)

A source h3 connection that outlives the destination proxy's hot-restart parent dies on
a QUIC stateless reset when the parent exits (mechanism in the runbook, "Source h3
requests die on a stateless reset at a destination's roll"). The carried Envoy
patches (#1064, #1066) address it with the chart default
`proxy.hotRestart.drainStrategy: gradual`; `immediate` made it worse on talos-main. Grade **per proxy roll**, source side, toward the
rolling node (resolve `upstream_host` as in (b) above):

```
log_name:aether_access_logs AND reporter:source
  AND response_code_details:~"Received_stateless_reset"
```

must be **0**, and so must the QUIC `503 UC` lines toward the rolling node:

```
log_name:aether_access_logs AND reporter:source
  AND response_code:503 AND response_flags:UC AND upstream_cluster:~"@"
```

`upstream_cluster` carries the twin's stat key, `<ns>/<svc>@<ns>/<sa>`, and never its
`quic:` config name. Over 2026-10-01, `upstream_cluster:~"^quic:"` matched 0 of
20,289,652 records and `upstream_cluster:~"@"` matched 10,146,248 source lines.

Also report the same query with `PEER_GOING_AWAY` in place of `Received_stateless_reset`:
a change that only turns one failure into another must not read as a pass.

Do **not** gate on the destination's `quic.dispatcher.stateless_reset_packets_sent`.
The draining parent also forwards its own time-wait connection IDs to the child, which
answers them with stateless resets that no live source connection ever sees, so the
counter is non-zero on a clean roll.

The kind form is `e2e/eastwest-quic-hotrestart.sh` in `HR_MODE=sparse` on two nodes.
Busy loops never reproduce it (every connection keeps drawing responses and so, sooner
or later, a GOAWAY); the sparse leg ends its loops at the SIGHUP, sends one request at
T+2, and bursts at the parent's `shutting down due to child request`:

```bash
# red: the pre-#1054 settings (HR_QUIC_IDLE is patched past the chart's check)
EWQ_WORKER=1 HR_DRAIN_STRATEGY=gradual HR_QUIC_IDLE=30s e2e/eastwest-quic-hotrestart.sh up
EWQ_WORKER=1 HR_MODE=sparse HR_REQUIRE_RESET=1 HR_FREEZE_PARENT_S=4 HR_FREEZE_AT=14 \
  e2e/eastwest-quic-hotrestart.sh verify
#   -> STATELESS_RESET=1 over 10 restarts (2026-09-28); unforced: 0 over 10
# green: the chart defaults. `up` again only upgrades the release.
EWQ_WORKER=1 EWQ_SKIP_BUILD=1 e2e/eastwest-quic-hotrestart.sh up
EWQ_WORKER=1 HR_MODE=sparse e2e/eastwest-quic-hotrestart.sh verify
#   -> STATELESS_RESET=0 PEER_GOING_AWAY=0 CLIENT_NON200=0 over 10
EWQ_WORKER=1 HR_MODE=sparse HR_FREEZE_PARENT_S=4 HR_FREEZE_AT=14 e2e/eastwest-quic-hotrestart.sh verify
#   -> STATELESS_RESET=0 PEER_GOING_AWAY=0 HUNG=0 over 10 (4 freeze handshake timeouts each)
e2e/eastwest-quic-hotrestart.sh down
```

Kind reproduces the race weakly. Unforced, the window is the few milliseconds between
the child unpausing its inherited UDP listeners and the parent's workers stopping, and
the red arm saw no reset in 10 restarts. `HR_FREEZE_PARENT_S` holds that window open
by stopping the parent just before the child's parent-shutdown timer fires; even then
the red arm produced 1 reset in 10. Its `503 URX,UF … QUIC_NETWORK_IDLE_TIMEOUT` lines
are new handshakes that cannot complete while the parent is stopped and the child is
still paused: an artifact of the freeze, present in both arms, and not gated. The
soak gate above is the real evidence.

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
   `kubectl top pods -n o11y` alongside the prober rate. A saturated collector used to
   crash-loop agents too (#662, fixed in #668); that fix is exercised on demand by
   `e2e/pressure/`, which deliberately induces shedding and must **never** be run
   during a soak.
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

9. **A workload set can make a whole feature untestable.** Proposal 037 shipped in
   rev234 and the soak, as configured, would not have touched one line of it: every
   pre-037 service declares a single protocol, so no per-port chain is ever built,
   and the one TCP-primary service had no client. The run would have reported PASS
   without exercising the feature it was meant to validate, and — worse — would have
   produced a zero on the Phase 4 gate that looked like evidence. This is gotcha 8's
   shape one level up: there, a churn step reset the state a defect needed to age;
   here, the workload set never created that state at all. When a release adds a data
   path, check that something in `aether-test` actually walks it before starting.

10. **A classifier can be confidently wrong, and it looks exactly like a quiet
    instrument.** From #846 until #887, `classify()` tested the HTTP status only
    inside an `error_code === 0` branch — but k6 sets a *non-zero* `error_code` for
    an HTTP error response (a 504 arrives as 1504, i.e. `1000 + status`). The status
    tests were therefore unreachable for every response they were written for:
    `http_4xx` and `http_5xx` could never be incremented at all, 4xx was swallowed by
    the `1400–1499` range test and filed as `proto`, and 5xx fell off the end into
    `other`. Both the rev231 and rev234 residuals were reported as unattributable
    `other`; the rev234 33 were later reconciled 33-of-33 against the source proxies'
    access logs as plain 504/UT responses. Two lessons: a "0" in a class is only
    evidence if that class has been *shown* to be reachable (cf. aether#853), and an
    enum-like mapping onto another tool's numbering needs its constants checked
    against that tool, not against the docs' prose. The ranges here were verified by
    running k6 against fixed-status targets — see the red-state recipe in #887.

## Files

- `echo.yaml` — the mesh_dns SLI target (3 replicas, soft hostname spread). Apply before
  a run; it is the workload the mesh_dns tier actually measures.
- `k6-mesh-soak.js` — load script (constant-arrival-rate, qualified mesh names, no OTLP).
  Since #846 it also splits failures into classes (`dns`, `conn`, `tls`,
  `timeout`, `proto`, `http_4xx`, `http_5xx`, `other`) and prints them at the
  end of the run, because k6's own summary aggregates across tags and makes
  every transport failure look alike — which is why the rev226 soak's ~510
  failures could never be attributed. Collect it from the runner logs:

  Since #887 the summary also breaks every non-zero class down **by target** and
  **by reason** (the exact k6 `error_code` / HTTP status pair), and every failure
  is logged verbatim as it happens, so a residual no longer needs a re-run to
  attribute. Collect it from the runner logs:

  ```bash
  kubectl -n aether-test logs <k6-runner-pod> | grep -A30 'failure classes'
  # machine-readable, one line (now also carries byEndpoint/byReason/attributed):
  kubectl -n aether-test logs <k6-runner-pod> | grep AETHER_METRIC
  # the verbatim per-failure sample, capped at 10 per class per VU:
  kubectl -n aether-test logs <k6-runner-pod> | grep AETHER_FAIL
  ```

  Read the two reconciliation lines first, in order:

  - `classified=N of http_req_failed≈M` — **N < M means a failure mode
    `classify()` does not recognise**, and that gap is itself the finding. A
    non-zero `other` means the same thing one level down.
  - `attributed=A of classified=N` — **A < N means a code/status pair the
    script never declared**, so it is missing from the *by reason* table. The
    summary prints a `GAP:` line when this happens; the `AETHER_FAIL` sample
    lines name the pair verbatim, and the fix is to add it to `HTTP_STATUSES`
    or `transportCodes()`.

  The `AETHER_FAIL` lines carry a timestamp, which is what lines a burst of
  failures up against a specific proxy roll — the only instrument that can see
  the #823 egress blip, since the external prober rides one keep-alive
  connection and is structurally blind to it (#846).

  > **Why a breakdown and not just raw output.** k6's `handleSummary` data
  > contains no per-tag submetrics: a counter tagged with `endpoint`/`code`/
  > `status` arrives as a single number aggregated across every tag value. The
  > only way to make a tag split visible is to declare each combination as a
  > threshold submetric up front (metrics cannot be created outside the init
  > context), and that costs real CPU and RSS on a container this repo has
  > already OOM-killed mid-soak. Hence the small declared key space plus the
  > bounded verbatim sample. Verified on k6 v2.3.0; see the comment block in
  > `k6-mesh-soak.js`.
- `k6-runner.yaml` — the 5-node runner DaemonSet.
- `churn.sh` — the 33-roll churn driver (incl. the two uds-csi steps) plus the two
  new-ServiceAccount steps, the no-roll window and the demand-set shrink; takes a build
  label for the log header.
- `udscsi-window.awk` — the uds-csi step's plugin-down detector (#1243), fed the step's
  `kubectl get pods -w` lines; `churn.sh` reads it from this directory.
- `restart-watch.sh` — the restart watchdog (#1242). See "The restart gate".
- `harness_test.sh` + `testdata/restart-watch/` — dry tests for `restart-watch.sh`
  (canned `kubectl get pods -o json` through a fake kubectl, including a refused call
  and a non-List answer) and `udscsi-window.awk`. No cluster; needs bash, jq and awk.
  Run `bash e2e/soak/harness_test.sh` after editing either file.
- `newsa-client.sh` — the new-SA step's workload (busybox `sh` + `curl` in the pod,
  shipped per step as a ConfigMap); never run on the workstation.
- `sample-proxy-rss.sh` — age-matched `aether-proxy` working-set sampler for #628.
  Standalone, and queued automatically by `churn.sh` after each proxy roll.
- `multiprotocol.yaml` — the proposal-037 leg: `mixed-svc` (HTTP :8080 primary + raw
  TCP :9000 on one pod, one ServiceAccount) and the `mp-dialer` DaemonSet that drives
  the three SANCTIONED raw-TCP spellings on every node. It deliberately never dials an
  unsanctioned port; that is the shim's territory and driving it would destroy the
  Phase 4 measurement.
- `udp.yaml` — the proposal-038 leg: `udp-echo` (UDP-primary, 3 replicas spread, a
  self-parented `UDPRoute`) and the `udp-dialer` DaemonSet that dials
  `udp-echo.<ns>.aether.internal:18082` from every node with a unique token per
  datagram. Plaintext by design; delivery under churn is the question it asks.
- `anyport-probe.sh` — the one-shot negative control for that gate: dials a TCP-primary
  service at an unsanctioned port (19999), so anyport connections are *shown* to appear
  in the L4 access log before the run relies on the gate query being empty. Its
  `cap_tcp_anyport_*` counter bump is only a pre-roll check, because the series does
  not survive a proxy roll (#1095).
