# Profiling symbols: why flame graphs go blank after a release

Continuous profiling on `talos-main` comes from the **OpenTelemetry eBPF profiler**,
which reports native stack frames by **GNU build ID** and nothing else. Pyroscope can
only turn those frames into function names if a *debuginfo* blob carrying the **same**
build ID has been uploaded to it.

Every aether release produces new binaries, therefore new build IDs, therefore a
Pyroscope that no longer knows how to symbolise them.

## The failure is silent, and that is the point

When symbols are missing, **nothing breaks and nothing alerts**:

- profiles keep arriving and keep being stored;
- CPU/memory totals stay correct;
- no error is logged anywhere;
- the flame graph simply renders hex addresses instead of function names.

You normally discover it at the worst possible moment — in the middle of an
investigation, on the one build you actually needed to read. Treat "the flame graph is
full of `0x...`" as a *symbol* problem, never as a profiler problem.

## How to tell

Ask Pyroscope what it knows:

```bash
kubectl -n o11y port-forward svc/pyroscope 4040:4040 &
profilecli debuginfo list --url http://127.0.0.1:4040
```

Each line looks like:

```
build_id=897d73e8b1684754d0cd127f021725619b6379db name=agent \
  type=TYPE_EXECUTABLE_FULL state=STATE_UPLOADED size=60.2 MiB uploaded_at=2026-09-01T23:35:56Z
```

Then get the build ID of the binary actually running, and compare:

```bash
crane export --platform linux/arm64 \
  "$(kubectl -n aether-system get ds aether-agent \
       -o jsonpath='{.spec.template.spec.containers[0].image}')" - \
  | tar -x -O agent > /tmp/agent
readelf -n /tmp/agent | grep 'Build ID'
```

If that ID is not in the list, that component's frames will not symbolise.

> **`--platform linux/arm64` is not optional.** Every node on `talos-main` is arm64, and
> the images are multi-arch. Omitting it silently gives you the amd64 binary, whose build
> ID differs completely (`897d73e8…` vs `da9ce3fa…` for the same image) — so you will
> compare the wrong ID, or worse, upload symbols that match nothing and appear to have
> worked.

## Who keeps this in sync

Nobody, by hand — this is automated **outside this repo**, in the GitOps cluster
repository:

> `bpalermo/k8s-talos-main` → `clusters/talos-main/pyroscope-debuginfo/`

An hourly CronJob in the `o11y` namespace reconciles Pyroscope's debuginfo store against
the image digests **actually running** in `aether-system` and `aether-ingress`. It
extracts the binaries with `crane`, uploads any build ID Pyroscope is missing, and
re-uploads anything that disappears from the store. Steady state is a no-op.

That directory's `README.md` is the operational reference: rollout, GC interlocks, alerts
and known risks all live there.

### Why it is not in this repo

The job needs cluster knowledge — namespaces, registry, Pyroscope's address, the node
architecture. Only one thing about it is aether knowledge: **where each binary sits inside
each image**. That table is `targets.tsv`, and it is the piece that changes when this repo
changes:

| image | binary path in image | defined by |
|---|---|---|
| `aether-proxy` | `/usr/local/bin/envoy` | `proxy/BUILD.bazel` |
| `agent`, `mesh-dns`, `registrar`, `controller`, `cni-install` | `/<name>` (Go binaries land at the image root) | `bazel/img/go_multi_arch_image.bzl` |
| `cni-install` | `/opt/cni/bin/aether-cni` | `cni/cmd/cni-install/BUILD.bazel` |

Deployment is also decisive: the `aether` chart is installed by hand, so a job living here
would only refresh symbols when a human ran `helm upgrade` — which is exactly the manual
step the automation removes.

**If you move or rename a binary inside an image, update `targets.tsv` in the GitOps
repo.** Nothing in this repo's CI will catch it; the symptom is one component's frames
going hex while every other component stays fine. The sync job alerts on this
(`DebuginfoSyncMissingSymbols`).

## Adding a new binary to the mesh

A new component only gets symbolised profiles once it is added there:

1. Add a row to `clusters/talos-main/pyroscope-debuginfo/targets.tsv`:
   `<image>` ⇥ `<artifact>` ⇥ `<tar-relative path>` ⇥ `executable-full` ⇥ `true`
   (the path has **no leading slash**, and `<artifact>` must be unique in that file).
2. Make sure the workload runs in a namespace the job discovers (`aether-system` or
   `aether-ingress`) from a **digest-pinned** `ghcr.io/bpalermo/aether/*` reference.

The next hourly tick picks it up.

## Related

- Alert rules: group `debuginfo-sync` in the cluster repo's `prometheus/values.yaml`.
- [`README.md`](./README.md) — the mesh-DNS alerting rules and how alerts are delivered
  on `talos-main` (helm values, not a `PrometheusRule` CRD).
