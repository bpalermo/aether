# Profiling symbols: why flame graphs go blank after a release

Continuous profiling on `talos-main` comes from the **OpenTelemetry eBPF profiler**,
which reports native stack frames by **GNU build ID** and nothing else. Pyroscope can
only turn those frames into function names if a *debuginfo* blob carrying the **same**
build ID has been uploaded to it.

A release that rebuilds a binary changes that binary's build ID, so Pyroscope no longer
knows how to symbolise it until a matching blob arrives.

> **Since #651/#653 the ID is a hash of the binary's own content, not of the release.**
> Every Go ELF that enters an image — the six binaries `//tools/buildid:release_build_ids`
> guards, plus the readiness probers that ride along as extra layers — gets
> `sha1(its own bytes)` written into `.note.gnu.build-id` by
> `//tools/buildid`, and the custom Envoy gets lld's
> `--build-id=sha1` over the linked output. So a binary that a release leaves
> **byte-identical keeps its build ID**, its already-uploaded symbols stay correct, and
> the sync job skips it. "Every release mints new build IDs" used to be literally true —
> it is not any more. Only genuinely rebuilt binaries need a new upload.

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
build_id=80542d6b86e62aa1fe6472680d45630c766a1280 name=envoy \
  type=TYPE_EXECUTABLE_FULL state=STATE_UPLOADED size=126.1 MiB uploaded_at=2026-09-05T19:17:23Z
```

Then get the build ID of the binary actually running, and compare:

```bash
crane export --platform linux/arm64 \
  "$(kubectl -n aether-system get ds aether-agent \
       -o jsonpath='{.spec.template.spec.containers[0].image}')" - \
  | tar -x -O agent > /tmp/agent
readelf -n /tmp/agent | grep 'Build ID'
```

The same recipe reads the proxy — note that the path handed to `tar` is **tar-relative**,
so it carries no leading slash:

```bash
crane export --platform linux/arm64 \
  "$(kubectl -n aether-system get ds aether-proxy \
       -o jsonpath='{.spec.template.spec.containers[0].image}')" - \
  | tar -x -O usr/local/bin/envoy > /tmp/envoy
readelf -n /tmp/envoy | grep -B1 'Build ID:'
```

`-B1` keeps the descriptor-size line, which is the point here:

```
  GNU                  0x00000014	NT_GNU_BUILD_ID (unique build ID bitstring)
    Build ID: 80542d6b86e62aa1fe6472680d45630c766a1280
