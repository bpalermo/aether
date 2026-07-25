# 8-hour soak harness

Validates a deployed build against sustained mesh load plus continuous rollout churn.
Used on the `talos-main` cluster before promoting a release.

Three components run together:

| Component | What it proves |
|---|---|
| **External prober** (`//prober`, DaemonSet, already deployed) | the availability SLI — **authoritative for PASS/FAIL** |
| **k6 runners** (`k6-runner.yaml`) | mesh load by NAME (~300/s) so DNS + cross-node paths are exercised |
| **Churn driver** (`churn.sh`) | 30 rolling restarts incl. mesh-dns/agent/proxy/edge + a concurrent triple |

The prober is external **on purpose**: the mesh's own self-reported metrics are blind to
the very churn being tested. Never grade a soak on mesh self-SLI alone.

## Run

```bash
# 1. Load the k6 script as a ConfigMap (source of truth is the .js file here).
kubectl create configmap k6-soak-script -n aether-test \
  --from-file=test.js=e2e/soak/k6-mesh-soak.js \
  --dry-run=client -o yaml | kubectl apply -f -

# 2. Pre-flight: every component Ready, 0 restarts, prober SLI live at 25/s with 0 errors.
kubectl get pods -n aether-system
# prober baseline (Grafana/Prometheus):
#   sum by (tier) (rate(aether_probe_requests_total{result="success"}[3m]))   -> ~25/s
#   sum by (tier) (rate(aether_probe_requests_total{result!="success"}[5m]))  -> 0

# 3. Start load, then churn (detached — churn sleeps ~7h15m).
kubectl apply -f e2e/soak/k6-runner.yaml
bash e2e/soak/churn.sh "rev183/0.86.0" &

# 4. Teardown at T0+8h.
kubectl delete -f e2e/soak/k6-runner.yaml
grep -c ROLLED /tmp/soak-churn.log      # expect 30
```

## Grading

Compute prober deltas over the churn window and compare against the last known-good run:

```promql
sum by (tier, result) (increase(aether_probe_requests_total[8h]))
```

- **liveness** tier (local, no DNS) — data-path SLI. Target **0.000%**.
- **mesh_dns** tier (resolves a real FQDN) — DNS + cross-node SLI. Target: `dns_error`,
  `dns_nxdomain`, `dns_timeout` **all zero**. A residual `http_error` (~0.02%) is the
  known cross-node drain path, tracked separately.

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

## Files

- `k6-mesh-soak.js` — load script (constant-arrival-rate, qualified mesh names, no OTLP).
- `k6-runner.yaml` — the 5-node runner DaemonSet.
- `churn.sh` — the 30-roll churn driver; takes a build label for the log header.