```

`0x14` is **20 bytes** — the SHA-1 width. That width is an assertion, not
decoration: the hermetic toolchain passes its own `-Wl,--build-id=md5` (16 bytes) earlier
on the link line, and lld honours the *last* `--build-id`, so a 20-byte descriptor is the
proof that `proxy/.bazelrc`'s `--linkopt=-Wl,--build-id=sha1` won. A 16-byte one means
the flag was lost.

If the ID you read is not in `debuginfo list`, that component's frames will not
symbolise.

> **`--platform linux/arm64` is not optional.** Every node on `talos-main` is arm64, and
> the images are multi-arch. Omitting it silently gives you the amd64 binary, whose build
> ID differs completely (`897d73e8…` vs `da9ce3fa…` for the same image) — so you will
> compare the wrong ID, or worse, upload symbols that match nothing and appear to have
> worked.

### Hex frames on an ID that *is* uploaded: cache eviction, not a missed upload

Pyroscope converts an uploaded blob into its own lidia symbol format lazily, on the first
query, and caches the result. **An upload silently evicts other binaries' converted
symbols from that cache.** Nothing is logged, and `debuginfo list` still reports every
build ID as `STATE_UPLOADED`, because the *blob* was never the problem.

On 2026-09-05 an upload of five Go binaries evicted Envoy's converted symbols; the
warm-up had queried `supervisor` and `proxy-ready` only, so
`{service_name="aether-proxy", process_executable_name="envoy"}` came back as hex at a
soak's T0 and re-symbolised on its own a few minutes later.

So: **hex frames on a build ID the sync reports as "still uploaded" mean an eviction, not
a missed upload.** Query that series once, wait ~5 minutes, and re-check before touching
anything.

**After any upload, warm up every series you intend to profile** — not just the ones that
were uploaded. Run a real profile query against each and confirm named frames before
declaring the warm-up done:

| `service_name` | `process_executable_name` |
|---|---|
| `aether-agent` | — |
| `aether-mesh-dns` | — |
| `aether-controller` | — |
| `aether-registrar` | — |
| `aether-proxy` | `envoy`, `supervisor`, `proxy-ready` |

Note the last row: the proxy pod's processes all roll up under
`service_name="aether-proxy"` and are separated by **`process_executable_name`**, *not*
by `k8s_container_name` — a selector on the container name matches nothing.

## Who keeps this in sync

Nobody, by hand — this is automated **outside this repo**, in the GitOps cluster
repository:

> `bpalermo/k8s-talos-main` → `clusters/talos-main/pyroscope-debuginfo/`

An hourly CronJob in the `o11y` namespace reconciles Pyroscope's debuginfo store against
the image digests **actually running** in `aether-system` and `aether-ingress`. It
extracts the binaries with `crane`, uploads any build ID Pyroscope is missing, and
re-uploads anything that disappears from the store.

Because build IDs are content-derived, steady state is a genuine no-op and a release only
re-uploads what it actually rebuilt. A run reads:

```
plan: envoy UPLOAD (digest or type changed)
plan: agent UPLOAD (digest or type changed)
plan: proxy-ready UPLOAD (digest or type changed)
plan: cni-install SKIP (digest unchanged, build ID f075a5df… still uploaded)
plan: aether-cni SKIP (digest unchanged, build ID f4359b3d… still uploaded)
```

A `SKIP` line is the healthy outcome, not a missing upload — see the eviction section
above before treating one as a fault.

That directory's `README.md` is the operational reference: rollout, GC interlocks, alerts
and known risks all live there.

### Why it is not in this repo

The job needs cluster knowledge — namespaces, registry, Pyroscope's address, the node
architecture. Only one thing about it is aether knowledge: **where each binary sits inside
each image**. That table is `targets.tsv`, and it is the piece that changes when this repo
changes:

| artifact | image | path in image | defined by |
|---|---|---|---|
| `envoy` | `aether-proxy` | `/usr/local/bin/envoy` | `proxy/BUILD.bazel` |
| `agent` | `agent` | `/agent` | `bazel/img/go_multi_arch_image.bzl` |
| `proxy-ready` | `agent` | `/proxy-ready` | `agent/cmd/agent/BUILD.bazel` (`tars_layer`) |
| `mesh-dns` | `mesh-dns` | `/mesh-dns` | `bazel/img/go_multi_arch_image.bzl` |
| `mesh-dns-ready` | `mesh-dns` | `/mesh-dns-ready` | `agent/cmd/mesh-dns/BUILD.bazel` (`tars_layer`) |
| `controller` | `controller` | `/controller` | `bazel/img/go_multi_arch_image.bzl` |
| `registrar` | `registrar` | `/registrar` | `bazel/img/go_multi_arch_image.bzl` |
| `cni-install` | `cni-install` | `/cni-install` | `bazel/img/go_multi_arch_image.bzl` |
| `aether-cni` | `cni-install` | `/opt/cni/bin/aether-cni` | `cni/cmd/cni-install/BUILD.bazel` (`tars_layer`) |

A Go binary produced by `go_multi_arch_image` lands at the image **root** under its own
name; anything else in an image is an explicit `tars_layer` entry in that image's
`BUILD.bazel`, and that map is the authority for the path.

The two readiness probers are the newest rows. `proxy-ready` (#673) ships inside the
**agent** image, because the initContainer that copies it onto the proxy pod already runs
that image; its `targets.tsv` row landed on 2026-09-05 (GitOps #39). `mesh-dns-ready`
(#683, #688) ships inside the **mesh-dns** image, which its own DaemonSet already runs;
its row is being added by a follow-up GitOps PR. Both are stdlib-only and tiny (~1.7 MB),
but they are separate ELFs with their own build IDs, so an unlisted prober profiles as
hex while the daemon beside it symbolises fine.

**If you move or rename a binary inside an image, update `targets.tsv` in the GitOps
repo.** Nothing in this repo's CI will catch it; the symptom is one component's frames
going hex while every other component stays fine. The sync job alerts on this
(`DebuginfoSyncMissingSymbols`).

## What this repo does guarantee

Three build-time gates keep the *inputs* to symbolisation honest. They cannot know about
`targets.tsv`, but they make "the binary is unsymbolisable" impossible:

- **`//tools/buildid:release_build_ids`** — fails the build if any released binary lacks
  a build-ID note, carries one that is not the hash of its own bytes, or shares an ID
  with another released binary. That collision is why the rule exists: before #653 one
  commit produced seven ELFs sharing a single stamped ID, and Pyroscope resolved all of
  them against whichever debuginfo happened to be uploaded — silently, with
  plausible-looking frames. It needs no `--stamp`, so the property holds in PR CI too.
- **`//integration:build_id_test`** (in the `proxy/` workspace) — asserts the linked
  Envoy carries a `.note.gnu.build-id` whose descriptor is exactly 20 bytes and not all
  zeroes. It deliberately does not recompute the hash: lld's sha1 is a tree hash over the
  output buffer, a linker internal. That the flag produced the note is asserted at the
  source instead, where `proxy-release.yml` greps the `CppLink` action's command line
  before publishing.
- **`//integration:symtab_test`** (#656) — asserts the shipped Envoy still has `.symtab`.
  A build ID is a key to nothing if the symbol table is gone, and the trap is subtle:
  Bazel's `--strip=always` passes the linker `--strip-debug`, which drops DWARF and
  *keeps* `.symtab` (~22 MB, ~944k symbols). A move to a real strip would read as a size
  win and would quietly cost the fleet its flame-graph names.

Since #697/#705 the proxy is a **bzlmod** module built against a rolling Envoy `main` dev
snapshot with the hermetic LLVM 22 toolchain, and the `--build-id=sha1` linkopt lives in
that workspace's `.bazelrc`. The migration relinked the binary, so the proxy's build ID
moved at rev197 (`6342923f…` → `80542d6b…`) — an expected one-off upload, not a symptom.

## Adding a new binary to the mesh

A new component only gets symbolised profiles once it is added there:

1. Add a row to `clusters/talos-main/pyroscope-debuginfo/targets.tsv`:
   `<image>` ⇥ `<artifact>` ⇥ `<tar-relative path>` ⇥ `executable-full` ⇥ `true`
   (the path has **no leading slash**, and `<artifact>` must be unique in that file).
2. Make sure the workload runs in a namespace the job discovers (`aether-system` or
   `aether-ingress`) from a **digest-pinned** `ghcr.io/bpalermo/aether/*` reference.
3. Add it to the warm-up list above — the first query on a cold series pays the lidia
   conversion, and you want to pay it deliberately rather than at T0 of an investigation.

The next hourly tick picks it up.

## Related

- Alert rules: group `debuginfo-sync` in the cluster repo's `prometheus/values.yaml`.
- [`README.md`](./README.md) — the mesh-DNS alerting rules and how alerts are delivered
  on `talos-main` (helm values, not a `PrometheusRule` CRD).
