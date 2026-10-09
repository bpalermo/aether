# Developer Runbook

A practical guide for **building, testing, and running Aether from a clone** —
the day-to-day developer loop and the local multi-cluster end-to-end harness.

For **installing and operating** Aether on a real cluster (workload onboarding,
routing, observability), see [`getting-started.md`](./getting-started.md). For the
chart values and CLI/annotation reference, see
[`configuration.md`](./configuration.md).

---

## 1. Prerequisites

| Tool | Why | Notes |
|---|---|---|
| **Bazel** via [Bazelisk](https://github.com/bazelbuild/bazelisk) | build system | The pinned version (`9.2.0`) is read from `.bazelversion`; just run `bazel …` and Bazelisk fetches it. |
| **Go** 1.27.1 | language toolchain | Managed by `rules_go`; you rarely invoke `go` directly (use `bazel run @rules_go//go …`). |
| **Docker** (or **Colima** on macOS) | container images + integration tests | Integration tests spin up a real etcd via testcontainers-go. |
| **kubectl**, **Helm 3** (OCI) | deploy / e2e | |
| **kind** | local multi-cluster e2e | Only needed for `e2e/multicluster_config.sh`. |

### macOS + Colima one-time setup

If you use Colima for Docker, generate the Bazel Docker-socket config once:

```bash
./bazel/configure_colima.sh
```

This writes `.bazelrc.colima` (gitignored); it is auto-enabled on macOS via
`--config=colima`, so sandboxed integration tests can reach the Docker socket.

---

## 2. Build

All build/test entry points are in the [`Makefile`](../Makefile) (thin wrappers
over Bazel).

```bash
make build              # bazel build //...  (everything)

make build-agent        # //agent/cmd/agent/...        (node agent + edge)
make build-mesh-dns     # //agent/cmd/mesh-dns/...     (slim standalone mesh-DNS daemon)
make build-proxy-supervisor  # //agent/cmd/proxy-supervisor/... (Envoy hot-restart supervisor)
make build-registrar    # //registrar/cmd/registrar/...
make build-cni-install  # //cni/cmd/cni-install/...
```

There is no `make build-controller`; build it directly with
`bazel build //controller/cmd/controller/...`.

> **The `aether-proxy` (custom Envoy) is NOT built here.** It lives in a separate
> sibling Bazel workspace under `proxy/` (its own `.bazelversion` = 8.7.0) and
> compiles Envoy from source (multi-hour; use a warm cache / CI). Build/load it
> with `make load-proxy-image` only when you need a fresh proxy image. See
> [`proxy/README.md`](../proxy/README.md) and proposal 010.

### Bumping the Envoy pin

The proxy's Envoy is ONE pin written in two files: the `envoy`/`envoy_api`
versions (plus the envoy `single_version_override` that carries the patches, and
the `.envoy`-suffixed sibling deps) in `proxy/MODULE.bazel`, and the
envoyproxy/bazel-registry commit on `proxy/.bazelrc`'s `--registry=` line. The
registry drops old snapshot directories as it moves on, so the version resolves
only at a registry commit that still carries it (`proxy/README.md`, "Envoy
version bumps", has the background and the traps).

Two scripts own it:

- **`scripts/check-envoy-pin.sh`** is the gate. Offline it asserts `envoy` ==
  `envoy_api` == the override's version, that `proxy/.bazelrc` has exactly one
  registry line and it names a full 40-hex commit (never a branch), and that no
  other snapshot version is spelled out in either file. `--online` (CI's
  `envoy-api-parity` job, which runs on every PR including proxy-only ones) also
  asserts the pinned registry commit serves `modules/<m>/<v>/MODULE.bazel` for
  `envoy`, `envoy_api` and every `.envoy` sibling, and that the snapshot's
  `source.json` archives the Envoy commit whose short sha is in the version (the
  workspace records only the short sha; the full one lives in that file).
  `--self-test` is its offline harness (CI's `shell` job).
- **`scripts/bump-envoy-pin.sh <envoy-commit | latest>`** is the maintainer tool.
  It is never run in CI and never builds Envoy.

The procedure:

1. `scripts/bump-envoy-pin.sh latest` (or an Envoy commit a snapshot was cut
   from). Dry run: it resolves the registry commit (the newest one that still
   carries that snapshot), prints the rewrite of both files as one diff, flags any
   `.envoy` module the new `envoy`/`envoy_api` request that the registry does not
   serve at that commit (then it is not self-consistent there: wait, or pick the
   commit by hand with the README's loop), and prints two reports:
   - **the upstream `.bazelrc` diff** between the old and new Envoy commits,
     restricted to the scopes `proxy/.bazelrc` mirrors (unconditional, `:linux`,
     `:clang*`, `:libc++`). `~` changed, `-` removed, `+` added; `[mirrored]`
     marks a flag `proxy/.bazelrc` sets. Every `[mirrored]` line and every `+`
     line needs a decision in `proxy/.bazelrc`'s "UPSTREAM: module wiring"
     section.
   - **the carried-patch report**: each patch in `proxy/bazel/patches/`, in
     `MODULE.bazel` order, applied to a sparse shallow checkout of the new Envoy
     commit (and of the current one as a baseline, where all of them must say
     `applies`). `applies` = still needed; `UPSTREAM` = reverse-applies cleanly,
     the fix is in this Envoy, drop the patch (issue #980); `CONFLICT` = refresh
     it against the new tree. A `CONFLICT` is not applied, so a later patch
     stacked on it can report `CONFLICT` only because of it.
2. Re-run with `--write`, then rewrite the pin's explanatory comment in
   `proxy/.bazelrc` ("why this bump") by hand; the script moves the commit and
   version it mentions but not the reasoning.
3. Drop every `UPSTREAM` patch (the file, its `single_version_override` entry,
   its comment block in `MODULE.bazel` and its `proxy/README.md` row) and refresh
   every `CONFLICT` one.
4. Refresh the lock and check resolution with the proxy workspace's own Bazel
   (module resolution only, no Envoy build): `cd proxy && bazel mod graph
   --depth=1 --lockfile_mode=update`, then `bazel mod explain @quiche @protobuf
   @abseil-cpp`.
5. `cd proxy && bazel test //bazel/patches:carried_patch_tests`.
6. `scripts/check-envoy-pin.sh --online && scripts/check-envoy-api-parity.sh`
   (a new Envoy minor can require bumping `go-control-plane/envoy`; the parity
   script prints the command).
7. One PR. Any change under `proxy/**` runs the hours-long proxy build on the PR
   and `proxy-release` on merge, so keep unrelated edits out of it.

---

## 3. Test

```bash
make test               # bazel test //...              (unit + integration; needs Docker)
make test-unit          # --test_tag_filters=-integration  (no Docker)
make test-integration   # --test_tag_filters=integration   (needs Docker)
make test-race          # all tests with the Go race detector
```

Run a single target directly:

```bash
bazel test //agent/internal/xds/cache:cache_test
```

Integration tests are tagged `integration` and sized `medium`; many are also
guarded by `testing.Short()`, so you can force unit-only behavior on a specific
target with:

```bash
bazel test //... --test_arg=-test.short
```

### `envoy --mode validate`: the released proxy or the one a PR builds

`//agent/test/envoy_validate` runs `envoy --mode validate` over aether-generated
configs, and it has two modes. **Default** (`bazel test //agent/test/envoy_validate/...`,
what the `ci` workflow runs): the binary is `@pinned_envoy_linux_<arch>`, lifted
by `//bazel/proxy_pin` from the aether-proxy image `charts/aether/values.yaml`
pins — the proxy the mesh deploys today (#709). **Built proxy**
(`agent/test/envoy_validate/validate-built-proxy.sh <envoy-binary>`): the script points
`--override_repository` for the host arch's pinned repo at a local repo whose
`envoy` symlinks to the given binary, so the same tests validate against an
Envoy built elsewhere — typically the `//proxy` workspace's `//:envoy`. Bazel
digests the binary behind the symlink, so a rebuilt proxy reruns the gate rather
than hitting a cached pass. `.github/workflows/proxy.yml` runs this mode on every
PR that touches `proxy/`, on both arch legs, against the binary that leg just
built: without it a proxy PR that compiles an extension out or changes a carried
patch was validated against the *previous* release. Either way the log names the
binary (`envoy under validation: <path> (version: ...)`), and the test fails on
anything whose `--version` is not Envoy's, so a stand-in like `/bin/true` cannot
pass the positive cases.

### The race gate

`.bazelrc` defines `test:race --@rules_go//go/config:race`, so `--config=race` is
the supported spelling and `make test-race` is the whole-tree run of it:

```bash
bazel test --config=race //agent/internal/meshdns:all //agent/storage:all
bazel test --config=race --runs_per_test=3 --nocache_test_results //common/grpcserver:all
```

Prefer the scoped spelling while developing: a bare `//...` race run currently
reports known test-only races that are being fixed separately (#772 phase A2), so
its failures are not necessarily yours. `--runs_per_test=3
--nocache_test_results` is what actually shakes out a scheduling-dependent race.
And a clean run is not evidence of absence: the detector only reports
interleavings a test actually produced, so a race between two goroutines no test
runs concurrently stays invisible no matter how often you run it.

### Code coverage

```bash
make coverage                                   # the whole unit suite
make coverage COVERAGE_FLAGS="--jobs=6"         # extra `bazel coverage` flags
```

`scripts/coverage.sh` writes `coverage-report/` (git-ignored): `coverage.lcov`,
`coverage.xml` (Cobertura), `summary.md` (the per-component table) and
`unrecorded.txt`. `.github/workflows/coverage.yaml` runs the same script on
every pull request and on every push to `main`, puts the table in the job
summary and keeps the files as the `coverage-report` artifact (90 days). On a
pull request its `gate` job then compares the total with `main`'s and fails the
`coverage` check when it dropped by more than one percentage point: "The gate"
below. The comparison is the repository's own, because GitHub's native code
coverage is not available to it ("The native upload is parked").

**What the number is.** Line coverage of every `go_test` **not** tagged
`integration`, `requires-root` or `manual` (97 of 101 when this was written),
over all first-party Go. Integration, e2e, kind, netns and soak runs do not
count yet, so code only they exercise reads as uncovered.

- *The whole suite, every time.* `ci` tests bazel-diff's impacted subset;
  coverage cannot, because a percentage over a different set of tests per pull
  request is not comparable with `main`'s. Bazel caches each test's coverage
  result, so locally an unchanged test is a cache hit (a second run: 1 s, 0 of
  97 tests executed). In CI only the compiles are; see "CI cost" below.
- *Components are derived, not listed.* Every top-level directory with a
  `go_library` or `go_binary`, minus `api` (generated proto Go), `test` and
  `e2e` (harnesses) and `bazel` (build tooling) — the `EXCLUDED` list in the
  script. A new top-level component is measured from its first Go target with
  no edit. `cmd/` mains are in: they are code.
- *One subtree of a component is out too:* `agent/test` (`EXCLUDED_SUBTREES`).
  It holds the Envoy-driven harnesses (`envoy_validate`, its generator,
  `mtlspool`, `envoyargs`, `envoybin`), which import `agent/internal/...` and therefore have
  to sit under `agent/` (#1311); before that they were in `test/` and excluded
  with it. Their two non-test files (the config builders and the generator's
  `main`) are fixtures, not product code, so they stay out of the denominator
  and the move changed no number. Their tests run like any other unit test, and
  the `agent/internal/...` lines those tests execute count. A subtree entry
  that no longer exists fails the run.
- *Untested code counts as zero.* By default `bazel coverage` instruments only
  packages that have a test in the run. The script passes an
  `--instrumentation_filter` over every component and makes every component
  `go_library` a target, so rules_go's baseline action (`go tool cover` over the
  sources the compile action would compile) gives each file no test links a
  zero-hit record with the line set a measured run would have. Files excluded by
  build constraints on linux/amd64 are absent, not uncovered; `unrecorded.txt`
  lists them (three today). Anything else in that list is a file no `go_library`
  has in its `srcs`: run `make gazelle`.
- `_test.go` files are never instrumented.

Baseline at introduction (2026-10-06, `main` at `56a330fc`): **77.82 %**
(21 993 of 28 262 lines). The same run reads 77.96 % without the zero-hit
records, and 78.91 % with Bazel's default instrumentation (27 853 lines): the
honest denominator is 409 lines larger than the default one.

**Run-to-run variation.** Four uncached runs covered 21 993, 21 994, 21 991
and 21 989 lines: 77.80–77.82 %, a spread of 0.018 percentage points.
Seventeen lines in six files were timing-dependent in those runs
(`cni/internal/util/watcher.go`, `common/signals/signals.go`,
`agent/internal/proxy/hotrestart/supervisor.go`,
`agent/internal/meshdns/lameduck.go`, `agent/internal/xds/server/refresh.go`,
`agent/internal/xds/cache/identitybinding.go`), so the widest gap two runs
could show is 0.06 points. The first two CI runs read 77.84 % and 77.81 %
(21 998 and 21 991 lines). Six of the runner's extra lines were
`watcher.go`'s closed-`Errors` exit (lines 119–121 and 135–137): `Close`
leaves the watch goroutine with three ready `select` arms and the scheduler
picks, which it did on the runner in every run, in about a third of plain
`go test` runs on the workstation and never under Bazel there — a scheduling
difference, not a privilege or kernel one (#1315). `watcher_loop_test.go` now
takes each exit of that loop deterministically, so `watcher.go` is out of the
timing-dependent set. Because CI re-runs every test (next paragraph), a pull request
and `main` are two independent samples of that noise: a `COVERAGE_MAX_DROP`
below roughly 0.1 would fail pull requests that changed nothing. The default
of 1.0 is sixteen times the widest gap.

**CI cost.** The `report` job took 13 min 5 s cold (every instrumented compile
executed on RBE) and 5 min 6 s warm (3 642 remote cache hits, 97 tests
executed on the runner). It never gets faster than the warm figure, because
test results are not cached in CI: Bazel reports *"--remote_upload_local_results
is set, but … the current account is not authorized to write local results to
the remote cache"*, and test execution is local by design (`--config=remote`,
`--strategy=TestRunner=local`). The same holds for every leg of `ci`; it is
only visible here because this job runs the whole suite.

Since #1314 the jobs that run on `main` write those results and pull requests
read them. Two BuildBuddy keys share one secret name, `BUILDBUDDY_ORG_API_KEY`:
the repository-level secret is a **CAS-only** key (it may upload inputs and
outputs, never an action result: the right type for a pull request, which could
otherwise store a forged "test passed" that `main` would later trust), and the
`main` **environment** holds a **Read+Write** key that shadows it. `main.yaml`'s
`test` job and this workflow's `report` job on `main` declare
`environment: main`; the environment admits the `main` branch only, so a pull
request's run cannot obtain the key. (`release` holds a Read+Write key too, for
the publishing builds, next to the registry credentials; test jobs stay out of
it.) What to expect: on `main` the warning above is gone; on a pull request it
remains (that run still cannot write) and Bazel reports the unchanged tests as
`(cached)` / `remote cache hit`. Two caveats: a coverage result is reused only
by another coverage run, and `race` runs nowhere on `main`, so a pull request's
`race` leg still executes every impacted test.

**The gate.** On every pull request the `gate` job of the coverage workflow
compares the pull request's total line coverage with a baseline from `main` and
fails when it is more than `COVERAGE_MAX_DROP` percentage points lower (default
**1.0**; a drop of exactly the threshold passes). The aggregate `coverage` job
fails with it, so `coverage` is the one check to require. Only the total gates.
The job summary also shows, for the reviewer and without gating: the two totals
and their delta, a per-component table with each component's delta, and the
coverage of every changed file (`git diff <baseline>...<head>`) that either
report names, new and deleted files included. When the total dropped it adds
the **largest per-file drops**: the ten files (`--top-drops`) whose covered-line
count fell most, whatever the diff touched, and the files that left the report
(`removed`) — open when the gate fails, collapsed when the drop is within the
threshold, absent when nothing dropped. Patch coverage is deliberately
not a gate: it punishes the pull request that touches old untested code.

- *Which baseline.* `scripts/coverage-baseline.sh` takes the `coverage-report`
  artifact of a **successful run of the coverage workflow on `main`** (a `push`
  or a `workflow_dispatch` run, never a pull request's or a fork's):
  1. the run for the pull request's **base commit**
     (`github.event.pull_request.base.sha`). The ruleset keeps a branch up to
     date with `main`, so this is normally `main`'s head and the delta is the
     pull request's own. If that run is still going (a pull request pushed
     right after a merge) the job waits up to ten minutes for it;
  2. otherwise **`main`'s most recent** successful run whose artifact still
     exists. The summary and the notice then say so and name the commit
     (`main at 1a2b3c4d (main's latest report, not the base commit …)`). The
     delta then also contains whatever `main` gained between the two commits.
  3. none: the job **fails** with `Coverage gate: no baseline`. It never passes
     with nothing to compare. Fix: run the workflow on `main` (*Actions >
     coverage > Run workflow*, or `gh workflow run coverage.yaml --ref main`),
     wait for it, re-run the failed job. Artifacts are kept 90 days, so this
     only happens after a quarter with no push to `main`, or if every recent
     run on `main` failed.
- *Stacked pull requests.* A `gh stack` member's base is the `upgrade/**`
  branch beneath it, a commit `main` never had, so it always takes path 2: it
  is compared with `main`'s latest, **cumulatively** with the members beneath
  it. That is the number that matters, because the stack lands on `main`: a
  member cannot hide a drop behind the one below it, and conversely a stack
  whose first member drops 0.8 and second 0.4 fails at the second, where the
  total crosses the line. Add the tests there or split differently.
- *The threshold.* One constant, `DEFAULT_MAX_DROP` in
  `scripts/coverage-compare.sh`, overridden by the repository variable
  `COVERAGE_MAX_DROP` (*Settings > Secrets and variables > Actions >
  Variables*; `gh variable set COVERAGE_MAX_DROP --body 0.5`). A plain number
  of percentage points between 0 and 100; unset or empty means the default,
  and anything else (`1%`, `-1`, `one`) fails the job naming the value rather
  than being ignored. `COVERAGE_MIN` is the same for an absolute floor on the
  total, in percent; unset (today) means no floor. A relative gate alone lets
  the number erode by up to a point per pull request, and the floor is what
  stops that if it ever matters.
- *Locally.* The same comparison, against any baseline you have:
  `gh run download <run id> --name coverage-report --dir /tmp/base`, `make
  coverage`, then `scripts/coverage-compare.sh --baseline
  /tmp/base/coverage.lcov --head coverage-report/coverage.lcov`.

**The gate failed.** The `::error` on the run gives both totals, the line
counts and the delta; the component and changed-file tables say where it came
from. When no measured file changed (the changed-files table reads `None of the
changed files is in either report`), read **Largest per-file drops**: it names
the files that lost covered lines although the diff did not touch them.

- *Code lost its tests, or new code has none.* Add the tests. A new file no
  test links counts at zero, which is the point.
- *A test stopped running.* A `go_test` that gained `manual`, `integration` or
  `requires-root`, or a deleted test target, drops out of the measured set
  while its library's lines stay in the denominator. No Go file changes, so
  only the per-file drops table shows which files that test was covering
  (#1319).
- *The drop is legitimate.* Deleting a large, well-tested package lowers the
  percentage although nothing got worse: 1 000 fully covered lines removed
  from today's tree is about −0.8 points. The lines columns of the tables show
  that shape (lines and covered fall together). The escape hatch is the
  threshold itself, and only the owner holds it: set `COVERAGE_MAX_DROP` high
  enough for that pull request (`gh variable set COVERAGE_MAX_DROP --body 2.5`),
  re-run the failed `gate` job, merge, and **restore it** (`gh variable delete
  COVERAGE_MAX_DROP`). While raised it applies to every open pull request, so
  keep the window short. The next pull request is compared with `main`'s new,
  lower number; nothing has to be reset. There is deliberately no label or
  commit-message bypass: those are things a pull request's author controls,
  and a gate its subject can switch off is not one.
- *`Coverage gate: no baseline`.* See path 3 above.
- *`Coverage gate: malformed threshold`.* Fix or delete the variable it names.

`//scripts:coverage_gate_test` holds the comparison to a golden report and to
verdicts worked out by hand, and the baseline selection to a fake `gh`.

**The one remaining owner step.** Nothing requires the workflow until
`coverage` is added to the `main` ruleset's required status checks (next to
`ci`, `proxy`, `codeql` and `CodeQL`). Do not add the ruleset's "Restrict code
coverage" rule: it reads GitHub's native coverage, which has no data here.

**The native upload is parked.** GitHub's native code coverage (GitHub Code
Quality) is not available to this repository: as of 2026-10-06 the upload API
answers HTTP 404 on pull requests and HTTP 500 on `main` even with
`code-quality: write` granted, and a repository owned by a personal account has
no *Code quality* page under Settings to enable anything on
(actions/upload-code-coverage#16). The `upload` job is kept but runs only when
the repository variable `CODE_COVERAGE_UPLOAD` is `true`; by default it is
skipped, with no warning, and a skipped upload does not fail `coverage`. If
GitHub enables the feature, set the variable. With it on, a rejected upload is
red (there is no `continue-on-error`): `Coverage upload failed (HTTP 404): Not
Found` means the endpoint still does not exist for the repository; HTTP 403
with "not authorized" is a missing `code-quality: write` on the job; a
processing failure names what GitHub could not parse. Convert locally and look
at the document: `scripts/lcov-to-cobertura.sh coverage-report/coverage.lcov`.
The three things read from it are the root `line-rate`, each `<class
filename=…>` (repo-relative; the converter refuses anything else) and each
`<line number=… hits=…>`. A fork's pull request is never uploaded (read-only
token). `//scripts:lcov_to_cobertura_test` holds the converter to a golden file
and `//scripts:coverage_test` holds the script's target selection.

### CI: external repository fetches (#1001)

BuildBuddy caches **actions**, not **repository fetches**. Every external
repository a CI job needs — each Go module behind `gazelle++go_deps+…`
(`fetch_repo` → `proxy.golang.org`), every release asset from github.com — comes
from Bazel's repository cache on the runner or from the network. Two mechanisms
keep a flaky upstream from turning a job red:

- **A persisted repository cache.** `bazel-contrib/setup-bazel` restores
  `~/.cache/bazel-repo` (Bazel 9 also keeps its repo *contents* cache under it,
  `contents/`, which is what makes a `go_repository` a local hit instead of a
  `fetch_repo` run). The key hashes `MODULE.bazel`, `MODULE.bazel.lock`, `go.mod`
  and `go.sum`, so a Go dependency bump gets a new key and the newest older entry
  is restored as the fallback. The root and `//proxy` workspaces are in separate
  namespaces (`cache-version: root-1` / `proxy-1`). **Exactly one job per
  workflow saves** — `diff` in `ci.yaml` and `main.yaml` (the `main` entry is the
  one every PR falls back to), the `test`/`build-push` matrix per arch for the
  proxy — because its warm-up fetches for all of `//...`; every other job
  restores only. All of it lives in one composite action,
  [`.github/actions/setup-bazel`](../.github/actions/setup-bazel/action.yml):
  a job picks `workspace: proxy` or the root default and, if it is the saver,
  `cache-save: true` (the release and signing workflows keep explicit copies of
  the same values). The first job to finish used to save, so a job that
  fetched a handful of repositories (or, before the namespaces, a *proxy* job)
  could write the entry every later job restored as an exact hit and never
  re-saved. That is how the 2026-09-27 `netns` failure fetched
  `googleapis/api` from the network with a 1.9 GB cache freshly restored.
- **A retried warm-up.** Each Bazel job's first Bazel step is
  `scripts/ci-bazel-warmup.sh <flags> <targets>`: `bazel build --nobuild`
  (loading + analysis, which is where the fetches happen), retried up to 3 times
  with a 20 s / 40 s backoff **only when the output shows a repository-fetch
  error**. A broken BUILD file fails on the first attempt. The warm-up clears the
  remote executor, cache and BES backend, so it needs no BuildBuddy key and never
  shows up as an invocation; the real steps keep `--config=ci` / `--config=remote`
  unchanged. `scripts/check-ci-bazel-warmup.sh` (in the `shell` job) pins that
  contract with a stub bazel.

Reading a red job:

| The log shows | What it means | Do |
|---|---|---|
| `::warning::bazel warm-up attempt 1/3 hit a repository-fetch error`, then success | the retry absorbed a blip | nothing |
| `::error::bazel warm-up: repository fetch failed on all 3 attempts` | the upstream was down for the whole ~1 min of backoff | **re-run the job** once the upstream answers (`gh run rerun <id> --failed`); a re-run restores the same cache, so it only re-fetches what failed |
| a `fetch_repo` / `Error downloading` failure in a step **after** the warm-up | that step needed a repository the warm-up's targets did not cover (e.g. a `bazel run` of a tool, `bazel-diff` at the base revision, which falls back to a full run on failure) | re-run; if it repeats, add the target to that job's warm-up |
| `bazel warm-up failed (exit N) with no repository-fetch error` | a real analysis failure | fix the change; a re-run will not help |

A plain re-run is still the answer for anything the warm-up does not cover: the
Go tool's own downloads in `deps-audit` (`go list -m all` talks to
`proxy.golang.org` directly, outside Bazel), docker/kind image pulls in the e2e
jobs, and a cold namespace (the first run after a `cache-version` bump, or after
GitHub evicted the entry: the repository has a 10 GB cache budget and each entry
is ~2 GB).

To check what a job restored, open its **Setup Bazel** step: `Cache hit for:
setup-bazel-root-1-linux-x64-repository-<hash>` is an exact hit;
`Successfully restored cache from …` with a different hash is the fallback.

### CI: what the `ci` check tests, and what makes it pass (#1459, #1460)

**One tree.** On a pull request every job of `ci.yaml` builds the run's commit,
which is the merge of the pull request's head into its base. The `diff` job
lists the impacted targets on that same commit: `scripts/ci-merge-range.sh`
reads the range from the merge (its first parent, the base, to the merge
itself) and `scripts/ci-impacted-targets.sh` refuses any other `HEAD_SHA` than
the checkout. The artifact carries the commit in `impacted_commit.txt`; a job
that reads the lists gets them through `.github/actions/impacted-lists`, which
fails when a list is missing, when the lists are for another commit than the
job's checkout, or when a `has_*` output disagrees with its list.

**The required check.** `ci` passes when `scripts/ci-gate.sh` accepts the
results and outputs of every other job. A skipped job passes only where a job
said so: `changes` wrote `control_plane=false`, or `diff` wrote `false` to the
output the leg depends on. "Nothing impacted" is `diff` writing `false` four
times, and the log of the `ci` job says which case it was:

| The `ci` job's log shows | What it means | Do |
|---|---|---|
| `decision: impacted; had to run: test race …` | those legs ran and succeeded | nothing |
| `decision: nothing impacted (diff wrote false to has_any, …)` | bazel-diff found no target the change reaches; the legs are skipped by design | nothing |
| `decision: the control plane is untouched (changes wrote control_plane=false)` | a proxy-only change; the `proxy` workflow is its gate | nothing |
| `job <x> ended as failure` / `cancelled` | that job is the one to read | fix it, or re-run it if no step ran |
| `job diff succeeded and its output has_… is not set` | `diff` wrote no decision, so the legs skipped on an empty value | read the `Compute impacted targets` step; this is a defect in the workflow or the script, not in the change |
| `job <x> was skipped, and diff says it had to run` | a leg did not run although its output is `true` (usually a job it needs failed, reported on its own line) | read the failed job first |
| `job <x> is not in the needs of the ci job` / `has no rule for it` | a job was added to `ci.yaml` and not to the `needs` of `ci` or to `scripts/ci-gate.sh` | add both; `//scripts:ci_gate_test` holds them to the workflow |

A cancelled run of `coverage` or `codeql` (both cancel the previous run of the
same pull request when a new commit arrives) shows its summary job as failed on
the commit that was superseded. That is deliberate: a summary job that was
skipped would satisfy the ruleset, so it runs and fails. The checks of the
newest commit are the ones that count. `ci` has no concurrency group and is not
cancelled this way.

### Stuck workflow runs

`publish.yaml`, `proxy-release.yml` and `pages.yaml` serialise on a concurrency
group with `cancel-in-progress: false`, and GitHub keeps at most one *pending*
run per group. A run whose job never gets a runner therefore blocks every later
run of that workflow, silently: the later ones show `pending` with no jobs, and
each newer one replaces the previous as `cancelled`. On 2026-10-05 a publish run
sat `queued` for five hours this way, and a pages run queued since 2026-10-02 had
held up every website deploy for three days. A job `timeout-minutes` does not
help: it only counts once the job has started.

`.github/workflows/stuck-runs.yaml` checks every 30 minutes
(`scripts/stuck-runs.sh`). A run counts as stuck once it has been `pending` /
`waiting` / `requested`, or has had a job sitting `queued`, for more than 60
minutes. Every stuck run goes on one rolling issue, **CI: workflow runs stuck
before they started**. The issue gets a new comment only when the set of stuck
runs changes, and it is closed once nothing is stuck.

The watchdog cancels a run only when the run is superseded. All of these must
hold:

- it is a `push` or `pull_request` run, on its first attempt, and not from a fork;
- either its branch has been deleted, or both:
  - its commit is no longer the head of its branch, and
  - a newer run of the same workflow on that branch is queued, running, or has
    succeeded.

The newer-run condition is there for path-filtered workflows like `pages`: a
newer commit on `main` does not mean a newer deploy is coming. The watchdog
**never** cancels the run for the head of its branch, a re-run, or a
scheduled or dispatched run. Those need a person:

```bash
gh run cancel <id>                       # or, if it will not cancel:
gh api -X POST repos/bpalermo/aether/actions/runs/<id>/force-cancel
gh run rerun <id>                        # the head's own run: run it again
```

GitHub can strand a run it then refuses to cancel. Run 34723047990, a `proxy`
run queued since 2026-09-12 on a branch that was later deleted, answers both
cancel and force-cancel with HTTP 409 ("Cannot cancel a workflow run that is
not in progress"). Nothing in this repository can clear a run like that (#1301).
When a cancel and the force-cancel after it both return 409, the watchdog
reports the run once as **uncancellable — needs GitHub support**. It records the
run ID in a hidden `<!-- stuck-runs-uncancellable: … -->` marker on the issue
and stops counting the run as stuck. Each check reads the marker back from the
newest issue with that title, even a closed one, so the issue can close and new
stuck runs are still reported. The ID drops out of the marker once GitHub stops
listing the run. To have such a run removed, open a GitHub support ticket.

To see what the watchdog would do without it cancelling or writing anything,
dispatch it as a dry run: `gh workflow run stuck-runs.yaml -f dry_run=true`.
A dispatch is a dry run unless you pass `dry_run=false`.

---

## 4. Format & lint

```bash
make format             # bazel run //:format        — gofumpt, buildifier (+ -lint=fix), shfmt, buf (in place)
make format-check       # bazel run //:format.check  — CI-friendly, fails on drift AND on buildifier lint warnings
make lint               # bazel build --config=lint //...  — buf, shellcheck, gocognit aspects
```

After changing Go imports or adding/removing Go files, regenerate BUILD files:

```bash
make gazelle            # bazel run //:gazelle
```

To add a Go dependency (never edit `go.mod` by hand, and never run `go mod tidy`
directly):

```bash
bazel run @rules_go//go get <package>
make gazelle
make tidy               # bazel mod tidy
```

### MODULE.bazel.lock in CI (#1284)

CI runs with `--lockfile_mode=error` (`common:ci` in `.bazelrc`), so a change to
`MODULE.bazel` (or a bump that moves a transitive module) without the matching
`MODULE.bazel.lock` fails the job with Bazel's own message naming the stale
entry. Fix it locally and commit the lock:

```bash
bazel mod deps --lockfile_mode=update
```

The one non-reproducible extension in the graph was rules_rust's `crate`, used by
protobuf for its Rust bindings (nothing here builds Rust): `crate.from_specs()`
without a lockfile re-resolved against live crates.io on every evaluation, so
any `bazel mod` command or a stale digest rewrote the lock with newer crates. A
patch on protobuf (`single_version_override` in `MODULE.bazel`) points it at
`bazel/protobuf_crates/{Cargo.lock,Cargo.Bazel.lock}`; with both set the extension
is reproducible and is not recorded in `MODULE.bazel.lock` at all. When a
protobuf or rules_rust bump changes the specs or the cargo-bazel digest, any
command that evaluates that extension (a `bazel mod` subcommand; a build of
`//...` does not, it never loads `@crates`) fails with "The current `lockfile`
is out of date for 'crates'"; repin with `CARGO_BAZEL_REPIN=1 bazel mod deps`
and commit both files.

### Go dependency hygiene

```bash
make deps-audit         # scripts/go-deps-audit.sh — also a required CI job
```

**`go mod tidy` is forbidden here, and `go mod tidy -e` is worse than useless.**
The generated proto packages `aethermesh.dev/api/aether/{agent,cni,config,registrar,registry}/v1`
exist only as Bazel outputs — there are no `.go` files for them in the tree — so
the Go tool cannot resolve a single import of them and fails with *"no matching
versions for query latest"*. The `-e` escape hatch does not fix that: it just
keeps going and strips every module that only generated code or a BUILD file
needs, which then breaks the `go_deps.gazelle_override`s and leaves `bazel mod
tidy` failing with *"Some gazelle_overrides did not target a Go module with a
matching path"*. Nothing that `go mod tidy -e` prints about this module is
trustworthy.

So `make deps-audit` does the job instead, from the module graph (`go list -m
all`, which works from `go.mod` alone) and the Bazel graph (`@repo//`
references in BUILD/.bzl files), and CI runs it on every PR. It reports:

1. **stale `go.sum` modules** — modules no longer anywhere in the module graph.
   Nothing prunes these automatically (`bazel mod tidy` does not touch go.sum),
   so they accumulate after every removal. Fix: delete their lines, then
   `bazel build //... //test/e2e:e2e_test` — gazelle's `go_deps` verifies every
   module it fetches against `go.sum`, so an over-eager deletion fails loudly and
   `bazel run @rules_go//go -- mod download <module>` puts it back.
2. **unused direct requires** — no Go import, no BUILD/.bzl reference, no `tool`
   directive.
3. **direct requires with no Go import** that are only reachable from Bazel and
   are not annotated.

Two rules follow from that, and both matter:

* **Reclassify, don't drop.** Most unimported requires (`golang.org/x/crypto`,
  `github.com/go-openapi/swag`, `github.com/moby/go-archive`, …) exist to force a
  CVE-clean version through MVS. Move them into the `// indirect` block — the pin,
  and therefore the selected version, survives; only the directness changes.
  Deleting the line silently lets the vulnerable version back in. Note that `go
  get` marks whatever it fetched as direct, so a floor bump needs its `// indirect`
  marker put back by hand.
* **Annotate the Bazel-only tools.** A module that is genuinely only named by a
  BUILD/.bzl file (`buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go`,
  from the `gazelle:resolve` directive in `//BUILD.bazel`;
  `github.com/uudashr/gocognit`, the lint aspect's binary) stays **direct** and
  carries a `// bazel-only:` comment, so the next reader knows why it looks
  unused. `//bazel/protodoc` is a real Go package, so protoc-gen-doc and protokit
  are ordinary imports, not Bazel-only.

To actually remove a module, use `go mod edit -droprequire`, not `go get
<mod>@none`: `@none` *downgrades* everything that requires the module, which is
how removing the unused `go.etcd.io/etcd/server/v3` once dragged
controller-runtime from 0.25.0 back to 0.9.7 (`k8s.io/apiextensions-apiserver`
requires it). Delete any matching `go_deps.gazelle_override` in the same change,
then `make tidy` and `make gazelle`.

### CodeQL code scanning

Code scanning is the advanced-setup workflow `.github/workflows/codeql.yaml`
(push to `main`, every pull request, weekly), not GitHub's default setup. The
two are mutually exclusive: with default setup enabled in the repository's
security settings GitHub rejects this workflow's uploads, so default setup must
stay **off**. Its last job, `codeql`, aggregates every language's job and is the
one name to require in the `main` ruleset (next to `ci` and `proxy`); it also
runs on pull requests whose base is `upgrade/**`, so a `gh stack` member can
report it. A status check only says the scan ran: blocking on *findings* is the
ruleset's separate "Require code scanning results" rule. A fork's pull request
is analysed but not uploaded (read-only token).

It exists because default setup cannot run a step before the scan. The proto
packages under `api/aether/` are Bazel outputs with no `.go` file in the tree,
so default setup's Go autobuilder reported *"6 packages could not be found"* and
analysed the 85 files that import them, the gRPC and API surface, without their
types. It also ran `go mod tidy -e` (forbidden, see above) and `make`.

What the `Analyze (go)` job does, in order:

1. `scripts/materialize-generated-go.sh` asks Bazel for every
   `go_proto_library`, builds their `go_generated_srcs` output group and copies
   the `.pb.go` / `_grpc.pb.go` files to `api/aether/<pkg>/v1/`, where their
   import paths point. The copies are git-ignored (`/api/aether/**/*.pb.go`) and
   listed in `.materialized-generated-go`. Bazel runs **before** CodeQL is
   initialised so the tracer never sees rules_go's own `go` processes.
2. `scripts/go-build-plain.sh` builds the module with the plain go command
   under CodeQL's tracer: one `go build ./...` over the whole module, pinned Go
   from `go.mod`, `GOFLAGS=-mod=readonly`, `GOTOOLCHAIN=local`, `GOOS=linux`,
   `CGO_ENABLED=0`. `go.mod` and `go.sum` must come out untouched. The tracer
   extracts each package and then lets the real build run, so this step is also
   the proof that the module compiles outside Bazel.
3. CodeQL analyses with `upload: never`.
4. `scripts/codeql-go-diagnostics.sh` reads the SARIF and fails the job if the
   extraction was incomplete (below).
5. Only then are the results uploaded, under the same categories default setup
   used (`/language:go`, `/language:c-cpp`, …), so existing alerts and
   dismissals keep matching. Every job also keeps its SARIF as a
   `codeql-sarif-<language>` workflow artifact.

The other four languages (`actions`, `c-cpp`, `javascript-typescript`,
`python`) are analysed from source with no build. Query suite `default`, threat
model `remote` (`.github/codeql/codeql-config.yml`). Go test files are not
scanned.

**Locally:**

```bash
make materialize-go        # copy the generated Go into the tree (git-ignored)
make go-build-plain        # materialize, then go build + go vet with the plain
                           # go command and the Bazel-pinned SDK
make materialize-go-clean  # remove the copies
```

The copies are inert for Bazel and Gazelle (no target lists them, and Gazelle
skips a `.pb.go` that sits next to its `.proto`), but they go stale when a
`.proto` changes: re-run `make materialize-go`, or clean them.

**Nothing is excluded from the plain build.** `scripts/go-build-plain.sh` runs
`go build ./...` (and, with `--vet`, `go vet ./...`, which also type-checks
every `_test.go`) over the whole module: no package list, no exception.
`//scripts:go_build_plain_test` holds it to exactly those two invocations.

Bazel does not enforce Go's internal-package rule (`visibility` is its own,
looser mechanism), so a package outside `agent/` can import
`agent/internal/...` and stay green in Bazel while the go command, gopls and
CodeQL's extractor refuse it (*"use of internal package ... not allowed"*).
That was the case for `test/envoy_validate`, its generator and `test/mtlspool`
until #1311 moved them to `agent/test/`, and it is the only way such a failure
is fixed: move the importer under the tree whose `internal/` it uses (or give
it a non-internal package to import). Do not exclude it from the build and do
not allow its extraction error.

What `./...` leaves out is decided by build constraints in the files
themselves, never by a list. Three non-test files are never part of a Linux
build and so are not in the database: `agent/internal/udscsi/mounter_other.go`,
`common/file/dirsync_windows.go`, `common/file/fadvise_unspecified.go`. And
`test/conformance` holds one `_test.go` behind `//go:build conformance`: it is
compiled inside the upstream gateway-api module, not this one (its README), so
the directory is not a package here for the go command or for Bazel.

**"The Go extraction is complete" failed.** CodeQL does not fail a scan it could
only half do, and a partial result uploads cleanly and then closes every alert
in the code it did not understand as *fixed*. The step reads the SARIF's
`runs[].invocations[].toolExecutionNotifications[]` and fails on any
`go/diagnostics/extraction-errors` entry (an unresolved package or type, an
internal-package violation): there is no allow-list, and
`//scripts:codeql_go_diagnostics_test` fails if one comes back. It also fails
on any `go/autobuilder/*` warning (the *"packages could not be found"* family),
on an extracted `_test.go`, and when a materialized file is missing from the
database. Nothing was uploaded; the SARIF artifact has the full list. The usual
cause is a generated package the materialize step did not cover.

**Adding a generated Go package.** A new `go_proto_library` anywhere in the root
workspace is discovered by `bazel query`; nothing lists the targets by hand. If
its import path is outside `api/aether/`, the script refuses to write it until
`.gitignore` covers the destination: add a pattern scoped to `*.pb.go` in that
directory, never a pattern that could match a hand-written file. Generated Go
that is **not** a proto (a `genrule` output in a `go_library`'s `srcs`, a
generated `embedsrcs` file) makes the script fail by name: teach it where the
file belongs first. `scripts/materialize-generated-go.sh --check-untracked`
(the `shell` CI job) fails if a generated file is ever committed.

---

## 5. Container images

In-repo images (agent, mesh-dns, cni-install, registrar) build with `rules_img`:

```bash
make load-all           # load agent + mesh-dns + cni-install + registrar into local Docker
make load-agent-image   # a single image (…-mesh-dns-image, …-registrar-image, …-cni-install-image likewise)
make push-all           # push all to the registry
```

`mesh-dns` is its own image (#583): the `aether-mesh-dns` DaemonSet must NOT ship
the full agent, so it gets the slim `/mesh-dns` binary alone (~7Mi vs the agent's
~20Mi) and the chart pins it separately via `agent.meshDnsDaemon.image`.

The `controller` image is not in `load-all`; load it with
`bazel run //controller/cmd/controller:image_load`.

### Which build is this binary (`--version`, #1378, #1429)

Eight of the thirteen binaries the images ship answer `--version` and exit
without starting anything: `agent`, `registrar`, `controller`, `prober`,
`proxy-supervisor`, `mesh-dns`, `uds-csi` and `cni-install`. Five do not, on
purpose:

- the four probe binaries (`proxy-ready`, `agent-ready`, `mesh-dns-ready`,
  `identity-ready`) are exec'd every few seconds or gate a pod's start, and each
  has a test that holds it to the smallest possible link set;
- the CNI plugin (`cni`) is not run by hand: the container runtime execs it with
  its command in the environment, and the CNI specification defines its own
  `VERSION` command for that.

`readelf -n` reads the build ID of any of them, and `make check-build-id` lists
the CNI plugin's next to the eight above (not the probe binaries'). The e2e
fixtures (`l4echo`, `udsecho`) are test images and have no flag either.

```console
$ kubectl -n aether-system exec ds/aether-agent -c agent -- /agent --version
agent build-id bbf3ba6860da40c812de4d2317532ec7d5f17ec0
package aethermesh.dev/agent/cmd/agent
go1.27.1 linux/amd64
```

The build ID is the binary's GNU build-ID note, which `//bazel/buildid` derives
from the binary's own bytes. It is the same value:

- `readelf -n <binary>` prints as `Build ID`;
- the component reports as OTel `service.version` (and `uds-csi` as its CSI
  `vendor_version`, visible in its `starting` log line);
- the continuous profiler files that binary's symbols under;
- `make check-build-id` prints for a checkout.

It is **not** a commit. No Go binary of this workspace has the commit linked in
any more (the separately built Envoy in the proxy image still does): a version
taken from the commit made every binary, and so every image, different from one
commit to the next even when its code had not changed. Two consequences:

- Two pods reporting the same `service.version` run byte-identical binaries,
  whatever commits their images were published from; a `service.version` that
  changes across a deploy means that component's code really changed.
- To go from a build ID to source, find the commit whose build produced it:
  `make check-build-id` at a candidate commit, or the image's provenance (see
  [verifying-releases.md](verifying-releases.md)). A build ID belongs to one
  architecture's binary: the amd64 and arm64 builds of a component have
  different IDs, and a pod reports its node's. `make check-build-id` prints
  both lists, `== linux/amd64` then `== linux/arm64`, from any machine (it
  runs `bazel build --platforms=@rules_go//go/toolchain:linux_<arch>
  //bazel/buildid:release_build_ids` for each). The chart's `appVersion`
  still names the commit the chart was packaged from.

There is no module line because a Bazel build records the main package and
every dependency's version, but no version for the main module itself
(`go version -m <binary>` shows the same). A binary run outside an image
(`bazel run //agent/cmd/agent`) reports the build ID the Go linker wrote, which
under rules_go is one constant shared by every binary; only the binaries inside
an image have a content-derived one. A binary with no readable note reports
`dev`.

---

## 6. Local multi-cluster end-to-end (proposal 026)

[`e2e/multicluster_config.sh`](../e2e/multicluster_config.sh) stands up **two kind
clusters** (`a` = exporter, `b` = importer) that share **one etcd** (a Docker
container on kind's network) and drives the cross-cluster GAMMA config loop:

```
cluster a: HTTPRoute + ServiceExport ──(registrar config-export)──▶ shared etcd
                                                                       │
cluster b: agent --import-config  ◀──(registrar ListAllConfig reads same etcd)─┘
```

### Commands

```bash
e2e/multicluster_config.sh up      # build+load images, create clusters + etcd, install aether on both
e2e/multicluster_config.sh test    # apply the Service+ServiceExport+HTTPRoute on 'a', assert propagation
e2e/multicluster_config.sh verify  # re-run the assertions only
e2e/multicluster_config.sh down    # delete both clusters + the shared etcd
e2e/multicluster_config.sh         # up + test (full run)
```

It builds images via `make load-all`, installs the Gateway API (experimental
channel) + MCS-API CRDs, then installs both aether charts per cluster with
`spire.enabled=false`, `agent.gamma=true`, `registrar.registryBackend=etcd`, and
`agent.importConfig=true` on cluster `b`.

### The `fs.inotify` gotcha

Two kind clusters each run an inotify-heavy agent DaemonSet; the host's default
limits are easily exhausted, leaving cluster `b`'s agent stuck in `Init:Error`.
`up` raises the limits (needs sudo):

```bash
sudo sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=524288
```

Without it, the **control-plane** half of the loop (export → shared etcd,
readable by `b`'s registrar) is still proven; only the agent-side materialization
on `b` is unobservable. See the script header and `e2e/kind-cluster.yaml` (which
also assigns non-overlapping pod/service CIDRs per cluster so cross-cluster
endpoints in the shared registry never collide).

### The other multi-cluster harnesses

Two sibling harnesses reuse the same two-kind pattern (same `up`/`test`/
`verify`/`down` verbs, same inotify gotcha); both run SPIRE on each cluster
under a **shared trust domain** (shared upstream CA in `e2e/certs/`):

- [`e2e/multicluster_waypoint.sh`](../e2e/multicluster_waypoint.sh) — the
  **019 east/west waypoint data path**: two clusters + ONE shared etcd,
  `agent.eastWestWaypoint=true`; asserts client(a) → echo(b) returns 200 over
  the node tunnel with mTLS end-to-end (nightly CI: `e2e.yaml` waypoint job).
- [`e2e/multicluster_replicator.sh`](../e2e/multicluster_replicator.sh) — the
  **006 two-region replicator failover**: one etcd PER cluster,
  `registrar.peerEtcd` cross-wired; asserts mirror visibility, the data path
  over the mirror, lease-lapse failover when a region's registrar dies, and
  recovery (nightly CI: `e2e.yaml` replicator job).

### Bumping the e2e Kubernetes version

Every kind e2e surface runs the **same Kubernetes**: the nightly conformance
jobs, the per-PR `//test/e2e`, the script-driven nightly suites and a local run
of any `e2e/*.sh` harness (#1251). [`e2e/kind-version.sh`](../e2e/kind-version.sh)
is the one place it is set: `KIND_VERSION` (the kind release),
`KIND_NODE_IMAGE` (a `kindest/node` image from **that release's notes**, with its
`@sha256` digest — node images are built per kind release) and
`KUBECTL_VERSION` (the same Kubernetes patch). To bump, change those three, copy
the node image into `test/e2e/testdata/kind-config.yaml` (the e2e framework
reads a kind config, not a shell file), and run `bazel test //e2e:kind_pin_test`,
which fails on any copy that disagrees — a kind config, a workflow's
`helm/kind-action` value, or a harness that creates a cluster without
`--image "$KIND_NODE_IMAGE"`. Locally the harnesses refuse a kind binary
**older** than `KIND_VERSION` (`go install sigs.k8s.io/kind@<KIND_VERSION>`, or
`KIND_ALLOW_SKEW=1` to try anyway) and only warn about a newer one;
`KIND_NODE_IMAGE=<image>` overrides the node image for one run.

### Bumping Go

The Go toolchain is pinned once: `go_sdk.download(version = ...)` in
`MODULE.bazel`, the SDK rules_go builds and tests everything with. The few CI
steps that run a bare `go` outside Bazel (the nightly conformance suites'
`go test`, the `cloud-provider-kind` install) get the same version from
`actions/setup-go` with `go-version-file: go.mod`, so go.mod's `go` line (or a
`toolchain` line, which setup-go prefers) must name the same full `X.Y.Z` (#1283).
Bump both together and run `bazel test //e2e:go_pin_test`. It fails on a go.mod
that disagrees with the SDK pin, a setup-go step with a literal `go-version` or
`check-latest`, and any workflow job or composite action that runs `go` without
setting it up first, which would leave it on the runner image's Go.

### Refreshing third-party image pins

An image this repository does not build (curl, the echo servers, OPA, etcd, the
OpenTelemetry collector, the kind node) is named in chart values, in the e2e and
soak harnesses and their manifests, and in test fixtures. Every such reference
carries the digest of the image's **multi-arch index**, and
[`scripts/third-party-images.txt`](../scripts/third-party-images.txt) is the one
list of them, one `pin <name> <tag> <digest>` line per image (#1400, #1401). A
reference is written `<name>:<tag>@sha256:…`: the digest is what a node or
`docker run` pulls, the tag is there for the reader. A few older references are
`<name>@sha256:…`, with the tag recorded only in the list.

`scripts/third-party-images.sh` has four commands:

| Command | Network | What it does |
| --- | --- | --- |
| `check` | no | The gate (CI's `shell` job). Fails on an image named by tag only or with no tag, on a digest the list does not have for that name, on a tag that disagrees with the list, and on a pin or an exception nothing uses any more |
| `list` | no | Every pin, with the files and lines that use it |
| `outdated [--newer-tags] [<name>...]` | yes | Asks each pin's registry what its tag points at now: `current`, `MOVED` (the tag was re-pushed; the new digest is printed) or `ERROR` (no answer; never reported as current). `--newer-tags` adds the registry's tags that have the pinned tag's shape and sort after it. Exit 0 all current, 1 a pin is behind, 2 a pin could not be checked |
| `resolve <name>:<tag>...` | yes | Prints the `pin` line for a tag, and says so when its index lacks `linux/amd64` or `linux/arm64` |

`outdated` and `resolve` read public registries anonymously: no credential is
read from the machine or sent (the anonymous pull token a registry hands out is
the only `Authorization` header there is). A private or rate-limited registry
answers `ERROR`, not `current`.

**Nothing moves a pin by itself.** There is no bot: Dependabot opens no pull
requests here, because they run without the repository's secrets and could
never pass CI. Run `scripts/third-party-images.sh outdated --newer-tags` in the
same pass that bumps the Bazel, Go and Actions pins, and whenever an image's
advisory says a tag was rebuilt.

To move a pin (or add an image):

```bash
scripts/third-party-images.sh resolve curlimages/curl:8.23.0   # prints: pin curlimages/curl 8.23.0 sha256:…
scripts/third-party-images.sh list                             # where the old pin is used
# Replace the pin line in scripts/third-party-images.txt, then the old
# <tag>@<digest> in every file `list` printed.
scripts/third-party-images.sh check
bazel test //scripts:third_party_images_test //charts/...
```

What a moved pin obliges:

- A pin used under `charts/<chart>/` is a change to that chart: bump its
  `Chart.yaml` and say what rolls. `charts/aether`'s
  `proxy.authzSidecar.opa.image` is a container of the proxy pod, so moving it
  rolls the proxy DaemonSet on every node where the OPA preset is on;
  `charts/udsecho`'s `client.image` and `charts/prober`'s `authzCanary.image`
  each roll one single-replica Deployment.
- The two etcd pins (`quay.io/coreos/etcd` for the kind harnesses,
  `gcr.io/etcd-development/etcd` for the testcontainers tests) move together,
  and stay on the minor line of the Go client in `go.mod` (#1223).
- `kindest/node` moves only with `KIND_VERSION` (*Bumping the e2e Kubernetes
  version* above); `//e2e:kind_pin_test` holds its copies together.

An exception is an `allow <path> <reference>` or `skip <path prefix>` line in
the list, with its reason beside it. An `allow` or `skip` that matches nothing any more
fails `check`.

What `check` cannot see: an image no pin names yet, written where no `image`
key, `--image` flag or `*_IMAGE` variable introduces it (a positional
`docker run <image>`). Give such a reference a `*_IMAGE` variable, as
`e2e/etcd-image.sh` does. It also reads only `charts/`, `e2e/`, `test/` and
`registry/etcdtest/`; the images the Bazel image rules pull (`MODULE.bazel`)
are pinned there by digest and are not in the list.

---

## 7. Installing on a real cluster

Aether ships **two** charts, and **install order matters**:

```
charts/crds     — the CRDs: MeshConfig + HTTPFilter + EdgeConfig + EndpointPolicy   ← install FIRST
charts/aether    — the whole system (agent DaemonSet + proxy + mesh-dns + registrar + controller)
```

Install the CRDs before the system chart. **The agent crashes if it starts before
the `HTTPFilter` CRD exists** (it watches that type at startup); installing the
`crds` chart first avoids the race (see #453).

```bash
# The commit you intend to deploy (the `publish` run that built it), plus each
# chart's X.Y.Z from its charts/<name>/Chart.yaml at that commit.
COMMIT=<full 40-char git sha>
CRDS_VERSION=<X.Y.Z>-$COMMIT
AETHER_VERSION=<X.Y.Z>-$COMMIT

# 1) CRDs first
helm upgrade --install aether-crds oci://quay.io/aethermesh/chart-crds \
  --version "$CRDS_VERSION"

# 2) then the system. Prefer this commit-pinned chart tag over the bare
#    `--version <X.Y.Z>`: the bare tag is mutable and re-pushed by every publish,
#    the commit tag never is (#692).
#    The chart does not create the namespace (namespace.create=false, the
#    default since 2.4.21, #1403): --create-namespace does. On a FIRST install
#    into a cluster that enforces Pod Security admission, label the namespace
#    before this command, or the agent, proxy, mesh-dns and uds-csi pods are
#    refused:
#      kubectl create namespace aether-system
#      kubectl label namespace aether-system \
#        pod-security.kubernetes.io/enforce=privileged \
#        pod-security.kubernetes.io/audit=privileged \
#        pod-security.kubernetes.io/warn=privileged
#    (see getting-started.md, "Install", and "Recovering from a failed first
#    install" below).
helm upgrade --install aether oci://quay.io/aethermesh/chart-aether \
  --version "$AETHER_VERSION" -n aether-system --create-namespace \
  --set clusterName=my-cluster --set meshDomain=aether.internal

# 3) ALWAYS assert what actually landed. The chart's appVersion is the commit
#    that built it, so this is the only check that catches a chart tag serving
#    a different build than you asked for — which happened on 2026-09-05, when
#    two racing publishes wrote the same bare tag and the older build won,
#    silently deploying a release without #689 in it (#692).
got="$(helm get metadata -n aether-system aether -o json | jq -r .appVersion)"
case "$got" in
  *"$COMMIT"*) echo "deployed $got — OK" ;;
  *) echo "DEPLOY MISMATCH: appVersion=$got, expected $COMMIT" >&2; exit 1 ;;
esac
```

`appVersion` is the chart's `{STABLE_GIT_VERSION}` — `git describe --tags --always
--long --dirty --abbrev=40` — so the full sha is embedded in it but may carry a
`v1.2.3-N-g` prefix or a `-dirty` suffix; hence the substring match rather than
string equality. If it fails, the chart tag you pulled was built from a different
commit: re-run that commit's `publish` workflow and upgrade again.

**After a proxy release, normally there is nothing to do.** `proxy-release`
publishes the image, pins `charts/aether/values.yaml` (`tag:` *and* `digest:`),
pushes `chore/proxy-image-bump-<short sha>`, opens the pin PR with the
`RELEASE_PAT` fine-grained token and arms `--auto --squash` (#727). GitHub merges
it the moment `ci` + `proxy` are green — the `main` ruleset is still the only
gate, and it has no bypass actors. Any superseded `chore/proxy-image-bump-*` PR
is closed and its branch deleted by the same run; `main-post-merge` updates the
pin PR's branch whenever another merge leaves it `BEHIND` (auto-merge does not do
that by itself). Watch the rolling **proxy releases pending chart pin** issue
(#724): every release comments there with the PR, the digest, the chart version,
the run link and the token's remaining life. The **deploy stays manual** — after
the pin merges, take that commit's `publish` run and use the commit-suffixed
chart tag (`<X.Y.Z>-<full sha>`, above) with the `appVersion` assert.

**Manual fallback.** When #724 says the token is missing, expired or rejected,
the run degrades to #703's hand-off instead of failing: the branch is pushed with
`GITHUB_TOKEN` and both the run summary and the #724 comment carry the exact
`gh pr create` one-liner (its `| release token |` row says which of the three it
was). Run it from a checkout, let `ci` + `proxy` go green, squash-merge, close
any superseded `chore/proxy-image-bump-*` PR. A PR opened by `GITHUB_TOKEN`
itself is never an option: GitHub's recursion guard means it fires no
`pull_request` events, so the required checks would never report.

**Rotating `RELEASE_PAT`.** The workflow warns 30 days ahead — a `::warning::` on
the release run and the `| release token |` row on #724 — and, once expired,
falls back to the manual hand-off. To rotate: Settings → Developer settings →
Fine-grained tokens → new token, resource owner `bpalermo`, **only** the
`bpalermo/aether` repository, repository permissions **Contents: Read and write**
+ **Pull requests: Read and write** and nothing else (no Workflows, no
Administration, no Actions; `Metadata: Read` is implied), maximum expiration.
Then Settings → Environments → `release` → replace the **environment** secret
`RELEASE_PAT` with it. It must stay an environment secret of `release`, whose
deployment-branch policy names `main` only: that is what keeps a `pull_request`
run, or a run of a workflow edited on a branch, from ever reading it. The next
`proxy-release` run reports the new expiry on #724.

From a checkout, the Bazel install targets do the same in order:

```bash
bazel run //charts/crds:crds.install
bazel run //charts/aether:aether.install
```

> Always pass the **full** values on every `helm upgrade` of the `aether` chart —
> do **not** use `--reuse-values` (it keeps the stale digest-pinned image). Bump
> the chart's `version:` on any change to its templates/values (CI enforces this).

### Pre-flight: did that commit actually publish? (#880)

Before pinning a deploy to a commit, check that the commit has artifacts:

```bash
make check-published COMMIT=<any commit-ish>   # one commit
make check-published                           # the last day of main
```

Read-only, no credentials needed (the packages are public), and it cannot push
anything. It prints every coordinate it checked — four commit-tagged charts,
nine images (eight for a commit before uds-csi), a cosign signature for each image's index **and for every child
manifest it lists** (40 coordinates today), plus the aether-proxy digest the commit's chart pins (see "Verifying the aether-proxy signature") — and exits non-zero naming each one
that is missing. It checks that a signature *exists*; to check that it
*verifies*, see "Verifying image signatures" below.

**Do not use `gh run list` for this.** A commit whose publish was superseded has
a run whose conclusion is `cancelled`, not `failure`: green-ish in the Actions
UI, nothing pushed. `.github/workflows/publish.yaml` serialises on one
concurrency group (#692), and GitHub keeps at most **one** pending run per
group, so a merge landing while a publish runs is cancelled by the *next* merge
before it starts a job. f332061 (#875) was lost that way on 2026-09-20. Two more
traps: `gh run list --commit=<sha>` matches only the **full 40 characters** and
answers an abbreviation with an empty list — which reads exactly like "no
publish ever ran" — and a run that *did* exit 0 still tells you nothing about
what reached the registry. Only the registry answers that question.

**Only push heads are published (#975).** `publish` runs once per push to
`main`, for the push's head commit. An atomic stack merge lands several squash
commits in one push, and only the last is ever built: the others have no
artifacts by construction and cannot be deployed — pin the stack's head instead.
`make check-published COMMIT=<intermediate>` correctly reports them MISSING;
the `--recent` sweep skips them, printing `skip <sha> (not a push head …)`. It
learns the heads from GitHub's activity log for `refs/heads/main`, so it needs
`gh` authenticated (or `PUSH_HEADS_FILE=<file of full shas>`); a push whose
publish run never started is still a head and still fails.

**Superseded push heads are skipped (#1282).** A push head whose publish run
did not succeed (`cancelled` by the next merge, or `failure`) on a `main` that
has since moved past it was superseded: the newer push's publish built a tree
that contains it. Both the `workflow_run` path (#1277) and the `--recent` sweep
skip it with a `::notice::` naming the commit — `skip <sha> (superseded push
head …)` in the log — instead of reporting its commit-addressed tags missing.
`main`'s own head, a head whose publish succeeded and a head with no publish
run on record are always checked. The rule is
`scripts/publish-verify-superseded.sh decide`; the sweep reads the publish runs
with `gh` (`PUBLISH_RUNS_FILE=<file of "<sha> <conclusion>">` overrides it) and
`main`'s head with `git ls-remote` (`MAIN_HEAD` overrides it). Do not deploy a
superseded commit: pin the head that superseded it.

`publish-verify` runs the same check automatically after every publish run
reaches a conclusion and every two hours over the push heads of `main` that the
last green scheduled sweep did not already verify — at most the last day,
the whole day when no sweep has been green within it (#1281;
`RECENT_SINCE_LAST_GREEN=1`, `sweep_since` in `scripts/push-heads-lib.sh`) — and
files (or comments on) the rolling **publish: artifacts missing for a commit on
main** issue. A sweep that times out or is cancelled files on the same issue,
saying the check did not finish: before #1281 three timed-out sweeps in a row
concluded `cancelled` and reported nothing. If you see that issue: re-run the cancelled publish run — `gh run rerun
<id>`, which re-runs at that same commit — or, if the commit is not the one you
need, deploy a later commit that did publish. Never push images or charts by
hand: the release workflow is the only publisher.

**The issue closes itself (#1316).** A green run for `main`'s head, or a green
scheduled sweep, closes it with one comment naming the run
(`scripts/publish-verify-close-issue.sh`). A green run for a commit `main` has
moved past, a superseded skip and a manual run leave it open. So an open issue
means no run has verified the head since the failure; a closed one names the
run that did.

**A registry hiccup is retried, not reported (#1316).** Run 37522284996 died in
the signature pass on one 502 from quay.io's token endpoint, with a Python
traceback, and filed the issue as UNVERIFIED for artifacts that were fine. The
pull-token and child-manifest lookups now retry a missing answer, a 408, 429 or
5xx and a 200 that is not JSON (four attempts, 2 s doubling;
`REGISTRY_FETCH_ATTEMPTS`, `REGISTRY_FETCH_INTERVAL`), and a failed
`cosign verify` is re-run (three attempts; `VERIFY_ATTEMPTS`, at most
`VERIFY_RETRY_BUDGET` re-runs per invocation) unless cosign fetched a signature
whose identity is another's. A lookup that still fails prints one
`registry-lib: GET <url> answered HTTP <status>; giving up after 4 attempt(s)`
line (or how the body starts) in place of the traceback; a 401, 403 or 404 is
the registry's answer and is not retried. The tag listing (`registry_all_tags`)
retries each page the same way (#1337) and prints nothing if a page cannot be
read. `publish.yaml`'s sign step lists through `registry_commit_tag`, whose 13
listings 5 s apart are already a retry, so there each page is asked once
(`REGISTRY_COMMIT_TAG_FETCH_ATTEMPTS`): 60 s of waiting at worst per
repository, not the 242 s of the two retries nested.

**It asks for each tag by name (#985).** Every coordinate is checked with
`HEAD /v2/<repo>/manifests/<tag>`. No tag list is read. The old scan paged
through every tag, and a sweep that overlapped a publish reported a present
signature `MISSING` (d526bf2, 2026-09-27); a re-run minutes later passed. 200
means present, and 404 means `MISSING`, but only beside a witness: a tag that
the same repository lists answered 200 to the same lookup. Any other answer, or
a repository with no witness, is exit 2 (inconclusive), never `MISSING`. A
`MISSING` line from this check is a real absence, so don't re-run it hoping it
goes away.

**Every run proves the gate can fail first (#930).** (The control covers the per-commit coordinates only: it runs the verifier with `PROXY_PIN_CHECK=0`, because the constructed commit pins main's signed aether-proxy digest, which is legitimately present; the proxy pin's own red is case 7 of `scripts/check-publish-verify-control.sh`.) Before the gate step,
`publish-verify` runs an *expected-red control*: `scripts/publish-verify-control.sh`
builds a commit with `git commit-tree` on `origin/main`'s tree (fixed identity
and dates, so the same base always gives the same sha). No ref points at that
commit and it is never pushed, so no publish can ever have produced it. The
control then asserts that the verifier goes red for the right reason: exit
exactly 1, 20 `MISSING` lines that all name that sha, no `ok` line, and every
chart and image absence backed by a witness (`witness <tag>: 200`: a tag the
same repository lists, answering 200 to the same lookup). A constructed commit, rather than a pinned one
like `ef44437` or `1e31e3a`, because a pinned red input ages out of the window
it is looked up through. That is how the gate lost its only demonstrated red
state (#929). The two steps read **Expected-red control** and **Gate** in the
log. A control's `MISSING` lines are expected and never reach `verify.log` or
the job summary. If the control fails, the gate still runs, and the run files
(or comments on) a rolling issue whose title says which failure it was (#1340,
`scripts/publish-verify-control-issue.sh`): **publish-verify: the expected-red
control went GREEN** (the verifier passed a never-published commit: the gate is
vacuous, a real defect), **… went red for the wrong reason** (part of the gate
cannot fail), or **… was inconclusive** (exit 2: the registry could not be
read; no verdict on the gate either way). The body quotes what the control
said. An inconclusive verifier is run again first, three runs in all, 10 s then
20 s apart (`CONTROL_ATTEMPTS`, `CONTROL_RETRY_INTERVAL`), so one bad answer
from the registry files nothing; a verifier that exited 0 or 1 is never asked
twice. Any later run whose control goes red as expected closes every open
control issue with a comment naming the run
(`scripts/publish-verify-close-issue.sh`). While a GREEN or wrong-reason issue
is open, a green gate proves nothing. An inconclusive control fails the run too.
Reproduce with `scripts/publish-verify-control.sh [<base>]`. The offline check,
`scripts/check-publish-verify-control.sh`, runs in `ci`'s `shell` job. It drives
the real verifier against a fake registry and shows the control rejects a
verifier that stopped counting `MISSING`, an absence with no witness, a `MISSING` line
naming another commit, a stray `ok`, and a red reported against the wrong
registry (every `MISSING` line must name the registry the control's tree names —
proposal 040). Its fake registry, like `scripts/check-registry-lookup.sh`'s, accepts only the
bearer token it issued and answers any other with a 401, so a verifier that
sends the wrong value as the token goes red offline (#999).

### Where images and charts are published: one setting (proposal 040)

Every published coordinate derives from **`bazel/registry/registry.bzl`** — registry
host, namespace, per-component name overrides and the chart-repository prefix —
and from nowhere else:

| reader | how |
|---|---|
| image pushes, chart pushes, chart template tests, e2e go_test defaults | `load("@aether_registry//:registry.bzl", ...)` — `image_repository()`, `image_reference()`, `chart_registry_url(<chart>)` |
| the `//proxy` workspace (`oci_push`) | the same file, `load("@aether_registry//:registry.bzl", ...)`: `bazel/registry/` is a local module each workspace reaches with `local_path_override` (`../bazel/registry` from `proxy/`), since `//proxy` cannot load from the root module |
| `//bazel/proxy_pin` (the Envoy the validate gate runs) | `image_reference("proxy")` + `registry_token_url()` |
| workflows | `scripts/image-registry.sh >> "$GITHUB_ENV"` after checkout → `IMAGE_REGISTRY_HOST`, `IMAGE_NAMESPACE`, `IMAGE_REGISTRY` (host/namespace), `PROXY_IMAGE`, `IMAGE_SIGNATURE_LAYOUT` |
| verifiers, e2e scripts | `scripts/image-registry.sh {prefix,host,repo <c>,ref <c>,chart-repo <c>,chart-ref <c>,signature-layout}`; `scripts/registry-lib.sh` resolves its repository lists through it |

It says **`quay.io` / `aethermesh`** since the phase-2 cut-over (proposal 040):
images `quay.io/aethermesh/<component>` (the proxy is plain `proxy`), charts
`quay.io/aethermesh/chart-<chart>`, signatures as OCI 1.1 referrers
(`SIGNATURE_LAYOUT = "referrer"`). Before it: `ghcr.io/bpalermo/aether/<component>`,
the proxy `…/aether-proxy`, charts `…/charts/<chart>`, signatures as cosign tags.
The cut-over commit is the phase-2 PR's merge commit — the first whose
`registry.bzl` says quay.io.

**Charts are pushed with oras, not `helm push`.** `helm push` appends the
chart's `name:` to its base and cannot name a flat `chart-<name>` repository;
Quay has no nested repositories (and bare `prober` / `udsecho` would collide
with those images). `chart_push` (`//bazel/helm:defs.bzl`) writes exactly the
artifact `helm push` writes — the packaged `.tgz` as the
`application/vnd.cncf.helm.chart.content.v1.tar+gzip` layer, `Chart.yaml` as the
`application/vnd.cncf.helm.config.v1+json` config (`//bazel/chartconfig`) — with
the pinned oras (`//bazel/oras`, v1.3.4, `version_test`) to
`chart_registry_url(<chart>)`, tagged with the packaged version. publish.yaml
runs `<chart>.push_images`, then `<chart>.chart_push` (and
`aether_commit.chart_push` for the commit-suffixed tag). `helm pull
oci://quay.io/aethermesh/chart-aether --version <v>` reads it like any
helm-pushed chart (proved against a local registry: same layer digest, same
config, `helm template` renders).

**Credentials.** Every job that pushes or signs (`publish.yaml`'s `publish`,
`proxy-release.yml`'s `build-push`, `manifest`, `sign`) declares
`environment: release` and logs in to `$IMAGE_REGISTRY_HOST` with the Quay robot
(`QUAY_USERNAME` / `QUAY_TOKEN`, secrets of the `release` environment only;
deployment-branch policy `main`). `GITHUB_TOKEN` is no longer a registry
credential; it stays for GitHub API calls (PRs, issues). The repositories are
pre-created public in the org — the robot **cannot create repositories** (the
quay-smoke gate), so a new component needs its repository created (public, robot
write) before its first publish.

**The sweep across the cut-over (the split rule).** `verify-published-artifacts.sh`
reads the registry setting **as of each push head it checks** (`git show
<sha>:bazel/registry/registry.bzl`, or `bazel/img/registry.bzl` for a head from
before the setting became its own module — `REGISTRY_SETTING_PATHS` in
`scripts/registry-lib.sh`), exactly as it reads each chart's version and the
release-tag prefix: heads at or after the cut-over are checked on quay.io
(`chart-<name>`, flat names, a signature REFERRER — and nothing else: a fallback
tag there, or a referrer plus a tag, is `MISSING`). Heads from **before** the
cut-over — a `registry.bzl` without `SIGNATURE_LAYOUT`, or none at all (before
#998) — are exit 2 and never read: phase 4 decommissioned the sweep's ghcr.io
branch (the first-version fallback, the `tag` default and `GHCR_TOKEN` are gone),
so naming such a commit by hand gets "decommissioned (proposal 040 phase 4)",
not a verdict. A post-cut-over head whose artifacts exist only on ghcr.io is
`MISSING`, never borrowed from the old registry. Each `commit` block of the output starts with a
`registry <host>/<namespace>, signatures as <layout>` line saying which setting
it used. The aether-proxy pin is looked up where **the pin** says (it moves with
the next proxy release, not with the flip), in the layout of the newest
`registry.bzl` under which that reference was `image_reference("proxy")`.

**The aether-proxy pin during the cut-over.** The flip could not move the pin
(it is data only `proxy-release.yml`'s bump-chart job writes), so right after the
cut-over `charts/aether/values.yaml` still named the ghcr.io image, and every pin
reader briefly accepted it too (`PROXY_PIN_LEGACY_REFERENCES`). The phase-2
merge triggered a proxy release whose bump-chart PR (#1027) re-pinned to
`quay.io/aethermesh/proxy`. Phase 4 removed the allowance: every pin reader
(`//bazel/proxy_pin`, the sweep, ci.yaml's proxy-pin job, `proxy_pin_rewrite`
in `scripts/proxy-pin-lib.sh`) accepts exactly `image_reference("proxy")`, and a
pin on any other reference is a hard failure — never a lookup on the old
registry.

**Migrating a chart consumer.** Every chart took a **major** bump (aether
`1.0.0`, and crds / prober / udsecho to `1.0.0`) because the default image
repositories moved. Upgrade from the new coordinates with the values you run
today — `helm get values <release> -n <ns> -o yaml > values.yaml`, then
`helm upgrade <release> oci://quay.io/aethermesh/chart-<name> --version <v> -f
values.yaml`; **never `--reuse-values`**, which pins the old chart's defaults.
Anyone mirroring images and overriding `repository` by prefix must now mirror
from `quay.io/aethermesh/<component>` and override each image's `repository`
individually. Releases published before the cut-over went to ghcr.io and were not
copied to quay.io.

**ghcr decommissioned (proposal 040 phase 4, #1167).** Nothing in the repository
reads, verifies or publishes to ghcr.io any more: the `ghcr-lib.sh` shim and the
`ghcr_*` aliases are gone, `PROXY_PIN_LEGACY_REFERENCES` is gone (the proxy pin
must name `image_reference("proxy")`), and the sweep refuses pre-cut-over heads
(above) with no `GHCR_TOKEN`. The ghcr.io packages themselves were **not**
touched: whether to delete them or keep them read-only as an archive is a
separate maintainer decision (deleting them breaks every historical pin).

`scripts/check-registry-config.sh` (in `ci`'s and `proxy`'s `shell` jobs)
keeps it one setting: the file parses, the proxy copy is identical, the
aether-proxy pin in `charts/aether/values.yaml` names exactly
`image_reference("proxy")`, no file
outside its written-down allow-list (docs, the website, the two READMEs, the
setting itself, the pin) spells the current registry out, and no file outside
its **legacy** allow-list (the setting's history note, proposals, this
runbook's records, observability notes) spells a pre-cut-over coordinate out.
`//bazel/img:registry_test` pins `image-registry.sh` against the Starlark helpers.

**Registry library.** `scripts/registry-lib.sh` (was `ghcr-lib.sh`; the shim and
the `ghcr_*` aliases were removed in proposal 040 phase 4) is registry-neutral: it speaks the
OCI distribution API against `REGISTRY_HOST` (default: the setting's host),
fetches the anonymous pull token from the registry's own endpoint (ghcr.io
`/token`, quay.io `/v2/auth`), and for private repositories takes
`REGISTRY_USERNAME` + `REGISTRY_PASSWORD` (a Quay robot account, sent only to
the host it was issued for). `registry_referrers` reads the OCI 1.1 Referrers API
(`GET /v2/<repo>/referrers/<digest>`): quay.io serves it, ghcr.io answers 404 —
which the library reads as "no referrers API", never as "no signatures".

**Quay smoke (the phase-2 gate).** Proves push + keyless sign + verify on
quay.io with one throwaway image. It gated the flip (green: run 36345331611 — the
robot pushes to a pre-created repository but cannot create one; cosign v3.1.2
writes the signature as a referrer, no tag, on the index and every child;
`verify_image_signatures` passes), and stays dispatchable for re-checking quay
after a cosign or robot change:

```bash
gh workflow run quay-smoke.yaml --ref main    # main only: the `release` environment holds the robot secrets
gh run watch "$(gh run list --workflow quay-smoke.yaml --limit 1 --json databaseId -q '.[0].databaseId')"
```

`.github/workflows/quay-smoke.yaml` (dispatch-only, every step is
`scripts/quay-smoke.sh <subcommand>`) pushes `//e2e/l4echo:smoke_push` — the
l4echo test image's rules_img amd64+arm64 index, built and pushed by the same
rules_img path as the released images — to `quay.io/aethermesh/smoke:<run id>-<attempt>`
with the robot account, signs it `cosign sign --recursive` by digest as
`…/quay-smoke.yaml@refs/heads/main`, verifies the index and every child with
`//bazel/cosign:verify_image_signatures`, then deletes the tag (and any cosign
fallback tags) and leaves the repository. The run summary is a table:

| row | meaning |
|---|---|
| repository existed before / created by this push | robot `tags/list` before the push. `no` then a successful push = the robot **can create** repositories in the org (open question "Repository auto-creation") |
| push | `denied` fails the step: pre-create the repository public and give the robot write, or give the robot the org **creator** role |
| public (anonymous pull) | Quay API `is_public` (anonymous) AND an anonymous pull of the tag. Anything but `yes` **fails the job**: make it public (repository settings), or set the org's default visibility before the robot creates repositories |
| digest | the index digest the registry holds under the tag (cross-checked against what Bazel pushed) |
| signed before this run | the digest is deterministic per commit, so a re-run at the same commit finds the earlier run's signature; the layout rows then include it |
| signature layout (index) / (index + children) | `referrer`, `bundle` (`sha256-<hex>` fallback tag), `legacy` (`.sig`), or `both` (a referrer AND a tag — the sweep's double-write defect), from `registry_signature_layout_direct`. `mixed` in the second row = the children disagree with the index. This answers where cosign v3.1.2 puts a signature on quay and decides phase 2's verify path |
| referrer artifactTypes | what the referrers API lists for the index (`application/vnd.dev.sigstore.bundle.v0.3+json` expected) |
| verify (index + every child) | `verify_image_signatures` with identity `^https://github\.com/<repo>/\.github/workflows/quay-smoke\.yaml@refs/heads/main$` |
| certificate SAN | the X.509 SAN URI of the signing certificate — the workflow ref, NOT the OIDC `sub` — read from the sigstore bundle `cosign download signature` returns (`verificationMaterial.certificate.rawBytes`, or the chain's leaf), decoded with openssl. cosign v3's `verify` JSON has no `optional` block, so there is nothing to read there. A SAN that cannot be read, or that does not match the verify identity, **fails the summary** |
| OIDC token | the Actions token's `sub` (with `environment: release`: `repo:<owner>/<repo>:environment:release`) and `job_workflow_ref`; the token itself is masked and never printed |
| cleanup | tags deleted with a robot `pull,push` token (`DELETE /v2/<repo>/manifests/<tag>`); a failure is a warning, not a red |

### Verifying image signatures (cosign v3, #925)

`publish.yaml` signs every published image keyless with `cosign sign
--recursive` — the multi-arch **index and each per-architecture child
manifest**.

**Each digest is signed and attested once (#1378).** A digest can be published
by more than one run: a re-run of the workflow, or a later commit that did not
change that image. `scripts/publish-sign.sh` asks first whether the index and
every child already verify under this workflow's identity (the registry's
referrers index, then the same `verify_image_signatures` the verify step runs)
and signs only what does not; a question that goes unanswered is answered by
signing. `scripts/publish-provenance.sh subjects` asks GitHub's attestation
store whether the digest has provenance that verifies as this workflow and
hands `actions/attest` only the digests that have none; there an unanswered
lookup **fails the step**, and when nothing is new the attest step is skipped
with a `::notice::`. Nothing is skipped in the checks: every reference, signed
or attested now or before, goes through both verify steps. The run summary
lists every artifact with its digest, whether the digest is new, the commit its
provenance names and the tag: that is the list of what a deploy of that commit
rolls. `//scripts:publish_sign_test` and `//scripts:publish_provenance_test`
hold the logic and the workflow's wiring.

The user-facing commands (signature and provenance, images and charts) are in
[Verifying a release](verifying-releases.md); this section is how it is built.

**Charts are signed as well, and everything gets build provenance.** After the
images, `publish.yaml` signs each chart manifest (`cosign sign`, no
`--recursive`: a chart is one manifest). The references come from
`scripts/published-chart-refs.sh <sha>`: every chart of `REGISTRY_CHARTS` under
its `X.Y.Z-<sha>` tag, plus aether's bare `X.Y.Z` tag, each resolved to a digest
right after the push. The run verifies them with
`verify_image_signatures -- --single --file signed-charts.txt` (`--single`:
no child walk, and a ref that turns out to be an index is refused). Then
`actions/attest` writes ONE SLSA v1 provenance statement whose subjects are
the image indexes and chart manifests that had no provenance yet (every chart,
and each image whose digest is new), stored in the repository's
attestation store on GitHub (not in the registry: `push-to-registry` is off),
and the last step runs `gh attestation verify oci://<ref>` for every
reference with the workflow pinned: with `--source-digest <commit>` as well for
a digest this run attested, without it for one attested by an earlier commit
(its provenance names that commit, and the step prints it). A failure in any of
these fails the publish run after the artifacts are pushed, like a failed image
signature does: re-run the workflow, which signs and attests only what the
failed run left unsigned or unattested.
The post-publish sweep (`publish-verify`) does not look at chart signatures or
attestations yet. The proxy image (`proxy-release.yml`) is signed but not
attested yet.

**Getting cosign: `bazel run //bazel/cosign`.** There is one cosign for this
repository, in CI and on a workstation: the official release binary that the
`rules_img_signer_cosign` bazel_dep (`MODULE.bazel`) downloads for the current
platform, sha256-pinned in that module's `cli/cosign_cli.lock.json` —
**cosign v3.1.2** at module 0.0.1. `publish.yaml`, `proxy-release.yml` and
`publish-verify.yaml` all sign and verify through `//bazel/cosign`, run
locally on the runner (never on RBE), so the three cannot drift apart.

```bash
bazel run //bazel/cosign -- version            # GitVersion: v3.1.2
bazel run //bazel/cosign -- verify …           # any cosign subcommand
```

The version is the module's lock, so **bumping cosign is bumping the
`bazel_dep`**; `//bazel/cosign:version_test` (network-free) fails until its
`EXPECTED` moves too, so a new cosign never lands unread. Until 2026-09-27 this
was `sigstore/cosign-installer` pinned by SHA (v4.1.2 → cosign v3.0.6).

**Why only the module's CLI, not its signer plugin.** `rules_img_signer_cosign`
also ships `sign-oci-artifact`, a signer plugin for rules_img's
`signing_config` / `img deploy`. Signing stays an explicit `cosign sign
--recursive` step after the pushes, by digest, with the same cosign every
verifier runs: that path is the one the quay-smoke gate measured on quay.io
(referrer, no tag), and the plugin's layout there is unmeasured.

**Layout.** quay.io serves the OCI 1.1 Referrers API, so cosign 3 attaches the
sigstore bundle as a real **referrer** of the signed manifest (artifactType
`application/vnd.dev.sigstore.bundle.v0.3+json`, annotation
`dev.sigstore.bundle.predicateType: https://sigstore.dev/cosign/sign/v1`) and
writes no tag — measured by the quay-smoke gate on our own index and children,
and observed on `quay.io/argoproj/argocd` and `quay.io/cilium/cilium`.
`bazel/registry/registry.bzl` records that as `SIGNATURE_LAYOUT = "referrer"`, and
the sweep holds every post-cut-over commit to it: a fallback tag on quay.io, or a
referrer plus a tag, is `MISSING`. Before the cut-over, on ghcr.io (no Referrers
API), the signature landed in a tag in the image's own repository: cosign 3's
OCI 1.1 fallback index `sha256-<hex>` (the bundle format — keyless v3 *requires*
`--new-bundle-format`; `=false` is rejected), and before 2026-09-24 cosign 2's
`sha256-<hex>.sig`; a `registry.bzl` without `SIGNATURE_LAYOUT` promises that
`tag` layout. Exactly one of the three layouts may exist per digest
(`scripts/registry-lib.sh`, `registry_signature_layout`; a bundle referrer with
any other predicateType is an attestation, not a signature). `cosign verify`
finds all three itself.
Do not pass `--new-bundle-format` to `cosign verify` expecting it to assert the
layout: `=true` still accepts a legacy `.sig`.

**Children are not verified unless you walk them.** `cosign verify` has **no
`--recursive`** (not v2.4.1, v3.0.6 or v3.1.2). Verifying the index digest says
nothing about the per-arch manifests a node actually pulls. Use the script,
which walks the index's `.manifests[]` from the registry and verifies every
child with the same identity and issuer:

```bash
# Read-only; no credentials. The target sets COSIGN to the pinned cosign.
bazel run //bazel/cosign:verify_image_signatures -- quay.io/aethermesh/agent@sha256:<index digest>
#   verified index quay.io/aethermesh/agent@sha256:…
#   verified child quay.io/aethermesh/agent@sha256:…   (linux/amd64)
#   verified child quay.io/aethermesh/agent@sha256:…   (linux/arm64)
```

Identity is `^https://github\.com/bpalermo/aether/\.github/workflows/publish\.yaml@`,
issuer `https://token.actions.githubusercontent.com`. Exit 1 names every index
or child that did not verify; exit 2 means it could not check (e.g. the digest
is not an index, or lists no children — a walk over nothing is never a pass).
The same target runs in `publish.yaml`'s verify step and in `publish-verify`.
`--file <refs>` takes one ref per line; relative paths resolve against your
working directory. Standalone, `COSIGN=/path/to/cosign
scripts/verify-image-signatures.sh …` still works with any cosign — the version
is then yours to vouch for.

cosign v3.1.2 verifies every layout: a pre-2026-09-24 legacy `.sig`
(`agent@sha256:14949387…`, commit 5b0c199) and a v3 fallback-tag bundle
(`agent@sha256:066267b6…`, commit 7f8f825) on ghcr.io both pass, index and
children, and so does a referrer on quay.io (the quay-smoke gate). Images
published before the cut-over are verified at their ghcr.io coordinates.

Not signed by this pipeline: any `agent`-family image published before f332061
(#875, 2026-09-20) — those verify as `no signatures found`. The proxy is signed
by its own workflow; see the next section.

### Verifying the aether-proxy signature (#984)

The proxy image (`quay.io/aethermesh/proxy` since the cut-over; before it, and
in the chart until the first proxy release after it re-pins,
`ghcr.io/bpalermo/aether/aether-proxy`) is built by `proxy-release.yml`, not
`publish.yaml`, and is versioned by the commit that changed `proxy/`, not by the
aether commit — the chart carries its **digest** in
`charts/aether/values.yaml` (`proxy.image.digest`). Since #984 that workflow's
`sign` job runs `cosign sign --recursive` on the index right after the manifest
job publishes it (same Bazel-pinned `//bazel/cosign`, keyless, v3 bundle
layout), verifies the index and every child, and only then lets `bump-chart`
open the pin PR. A proxy index that does not verify is never pinned.

**The identity is different.** Fulcio binds the certificate to the workflow
that signed, so proxy signatures carry

```
https://github.com/bpalermo/aether/.github/workflows/proxy-release.yml@refs/heads/main
```

(issuer `https://token.actions.githubusercontent.com`). The publish identity
rejects them, and vice versa. By hand, for whatever digest a chart pins:

```bash
# The pin names its own registry: read repository AND digest from the chart.
pinned="$(bash -c '. scripts/proxy-pin-lib.sh && proxy_pinned_ref' < charts/aether/values.yaml)"   # "<repository> <digest>"
CERT_IDENTITY_REGEXP='^https://github\.com/bpalermo/aether/\.github/workflows/proxy-release\.yml@refs/heads/main$' \
  bazel run //bazel/cosign:verify_image_signatures -- "${pinned% *}@${pinned#* }"
```

**Pins older than signing are unsigned, permanently.** Every proxy image
published before #984 has no signature (0 signature tags in the repository), and
that includes the digest pinned when #984 merged
(`sha256:938c5a57…`, children `bb53bed6…` amd64 / `4a90a7fb…` arm64). The
hand check above fails on it with `no signatures found` ×3 — that is the
expected answer, not a regression. The first signed digest is whatever the
proxy-release run triggered by #984's merge pins; the first one verified by hand
(2026-09-27, cosign v3.1.2 under the identity above) is `sha256:574d5211…`,
index plus both children (`4d7b967b…`, `1505e36b…`).

**The sweep.** `make check-published` / `publish-verify` check, for every
commit, the proxy digest that commit's `values.yaml` pins: it must exist, and
the index and every child must carry a signature (the cosign pass then verifies
them under the proxy identity above). Every pin is checked, so an unsigned proxy
pin goes red, and that includes a revert to an old unsigned digest. There used to be a
`PROXY_SIGNING_CUTOVER` that skipped the unsigned pre-signing pins as history.
#1191 removed it as unreachable: every one of those pins named the pre-cut-over
registry, which the pin reader refuses, and the sweep refuses any commit that
predates the Quay cut-over (proposal 040 phase 4). Each digest ever pinned under
`image_reference("proxy")` was introduced after the signing cut-over.

### Recovering from a failed first install (#1406)

Every command below was run on kind (Kubernetes 1.35, Helm 3.18.4) with the API
server enforcing Pod Security `baseline` by default. Start by looking at what
is there:

```bash
helm list -n aether-system -a                      # the release and its status
kubectl get namespace aether-system --show-labels  # exists? labelled privileged?
kubectl -n aether-system get ds,deploy
kubectl -n aether-system get events --field-selector reason=FailedCreate
```

**`namespaces "aether-system" already exists`** — a chart older than 2.4.21
installed with `--create-namespace` (the chart rendered the namespace Helm had
just created). What it leaves: a release in status `failed`, a namespace without
the pod-security labels, and everything else the chart renders. The Deployments
run; where Pod Security is enforced the four DaemonSets have no pods (`FailedCreate`:
`violates PodSecurity`). Start again from nothing, with chart 2.4.21 or newer:

```bash
helm uninstall aether -n aether-system     # prints: kept ... [MeshConfig] default
kubectl delete namespace aether-system     # takes the kept MeshConfig with it
# then the install from getting-started.md, "Install": create and label the
# namespace, and `helm upgrade --install ... --create-namespace`
```

`helm install` over the failed release answers `cannot re-use a name that is
still in use`, which is why the uninstall comes first. Nothing cluster-scoped
is left behind by the uninstall.

To keep the namespace instead (say, something else already lives in it), do not
simply run the new chart over the failed release: that release's manifest lists
the namespace, the new chart's does not, and Helm deletes an object that leaves
the manifest. Chart 2.4.21 refuses that upgrade and names the fix, which is to
mark the namespace first:

```bash
kubectl annotate namespace aether-system helm.sh/resource-policy=keep
kubectl label namespace aether-system --overwrite \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/audit=privileged \
  pod-security.kubernetes.io/warn=privileged
# then the same `helm upgrade --install ... --create-namespace` again
```

**`namespaces "aether-system" not found`** or **`invalid ownership
metadata`** — a chart older than 2.4.21 installed without `--create-namespace`,
or into a namespace made with a plain `kubectl create namespace`. Nothing was
created and no release was stored. Run the documented install with chart 2.4.21
or newer; an existing namespace can stay.

**The release is `deployed` but the DaemonSets have no pods** — chart 2.4.21 or
newer on a cluster that enforces Pod Security admission, namespace not
labelled. The install notes print a `NAMESPACE NOTE`, and the events say
`violates PodSecurity "baseline:latest"`. Label the namespace; nothing has to be
reinstalled (`--overwrite` because the namespace may already carry another
level):

```bash
kubectl label namespace aether-system --overwrite \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/audit=privileged \
  pod-security.kubernetes.io/warn=privileged
kubectl -n aether-system rollout restart daemonset
kubectl -n aether-system get ds   # CURRENT reaches DESIRED
```

The `rollout restart` is there because the DaemonSet controller retries a
refused pod at a growing interval and a label on the namespace does not wake
it. On kind the pods appeared within 5 s of the label when it came half a minute
after the install; when it came four minutes after, they had not appeared 20 s
later and did at once after the restart.

**`namespace.create=true cannot create "aether-system"`** — a render error;
nothing was created, not even the namespace `--create-namespace` would have
made. Drop `namespace.create=true` (and label the namespace yourself), or store
the release in another namespace (`helm -n <other> ... --set
namespace.name=aether-system`).

**A first install that failed for another reason** (a `--wait` that timed out)
is retried with the same command: `helm upgrade --install` upgrades the failed
release in place. If it failed very early, before the agent ServiceAccount was
written (one of that name already existed, say), the retry may ask once for
`kubectl annotate namespace aether-system helm.sh/resource-policy=keep`: without
the mark that ServiceAccount carries, the chart cannot see that the failed
revision rendered no namespace, and it refuses rather than risk a delete. The
command is safe to run; nothing is deleted either way.

### Which workloads a chart upgrade rolls (#1363)

Since chart **2.4.15** the chart itself no longer puts the release into a pod
template: a `helm upgrade` rolls a workload only when that workload's pod
template changed (an image digest, a flag, a resource, a volume).

**And an image changes only when what is in it changes (#1378).** Until #1378
every image this repository builds got a new digest with every commit, whether
or not its content changed: the image carried the commit as its
`org.opencontainers.image.revision` label and annotations (#837), and seven
binaries had the commit linked in as their version. On a test cluster all seven
in-repo image digests changed in each of four consecutive deploys, including one
whose commits touched only the registrar, tests and scripts; and since every
workload runs at least one of those images, a deploy of any new commit rolled
everything, the node proxy included.

Neither is true any more for the Go images this workspace builds (agent,
mesh-dns, proxy-supervisor, uds-csi, cni-install, registrar, controller,
prober): none of them and none of their binaries carries the commit, so the same
inputs build the same digest at any commit, the chart pins the same digest, and
the pod template does not change. CI holds the build to it, for those images:
the `test` job fails if any action under one of their image indexes takes a
workspace-status file, then builds every index twice, as two made-up commits,
and fails on a digest that differs (`scripts/check-image-digest-stability.sh`,
or `make check-image-digests` locally; it takes seconds once the images are
built).

The `aether-proxy` image is the exception, and stays one. It is built in the
separate `proxy/` workspace by its own release workflow, and it still carries
the aether commit twice: as `org.opencontainers.image.revision` (label and
annotation, `proxy/BUILD.bazel`, the `image_metadata` genrule) and inside the
Envoy binary, through Envoy's own version linkstamp (`proxy/README.md`, "Where
the Envoy revision actually is"). That costs no roll: the chart pins the proxy
image statically (`proxy.image` in `values.yaml`), so it changes only when the
pin is bumped. What a deploy of a new commit rolls:

| The commits changed | Rolls | Does not roll |
| --- | --- | --- |
| docs, tests, CI, scripts only | nothing | everything |
| the agent only (`agent/` code outside the other binaries) | the agent DaemonSet, the edge Deployment (it runs the agent image) and the controller (its identity-gate init container image defaults to the agent image, which is a flag in the controller's pod template) | the node proxy: **no Envoy hot restart**; mesh-dns, uds-csi, the registrar |
| one other component only (the registrar, mesh-dns, uds-csi, the controller, the proxy supervisor, cni-install) | the workloads that run that image: the proxy DaemonSet for the supervisor, the agent DaemonSet for cni-install (its init container) | the rest |
| the chart only (templates, values) | only the workloads whose pod template changed | the rest |
| a package under `common/` (or `api/`, or a dependency or toolchain pin) | every workload whose image links it, which for a widely used package is all of them | workloads whose images do not link it |

Two things follow from "a function of what is in it":

- "Did this deploy change component X" has an exact answer before the deploy:
  compare the digest the new chart pins for X with the one that is running.
  `bazel query 'rdeps(//..., //common/foo)'` restricted to the image targets
  says which images a source change reaches.
- A proxy roll now means the supervisor image (or the proxy image, a static pin
  in the chart) really changed. The headroom pre-flight below is for those
  deploys, not for every deploy.

**The first deploy from a commit that has #1378 rolls everything one last time**:
removing the label, the annotations and the linked version changes every
digest once. Plan it like any full roll. The same holds for a rollback across
that commit.

Those Go images no longer say which commit built them; three things outside the
digest do (the signature's certificate, the provenance attestation and the
`dev-<sha>` tag). See [verifying-releases.md](verifying-releases.md), "Which
commit built this digest". A Go component's own `--version` and
`service.version` are its binary's build ID, not a commit (section 5). The
proxy image still names its commit in its labels, and Envoy in its version.

What chart 2.4.15 removed is the chart's own share of this: an upgrade that
changes the chart version but not the images (a re-cut chart, a values-only
change) rolls nothing it did not change.

Until 2.4.15 every pod template carried `helm.sh/chart: aether-<version>` and
`app.kubernetes.io/version: <appVersion>`. The chart version changes with every
release, and a deploy from the commit tag (`<X.Y.Z>-<full sha>`, above) changes
both labels with every commit, so **every** upgrade rolled the agent, the proxy
(a hot restart on every node), mesh-dns, uds-csi, the registrar, the controller
and the edge. Both labels are still on the DaemonSet / Deployment objects; they
are no longer on the pods.

**The upgrade that crosses 2.4.15 rolls everything one last time**, because
removing the two labels is itself a pod-template change. Plan it like any full
roll (the headroom pre-flight below). The same is true of a rollback to a chart
older than 2.4.15, which puts the labels back.

To see what an upgrade rolled, compare the pods' template hashes before and
after it. A pod that was not rolled keeps its name, its age and its hash:

```bash
for ns in aether-system aether-ingress; do
  kubectl -n "$ns" get pods -l app.kubernetes.io/part-of=aether \
    -L controller-revision-hash,pod-template-hash \
    --sort-by=.metadata.creationTimestamp
done
# The release each workload object belongs to (the labels the pods lost):
kubectl get ds,deploy -A -l app.kubernetes.io/part-of=aether \
  -L helm.sh/chart,app.kubernetes.io/version
```

To know before an upgrade, render both charts with the values you deploy with
and compare; a workload whose `spec.template` is unchanged is not rolled:

```bash
helm get values aether -n aether-system -o yaml > values.yaml   # never --reuse-values
helm get manifest aether -n aether-system > before.yaml
helm template aether oci://quay.io/aethermesh/chart-aether --version "$AETHER_VERSION" \
  -n aether-system -f values.yaml > after.yaml
diff before.yaml after.yaml
```

`//charts/aether:aether_pod_template_version_test` keeps it that way: it renders
the chart at two versions and fails if any pod template differs. A new pod label,
annotation, env var or argument that carries the chart version or the appVersion
fails it.

A config checksum annotation is a different thing: it rolls a pod when the
configuration it reads at start changes, which is the point. The version labels
used to do that by accident, so 2.4.15 added the one that is needed:

| Pod | Annotation | Changes when | Why a roll is needed |
| --- | --- | --- | --- |
| edge | `checksum/edge-config` | the edge's Envoy bootstrap (`aether-edge-config`, data only) | plain `envoy -c`, nothing watches the file |

Two ConfigMaps have no checksum on purpose, because their reader watches the
mounted file. Each node picks the change up when its own kubelet delivers it
(tens of seconds observed, no guaranteed bound), independently of the others, not
one node at a time like a DaemonSet roll:

- The node proxy's own bootstrap (`aether-proxy-config`): the supervisor watches
  it (`--watch-config=true`) and hot-restarts Envoy in place, without replacing
  the pod.
- The OPA preset's policy (`aether-opa-policy`), since chart **2.4.19** (#1383):
  the sidecar runs `opa run --watch` and reloads it with no restart of anything.
  Chart 2.4.15–2.4.18 carried `checksum/opa-policy` and rolled the proxy
  DaemonSet for every policy change. The upgrade that crosses 2.4.19 with the
  OPA preset on rolls the proxy DaemonSet one last time (the annotation leaves
  the pod template and the sidecar's arguments gain `--watch`). See *Changing
  the OPA policy* under the ext_authz sidecar.

#### An agent image change also rolls the controller (#1428)

The controller's pod template names the **agent** image: the egress identity
gate's init container runs `/identity-ready`, which ships as a layer of the
agent image, and the controller is told which image to inject with
`--identity-gate-image` (default: `agent.image`). A new agent image digest is
therefore a new controller argument, and the controller Deployment rolls with
the agent DaemonSet even when the controller's own image did not change. This
is accepted, not a defect: the node's agent pod runs the same image, so the
init container normally finds it in the node's image cache, and the alternative
(an image of its own for `identity-ready`) would give that up to save a
controller roll.

"Normally", not always. During an upgrade the controller and the agent
DaemonSet roll independently: a controller replica that already has the new
digest can admit a pod onto a node whose agent has not been replaced yet, and
that pod's init container then pulls the new agent image itself
(`pullPolicy: IfNotPresent`) before it can start. The same holds on a node
that has just joined. The cost is one image pull on that pod's start, once
per node.

Three things follow:

- A controller roll replaces two replicas behind a Service one at a time
  (surge 1, none unavailable) and does not touch running mesh pods. A pod
  keeps the init container image it was admitted with; only pods admitted
  afterwards get the new one.
- `controller.webhook.identityGate.image.*` pointed at a fixed mirror tag
  removes the coupling, at the price of keeping that mirror in step with the
  agent image yourself.
- The argument is not passed at all when the gate is off: `spire.enabled=false`,
  `controller.webhook.identityGate.enabled=false`, or no mutating webhook
  (`namespaceInjection` and `injectPodNdots` both off, chart >= 2.4.20, #1432).
  In those releases an agent image change does not roll the controller.

To see the coupling on a cluster (the two digests are the same):

```bash
kubectl -n aether-system get deploy aether-controller \
  -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}' | grep identity-gate-image=
kubectl -n aether-system get ds aether-agent -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
```

#### Chart 2.4.20: the registrar, the controller and the edge roll once (#1434)

The upgrade that crosses 2.4.20 changes the pod template of the three
Deployments that run two replicas, so each rolls once, on Kubernetes >= 1.27:

| Deployment | What changes in its pod template |
| --- | --- |
| registrar | its spread constraint gains `matchLabelKeys: [pod-template-hash]` |
| edge (when enabled) | the same |
| controller | its default affinity (two preferred anti-affinity terms: its own replicas, the registrars' node) is gone; a spread constraint with `matchLabelKeys` keeps its replicas apart. A `controller.affinity` you set is rendered as before |
| controller, only with `namespaceInjection` and `injectPodNdots` both off | additionally `--identity-gate=true` and the gate's flags become `--identity-gate=false` (#1432) |

No DaemonSet changes. Below Kubernetes 1.27 the registrar and the edge render
exactly as before and only the controller rolls.

What it is for: the registrar and the edge already had a soft spread
constraint, and both registrar replicas were still found on one node of a
five-worker cluster. Two things defeated it, measured on kind (Kubernetes
1.35, a tainted control plane plus workers, stand-in pods carrying the chart's
scheduling fields, the three Deployments rolled together twenty times;
rollouts that ended with both replicas of a Deployment on one node, as
controller / edge / registrar):

| Pod templates | 2 workers | 3 workers |
| --- | --- | --- |
| chart 2.4.18 | 8 / 5 / 10 | 1 / 0 / 5 |
| 2.4.18 without the controller's "off the registrars' node" term | 0 / 6 / 2 | 0 / 0 / 1 |
| `matchLabelKeys` added, that term kept | 9 / 0 / 11 | 1 / 0 / 3 |
| chart 2.4.20: `matchLabelKeys` added, that term gone | 0 / 0 / 0 | 0 / 0 / 0 |

- The controller's default affinity preferred nodes without a registrar. The
  scheduler applies a preferred anti-affinity term in both directions, so
  every new registrar was also pushed off the nodes that held a controller,
  with the same weight as its own spread constraint pulled it onto an empty
  one. The two cancelled, and the tie fell to the scheduler's other scores.
- Without `matchLabelKeys` the constraint counted the pods of the ReplicaSet
  being rolled away beside the new ones.

The default stays a preference (`<component>.nodeSpread: soft`); `required`
makes it a rule, and `none` removes it (`docs/configuration.md`). If you set
`controller.affinity` to keep the controller off the registrars' node, expect
the third row (`matchLabelKeys` added, that term kept): the constraint still
has `matchLabelKeys`, and the term cancels it as before.

Nothing moves a running pod, before or after the upgrade: a spread constraint
acts when a pod is scheduled. The roll this upgrade causes is what re-places
the replicas. To see where they are, before and after:

```bash
kubectl get pods -A \
  -l 'app.kubernetes.io/part-of=aether,app.kubernetes.io/component in (registrar,controller,edge)' \
  -o 'custom-columns=COMPONENT:.metadata.labels.app\.kubernetes\.io/component,POD:.metadata.name,NODE:.spec.nodeName' \
  --sort-by='.metadata.labels.app\.kubernetes\.io/component'
# The constraint each Deployment carries:
kubectl get deploy -A -l app.kubernetes.io/part-of=aether \
  -o custom-columns=NAME:.metadata.name,SPREAD:.spec.template.spec.topologySpreadConstraints
```

Two replicas of one component on the same node after the roll means the
preference lost to the scheduler's other scores (or only one node had room).
`kubectl -n aether-system rollout restart deploy/aether-registrar` schedules
them again; `registrar.nodeSpread=required` stops it happening.

#### Chart 2.4.21: the chart no longer creates the release's namespace by default (#1403)

> **Before upgrading a release that an older chart installed with
> `namespace.create=false`, run this once** (it is what every install that
> passed `--set namespace.create=false`, or whose values file says so, is):
>
> ```bash
> kubectl annotate namespace aether-system helm.sh/resource-policy=keep
> # the prober chart, before its first upgrade to 1.0.6 (its default was
> # already namespace.create=false, so this is every prober release):
> kubectl annotate namespace <prober namespace> helm.sh/resource-policy=keep
> ```
>
> Without it the first upgrade is **refused** with a message naming that
> command; nothing is changed and nothing is deleted. Under a GitOps controller
> that drives Helm the symptom is a release that fails to upgrade, with that
> message, until the namespace is annotated. A release installed with the old
> default (`namespace.create=true`) needs nothing.

`namespace.create` defaults to `false`. No workload rolls: no pod template
changed. What the first upgrade to 2.4.21 does depends on what the chart can
see on the live namespace, and in no case does it let Helm delete it:

| The release was installed with | The live namespace | First upgrade to 2.4.21 (any `namespace.create`) |
|---|---|---|
| the old default, or `namespace.create=true` | carries Helm's ownership annotations for the release, or at least the chart's labels (`app.kubernetes.io/managed-by: Helm`, `app.kubernetes.io/instance: <release>`) | goes through; the Namespace stays in the manifest and gains `helm.sh/resource-policy: keep` |
| `namespace.create=false` (+ `--create-namespace`, or a namespace you made) | carries neither | **refused once**, with the command below; goes through after it |
| the old default, but the namespace lost the annotations **and** the chart's labels | carries neither | the same refusal |

**The one extra step, for a release installed with `namespace.create=false`**
(and for the rare namespace that lost every sign of its owner). Run it before
the upgrade, or when the upgrade's error names it:

```bash
kubectl annotate namespace aether-system helm.sh/resource-policy=keep
```

Why: Helm deletes an object that was in the previous manifest and is not in the
new one, and a chart cannot read the previous manifest. A release written by an
older chart looks the same from the cluster whether its manifest holds the
Namespace or not, so the chart leaves the Namespace out only when the live
object says `keep`, or when the previous revision was written by 2.4.21 or
newer and recorded that it rendered none (the
`aether.io/release-namespace-rendered: "false"` annotation on the agent
ServiceAccount). The annotation is harmless on a namespace the release never
rendered, the command is needed once, and you may remove the annotation again
after that upgrade. With a GitOps controller that drives Helm, the refused
upgrade shows as a failed release with this message; annotate, then let it
retry. The `prober` chart 1.0.6 asks the same of its namespace on the first
upgrade from an older chart (`kubectl annotate namespace <prober namespace>
helm.sh/resource-policy=keep`).

The check, before and after (the UID must not change):

```bash
# Owned by the release? Both annotations name it when it is.
kubectl get namespace aether-system \
  -o jsonpath='{.metadata.uid}{"\n"}{.metadata.annotations}{"\n"}'
# Is the Namespace part of the release? 1 = yes, 0 = no.
helm get manifest aether -n aether-system | grep -c '^kind: Namespace'
# What the upgrade would send, rendered against the cluster (never
# --reuse-values: read the values back and pass them with -f):
helm get values aether -n aether-system -o yaml > values.yaml
helm upgrade aether oci://quay.io/aethermesh/chart-aether --version "$AETHER_VERSION" \
  -n aether-system -f values.yaml --dry-run=server | grep -A6 '^kind: Namespace'
```

For a namespace in the first row the last command must print the `Namespace`
with `helm.sh/resource-policy: keep`; for the other rows it fails with the
`kubectl annotate` command until that has been run. **`helm template` cannot answer this
question**: it renders without a cluster, sees no owner, and prints no
`Namespace`. For the same reason, a pipeline that renders with `helm template`
and prunes what is no longer rendered (`kubectl apply --prune`, a GitOps tool in
that mode) would remove a namespace the old chart rendered: exclude the
namespace from pruning in that tool, or keep it in your own manifests, before
upgrading. Helm itself (the CLI, or a controller that drives the Helm SDK
against the cluster) is not affected.

Consequences to know:

- `helm uninstall` no longer deletes a namespace the chart rendered; delete it
  yourself.
- `namespace.create=true` with the release stored in the namespace it names is
  refused at render time on a **first** install (it could never be installed);
  an existing release that owns its namespace is not refused.
- A release whose only revision is a **failed** first install of an older chart
  gets the same refusal and the same command; see "Recovering from a failed
  first install".
- Moving a release to another namespace (`namespace.name`, or `edge.namespace`,
  changed on an upgrade) is refused while a namespace the release owns would be
  left behind unprotected: Helm would delete it with everything in it. The
  message names it; annotate it with `helm.sh/resource-policy=keep`, upgrade,
  and delete the old namespace yourself if it should go. The prober chart
  checks this when `namespace.name` or `namespace.create` is set.

#### Chart 2.4.22: with the OPA preset on, the proxy DaemonSet rolls once (#1401)

`proxy.authzSidecar.opa.image` was `openpolicyagent/opa:1.21.1-envoy-static`,
a tag, and is now the same tag with the digest of its multi-arch index
(`…-envoy-static@sha256:b4a8bbe8…344b`): a tag can be re-pushed, and this
container sits next to the proxy on every node. The OPA version does not
change.

The sidecar is a container of the proxy pod, so **the upgrade that crosses
2.4.22 rolls the proxy DaemonSet once on a cluster where
`proxy.authzSidecar.opa.enabled` is true and `opa.image` is the chart's
default**. With the preset off (the default), or with `opa.image` set to your
own reference (a mirror), nothing renders differently and nothing rolls. A
mirror reference is used as written: pin it by digest yourself.

Before and after the upgrade:

```bash
# Is the preset on, and which image does the release set? (never --reuse-values)
helm get values aether -n aether-system -a -o yaml | grep -A12 'authzSidecar:'
# The image each proxy pod's sidecar really runs: the digest the node resolved.
kubectl -n aether-system get pods -l app.kubernetes.io/component=proxy \
  -o jsonpath='{range .items[*]}{.spec.nodeName}{"  "}{.spec.initContainers[?(@.name=="authz")].image}{"  "}{.status.initContainerStatuses[?(@.name=="authz")].imageID}{"\n"}{end}'
# Whether the DaemonSet rolled: its pods' revision hash and age.
kubectl -n aether-system get pods -l app.kubernetes.io/component=proxy \
  -L controller-revision-hash --sort-by=.metadata.creationTimestamp
```

#### The prober chart (#1372, #1373, #1374)

The `prober` chart has the same rule since chart **1.0.5**: its DaemonSet's pod
template no longer carries `helm.sh/chart` or `app.kubernetes.io/version` (both
stay on the DaemonSet object). The chart is deployed from its commit tag, so
before 1.0.5 every prober release replaced all prober pods, and each replaced
pod starts new probe series (the `pod` label). As with the `aether` chart, a
release built from a new commit still carries a new prober image digest and
still rolls the DaemonSet; what is gone is the chart's own share.

**The upgrade that crosses 1.0.5 rolls the prober DaemonSet one last time**
(removing the labels is a pod-template change), and so does a rollback below it.
With `authzCanary.enabled` it also rolls the one-replica `authz-canary`
Deployment once: its image is now written by digest
(`curlimages/curl@sha256:…`, the index of `8.22.0`). The `authz-echo` Deployment
is not rolled: its pod template is unchanged. No selector changed.

Before and after the upgrade (the prober's namespace; `aether-test` on
talos-main):

```bash
NS=aether-test
# A pod that was not rolled keeps its name, its age and its hash.
kubectl -n "$NS" get pods -l 'app in (authz-canary,authz-echo)' \
  -L pod-template-hash --sort-by=.metadata.creationTimestamp
kubectl -n "$NS" get pods -l app.kubernetes.io/component=prober \
  -L controller-revision-hash
# The release each workload object belongs to (the labels the pods lost):
kubectl -n "$NS" get ds,deploy -l app.kubernetes.io/instance=prober \
  -L helm.sh/chart,app.kubernetes.io/component
# The image the canary really runs: the digest the node resolved.
kubectl -n "$NS" get pods -l app=authz-canary \
  -o jsonpath='{range .items[*]}{.spec.containers[0].image}{"  "}{.status.containerStatuses[0].imageID}{"\n"}{end}'
```

To know before the upgrade, render with the values you deploy with (never
`--reuse-values`) and compare:

```bash
helm get values prober -n "$NS" -o yaml > values.yaml
helm get manifest prober -n "$NS" > before.yaml
helm template prober oci://quay.io/aethermesh/chart-prober --version "$PROBER_VERSION" \
  -n "$NS" -f values.yaml > after.yaml
diff before.yaml after.yaml
```

Since 1.0.5 the canary's five objects (two ServiceAccounts, two Deployments, the
`HTTPFilter`) carry `app.kubernetes.io/component: authz-canary` on their own
metadata; until then they carried `component: prober`, so
`-l app.kubernetes.io/component=prober` listed them with the prober. That
selector now returns the prober's DaemonSet, ServiceAccount and pods (and the
Namespace, when the chart creates it with `namespace.create`) only; use
`-l app.kubernetes.io/component=authz-canary` for the canary's objects, and
`-l app=authz-canary` / `-l app=authz-echo` for its pods, whose labels did not
change.

Nothing moves the `authzCanary.image` and `authzCanary.echo.image` digests by
itself. Both are listed in `scripts/third-party-images.txt` since chart 1.0.7
(#1401; the values themselves did not change, so that upgrade rolls nothing):
`scripts/third-party-images.sh outdated` says when the registry has moved past
one. See *Refreshing third-party image pins*.

#### The udsecho chart (#1400, #1402)

Two changes in chart **2.0.3**, neither of which touches a selector:

- **Labels.** Until 2.0.3 no object of the chart carried any label. Its seven
  objects (three ServiceAccounts, three Deployments, the `EndpointPolicy`) now
  carry the standard set on their own metadata: `helm.sh/chart`,
  `app.kubernetes.io/name: udsecho`, `instance`, `component` (the workload:
  `uds-echo`, `uds-cr-echo` or `uds-client`; the `EndpointPolicy` belongs to
  `uds-cr-echo`), `part-of: aether`, `managed-by` and `version`. The pod
  templates are unchanged (`app: <name>` and `aether.io/managed: "true"`, as
  before), so the labels roll nothing, and the pods themselves are still found
  with `-l app=<name>`, not with the new labels.
- **The client image is pinned by digest.** `client.image` was
  `curlimages/curl:8.22.0` and is now that tag with the digest of its
  multi-arch index (`curlimages/curl:8.22.0@sha256:58adaa4e…6777`). **The
  upgrade that crosses 2.0.3 rolls the one-replica `uds-client` Deployment
  once** for it, unless you set `client.image` yourself. The two echo
  Deployments are not rolled by the chart (a release built from a new commit
  still carries a new udsecho image and rolls them for that).

Before and after the upgrade (the chart's namespace, `aether-test` by default):

```bash
NS=aether-test
# Everything the release owns (empty before 2.0.3):
kubectl -n "$NS" get deploy,sa,endpointpolicy -l app.kubernetes.io/name=udsecho \
  -L helm.sh/chart,app.kubernetes.io/component
# A pod that was not rolled keeps its name, its age and its hash; only
# uds-client's changes.
kubectl -n "$NS" get pods -l 'app in (uds-echo,uds-cr-echo,uds-client)' \
  -L pod-template-hash --sort-by=.metadata.creationTimestamp
# The image the client really runs: the digest the node resolved.
kubectl -n "$NS" get pods -l app=uds-client \
  -o jsonpath='{range .items[*]}{.spec.containers[0].image}{"  "}{.status.containerStatuses[0].imageID}{"\n"}{end}'
```

### Rendering the chart reproducibly (#1364)

Two `helm template` renders of the `aether` chart with the same values are
byte-identical, with **one exception**: with the default
`controller.webhook.spire=false` the chart generates the controller's
self-signed webhook CA and serving certificate, and a render without a cluster
generates a new pair every time. The lines that differ are exactly six: the
Secret's `ca.crt`, `tls.crt` and `tls.key`, and the `caBundle` of the three
webhooks. Nothing else in the chart is generated at render time
(`//charts/aether:aether_reproducible_masked_test` fails if that stops being
true).

To compare two renders (two chart versions, two values files, a change under
review), mask those six lines on both sides:

```bash
mask() { sed -E 's/^([[:space:]]*(ca\.crt|tls\.crt|tls\.key|caBundle):).*$/\1 <masked>/'; }
helm template aether "$OLD_CHART" -n aether-system -f values.yaml | mask > before.yaml
helm template aether "$NEW_CHART" -n aether-system -f values.yaml | mask > after.yaml
diff before.yaml after.yaml
```

or render both sides with `--set controller.webhook.spire=true`, which renders
no certificate at all. That is only a way to diff, not a way to run: it also
drops the Secret and adds the controller's SPIRE flags, and it renders the
controller's `ClusterSPIFFEID` only when
`controller.webhook.clusterSpiffeID.className` is set (empty by default), so the
flag alone does not give the controller a usable SPIRE identity. Use the mask
when the change under review touches the webhook.

**A real install or upgrade is not affected.** `helm upgrade` looks the Secret
up in the cluster and renders the certificate it finds, so an upgrade changes
neither the Secret's data nor any `caBundle`, and the controller keeps serving
the same certificate. (The Secret object is still patched by each release,
because its `helm.sh/chart` label changes; its data is not.) What does not look
the Secret up, and therefore shows a new certificate every time:

- `helm template`, a client-side `helm upgrade --dry-run`, and `helm diff`
  unless it is run with `--dry-run=server`. Use the mask, or the server-side
  dry run.
- A GitOps tool that renders with `helm template` and applies the result (Argo
  CD). It would apply a new CA on every sync: serve the webhook with SPIRE
  there (`controller.webhook.spire=true` **and** a
  `controller.webhook.clusterSpiffeID.className`, or a `ClusterSPIFFEID` of your
  own that grants the controller the webhook Service's DNS names; see
  `docs/configuration.md`). Flux's helm-controller runs a real upgrade and is
  not affected.

To check on a cluster that an upgrade left the certificate alone, hash it
before and after (never print it):

```bash
kubectl -n aether-system get secret aether-controller-webhook-cert \
  -o jsonpath='{.data}' | sha256sum
kubectl get validatingwebhookconfiguration,mutatingwebhookconfiguration \
  -l app.kubernetes.io/name=aether-controller \
  -o jsonpath='{range .items[*].webhooks[*]}{.clientConfig.caBundle}{"\n"}{end}' | sha256sum
```

The chart generates a new pair in three cases only:

| Case | What the upgrade does |
| --- | --- |
| No Secret (first install, or you deleted it) | new CA and certificate; `caBundle` = the new CA |
| The Secret has an empty or missing `ca.crt`, `tls.crt` or `tls.key` | new CA and certificate. Until chart 2.4.16 only a missing `ca.crt` did that: with an empty `tls.key` every upgrade **failed** with `Secret "aether-controller-webhook-cert" is invalid: data[tls.key]: Required value` until someone deleted the Secret |
| `controller.webhook.certRotation` set to a value the Secret was not generated for | new CA and certificate, the value stamped on the Secret as `aether.io/webhook-cert-rotation` |

**Rotating.** The pair is valid for ten years and nothing renews it. To rotate,
set `controller.webhook.certRotation` to a new value and upgrade:

```bash
helm get values aether -n aether-system -o yaml > values.yaml   # never --reuse-values
helm upgrade aether oci://quay.io/aethermesh/chart-aether --version "$AETHER_VERSION" \
  -n aether-system -f values.yaml --set-string controller.webhook.certRotation="$(date +%F)"
```

Afterwards leave the value set or remove it: neither rotates again, and the
Secret keeps its stamp when the value is removed, so putting the same value back
later (a reverted values change) does not rotate either. Only a *different,
non-empty* value does.

After that upgrade the webhooks trust both CAs (`caBundle` holds the new one
and the one being replaced), so the certificate the controller is still serving
keeps verifying. The controller reloads the new certificate from its mounted
Secret by itself, once the kubelet has synced the volume (a minute or two);
`kubectl -n aether-system rollout restart deployment/aether-controller` does it
at once. The next upgrade renders the new CA alone, so do not run it before the
controller has reloaded. If it does happen, the webhooks fail open
(`failurePolicy: Ignore`) until the reload: pods admitted in that window are
not mutated (no mesh injection, no `ndots`, no identity gate).

The rotation is not gap-free, so do it in a quiet moment and check the upgrade
succeeded. The Secret and the webhook configurations are separate objects and
Helm patches the Secret first (its kind order). Normally the webhook
configurations follow within the same upgrade, long before the kubelet delivers
the new Secret to a running pod. But if the upgrade fails in between, or a
controller pod starts in between, that controller serves the new certificate
while the webhooks still trust only the old CA, and they fail open until the
upgrade completes. If an upgrade with a new `certRotation` fails, re-run it
before anything else. Closing the gap needs the new CA trusted one upgrade
before the new certificate is served; the chart does not stage that.

### Pre-flight: node headroom before a roll (#812)

Do this **before** any `helm upgrade` that rolls the DaemonSets. A roll is a
scheduling event, and on a node that is already tight the scheduler — not the
code — decides whether it succeeds.

Two distinct exposures, and only one of them is fixed in the chart:

- **Edge (Deployment).** Fixed by default since #812: with `replicaCount >= 2`
  the strategy is surge-free (`maxSurge: 0, maxUnavailable: 1`), so no extra
  400m pod has to be placed and the scheduler is out of the rollout path. If you
  override `edge.rollingUpdate` back to a surging strategy, this pre-flight is
  mandatory — on rev214 that surge pod was preempted on a node at 84 % CPU
  requests and wedged the roll.
- **The DaemonSets (`aether-agent`, `aether-proxy`, `aether-mesh-dns`).** The
  proxy and mesh-dns always surge, and so does the agent when
  `agent.updateStrategy.surge` is on (proposal 041). A surge pod needs its
  *whole* request on top of the old pod's. Not fixed and not fixable by a
  strategy: a DaemonSet pod must land on *its* node
  or not at all. The replacement needs the departing pod's request back, and if
  anything on that node is stuck `Terminating` it is still holding its CPU
  request — at which point the scheduler will not even preempt (`preemption: not
  eligible due to a terminating pod on the nominated node`). That is #796.

```bash
# 1) Rank nodes by CPU requests. Anything at/above ~85 % is where a roll stalls.
#    Sort on the percentage as a NUMBER: a lexical sort puts "(100%)" below
#    "(58%)" and hides the one node you are looking for.
kubectl get nodes -o name | sed 's|node/||' | while read -r n; do
  kubectl describe node "$n" \
    | sed -n '/Allocated resources/,/Events/p' \
    | awk -v n="$n" '/^  cpu /{pct=$3; gsub(/[()%]/,"",pct); printf "%6.1f  %-14s %s %s\n", pct, n, $2, $3}'
done | sort -rn

# 2) On any node above the line, name what is actually holding the requests, so
#    the decision is "move this tenant" rather than "hope it lands elsewhere".
#    QUOTE the -o argument: the unquoted [*] is a glob and zsh fails the
#    command outright ("no matches found") before kubectl ever runs.
kubectl get pods -A --field-selector spec.nodeName=<node> \
  -o 'custom-columns=NS:.metadata.namespace,NAME:.metadata.name,CPU:.spec.containers[*].resources.requests.cpu,PRIO:.spec.priorityClassName' \
  | sort -k3 -hr | head -20
#    A multi-container pod prints its requests comma-joined (`500m,10m`, the
#    proxy), so read that column rather than trusting its sort position.

# 3) Nothing may be stuck Terminating anywhere — one of these makes its whole
#    node unrollable regardless of headroom.
kubectl get pods -A --field-selector status.phase!=Running,status.phase!=Succeeded \
  -o wide | grep -i terminating
```

**Verdict.** All nodes comfortably under the line and nothing `Terminating` →
roll. A node over it → either move the tenant workload off first (the platform
did exactly this on 2026-09-19, spreading the o11y stack and taking w01 from
88 % to 66 %), or accept that that node's DaemonSet pods may sit `Pending` and
watch them specifically. A pod stuck `Terminating` → resolve it first via
§8 "A pod is stuck `Terminating`"; do not start a roll on top of it.

**Load generators must not be the victim.** The priority-0 k6 runner was picked
as the preemption victim twice on a dense node and cost a soak its data; it runs
under the `aether-soak-loader` PriorityClass since #811. Any new load or probe
workload needs a PriorityClass for the same reason — an evicted generator looks
exactly like a passing test.

### Agent surge roll (proposal 041)

`agent.updateStrategy.surge` (chart >= 2.3.0, default `false`) rolls the agent
DaemonSet with `maxSurge: 1, maxUnavailable: 0` instead of delete-then-create.
It removes the pod replacement and the new agent's startup from the window in
which the node's proxy has no ADS stream (#1123): the gap becomes the proxy's
reconnect backoff plus a small storage diff.

**Turning it on: two upgrades, never one.**

```bash
# 1. Upgrade to chart >= 2.3.0 with surge OFF (the default). Delete-then-create:
#    every agent comes up holding the node lock, and the registrar learns the
#    per-agent watch key.
helm upgrade aether <chart> -n aether-system -f values.yaml --set agent.updateStrategy.surge=false
kubectl -n aether-system rollout status ds/aether-agent
kubectl -n aether-system rollout status deploy/aether-registrar
# 2. Only then turn surge on. Changing only the strategy rolls nothing; the
#    next agent roll is the first surge.
helm upgrade aether <chart> -n aether-system -f values.yaml --set agent.updateStrategy.surge=true
```

Turning surge on in the same upgrade that first ships it is unsafe. That
surge would run the new agent beside an old one, and three things go wrong:

- **An older registrar ignores the watch `instance` field.** It keys watch
  streams by cluster/node alone, so the owner's and the standby's
  `WatchEndpoints` streams replace each other. Each replacement is a DataLoss
  close and a full resync, and they ping-pong for the whole overlap. The
  registrar Deployment rolls in the same upgrade, so this cannot be ordered
  away.
- **The old agent holds no lock.** The standby falls back to probing the
  node sockets. An old agent that restarts between those probes can rebind
  `xds.sock`/`cni.sock` after the standby has taken the node. When its pod is
  deleted, Go unlinks both paths by name, and the node is left with no
  sockets while the new agent runs.
- **The old agent's last flush can land after the takeover.** Without a lock
  it flushes the observed upstreams on its way out, possibly after the new
  agent has already written them.

Do the node-headroom pre-flight below first. With surge each node
briefly runs two agents, so it needs room for a second `agent.resources`
request (200m CPU, 96Mi since chart 2.4.2; 64Mi before, see *The node agent
is OOMKilled*). The pod is `system-node-critical` and preempts if it
has to. If it cannot be placed, it stays `Pending` and the roll stalls on that
node, visibly. Nothing breaks: the old agent keeps serving.

**What the overlap looks like.** Per node, in the new agent's log:

```
another agent owns this node; starting as a standby: building everything, binding nothing until its lock is released
... (the usual startup: SVID, registry connected, client certificates)
node lock acquired: the previous owner is gone; taking the node over   standby=6.2s
takeover step done   step="merge the previous agent's persisted node state"
takeover: applied the previous agent's CNI ADD/DEL from the overlap   added=1 updated=0 removed=1
this agent owns the node; binding its sockets and starting its writers   takeover=1ms
```

`takeover: local storage unchanged during the overlap` is the common case. The
pod is Ready once its first snapshot is complete (`standby` readiness check).
The DaemonSet controller then deletes the old pod, the old agent exits in
20-100 ms, and the kernel hands the lock (`/run/aether/agent.lock`) to the
standby. Proxy side, `control_plane.connected_state` drops to 0 for about 0.1 s
(on kind: median 0.08 s, p99 0.24 s; what `e2e/agent-restart-gap.sh` measures):

```bash
kubectl -n aether-system exec <proxy-pod> -c aether-proxy -- \
  curl -s 'http://127.0.0.1:9901/stats?filter=^control_plane.connected_state$'
```

**A standby that never takes over** is waiting on an old agent stuck
`Terminating`. It stays Ready and binds nothing; the old pod's
`terminationGracePeriodSeconds` (30 s) bounds the wait, then the kubelet kills
the process and the kernel releases the lock. If the old pod is stuck past that
(a wedged kubelet, #796), the old *process* may already be gone. Check:

```bash
kubectl -n aether-system exec <new-agent-pod> -c agent -- /agent-ready --path=/readyz   # prints the failing check, if any
kubectl -n aether-system logs <new-agent-pod> -c agent | grep -E "standby|node lock|owns the node"
```

A standby still logging `another agent owns this node` with no `node lock
acquired` means some process on the node still holds the lock. On the node,
`fuser /run/aether/agent.lock` names it. Do not delete the lock file: an
unlinked lock file is a different inode from the one the next agent opens, and
the two would both believe they own the node.

**`kubectl delete pod` of an agent** (any strategy) is still delete-then-create:
the lock is free when the new pod starts, so it owns the node at once and
serves exactly as before (no takeover step runs).

**Turning it off** (`surge: false`) takes effect on the next roll. The lock
stays: it costs nothing when nothing contends for it.

### Version-ordering constraint: issue #815 (per-source client certificates)

**You may not upgrade a cluster from a chart/agent older than `0.92.27` (the
#819 + #821 "release one" build) straight to `0.92.28` or newer.** Go through a
release-one build first, let every node's proxy pick up the new listeners, and
only then upgrade again. Skipping the step is not a hard failure, but for the
length of one config transition a pod's egress can present the **node** identity
instead of its own, which inbound RBAC / `ext_authz` / peer-identity
expectations will see.

Why: the client certificate a pod's egress presents is selected by a cluster
transport-socket matcher that reads a filter-state key the pod's *listener*
stamps. Release one (`0.92.27`) started stamping `aether.source.spiffe_id`
alongside the old `aether.network.network_namespace`; release two (`0.92.28`)
moved the matcher onto it. The agent publishes listeners and clusters in the
same snapshot, but LDS and CDS are separate xDS responses with no ordering
guarantee, so a release-two cluster can briefly face a pre-release-one listener.
That connection matches nothing and takes `on_no_match` = the node identity.

A third release that dropped `aether.network.network_namespace` from the
listeners was evaluated on 2026-09-19 and **closed**: on a listener that key
carries no per-pod state (its value is a constant format string), so removing it
would save about 240 bytes per mesh-originating filter chain and nothing else —
while costing another full re-key of every mesh pod's filter chains to deploy.
The netns key is still stamped (the access log's `source_netns` reads it).

While the listeners stamped both keys, **rolling back below `0.92.28` was
safe**: a proxy whose listeners stamp both keys matches correctly against
clusters from *either* side of release two. This issue needed exactly that once
— release one was rolled back on talos-main on 2026-09-19. That safety ended
with #1165 (next section). Verification and the runtime failure signature are
under *"#815 release two"* in §8.

### Rollback floor: chart `0.93.0` (issue #1165)

**After the chart that ships #1165, rolling back to a chart older than `0.93.0`
is no longer hitless.** `0.93.0` (2026-09-20) is the chart that shipped #842,
the per-connection certificate selector; use `0.93.1` or newer as the floor in
practice, since `0.93.0` itself carried the #842 SDS-dedup stall that `0.93.1`
(#865) fixed. Rolling back to any chart `>= 0.93.0` is unaffected.

Why: from `0.92.27` the source pod's SPIFFE ID was stamped under
`aether.source.spiffe_id`, and every cluster before `0.93.0` selects its client
certificate with a `transport_socket_matcher` that reads that key. #842 moved
selection to the `filter_state_override` certificate mapper, which reads
`envoy.tls.certificate_mappers.on_demand_secret`, and kept stamping the old key
for one release purely so a rollback stayed hitless. #1165 stopped stamping it.
A node proxy running post-#1165 listeners that is handed pre-`0.93.0` clusters
during a rollback matches nothing and takes `on_no_match` — the **agent's own
SVID** (`spiffe://<td>/ns/aether-system/sa/aether-agent`, #825) — so every
pod's egress presents the wrong identity until the old agent's listeners land
too (LDS and CDS are separate responses with no ordering guarantee).
`ext_authz` / RBAC / peer-identity checks at the destination will see it.

If you must go below `0.93.0`: step through a `0.93.x` / `0.94.x` chart first
(its listeners stamp both keys), let every node's proxy take the new listeners,
then roll back again.

Nothing else in the observable surface changed. The access log's
`source_spiffe_id` attribute (HTTP `aether_access_logs` and L4
`aether_l4_access_logs` streams) keeps its **name** and value; only the
filter-state key it reads moved to the mapper key, so VictoriaLogs queries and
dashboards that filter or extract on `source_spiffe_id` need no change. The
proposal 038 QUIC selection matcher reads the mapper key too, so the arm a
request takes and the client certificate it presents now come from one value.
Rolling **forward** re-keys every mesh-originating filter chain (one
`set_filter_state` entry fewer) and every QUIC-selecting route, so those chains
drain once on `--drain-time-s`.

There are also two standalone charts, installed independently: **`prober`**
(`charts/prober`) — the external mesh-availability prober (proposal 013; its
flags, metrics and deployment shape are in
[`configuration.md`](./configuration.md) § *`prober`*) — and
**`udsecho`** (`charts/udsecho`) — the UDS validation workloads (proposal 034)
that exercise both socket-delivery paths (annotation and `EndpointPolicy`) under
continuous mesh traffic, on the `csi.aether.io` carrier (udsecho `2.x` needs
aether `2.x`; see *Upgrading to chart 2.0.0* below).

See [`charts/README.md`](../charts/README.md) for chart layout, image mirroring,
and the `--stamp` versioning scheme, and [`getting-started.md`](./getting-started.md)
for the full install + onboarding walkthrough.

> Moving or renaming a binary **inside** an image — the Bazel image rules, a `tars_layer`
> entry (`/proxy-ready`, `/mesh-dns-ready`, `/opt/cni/bin/aether-cni`), or `proxy/` —
> breaks profile symbolisation, silently: flame graphs decay to hex addresses with no
> error and no failing test. **Adding** a binary to an image is the same trap; it profiles
> as hex until it is listed. See
> [`observability/profiling-symbols.md`](./observability/profiling-symbols.md) for the
> path table that has to be updated alongside it.

### Upgrading to chart 2.0.0: the UDS carrier cut-over (proposal 039 Phase 2)

**Breaking, no compatibility window.** Chart `2.0.0` makes the `csi.aether.io`
CSI volume the **only** carrier for a UDS-delivered workload socket (proposal
034). The `emptyDir` carrier, the proxy's `/var/lib/kubelet/pods` mount, the
agent's `--kubelet-pods-dir` and the `proxy.udsWorkloads` value are gone;
`udsCsi.enabled` (the plugin DaemonSet + `CSIDriver`, the agent's
`--uds-csi-root`, the proxy's read-only mount of `udsCsi.root`) is now **on by
default**. TCP-served workloads are untouched.

**Before upgrading the chart, every UDS workload must switch its socket volume**
(or ship the switch in the same rollout — the old carrier stops resolving the
moment the agent rolls):

```yaml
spec:
  securityContext:
    fsGroup: 65532                  # NEW, required: the socket dir is root:<fsGroup> 2770
  volumes:
    - name: s                       # the <volume> the annotation / EndpointPolicy names
      csi: {driver: csi.aether.io}  # was: emptyDir: {} (or {medium: Memory})
```

Find them first — every pod carrying the annotation, and every `EndpointPolicy`:

```bash
kubectl get pods -A -o json | jq -r '.items[]
  | select(.metadata.annotations["endpoint.aether.io/uds-socket"] != null)
  | "\(.metadata.namespace)/\(.metadata.name) \(.metadata.annotations["endpoint.aether.io/uds-socket"])"'
kubectl get endpointpolicies -A
```

A workload that misses the switch is not deleted, but it stops being delivered:

- **new pods** (a roll, a reschedule) carrying the annotation are **denied** by
  the controller's pod webhook, with a message naming the fix
  (`… cannot be delivered (not_csi): … change the volume's source to
  csi: {driver: csi.aether.io} and set the pod's securityContext.fsGroup`);
- **running pods** — and policy-declared pods, which admission cannot see — fall
  back to TCP; a UDS-only app has nothing listening there, so its endpoint stays
  **unpromoted**, counted as `aether_agent_uds_resolve_failures_total{reason="not_csi"}`.

Verify after the upgrade (every reason is exported at zero, so an absent series
means the agent is not exporting, not "no failures"):

```bash
# expect 0 for every reason; non-zero names what is wrong (see §8)
kubectl get --raw "/api/v1/namespaces/aether-system/pods/<agent-pod>:8080/proxy/metrics" \
  | grep '^aether_agent_uds_resolve_failures_total'
kubectl -n aether-system rollout status ds/aether-uds-csi
kubectl get csinode -o jsonpath='{range .items[*]}{.metadata.name}{": "}{.spec.drivers[*].name}{"\n"}{end}'   # lists csi.aether.io
```

Talos needs nothing extra (the default `udsCsi.kubeletRoot`, `/var/lib/kubelet`,
is right). On k0s / microk8s set `udsCsi.kubeletRoot`. Rolling back to `1.x`
requires reverting the workloads to `emptyDir` too.

### East-west QUIC (proposal 038 Phase 4): prerequisites, verifying, escape hatch

East-west QUIC is **unconditional**. Every mesh pod has an HTTP/3 inbound on
UDP:18008 beside the TCP one (#953: same SVID, same client-certificate
requirement, same SAN pin), and every node proxy dials every service in its
dependency set over HTTP/3 from every local ServiceAccount that calls it (#956;
the per-source clusters are built on first use, #1020). There is no
value or flag for it: the per-destination allow-list (a chart value + agent flag)
was a proving gate for QUIC on a real cluster and was removed by #979 (33ff5e9,
chart 1.0.12; decision 2026-09-26: no opt-in for QUIC). The first proving soak
(2026-09-27) did not pass — k6 saw 1,060 × 503 NC at first use (#1008, closed) —
and #979 merged on 2026-09-29; the re-soak of the merged build:
**2026-09-30, chart 1.0.13-84fb413 (T0 10:57:18Z): every QUIC gate passed; the run failed only on liveness, 9 of 719,984, with a cause outside QUIC.**
- **The two defects that failed the 09-29 run did not recur.**
  - No agent logged a `pruned persisted east-west QUIC pairs` line, and all 30 fresh xDS streams re-stated their held twins as served.
  - There was no source-side `503 NC` in the window.
  - There was no Envoy crash across 6 hot restarts per node.
- **The new-ServiceAccount steps, wedge gates, stateless-reset gate and L4 gates were all at zero.** h3 costs 1.18× h2 per request.
- **The 9 liveness errors are a new race (#1085).**
  - In the TRIPLE, main-worker-05's agent was down while its proxy forked a successor.
  - The successor's CDS and LDS initial fetches timed out (15 s each), and Envoy started workers with no listeners.
  - The parent then drained, so `127.0.0.1:18081` refused new connections for 1.6 s.
  - A successor that logs `initial fetch timed out for …Listener` next to `starting workers` is this case.
- **k6 had 147 failures in 9.18 M requests.**
  - 141 were `503 NC` first-use timeouts at loader start, before T0 (#1086).
  - 6 were `504 UT` to a terminating svc-3 pod in the TRIPLE (#1087).
The one exception is a service with any endpoint behind the east/west
waypoint (019): it stays h2, because the waypoint tunnel has no QUIC leg.

**Mesh-wide SPIRE prerequisite.** Every workload SVID must carry two DNS SANs —
`<sa>.<ns>.<meshDomain>` and `*.<sa>.<ns>.<meshDomain>` — until
envoyproxy/envoy#47740 is in a *plain* proxy pin. Envoy's QUIC client verifies the
SNI (`<port>.<sa>.<ns>.<meshDomain>`) against the leaf's DNS SANs *after* the SPIFFE
pin succeeds (aether#957); without the SANs every HTTP/3 handshake fails closed
(503 at the source, `Leaf certificate doesn't match hostname` /
`QUIC_TLS_CERTIFICATE_UNKNOWN` in the proxy log at `quic:info`). The proxy pin since
chart 0.95.3 carries #47740 as a patch (#972), which defers that check to the SAN
pin, so today a missing SAN does not fail — but the carry is transitional and a pin
bump can drop it, so the SANs stay required until upstream ships it. On the
spiffe/spire chart:

```yaml
spire-server:
  controllerManager:
    identities:
      clusterSPIFFEIDs:
        default:
          dnsNameTemplates:
            - "{{ .PodSpec.ServiceAccountName }}.{{ .PodMeta.Namespace }}.<meshDomain>"
            - "*.{{ .PodSpec.ServiceAccountName }}.{{ .PodMeta.Namespace }}.<meshDomain>"
```

Changing the entry re-issues every workload SVID on the agents' next fetch; no pod
roll. **Order of operations on a live cluster upgrading from a chart that still
had the allow-list: SPIRE first, wait for the agents' `envoy_sds_*_version` to move
on every node, then the chart** — the upgrade turns QUIC on for every destination
at once.

**Network prerequisite.** UDP:18008 must be open wherever TCP:18008 is: from every
node (the source proxy is host-network) to every mesh pod IP. A NetworkPolicy or
host firewall that allows only TCP:18008 breaks every mesh request, not just a few.

**What to expect.** Every service in a node's dependency set is QUIC-eligible
(default entry only, identity-ready, never a service with an endpoint behind the
east/west waypoint), but twins are **demand-scoped**
(#1020): a node builds a `quic:<svc>.<ns>.<domain>@<ns>/<sa>` cluster only for a
(source ServiceAccount, destination) pair that has actually dialled. The route to an
eligible destination carries one selection arm per local ServiceAccount (a matcher
cluster specifier keyed on the connection's
`envoy.tls.certificate_mappers.on_demand_secret` filter state — the same
source-identity stamp the certificate mapper reads; `aether.source.spiffe_id`
before #1165), each naming that source's twin, built or not. A source's **first** request
resolves to a twin the proxy does not have yet; the HCM's `on_demand` filter asks
the agent for it by name over ODCDS; the agent checks the name (destination in the
dependency set, source a ServiceAccount with a pod on the node), records
the pair and publishes the twin with its own load assignment; the paused request
resumes on it, over HTTP/3. That costs one local round trip. It was 17–24 ms in
`//agent/test/mtlspool`'s `TestOnDemandQUICTwinPerPair`, and every later request from
the pair skips it. A connection with no identity stamp (stamped before the trust
domain was known) takes `on_no_match`, the h2 cluster. A GAMMA (HTTPRoute) rule
whose single backendRef is the parent rides QUIC too; a weighted split stays h2
(#961) and never fetches a twin.

The agent logs `east-west QUIC fan-out quic_clusters=N observed_pairs=P
local_identities=I awaiting_client_cert=W` whenever `N` or `W` changes, and for each
new pair `observed east-west QUIC pair (ODCDS); building its twin cluster=…`.
`N` is the number of observed pairs whose source is still local, so it is at most
`I × S`, where `S` is the node's eligible dependency-set services (the line no
longer carries a service count since #979), and on a real fleet much less (rev242,
with an allow-list of 2: 118 possible, 2 used; without the allow-list the ceiling on
talos-main is 8–14 SAs × ~19 services ≈ 150–270 per node, the pairs actually dialled
stay the same).

**What a healthy node reads.** `observed_pairs` ≈ the (source ServiceAccount,
destination) pairs that carry traffic on that node: on talos the k6 loaders, the
prober and the dialers, so **single digits per node**, and `quic_clusters` equal to
it. `observed_pairs` that is a whole multiple of `local_identities` on every node
(every local SA × the same destinations) is the #1033 red reading (rev245, 2026-09-28: 24/24/12, 18/18/9, 28/28/14, 20/20/10,
20/20/10), not a busy fleet.

**Reading twin count against pairs.** On one node, the `quic:` clusters in the
proxy admin (`/clusters`) must be exactly the pairs with traffic. Pairs are
persisted beside the observed upstreams (`state/observed-upstreams.json`,
`quic_pairs`), so they survive an agent or agent+proxy roll. There is **no idle
expiry**: nothing in xDS tells the agent that a twin stopped carrying traffic. A
pair is pruned when its source ServiceAccount has no pod left on the node, or when
its destination leaves the dependency set, or by the post-start
prune below. In the first two cases, a pair whose twin the proxy fetched on demand
loses its twin but is kept **dormant** and republished when it is valid again (#1036,
"Stranded twin" below). A pair that did receive traffic therefore keeps its twin while both
ends stay put, even if it goes quiet. `envoy_cluster_manager_active_clusters` rises
by one when a pair first dials, falls when a pair is pruned, and is otherwise
**flat**. A step that tracks pod churn rather than new callers is the pre-#1020
fan-out and a regression.

**Agent restarts (#1033).** A restarted agent opens a fresh xDS stream, and the
running proxy re-states its twins in the stream's first CDS request. Only the
persisted pairs carry over; the re-statement is read two ways:

- A twin the proxy merely **holds** (`initial_resource_versions`, delivered by the
  wildcard, e.g. built up front by an older agent) admits nothing. If its pair is
  not persisted it is answered absent (`removed_resources`) and the proxy drops
  it; the next request that routes to it opens an on-demand fetch, one ~20 ms round
  trip, no 503. The #1032 agent re-admitted these, which is how rev245 persisted the
  whole SAs × destinations fan-out on every node.
- A twin the proxy **re-subscribes** by name holds a live on-demand subscription: a
  request routed to it at some point in this proxy's life. Its pair is admitted.
  This is not optional. Envoy's ODCDS manager keeps one subscription per name for
  the life of the process and answers every later request for the name "already
  subscribed, skipping", so a subscribed twin answered absent is **stranded**: every
  request from that pair 503s `NC` at the 2 s `on_demand` timeout until the proxy
  restarts. `//agent/test/mtlspool`'s `TestOnDemandQUICSubscribedTwinsServedAfterAgentRestart/strand_control`
  shows it.

The agent logs one line per fresh stream: `fresh xDS stream re-stated QUIC twins:
held-only twins admit nothing, live on-demand subscriptions are served
resubscribed=R resumed_pairs=P held_only=H held_served=S answered_absent=A`.

**Post-start prune of pairs with no evidence of use (`--east-west-quic-pair-fetch-window`,
default `1h`).** A persisted pair that no agent process has ever seen evidence of use
for is dropped with its twin once the window has elapsed (checked on the one-minute
prune tick). `0` disables the prune. Evidence of use is any of:

- an on-demand fetch of the twin (first use);
- an on-demand subscription for it, held by a live stream or re-subscribed on a fresh one;
- the twin re-stated as **held** (`initial_resource_versions`) on a fresh stream while
  the agent serves the pair (#1073).

The evidence is persisted with the pair (`demand_confirmed` in
`observed-upstreams.json`), so it survives agent restarts and node reboots. A pair
with evidence is never pruned by the window; it goes only on removal evidence
(source left the node, destination left the dependency set).

What is left for the window is the fan-out rev245 persisted, and any pair persisted
before #1073 that the proxy neither holds nor fetches. The agent has no traffic signal
of its own: it makes no admin calls, and a twin the proxy already holds is never
fetched again. So a held twin counts as used.

**#1073, why "fetched" was the wrong signal.** After a proxy restart, every twin
reaches the new generation through the wildcard, with no fetch and no subscription,
and carries its traffic that way. On an agent-only roll the fresh stream re-states
those twins as held-only. The pre-#1073 rule ("no on-demand fetch since agent start")
then pruned every one of them exactly one window later, while they carried traffic:

- the #979 proving soak saw about 200 × `503 NC` per burst fleet-wide;
- Envoy crashed on one node (#1074, SIGBUS removing a QUIC cluster with a live connection;
  fixed by the carried patch #1077, chart 1.0.13 — see "Envoy SIGBUS/SEGV in
  `QuicConnection` after an h3 cluster removal (#1074)" below).

A held twin whose pair the agent does not serve still admits nothing and is answered
absent (#1033). The fresh-stream line's `held_served=S` counts the held twins that
were confirmed. The first time a pair is confirmed that way, the agent logs:

```
confirmed east-west QUIC pairs the proxy holds without an on-demand subscription: never pruned by the fetch window count=N held_served=S
```

When the window prunes anything, the agent logs one line per node:

```
pruned persisted east-west QUIC pairs with no evidence of use: never fetched, subscribed or held by the proxy count=N remaining=M window=1h0m0s pairs=[…]
```

It is followed by a `east-west QUIC fan-out` line with the new, smaller
`quic_clusters`. Builds before #1073 logged `… with no on-demand fetch since agent
start` instead.

If that line shows up **after an agent roll on a node whose proxy kept running**,
with `503 NC` on QUIC destinations right after it, that is #1073 again: the prune
removed twins in use. File it with the fresh-stream line from the same agent
generation.

**Stranded twin: 503 `NC` at 2 s for a source that came back (#1036).** Envoy's
ODCDS manager keeps **one subscription per cluster name for the life of the
process**. When the agent removes a twin the proxy fetched on demand, Envoy drops
the *cluster* and keeps the *subscription*. Every later on-demand request for that
name is skipped inside the proxy and never reaches the agent. The pair is then
**stranded** until the proxy restarts. The failure signature:

- **Access log.** `reporter:source`, `response_code:503`, `response_flags:NC`,
  `duration_ms` ≈ `2000` (the `on_demand` timeout) on every request from one source
  ServiceAccount to one QUIC destination, starting when a pod of that
  ServiceAccount came back to the node. `upstream_cluster` is `-` on an `NC` line, so
  scope by `authority` and the source pod.
- **Proxy debug log.** `ODCDS-manager: resource quic:<svc>.<ns>.<domain>@<ns>/<sa> is
  already subscribed to, skipping` for each of those requests.
- **Agent.** Nothing for that name: no `observed east-west QUIC pair (ODCDS)` line, no
  refusal, and `aether_agent_quic_twin_refused_total` flat, because no request arrives.
  `envoy_cluster_manager_odcds_init_fetch_timeout_total` does not move either. The
  timeout is the HCM `on_demand` filter's, not the subscription's.

Until #1036 this was reachable on every Deployment roll. A pair was *forgotten* when
its source ServiceAccount's last pod left the node, or when its destination left the
dependency set (or, before #979, the allow-list). The next pod of that ServiceAccount on the node
then routed to a name Envoy would never ask for again. `//agent/test/mtlspool`
`TestOnDemandQUICDormantTwinRepublishedWhenSourceReturns/forget_control` reproduces
it: `status=503 … in 2.000099268s`, and no CDS request reaches the control plane.

**`DC` 200s on a QUIC destination at a source-proxy roll (#1009)** are benign when the line is `DC` + `downstream_remote_disconnect` + 200 + the clean-line `bytes_sent`: the HTTP/1.1 client closed after a complete body before the h3 FIN was decoded. `upstream_rx_ms` and `downstream_tx_end_ms` read `-` on such a line. The rule and its LogsQL are in `e2e/soak/README.md`, "Benign `DC` at a source-proxy hot restart"; any other `DC` is a real failure.

**Why the agent never forgets a subscribed pair.** The agent tracks which twins the
proxy holds an ODCDS subscription for: those it asked for by name, and those it
re-subscribes on a fresh stream. When such a pair loses its source or its
destination, it goes **dormant**:

- **Leaving.** The twin still leaves the snapshot, exactly as before. The pair moves
  to `dormant_quic_pairs` in `state/observed-upstreams.json`, and the prune line reads
  `pruned east-west QUIC pairs … (source_left_node, dormant)`.
- **Returning.** The pair is valid again when a pod of the ServiceAccount is back on
  the node, or the destination is back in the dependency set. The **same snapshot** then republishes
  the twin and its load assignment, with no request. The subscription never closed, so
  its delta watch is still on the stream and Envoy takes the pushed cluster. The agent
  logs `republished dormant east-west QUIC pairs: the proxy still holds their on-demand
  subscriptions, so the twin is pushed with no request count=N pairs=[…]`. The
  returning source's first request is 200 over HTTP/3 in milliseconds (6.7 ms in the
  gate above).
- **Pruning.** A dormant pair is dropped only when no live proxy generation holds the
  subscription. Subscriptions are tracked per xDS stream, because one Envoy process
  speaks one ADS stream and the node proxy's `Node` does not tell hot-restart
  generations apart (#1052). A pair is held by the stream that re-subscribed it in its
  first CDS request, or that asked for it since. It is pruned when a fresh stream's
  re-statement leaves no live stream holding it, or when the stream that held it ends
  while another stream is live (the draining parent of a hot restart exiting). The
  last live stream ending prunes nothing, since the proxy may be reconnecting. The
  agent logs `pruned dormant east-west QUIC pairs: no live proxy stream holds an
  on-demand subscription for their twins stream=S count=N dormant=M pairs=[…]` or
  `… the proxy generation that held their on-demand subscriptions ended its stream
  while a newer one is live …`. After that the name is free, and the next request that
  routes to it is an ordinary first use. Before #1052 an agent restart that landed mid
  hot restart could leave dormant pairs no live generation subscribed to: the child's
  fresh stream (naming nothing) pruned them, and the parent's stream seconds later
  (naming them) parked them again, past the parent's exit.
- **Refusals.** A first-use request the agent has to refuse opens a subscription too:
  a `source_not_local` race, say, with a pod whose records are not loaded yet. So the
  pair is parked dormant as well, and only the refused request 503s. A re-subscribed
  pair that is not servable on a fresh stream logs `kept east-west QUIC pairs dormant
  … count=N`.
- **The fetch-window prune.** It never touches a dormant pair, because a dormant pair
  is not a served pair. It also never touches a served pair that a live stream holds a
  subscription for. And it never touches a pair with persisted evidence of use: fetched,
  subscribed, or re-stated as held (#1073).

A dormant pair costs a few bytes in the state file and nothing in the proxy. It is
bounded by the names the proxy has subscribed to since its last restart.

**If you see the signature anyway:** restart the node's proxy (a hot restart,
`kubectl -n aether-system delete pod <aether-proxy pod>`). The new generation holds no
subscription, so the next request fetches the twin (~20 ms). Then file it. The
agent's `republished dormant` / `pruned dormant` lines around the source's return say
which half failed.

```promql
# twins per node, then the pairs among them that carried traffic in the last hour.
# Under steady load the two are equal; the difference is pairs that went quiet
# (kept by design, see above), never pairs that did not dial.
count by (node) (envoy_cluster_upstream_rq_total{aether_cluster=~".+@.+"})
count by (node) (increase(envoy_cluster_upstream_rq_total{aether_cluster=~".+@.+"}[1h]) > 0)
envoy_cluster_manager_active_clusters
```

**The failure mode of the first request.** A twin the agent refuses, or one that
takes longer than the `on_demand` timeout (2 s, `onDemandClusterTimeout`, the same
bound the mesh catch-all's cold path uses), fails that request with **503 `NC`**.
Envoy's API has no per-route fallback from an on-demand miss to the h2 cluster: the
matcher's action names one cluster and nothing else. Refusals are counted by the
agent as `aether_agent_quic_twin_refused_total{reason}`, and each one is a request
that 503'd. `source_not_local` and `destination_not_in_dependency_set` are races
with a pod leaving or the destination's idle TTL expiring (`destination_not_quic_enabled`
is gone with the allow-list, #979). `malformed_name` means a route and the
agent disagree on the naming. On the proxy, watch the ODCDS subscription:

```promql
# on-demand CDS fetches that timed out (a 503 NC per paused request): MUST stay 0
sum(increase(envoy_cluster_manager_odcds_init_fetch_timeout_total[1h]))
# agent refusals, by reason
sum by (reason) (increase(aether_agent_quic_twin_refused_total[1h]))
```

The one gap that remains by design is an agent that is down while a new pair
dials. That is the same exposure the capture catch-all already has (#682): the
paused request 503s at the timeout, and the next request retries the fetch.

**The fast path, and why a twin waits for its source's certificate (#1049).** The
agent side of a first use is not the slow part. It publishes the twin 2-10 ms after
the ODCDS request (a dormant pair returning is republished by the CNI ADD's own
snapshot), with no debounce and no wait on a registry reload. What used to lose to
the 2 s timeout was Envoy warming the twin. A twin names its source ServiceAccount's
SVID statically in its transport socket, so a twin published before that secret is
in the snapshot warms on SDS until SPIRE delivers it. On rev248 (2026-09-28
15:44Z, the k6 loaders' first pods on every node right after an agent roll) that
took 6.9-7.4 s from the CNI ADD, and 428 requests failed 503 `NC`. Two rules close
it:

- **An identity whose certificate is not in the snapshot yet gets no selection arm
  and no twin.** Its requests ride the warm h2 cluster, whose certificate is fetched
  per connection. The snapshot that carries the certificate carries the arm, the
  twin and the twin's load assignment together. The fan-out line shows the wait:
  `east-west QUIC fan-out … awaiting_client_cert=N`. `N` above 0 for more than a few
  seconds is a stuck SVID (see the SPIRE sections), not a QUIC fault. Before SPIRE
  serves any secret at all, nothing is held back.
- **A named subscribe for a twin is always answered.** Envoy's ODCDS manager
  subscribes to a twin by name whenever a request routes to it while it is not
  active, including when the wildcard already delivered it. go-control-plane stays
  silent on an unchanged resource. The per-name subscription then waited out its 15 s
  initial-fetch timeout and Envoy logged `cm odcds: cluster quic:… not found during
  on-demand discovery`, which fails any request still waiting on the name. That was
  main-worker-03 at 15:45:04.7, 15 s after its loader's first request. The agent now
  re-sends a subscribed twin it already sent (`cache.SnapshotCache.CreateDeltaWatch`).

The invariant, with #1035/#1036: for a well-formed twin name of an eligible
destination, the agent answers with the twin or holds the subscription open. It
never answers such a name absent while the pair is servable. A twin leaves the
snapshot only in two cases. Its source or destination has gone, and then the pair
is dormant and republished on return (#1036). Or the proxy holds the twin through
the wildcard alone, with no subscription (#1035).

**Verifying.** Per twin, the admin `/clusters` host rows (`<cluster>::<ip:port>::
rq_total::N`) are the ground truth. In Prometheus the twins carry their own stats
key `<ns>/<svc>@<ns>/<sa>` (#960):

```promql
# HTTP/3 requests per (destination, source ServiceAccount)
sum by (aether_cluster) (rate(envoy_cluster_upstream_rq_total{aether_cluster=~".*@.*"}[5m]))
# h2 + h3 together for one destination
sum(rate(envoy_cluster_upstream_rq_total{aether_cluster=~"aether-test/svc-1(@.*)?"}[5m]))
# a twin that cannot connect: the #957 DNS-SAN shape, or UDP:18008 blocked between nodes
increase(envoy_cluster_upstream_cx_connect_fail{aether_cluster=~".*@.*"}[5m])
# a twin whose endpoints never arrived (#1008): MUST be 0, fleet-wide, always
sum(envoy_cluster_init_fetch_timeout_total{aether_cluster=~".*@.*"})
```

Each twin subscribes to its **own** EDS resource, named after the twin cluster
(`quic:<svc>.<ns>.<domain>@<ns>/<sa>`), and the agent publishes the h2 cluster's
load assignment under that name too. In `/config_dump` a twin's
`eds_cluster_config.service_name` equals its own cluster name, never the bare
`<ns>/<svc>` the h2 cluster uses; if it ever does again, see "QUIC twin never
leaves warming" below.

On the destination, `listener.inbound_<pod>_h3.http.inbound.downstream_rq_2xx`
(admin `/stats`) is the per-pod count of requests that arrived over HTTP/3. The
kind harness `e2e/eastwest-quic.sh` asserts all of this end to end (E0–E5), including E4c: the node's `quic:` cluster count equals the (source, destination) pairs the suite drove. Its `QUIC_DNS_SANS=off` negative control reproduces the missing-SAN failure.

**HTTP/3 per-request cost and connection counts (#1021).** A same-build A/B on
2026-09-28 (rev247, 300 rps, matched 80-min no-roll windows, Pyroscope fleet envoy
cores) measured an HTTP/3 mesh request at **1.18×** the proxy CPU of an h2 one
(10.5 ms vs 8.9 ms across both proxies) — under the ≤ 1.5× gate the allow-list drop
(#979, merged as 33ff5e9) was held to ([#1021, "Same-revision measurement, 2026-09-28"](https://github.com/bpalermo/aether/issues/1021)).
The earlier ~3.3× (~11 ms vs ~3.3 ms, rev242 QUIC vs rev239 h2) compared two
builds and is superseded. Grade it only with the
matched-window method in `e2e/soak/README.md` ("The QUIC per-request cost gate"):
envoy-only Pyroscope cores over the T0+6h05m→T0+7h25m no-roll window of a QUIC run
and of an h2 reference run with matched per-destination rps, loaded minus idle, per
request. Fleet CPU alone says nothing, because the QUIC share of the load changes
between runs.

Expected upstream QUIC connections to one destination are **one per (source node,
source ServiceAccount that dialled it, Envoy worker that SA's app connections landed
on, destination endpoint)**. They are NOT one per app connection. A twin does not
pool per downstream connection (`QUICClusterFrom` pins
`connection_pool_per_downstream_connection` off), so a k6 runner's 60 keep-alive
connections share its node's per-worker pools. Pods of one ServiceAccount share a
twin's connections, because the SA is the identity. The upper bound per destination
is `Σ_nodes (dialling SAs × workers × endpoints)`:

```promql
# live QUIC connections per twin, and the node's worker count (the per-endpoint multiplier's ceiling)
sum by (node, aether_cluster) (envoy_cluster_upstream_cx_active{aether_cluster=~".*@.*"})
max by (node) (envoy_server_concurrency)
# endpoints per destination
max by (aether_cluster) (envoy_cluster_membership_total{aether_cluster=~".*@.*"})
# density: QUIC rps per live QUIC connection (#1006: ~0.8, i.e. every request is its own flight)
sum(rate(envoy_cluster_upstream_rq_total{aether_cluster=~".*@.*"}[5m]))
  / sum(envoy_cluster_upstream_cx_active{aether_cluster=~".*@.*"})
```

A connection count that grows with app connections (k6 VUs, a client's pool size)
rather than with SAs × workers × endpoints means a twin started pooling per
downstream connection again. `//agent/test/mtlspool` `TestQUICTwinUpstreamConnections`
reproduces both shapes: 12 app connections open 1 QUIC connection (one worker), 4
(four workers), or 12 (option forced on). The same counts hold on the h2 path, whose
pool key has carried the source identity instead of the downstream connection since
#842.

The inbound QUIC listener reads with UDP GRO (`prefer_gro: true`, off by Envoy default
for listeners). It writes with Envoy's automatic GSO batch writer, which is used when
the kernel has `UDP_SEGMENT` (Linux ≥ 4.18; talos 6.18). A kernel without UDP GRO
logs `GRO requested but not supported by the OS` once per listener and reads without
it. That warning costs performance, never correctness.

**If UDP:18008 is blocked, or HTTP/3 fails mesh-wide.** Symptom: twins'
`cx_connect_fail` climbing, 503s from every caller, h2 clusters idle. There is no
per-destination or per-node off switch any more, by design, and none should be
added. In order:

1. **Fix the path** — open UDP:18008 (NetworkPolicy, host firewall, cloud security
   group) or, for handshake failures, re-issue the SVIDs with the DNS SANs above.
   Both converge without a pod or proxy roll.
2. **Roll the chart back** to the last release that still had the allow-list
   (1.0.11, the release before #979): its default (an empty list) is h2 for every
   destination, and the twins and selection disappear on the next push. Take the
   values from `helm get values -o yaml` and pass them with `-f`; never
   `--reuse-values`. This is the escape hatch; there is no other.

   **It is not proxy-neutral.** 1.0.11 pins `proxy.image` at `a1bcf05…` (#1070);
   main pins `71be75f…` (#1078, chart 1.0.13). A plain rollback therefore rolls
   every node's proxy (and the edge, which runs the same image) and loses:

   - the #1074 carried patch (#1077). Without it Envoy can SIGBUS/SEGV when a CDS
     removal drops an h3 cluster with streams in flight — and the rollback's first
     push removes every twin at once;
   - the #1073 fix (#1076): the agent goes back to the build whose fetch-window
     prune removes held twins. Moot while every destination is h2, but it returns
     with QUIC.

   To keep the proxy where it is, roll back the agent only: add the current
   `proxy.image` **and** `proxy.supervisor.image` to the `-f` file. The 1.0.11 chart
   reads the same keys, its `aether.image` helper uses the digest when one is set,
   and its proxy DaemonSet template is unchanged through 1.0.13, so with both images
   pinned the proxy DaemonSet does not roll at all. (Pinning only `proxy.image` keeps
   the patched Envoy but still rolls the proxy pods, because the supervisor image is
   in the same pod template.) The proxy half, for chart 1.0.13:

   ```yaml
   proxy:
     image:
       repository: quay.io/aethermesh/proxy
       tag: 71be75f9244c3eaddb1f05e0b9a0f08f0afcb6fa
       digest: "sha256:7cb11392fc028946e7dc5bb4a42a4882932bb214c15535a07240d3fa1dc1d167"
   ```

   Copy `proxy.supervisor.image` (and, for a later chart, `proxy.image`) from
   `helm get values <release> -n <ns> --all -o yaml`.

`spire.enabled=false` also removes QUIC (no TLS, no QUIC) but turns off mTLS
mesh-wide — it is not an escape hatch.

## 8. Troubleshooting

### QUIC twin never leaves warming / 503 NC on a new ServiceAccount (#1008)

**Symptom.** The first pod of a ServiceAccount that is new on a node gets
`503` with response flag `NC` (no cluster) for ~15 s on every request to every
QUIC destination, then recovers on its own. Other callers on the node
are unaffected; h2 destinations are unaffected. On talos (rev242) it was 1,060
client-visible 503/NC in 11 s when the k6 loaders started.

Since #1020 every twin is a late twin, because it is built on the pair's first
request. A regression of this fix would therefore hit **every** new (source,
destination) pair, not only a new ServiceAccount. A 503 `NC` that ends after
**~2 s** is a different failure: the on-demand fetch itself timed out or was
refused. See "The failure mode of the first request" above.

**Read.**

```promql
# the twin sat in warming (1) from its CDS add until the timeout
envoy_cluster_warming_state{aether_cluster=~".*@.*"}
envoy_cluster_manager_warming_clusters
# and gave up waiting for its endpoints: 1 per affected twin per proxy
envoy_cluster_init_fetch_timeout_total{aether_cluster=~".*@.*"}
```

In the proxy log: `cds: response indicates N added/updated cluster(s)`, then
~15 s later `gRPC config: initial fetch timed out for
type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment`, one per
late twin. The agent is idle through the gap: nothing on the control-plane side
is pending.

**Cause.** A twin that shares its h2 base's EDS resource name. Envoy's delta-ADS
`WatchMap` deduplicates subscription interest per (type_url, resource name):
when the twin arrives *after* the base is subscribed, its watch adds nothing to
`resource_names_subscribe`, no request is sent, the control plane (correctly)
sends nothing because the resource did not change, and the twin waits out its
15 s `initial_fetch_timeout`. At agent start base and twins arrive in one CDS
response, so only a *late* twin — a new local ServiceAccount — is hit. Fixed in
#1008: `proxy.QUICClusterFrom` points the twin at its own EDS name and the cache
publishes the base's `ClusterLoadAssignment` under it
(`proxy.LoadAssignmentAlias`). Seeing this again means that pairing broke;
`//agent/test/mtlspool`'s `TestLateQUICTwin*` pair reproduces it against the pinned
proxy (the negative control times out at ~15 s by design).

**Invariant.** A delta-ADS subscriber must never share a resource name with an
already-subscribed sibling. It has bitten twice and was found latent a third
time:

- **SDS (#842):** the on-demand certificate selector behind every static SVID
  reference. Fixed in #865 with its own SotW stream.
- **EDS, QUIC twins (#1008):** a twin behind its h2 base.
- **EDS, port aliases and TCP floors (#1013):** the `<fqdn>:<port>` alias
  clusters (the ODCDS cold-path authorities, e.g. `:18081`) and the TCP floor
  `tcp:<fqdn>` with its primary-port alias `tcp:<fqdn>:<port>` all subscribed to
  the bare `<ns>/<svc>` name that the default cluster holds. They usually arrived
  in the same CDS response as the default cluster, so nothing showed. A *later*
  one would warm for 15 s: a Service gaining a port after the node depends on
  it, or a service joining the capture TCP set (a TCPRoute attached) after it
  is in the dependency set. For a floor that means every captured TCP
  connection to the service is closed, because `tcp_proxy` has no cold path.

The fix is the same for every EDS case. The cluster's
`eds_cluster_config.service_name` is its own cluster name, and the agent
republishes the bare `ClusterLoadAssignment` under that name in the same
snapshot pass that emits the cluster (`proxy.LoadAssignmentAlias`, the one
helper for twins, aliases and floors). The TCP per-port clusters
`tcp:<fqdn>:<port>` keep their own port-filtered membership, now named after
the cluster rather than the HTTP spelling `<fqdn>:<port>`. In `/config_dump`,
only a service's default cluster `<svc>.<ns>.<domain>` may carry
`service_name: <ns>/<svc>`. Every other EDS cluster's `service_name` equals its
own name.

Three gates keep it that way:

- `//agent/internal/xds/cache` `TestNoNonDefaultClusterSharesTheBareServiceEDSName`
  and `TestLate{PortAlias,TCPFloor}SubscribesToItsOwnEDSResource`.
- `//agent/test/envoy_validate` `ClustersSharingServiceEDSName`, over every fixture.
- `//agent/test/mtlspool` `TestLate{PortAlias,TCPFloor,QUICTwin}*`, against the
  pinned proxy, each with a shared-name negative control that must time out at
  ~15 s.

Any new cluster, secret or config that is a clone or second consumer of an
existing resource needs either its own resource name or its own
`api_config_source`.

**Fleet gate.** HTTP port aliases and per-port HTTP clusters keep the default
cluster's `alt_stat_name` (the bare `<ns>/<svc>`), so their stats land in the
default cluster's `cluster.<ns>/<svc>.*` tree. L4 clusters no longer do (#1023):
the TCP floor reports as `tcp_<ns>/<svc>`, a TCP per-port or primary-port alias
cluster as `tcp_<ns>/<svc>_<port>`, a UDP floor as `udp_<ns>/<svc>` (see "L4 stat
keys and the L4 access log"). A twin has its own key (`<ns>/<svc>@<ns>/<sa>`,
#960). A mesh EDS cluster that follows the invariant never times out, so the gate
is zero on the whole family, twins included, across a soak:

```promql
# #1013 (HTTP aliases, per-port and default clusters as <ns>/<svc>; L4 clusters as
# tcp_<ns>/<svc>[_<port>] -- the selector matches both, as it did before #1023)
sum(increase(envoy_cluster_init_fetch_timeout_total{aether_cluster=~"[^@]+/[^@]+"}[8h]))   # MUST be 0
# #1008 (QUIC twins)
sum(increase(envoy_cluster_init_fetch_timeout_total{aether_cluster=~".+@.+"}[8h]))         # MUST be 0
```

A non-zero first line says which service and, since #1023, whether it was an L4
cluster (and which port) or the HTTP family. Tell the HTTP clusters apart with
`/config_dump`, as above, and the proxy log line
`initial fetch timed out for …ClusterLoadAssignment`.

### Attributing a prober failure: its labels and the `AETHER_PROBE_FAIL` line (#1040, #1041)

**Labels.** `aether_probe_requests_total` and `aether_probe_request_duration_seconds`
carry:

| label | value | set by |
|---|---|---|
| `node` | the **Kubernetes node** the prober pod runs on (`main-worker-03`): the **source** of the probe, never where it went | the collector, from the resource's `k8s.node.name` (chart env `OTEL_RESOURCE_ATTRIBUTES`, downward API `spec.nodeName`) |
| `pod` | the prober pod (`prober-h2mzs`) | the prober, as a datapoint attribute |
| `tier` | `liveness`, `reachability`, `mesh_dns` | the prober |
| `target` | the probed name (`egress`, `echo.aether-test.aether.internal:18081`, …): what was asked for, not the endpoint that would have answered | the prober |
| `result` | `success`, `http_error`, `connection_error`, `timeout`, `saturated`, `dns_error`, `dns_nxdomain`, `dns_timeout` | the prober (`classifyFailure`: the error, plus the request phase the deadline interrupted) |

**There is no destination label (#1391).** A burst grouped `by (node)` is placed at its
source. Where each failed probe went is not in the metric and cannot be: a label for it
would be a series per endpoint. It is found from the failure line, through the access
logs: see "Where a failed probe went" below.

**`timeout` vs `dns_timeout` (#1252).** A probe whose deadline fires while its name
lookup is still in flight is `dns_timeout`. Before #1252 it was `timeout`: Go's
transport dials on a context detached from the request and, when the request deadline
fires first, returns that deadline (`context deadline exceeded`), never a DNS error, so
the prober could not tell a resolution stall from a connect or upstream stall. Every
probe now carries an `httptrace` trace, and the deadline is classified by the phase it
interrupted. A deadline in any later phase (connect, TLS, write, waiting for the first
response byte) is still `timeout`; which one it was is the `phase` field of the
failure line below, not a label. In series from before #1252, part of the mesh_dns
`timeout` count was resolution stalls; from #1252 on, those count as `dns_timeout`.

**Until #1041, `node` held the POD name** (`node="prober-h2mzs"`). The prober's resource
carried `host.name`, and for a pod that is not hostNetwork that is the pod name. The
talos collector's `transform/promote` set `node` from `host.name` before it looked at
`k8s.node.name`. A pod that had since been rolled away could not be placed on a node
(#1040). Two changes fix this: the prober no longer sets `host.name`, and the
collector now prefers `k8s.node.name`
(bpalermo/k8s-talos-main#128). Series from before the
fix still show a pod name in `node`. For any window that spans the change, group by
`pod`, which exists only on series from after the fix, or translate the old values with
`kubectl -n aether-test get pods -o wide` while those pods still exist.

Group per node with `by (node)`, and per prober generation with `by (node, pod)`. A
DaemonSet roll starts a new `pod` series on the same `node`, so only a `by (node)`
aggregate spans the roll:

```promql
# per node, across prober generations
sum by (node, tier, result) (increase(aether_probe_requests_total{result!="success"}[10m]))
# per prober pod: a new generation starts a new series
sum by (node, pod, tier, result) (increase(aether_probe_requests_total{result!="success"}[10m]))
```

Any rule that guarded against dead prober generations with
`and on (node) max by (node) (present_over_time(...[3m]))` (#47) relied on `node` being
the pod. Now that `node` is the node, a new pod on the same node satisfies that guard
for a dead pod's frozen burst. Guard `on (node, pod)` instead.

**The failure line.** Every probe that does not succeed prints one line to the prober's
stdout. It follows the soak's k6 `AETHER_FAIL` convention: a fixed marker, then one
JSON object:

```
AETHER_PROBE_FAIL {"t":"2026-09-28T04:37:52.114Z","tier":"mesh_dns","target":"echo.aether-test.aether.internal:18081","result":"timeout","err":"Get \"http://echo.aether-test.aether.internal:18081/\": context deadline exceeded","elapsed_ms":2000.4,"phase":"first_byte","reused":false,"conn_ms":412.6,"dns_ms":0.9,"connect_ms":411.5,"tls_ms":-1,"write_ms":0.1,"ttfb_ms":1587.6,"dial":"10.96.14.7:18081","remote":"10.96.14.7:18081","local":"10.244.3.114:49292","trace_id":"9fa32b3befe77ea2253e8831d3472fa8","pod":"prober-h2mzs","node":"main-worker-01","n":1,"truncated":false}
```

- `t` is the client-side timestamp. Line it up against the proxy's hot-restart
  parent-exit time and the mesh-dns handoff on `node`.
- `elapsed_ms` separates a probe that used its whole budget (`timeout` at about 2000 ms,
  meaning the request went out and nothing came back) from a fast `connection_error`
  (a refusal or reset in a few ms, meaning nothing was listening).
- `phase` (#1252) is the step of the request the probe was in when it ended: the one
  the deadline interrupted for a `timeout`/`dns_timeout`, the one that failed for a
  `connection_error`. One of `dns`, `connect`, `tls`, `write` (connection up, request
  not fully written), `first_byte` (request written, nothing back yet), `headers`
  (first byte back, headers incomplete), `conn_wait` (waiting for a connection with no
  dial step running), `response` (an `http_error`: the answer arrived, the status is
  the failure), or empty when the probe never reached the transport (`saturated`). The example above connected after a slow 411 ms and then waited 1.6 s
  for the proxy to answer: the time went to the proxy, not to DNS. (Its phase values are
  illustrative; lines written before #1252 carry no phase fields.)
- `dns_ms`, `connect_ms`, `tls_ms`, `write_ms`, `ttfb_ms` are each phase's duration in
  ms (`ttfb_ms` is from the request written to the first response byte). `conn_ms` is
  everything it took to get a connection (pool wait, resolution, connect, TLS). The
  phase the probe ended in is measured up to the failure. **`-1` means the phase never
  started**: `dns_ms` is always `-1` on the liveness and reachability tiers (they dial
  an IP), and `dns_ms`/`connect_ms` are `-1` on a reused connection.
- `reused` is `true` when the probe ran on a pooled keep-alive connection (liveness,
  reachability). The mesh_dns tier never reuses, so it is always `false` there.
- `pod` and `node` are the prober's own: the **source**.
- `dial`, `remote`, `local` and `trace_id` (#1391) are what the client knows of where
  the probe went. `dial` is the address of the last connect attempt, which for the
  mesh_dns tier is what the name resolved to (empty on a reused connection and when the
  lookup failed). `remote` and `local` are the two ends of the connection the request
  was sent on (both empty when the probe never had one: a refused or blackholed
  connect, a lookup that failed). `trace_id` is the trace id of the `traceparent`
  header the probe sent (empty for `saturated`, which sent nothing). Lines written before #1391 have none of the four.
- Every key is present on every detail line, so a query can filter on any of them
  (`AND "\"phase\":\"dns\""`).
- `err` is the Go error string. For `http_error` it is `HTTP <status>`. For `saturated`
  the probe was never sent because `--max-concurrent` probes were already in flight.

**Where a failed probe went (#1391).** The client cannot say. On the mesh its
connection ends at the node proxy of its own node, so `remote` is the address it
dialled: `127.0.0.1:18081` for the liveness and reachability tiers, the mesh Service's
address for mesh_dns (the capture diverts the connection and keeps the original
destination). It is never the pod that would have answered, and the proxy adds no
response header that names its upstream. What the line has instead is the key to the
proxies' own records of that one request: `trace_id`, the trace id of the `traceparent`
header the probe sent. The HTTP access log has a `traceparent` field, and every row of
the request carries that trace id in it:

```
log_name:aether_access_logs AND traceparent:"00-9fa32b3befe77ea2253e8831d3472fa8-"*
```

**The trace id, not the whole header.** A proxy with tracing on writes its own span id
into the header at every hop, also for a request that is marked not-sampled, as every
probe is. The rows of one probe therefore share the 32 hex digits of the trace id and
differ in the 16 that follow, and a search for the header exactly as the prober sent it
finds nothing. Two rows of one mesh_dns probe, read from a test cluster on 2026-10-08:

```
reporter=source       node_name=main-worker-01  traceparent=00-7562d29beca08c9375101ae165a94196-5fe29c53fe07c4da-00  downstream_remote_address=10.244.3.114:47098  upstream_host=10.244.4.106:18008
reporter=destination  node_name=main-worker-02  traceparent=00-7562d29beca08c9375101ae165a94196-64629e6a312a85b9-00  upstream_host=127.0.0.1:8080
```

- The `reporter:source` row is the prober's own node proxy. Its `upstream_host` is the
  endpoint it chose (`<pod ip>:18008`), its `upstream_cluster` the service, and its
  `response_flags` what the proxy thinks happened (`DC`: the prober gave up first). Its
  `downstream_remote_address` is the line's `local`, and its `downstream_local_address`
  the line's `remote`. Translate the pod IP to a node with `kubectl get pods -A -o wide`
  while the pod exists, or from that pod's own access-log rows (`node_name`).
- A `reporter:destination` row with the same trace id, written by another node's
  proxy, means the request arrived there: the destination is that row's `node_name`.
  No such row means it did not arrive, or the destination proxy has not logged it yet
  (a row is written when the stream ends).
- **No row at all** means the request never became an HTTP request at the source
  proxy: `phase` is `dns` or `connect`, and `dial` is all there is. A liveness probe
  has no row either way: its route is a local reply, excluded from the access log.

What was measured and what was not: the two rows above, and that a prober row's
`downstream_remote_address` is the prober pod's `ip:port` and its
`downstream_local_address` the mesh Service address, were read from a test cluster's
access logs for probes that succeeded. A failed probe's line has not yet been joined to
its rows on a cluster: the prober that prints `trace_id` had not been deployed when
this was written.

It is **bounded**: at most 20 detail lines per `(tier, result)` per minute (`n` counts
them, and `truncated:true` marks the 20th). Later failures in that minute are only
counted, and when the minute closes they produce ONE summary line under the same marker:

```
AETHER_PROBE_FAIL {"t":"…","tier":"mesh_dns","result":"timeout","suppressed":122,"window_s":60,"window_start":"…","pod":"prober-h2mzs","node":"main-worker-01"}
```

A summary's `t` is when it was written, and the failures it counts lie between
`window_start` (the minute's first failure) and `t`. A prober built before the fix for
#1463 has no `window_start`, and dates the summary it writes as it stops 60 s in the
future: a last line dated after the pod was gone is that.

A 30 s burst of about 142 timeouts therefore prints 20 lines plus one summary, not 142
lines. The budget renews every minute, so the next burst is still attributable.

Pull the lines from VictoriaLogs (the logs are not in Loki):

```
_stream:{k8s.namespace.name="aether-test"} AND "k8s.container.name":prober AND "AETHER_PROBE_FAIL"
```

To see one pod or one class, add `AND "prober-h2mzs"` or `AND "\"result\":\"timeout\""`.
To read them straight from a live pod:
`kubectl -n aether-test logs <prober-pod> | grep AETHER_PROBE_FAIL`.

### Forwarded DNS keeps failing after a kube-dns roll

The mesh-DNS forward path keeps a small pool of **connected** UDP sockets per upstream
(issue #674) rather than dialling one per query. The upstream is a ClusterIP, so
connecting pins a conntrack entry to **one** kube-dns backend pod; when that pod rolls
the entry survives pointing at a corpse and datagrams are black-holed with **no ICMP** —
the socket only ever sees a read timeout.

This self-heals: any exchange error retires the socket, and every socket also expires on
its own budget (a jittered ~30s, or 1000 queries). Symptoms are therefore a burst of
`aether_mesh_dns_forward_conn_recycles_total{reason="error"}`, not a sustained outage.

**Forward timeouts (#1254).** A pod's resolver gives up on a datagram after 1 s and
sends it again (`timeout:1 attempts:3`), so the forwarder retries inside that window.
For each upstream, in order:

| step | bound | what happens |
|---|---|---|
| first try | 600 ms (`forwardTryTimeout`) | the query goes out on a pooled socket (or a fresh one when every slot is busy or pooling is off) |
| one re-send | until 2 s from the start (`forwardTimeout`, the per-upstream budget) | only if the first try **timed out**: the query is sent once more on a freshly dialled socket (a new source port, so a new conntrack entry and possibly another kube-dns backend). The first socket keeps listening; the first reply from either socket is delivered and the other is read and dropped |
| next upstream / SERVFAIL | | neither answered within 2 s |

- A lost datagram costs about 0.6 s plus one round trip, inside the client's 1 s
  window. Before, it cost 2 s and then a cold dial.
- A black-holed upstream fails at 2 s with exactly one re-send. Before, a pooled socket
  waited 2 s and its cold-dial fallback waited another 2 s, so 4 s per upstream. The
  budget is per upstream: N dead upstreams still cost N x 2 s.
- A reply that arrives within 2 s is accepted, as before. The change only makes answers
  come sooner.
- A first try that fails **fast** (ECONNREFUSED from an ICMP port-unreachable, a write
  error) does not re-send. It works as before: the pooled socket is retired and the
  query gets one cold dial (`forward_conn_dials_total{reason="fallback"}`).
- A pooled socket whose reply came back after the re-send went out is healthy and is
  kept. One that got nothing by 2 s is retired (`reason="error"`), because that is what a
  stale conntrack entry looks like.
- TCP queries and the TC=1 re-fetch over TCP do not re-send: TCP retransmits on its own.

Both bounds are compile-time constants in `agent/internal/meshdns`, like the 2 s timeout
before them. They follow the clients' resolv.conf timeout, so there is no flag and no chart
value.

How often the re-send fires, and why:

```promql
# Re-sends per forwarded query, by which reply was delivered:
#   resend = the re-send answered (datagrams to the upstream are being lost)
#   late   = the first try answered after 600 ms (the upstream is slow, not lossy)
#   failed = nothing within 2 s (the upstream is down or black-holed)
sum by (result) (rate(aether_mesh_dns_forward_retries_total[5m]))
  / scalar(sum(rate(aether_mesh_dns_queries_total{result=~"forwarded|forward_error"}[5m])))
```

A steady `late` share means kube-dns answers slower than 600 ms. That is a kube-dns
capacity problem, and every such query also pays an extra datagram and a dial
(`forward_conn_dials_total{reason="retry"}`).

If it does NOT settle:

```bash
# Dials per forwarded query -- should be well under 0.01, and is 1.0 when pooling is off.
sum(rate(aether_mesh_dns_forward_conn_dials_total[5m]))
  / sum(rate(aether_mesh_dns_queries_total{result="forwarded"}[5m]))

# Open pooled sockets -- flat at pool size x upstreams at steady state.
aether_mesh_dns_forward_conn_pool_open
```

To rule the pool out entirely, disable it — `--forward-pool-size=0` restores the exact
pre-#674 dial-per-query behaviour:

```bash
helm upgrade ... --set agent.meshDnsDaemon.forwardPoolSize=0
```

### Grading the mesh-DNS lame-duck handoff across a roll

On SIGTERM the resolver keeps serving until it has PROVEN a successor is answering on
the shared SO_REUSEPORT address, and only then closes its sockets (issue #729). Every
window ends with exactly one exit, counted by `reason`:

| `reason` | meaning |
| --- | --- |
| `successor` | a DIFFERENT instance answered on our listen address — the handoff drained. **This is what a healthy DaemonSet roll must show on every node.** |
| `deadline` | `--lame-duck-max` elapsed with no successor. Expected on a scale-down or node drain; on a roll it means the successor never came up, and the queued datagrams were dropped as they were before #729. |
| `signal` | a second termination signal cut the window short. |

The exit is a **once-per-process event**, so the counter carries an `instance` attribute
— this process's 64-bit lame-duck identity stamp, exactly one value per mesh-DNS
generation (issue #736). Without it every successor restarts the same `{node,reason}`
series at 1, Prometheus never sees a reset, and `increase()` over a window spanning
several rolls reads ~0 (the #729 validation had to count 50 exits from raw per-roll
snapshots). With it each generation is its own series and a genuine 0 → 1 rise:

```promql
# Exits per node over the last 30m. After two rolls this is 2 per node -- it counts
# every generation, which the pre-#736 series could not.
sum by (node) (increase(aether_mesh_dns_lame_duck_exits_total[30m]))

# The only breakdown that matters: everything must be reason="successor".
sum by (node, reason) (increase(aether_mesh_dns_lame_duck_exits_total[30m]))

# How long each generation held its sockets open, p95 (10s is --lame-duck-max,
# i.e. a window that ran to its deadline).
histogram_quantile(0.95,
  sum by (le) (rate(aether_mesh_dns_lame_duck_duration_seconds_bucket[30m])))
```

> **Sample count.** A generation records its exit and then dies, so its series is
> exported by the shutdown flush and usually carries a **single** sample. `rate()` and
> `increase()` need two samples in the range to produce anything for a series, so if a
> window you know had rolls still comes back empty, count the series instead — each one
> tops out at exactly 1, so this is exact regardless of how many samples landed:
>
> ```promql
> # Rolls per node, sample-count independent.
> sum by (node) (max_over_time(aether_mesh_dns_lame_duck_exits_total[30m]))
> sum by (node, reason) (max_over_time(aether_mesh_dns_lame_duck_exits_total[30m]))
> ```
>
> `max_over_time` is also what survives the series ageing out with its generation — the
> same trap that produced two premature "zero" readings on #638.

`instance` is also the join key back to the logs: the same stamp appears as `instance=`
on that generation's `lame duck started` and `successor observed` lines (and as
`successor=` on the *predecessor's* line, naming the generation that replaced it), so a
suspicious series resolves to one pod's window.

```
_stream:{service.name="aether-mesh-dns"} AND instance:"<stamp>"
```

Cardinality is one series per generation per node — bounded by how often the DaemonSet
rolls, and the series age out with the generation. That ageing-out is why an **instant**
query is never the right read here: minutes after a roll the generation's series is
gone, and the instant read returns nothing at all rather than the exit it recorded.

### mesh-dns CPU throttling

mesh-dns is every managed pod's resolver, so a CFS-throttled period on it is added
to the DNS latency of every lookup in flight on that node. Until chart 2.4.8 it ran
with a `25m` request and a `100m` limit and was throttled in 8.7 % of 100 ms periods
at steady state (talos w05, #1253) while *averaging* ~24.5m: its CPU comes in bursts,
and a burst that spends the period's 10 ms of quota parks the daemon until the next
period. Since #1253 it has a `50m` request and **no CPU limit** by default, with
`GOMAXPROCS=2` pinned in the chart (`agent.meshDnsDaemon.goMaxProcs`).

Read the throttling straight from the container's cgroup. The image is distroless
(no shell), so go through a node debug pod; the `find` works under both the
`systemd` and `cgroupfs` cgroup drivers:

```bash
NODE=<node>
POD=$(kubectl get pod -n aether-system -l app.kubernetes.io/component=mesh-dns \
  --field-selector spec.nodeName="$NODE" -o jsonpath='{.items[0].metadata.name}')
CID=$(kubectl get pod -n aether-system "$POD" \
  -o jsonpath='{.status.containerStatuses[?(@.name=="mesh-dns")].containerID}' | sed 's|.*://||')
kubectl debug node/"$NODE" -it --profile=sysadmin --image=busybox:1.37 -- sh -c \
  "d=\$(find /host/sys/fs/cgroup -type d -name \"*$CID*\" | head -1); echo \$d; cat \$d/cpu.max \$d/cpu.stat \$d/cpu.pressure"
```

- `cpu.max` is `max 100000` with no limit, `10000 100000` at `100m`.
- `cpu.stat`: `nr_throttled / nr_periods` is the fraction of periods throttled and
  `throttled_usec` the total time parked. With no limit both stay at 0 — `nr_periods`
  only counts while a quota is set. `usage_usec` over the pod's age is its average CPU.
- `cpu.pressure`: the PSI `some avg10/avg60` is the share of time the daemon had a
  runnable task waiting for CPU for *any* reason — quota throttling or node
  contention. With no limit, this is the number to watch: non-zero means the `50m`
  request (its CFS weight) is too low for the node it shares.

If the kubelet's cAdvisor is scraped, the same counters are
`container_cpu_cfs_throttled_periods_total` / `container_cpu_cfs_periods_total`:

```promql
# Fraction of CFS periods throttled, per mesh-dns pod (0 once the limit is gone)
sum by (pod) (rate(container_cpu_cfs_throttled_periods_total{namespace="aether-system", container="mesh-dns"}[5m]))
  / sum by (pod) (rate(container_cpu_cfs_periods_total{namespace="aether-system", container="mesh-dns"}[5m]))

# CPU actually used, millicores
1000 * sum by (pod) (rate(container_cpu_usage_seconds_total{namespace="aether-system", container="mesh-dns"}[5m]))
```

What the throttling costs shows in the daemon's own latency histogram, split by path
(`answered` = an authoritative mesh hit from memory, `forwarded` = a kube-dns round
trip). Throttling lifts the tail, not the median:

```promql
histogram_quantile(0.99,
  sum by (le, result) (rate(aether_mesh_dns_query_duration_seconds_bucket{result=~"answered|forwarded"}[30m])))
```

To put a cap back, set `agent.meshDnsDaemon.resources.limits.cpu` (an empty value
renders no limit) and consider `agent.meshDnsDaemon.goMaxProcs=0`, which hands
`GOMAXPROCS` back to the Go runtime to derive from that limit.

### uds-csi CPU throttling (and the controller's and registrar's CPU limits)

The `csi.aether.io` plugin serves `NodePublishVolume` / `NodeUnpublishVolume`, so a
CFS-throttled period on it is added to the start (or the termination) of a UDS pod
on that node. Until chart 2.4.10 it ran with a `5m` request and a `100m` limit, and
the 2026-10-06 8 h soak — the first with cAdvisor scraped — showed it throttled in
**57.6 %** of the periods it ran in (20,248 of 35,169, 15 pods, #1321) while using
11–23 CPU-seconds per pod over the whole run. Same mechanism as mesh-dns above: an
average far under the limit, delivered in bursts (a publish; and every 30 s the
liveness probe, a second Go process inside the same cgroup) that spend a period's
10 ms of quota and park the container until the next one. Since #1321 it keeps the
`5m` request and has **no CPU limit** by default, with `GOMAXPROCS=2` pinned in the
chart (`udsCsi.goMaxProcs`).

The same soak measured the controller at 1.0 % and the registrar at 1.5 %. Their
`100m` limits were **kept**: neither is on a request path (a throttled period is
under 100 ms on an admission call with a 5 s timeout, or on an endpoint update on
its way to the agents), the registrar's `requests == limits` is what makes it QoS
Guaranteed, and both derive `GOMAXPROCS` from `limits.cpu`. The reasoning is next to
`controller.resources` and `registrar.resources` in `charts/aether/values.yaml`.

Fraction of CFS periods throttled, per container, over a window (cAdvisor; set the
range to the run you are grading):

```promql
sum by (container) (increase(container_cpu_cfs_throttled_periods_total{namespace="aether-system", container!=""}[8h]))
  / sum by (container) (increase(container_cpu_cfs_periods_total{namespace="aether-system", container!=""}[8h]))
```

Per uds-csi pod, as a rate, with the time parked and the CPU actually used:

```promql
# Fraction of CFS periods throttled, per uds-csi pod
sum by (pod) (rate(container_cpu_cfs_throttled_periods_total{namespace="aether-system", container="uds-csi"}[30m]))
  / sum by (pod) (rate(container_cpu_cfs_periods_total{namespace="aether-system", container="uds-csi"}[30m]))

# Seconds parked per second
sum by (pod) (rate(container_cpu_cfs_throttled_seconds_total{namespace="aether-system", container="uds-csi"}[30m]))

# CPU actually used, millicores
1000 * sum by (pod) (rate(container_cpu_usage_seconds_total{namespace="aether-system", container="uds-csi"}[30m]))
```

**A container with no CPU limit has no `container_cpu_cfs_*` series at all** —
cAdvisor only exports them for a cgroup with a quota — so after #1321 the uds-csi
queries return *no data*, not `0`, and the first query simply stops listing
`uds-csi` (as it does not list `agent`, `mesh-dns` or `proxy`). Read "no data" as the
pass, but prove the scrape is alive with the usage query, which must still return
one series per pod, and confirm the limit is really gone:

```promql
# Expect: empty. Any series here is a uds-csi container that still has a quota
# (an old pod not yet rolled, or an operator-set limits.cpu).
count by (pod) (container_cpu_cfs_periods_total{namespace="aether-system", container="uds-csi"})
```

```bash
kubectl get ds -n aether-system aether-uds-csi \
  -o jsonpath='{.spec.template.spec.containers[0].resources}{"\n"}'   # no limits.cpu
```

With no quota, what can still make the plugin wait is node contention, where its
`5m` request is its CFS weight. The cgroup's `cpu.pressure` shows that; read it with
the node-debug recipe in the mesh-dns section above, with
`app.kubernetes.io/component=uds-csi` and container name `uds-csi` (the image is
distroless too). A `some avg60` that stays above zero is the signal to raise
`udsCsi.resources.requests.cpu`.

To put a cap back, set `udsCsi.resources.limits.cpu` (an empty value renders no
limit) and consider `udsCsi.goMaxProcs=0`, which hands `GOMAXPROCS` back to the Go
runtime to derive from that limit.

### cni-install CPU limit (an init container cAdvisor never sees)

`cni-install` is the agent pod's init container: it runs ahead of the agent on every
agent pod start (a roll, a node boot, a crash restart). Until chart 2.4.12 it had
request = limit = `100m` CPU. Since #1335 it keeps the `100m` request and has **no CPU
limit**.

**The throttling could not be read from metrics, and it will not be for any
sub-second init container.** On talos-main (2026-10-07, cAdvisor scraped every ~60 s
for the `aether-.*` namespaces) there is no `container_cpu_cfs_*` and no
`container_cpu_usage_seconds_total` series for `container="cni-install"` at all — nor
for the proxy pod's `install-supervisor` — in the 45 h cAdvisor had been scraped,
eight agent rolls included: the container's cgroup lives for under a second and never
meets a scrape. kube-state-metrics exports only
`kube_pod_init_container_status_restarts_total` there (no `..._state_started`), and
`kubectl` shows `startedAt`/`finishedAt` at one-second resolution and no CPU time. So
the evidence is the installer's own log timestamps (nanosecond, in VictoriaLogs), and
the CPU figure below is an **inference**, not a counter.

What it does (`cni/internal/install`): Go start-up, then for the plugin binary a
byte-for-byte compare of the copy already on the host against the one in the image
(#1123: an unchanged plugin is not rewritten), then a few small conflist reads. On
that path it writes nothing, and the timings below say the wait is the quota rather
than the reads: disk latency does not come in 100 ms units. All 40 agent starts of
2026-10-05..07 (8 rolls × 5 nodes) took the unchanged path:

| First log line → last, 40 starts | |
|---|---|
| min / median / max | 496 ms / 598 ms / 802 ms |
| "running CNI installer" → "already installed and unchanged" (the compare) | 400–700 ms |
| every other gap between consecutive lines | ~0–10 ms or ~90–100 ms |

Every total is a multiple of ~100 ms (500, 600, 700, 800) and every gap is either
nothing or a whole CFS period: the process spends its 10 ms of quota, is parked until
the next 100 ms period, and repeats. Five to eight periods at 10 ms each bounds the
work at 50–80 ms of CPU, so roughly half a second of each agent start was the quota
(estimated; Go start-up before the first log line is not visible and is throttled the
same way). A start that actually rewrites the plugin (a new CNI build: copy, fsync,
directory sync) was not in the sample and can only be longer.

To repeat the measurement on any chart version (LogsQL, VictoriaLogs; group the lines
by `k8s.pod.name` and subtract the first `timestamp` from the last):

```logsql
_time:48h "cni-install" ("installing CNI binaries" OR "running CNI installer" OR "not rewriting it" OR "CNI binaries installed")
  | sort by (_time) | fields _time, _msg, k8s.pod.name, k8s.node.name
```

After 2.4.12 the totals should fall to tens of milliseconds with no ~100 ms steps (not
yet measured on a cluster when this was written). If the steps are still there, check
that the limit is really gone:

```bash
kubectl get ds -n aether-system aether-agent \
  -o jsonpath='{.spec.template.spec.initContainers[0].resources}{"\n"}'   # no limits.cpu
```

The other init containers, checked in the same pass: the proxy pod's
`install-supervisor` rendered **no `resources` at all** until chart 2.4.13 (no limit,
so nothing to throttle, but also no request: it ran at the minimum CFS weight and was
invisible to request accounting; ≤ 1 s wall on talos-main). Since #1344 it has a
`100m` / `32Mi` request, a `64Mi` memory limit and no CPU limit
(`proxy.supervisor.resources`); the self-copy it does costs about 50 ms of CPU and
peaks at 14 MB RSS (a workstation build of the binary, `/usr/bin/time -v`). The
`authz` native sidecar has a `10m` request and no CPU limit, and the webhook-injected
`aether-identity-ready` has a `5m` request and no CPU limit
(`controller.webhook.identityGate.resources`, `limits.cpu: ""`). The edge pod's
`agent` container has no CPU limit either, so its `GOMAXPROCS` is pinned in the chart
(`edge.goMaxProcs`, default 2, #1335) like the node agent's.

```bash
kubectl get ds -n aether-system aether-proxy \
  -o jsonpath='{.spec.template.spec.initContainers[0].resources}{"\n"}'   # requests, limits.memory, no limits.cpu
```

### Edge Envoy worker count

Until chart 2.4.13 the edge `envoy` container was started without `--concurrency`, so
it ran one worker per node core (4 on talos-main) for about 12m of CPU on average.
Since #1344 the chart passes `--concurrency` from `edge.concurrency` (default `2`, the
node proxies' count on talos-main; `0` omits the flag). Unlike `proxy.concurrency`,
changing it is an ordinary rolling update of the edge Deployment: there is no hot
restart and nothing shared between the old and the new replica.

To read the count a running edge Envoy actually has, before and after the upgrade
(the first is the pod spec, the second is Envoy's own gauge in the OTLP stats; expect
one series per edge replica, each equal to `edge.concurrency`, or, when it is `0`, to
Envoy's default: the node's core count unless the container has a CPU limit or a
restricted CPU mask, which lower it):

```bash
kubectl get deploy -n aether-ingress aether-edge \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="envoy")].command}{"\n"}'
```

```promql
envoy_server_concurrency{job="aether-edge-proxy"}
```

A worker count that is too low shows up as the edge Envoy's CPU approaching
`edge.concurrency` whole cores
(`sum by (pod) (rate(container_cpu_usage_seconds_total{container="envoy", namespace="aether-ingress"}[5m]))`);
at 12m it is over two orders of magnitude away.

### What the proxy supervisor does on SIGTERM (`kubectl delete pod`, drain, eviction)

The `aether-proxy` pod is `hostNetwork` with `maxSurge: 1`, so the node's Envoy is a
*node-scoped* resource that happens to live in a pod. Terminating that pod is only free
if the node's Envoy is handed to a successor rather than killed, and SIGTERM is not a
drain — Envoy's handler exits the server outright (0.33–0.74 s measured), taking every
connection on the node with it. So the supervisor resolves a SIGTERM onto exactly one of
four branches, chosen from one authoritative `/server_info` probe, and records which one
it took (issue #795):

| branch | when | what happens | data-plane cost |
| --- | --- | --- | --- |
| `handoff` | admin answers at a **newer** epoch — a successor already holds our listen sockets | wait, without signalling Envoy, for the successor's parent-shutdown protocol to terminate it | none |
| `successor_wait` | admin answers **LIVE at our own epoch** — no handoff has begun | keep serving and wait (bounded by `successorWaitBudget`, 155 s at the chart default) for the DaemonSet's surge replacement, which is created within ~1 s and hot-restarts us | none |
| `drain_fallback` | that wait expires, or `--shutdown-drain-immediately` is set | `POST /drain_listeners?graceful` **to our own Envoy only** (see below), wait `--drain-time`, then SIGTERM and reap | in-flight requests finish; new connections go to whatever else serves the node |
| `child_dead` | the admin does not answer at all | SIGTERM and reap; there is nothing to drain | already gone |

**The drain only ever reaches this pod's own Envoy (#1127).** `127.0.0.1:9901` is one
address for the whole node, so whoever answers it may be another proxy pod's Envoy: a
successor that took the admin over the hot-restart protocol, or a *fresh* Envoy the new
pod started after its successor crashed. Before #1127 the old pod drained whatever
answered. On 2026-10-01 that was the new pod's fresh epoch-0 Envoy, which then added no
listeners for pods created on the node until the next handoff (about 12 min on w01 and
w03). Now every supervisor starts its Envoys with `--admin-address-path
<ready-marker dir>/envoy-admin-address.<pod>.<nonce>`, a per-supervisor nonce that
`/server_info` echoes as `command_line_options.admin_address_path`. The drain is sent only
when `/server_info` returns that nonce **and** an epoch whose child this supervisor still
tracks. The check and the drain go over **one** TCP connection, so the drain reaches the
process that passed the check, or nothing. If the check fails, or our Envoy has already
exited, the supervisor sends nothing, logs one WARN, and goes on to SIGTERM its own
Envoy:

```
not sending envoy admin request: the shared admin address is answered by ANOTHER envoy, not this supervisor's …
    adminOwner=foreign answeredBy=envoy-admin-address.aether-proxy-<new>.<nonce> answeredEpoch=0 ourEpoch=1
not sending envoy admin request: this supervisor's envoy has exited, …   adminOwner=own_envoy_gone
```

`listeners drained; stopping envoy` then logs `drainAccepted=false`. The decision is
counted in `aether_supervisor_admin_mutations_total{aether_supervisor_admin_request="drain_listeners",
aether_supervisor_admin_owner=own|foreign|unreachable|own_envoy_gone}`, seeded at zero. Only
`own` sends the request. `foreign` during a roll means the old pod's Envoy had already
handed its admin to another Envoy, so it could not be drained gracefully. The supervisor
reserves `--admin-address-path` and refuses to start if an `--envoy-arg` sets it (as it
does for every other Envoy flag it passes itself; the list is in `docs/configuration.md`).

`kubectl delete pod aether-proxy-<x>` takes the **`successor_wait`** branch. Expect
**≈20–25 s** of termination (that is the successor initializing, not a hang) and **zero**
prober errors on that node. A 1–2 s termination is the symptom to be alarmed by: it means
the supervisor won the race against its own replacement and left the node with no Envoy —
which cost 7.95 s / 8.11 s of blackout and 130 / 126 prober `connection_error`s per delete
on rev214, before this branch existed.

The supervisor's lines **are** in VictoriaLogs. Since #772 the supervisor is PID 1 of
the pod's `proxy` container, so its stdout (and Envoy's, which it inherits) is that
container's log stream: `service.name` `aether-proxy`, `k8s.container.name` `proxy`.
They outlive the pod, so this is where to read a delete after the fact. The whole #1050
timeline (`live predecessor confirmed`, `successor ready gate anchored`, `liveness
watchdog fired`, `drain deadline elapsed`) came from this stream (#1059):

```
_stream:{service.name="aether-proxy"} AND "k8s.container.name":proxy AND "liveness watchdog fired"
```

Swap the phrase for any line below. Add `AND "<pod-name>"` for one pod. Keep the
container filter: `service.name="aether-proxy"` also matches the access logs (by
`log_name`). To follow a delete live instead:

```bash
kubectl -n aether-system logs -f <proxy-pod> -c proxy | tee /tmp/sigterm.log &
kubectl -n aether-system delete pod <proxy-pod>
```

Lines to look for, in order, on a healthy delete:

```
waiting for a successor before draining        successorWaitBudget=2m35s drainTime=10s
envoy epoch terminated by successor            epoch=N
successor took over during shutdown wait       epoch=N midHandoff=false
```

and on a termination where no successor can come (node shutdown, scale-down,
`kubectl delete daemonset`, a replacement stuck Pending/unschedulable):

```
no successor terminated our envoy within the termination-grace budget; …   (WARN)
no successor within budget; draining listeners  successorWaitBudget=2m35s drainTime=10s
listeners drained; stopping envoy               drainAccepted=true elapsed=10.0s
reaped envoy epoch                              epoch=N
```

Both are also gradeable without logs, which matters because the pod is gone by the time
you look. The branch counter is **seeded at zero for all four values** (#717), so an
empty result means the metric never arrived, not that nothing happened:

```promql
# Which branch each node's last terminating supervisor took. drain_fallback or
# child_dead during a rolling upgrade means the surge replacement never arrived;
# handoff during a rolling upgrade means the old pod was deleted mid-handoff
# (#991, see the next section) -- expected ~0 per roll.
sum by (node, aether_supervisor_shutdown_branch) (
  increase(aether_supervisor_shutdown_branch_total[30m]))

# How long a fallback drain actually took (the graceful window included).
histogram_quantile(0.95,
  sum by (le) (rate(aether_supervisor_drain_duration_seconds_bucket[30m])))
```

> **A dying process exports once.** The branch is recorded immediately before `Run`
> returns, and `supervisorcmd`'s deferred telemetry flush is what pushes it — a supervisor
> SIGKILLed at the grace period flushes nothing, so a *missing* branch sample on a node is
> itself the finding. Use `increase()`/`max_over_time`, never an instant read: like the
> mesh-DNS lame-duck series, these age out with the generation that wrote them.

### How a proxy roll hands the node over: the successor's ready gate and the parent's hold (#991)

A rolling upgrade (`maxSurge: 1`, `maxUnavailable: 0`) is safe only if the **old** pod
stays until the new pod's Envoy has taken over — the #132/#795 invariant. The DaemonSet
deletes the old pod about 1 s after the new one turns Ready, and at once if the old one
turns NotReady while a surge pod exists. Two supervisor rules decide both moments.

**1. The successor's ready gate is anchored on its first LIVE, not only on its fork.**
Envoy arms `--parent-shutdown-time-s` in `startWorkers()`, the step that also turns its
admin LIVE. The predecessor Envoy therefore exits at *workers + ParentShutdownTime*
(measured 14.93–15.00 s after `starting workers`, then 0.4–0.9 s to exit), not at
*fork + ParentShutdownTime*. The successor's supervisor goes Ready at

```
max(fork + ParentShutdownTime + 3s,  firstLiveObserved + ParentShutdownTime + 2s)
```

`ParentShutdownTime` is the chart's `proxy.hotRestart.parentShutdownTime` (15 s), the
value the supervisor passes to Envoy, not a literal. The fork anchor used to be the only
one. Under soak load on rev242 the successor took 3.2–6.0 s from fork to workers, so the
old Envoy was still serving at fork + 18 s, and the old pod was deleted mid-handoff in 13
of 30 rolls. Cost: Ready moves from fork + 18 s to roughly fork + 19.5–23 s under load,
1.5–5 s per node-roll. An unloaded successor that is LIVE within ~1 s keeps the fork gate.

**2. The old pod keeps its readiness through a busy successor's admin.** While the old
supervisor's Envoy is still tracked, it holds its ready marker whenever the admin answers
at another epoch (#132). During the handoff that admin port belongs to the **successor**,
whose main thread is busy loading its first listener batch before `starting workers`, so
a `/server_info` probe can miss its 1 s timeout. The hold used to drop on the first
miss, and three failed exec probes (2 s period) later the DaemonSet deleted the pod. That
happened in 9 of 30 rolls on rev242, 5.0–6.3 s after `pod not ready`. The hold now lasts
through an unreachable admin for up to the admin watchdog's existing bound
(`--admin-unresponsive-deadline`, 30 s default). A genuinely wedged admin still ends it at
that bound, on the same tick the admin watchdog restarts the container. With no Envoy of
ours tracked, an unreachable admin never holds.

Lines to grep in the `aether-proxy` container logs, per roll:

```
# successor pod: which anchor set its gate, and where the gate landed
live predecessor confirmed; starting cross-pod hot restart  ... readyGateIn=18s readyGateAnchor=fork
successor ready gate anchored   epoch=N anchor=live sinceFork=4.9s readyGateIn=17s readyGateAfterFork=21.9s parentShutdownTime=15s
pod ready: envoy live at newest epoch                       epoch=N

# old pod: the hold carried through the successor's busy init (once per unreachable streak)
holding readiness: serving as hot-restart parent mid-handoff              epoch=N-1
holding readiness through unreachable admin: successor likely initializing  epoch=N-1 unreachableFor=0s holdBound=30s
newest epoch terminated cleanly by successor; awaiting pod deletion        epoch=N-1
```

`anchor=fork` on the second line means the successor went LIVE fast enough that the fork
gate was already the later one. On a healthy roll the old pod logs **no**
`termination requested mid-handoff`. Its Envoy is terminated by the successor's
parent-shutdown first, the pod turns NotReady because nothing of its own is left
running, and the DaemonSet deletes an idle pod. So during a rolling upgrade:

- `aether_supervisor_shutdown_branch_total{branch="handoff"}` is **≈ 0 per roll**. The
  fleet gate for a soak is the per-node `increase()` over the roll window. A non-zero
  count means an old pod was deleted while its Envoy was still the serving parent, which
  is a finding again, not background. On rev242, before this fix, it was 1 per node on
  22 of 30 rolls.
- `termination requested mid-handoff` lines are **0**. One that comes 5–6 s after the same
  pod's `pod not ready` is rule 2 failing. One at about fork + 19 s, after the successor's
  `pod ready`, is rule 1 failing.

### The ext_authz sidecar across a proxy roll (#1275)

**Symptom (chart <= 2.4.8).** `ext_authz` errors on a node right after its proxy pod was
replaced: `envoy_http_<stat_prefix>_ext_authz_error_total` goes up by a few, and with
`failureMode: DENY` those requests got a 403. The prober's `AetherAuthzCanaryErrors` fires
on it. It is worst on the first roll onto a new sidecar image. On talos-main on 2026-10-05
(OPA 1.21.1, chart 2.4.8) the `authz` container started 7–13 s after `proxy` in every new
pod, because the kubelet starts regular containers in order without waiting and the new
image had to be pulled (10.4 s on worker-01). The new Envoy hot-restarted and took the
node's listeners before anything listened on `/run/aether/authz/authz.sock`.

**Since chart 2.4.9** `authz` is a native sidecar: an init container with
`restartPolicy: Always`, after `install-supervisor` and before `proxy`. Its startupProbe
execs the staged `proxy-ready --unix-socket=/run/aether/authz/authz.sock` in the sidecar's
container, and passes once a `connect(2)` to the socket succeeds.

What the ordering guarantees:

- **Startup.** The kubelet does not start `proxy` until the probe passes. A new pod's Envoy
  therefore cannot fork, hot-restart, or take a listener before its own authz accepts. The
  image pull and OPA's start now delay the new pod instead: it turns Ready later, and with
  `maxUnavailable: 0` the old pod serves meanwhile. If the sidecar never comes up, the new
  pod stays in `Init`, the roll stops, and the old pod keeps serving.
- **Shutdown.** On deletion the kubelet sends SIGTERM to `proxy` and stops `authz` only after
  `proxy` has exited. The old Envoy is a child of the supervisor in `proxy`, so it has its
  pod's authz for as long as it runs. That covers `successor_wait` after `kubectl delete pod`
  (about 20–25 s of serving, see *What the proxy supervisor does on SIGTERM*), the
  `drain_fallback` drain, and a parent that is still draining when its pod is deleted.
  Before 2.4.9 both containers got SIGTERM at once, and OPA closed its socket while the old
  Envoy was still the node's only Envoy. In a plain DaemonSet roll this matters less: the
  successor turns Ready only after the parent Envoy has exited (see *How a proxy roll hands
  the node over*), so the old pod is usually deleted after its Envoy is gone.
- **Both Envoys of a handoff have an authz.** Each Envoy reaches the socket in its own pod's
  `authz-socket` emptyDir. The old one uses the old pod's sidecar and the new one uses the
  new pod's sidecar.

What it does not guarantee:

- **A sidecar that dies mid-life.** The kubelet restarts it in place (`restartPolicy:
  Always`) while `proxy` keeps serving. Checks fail per `failureMode` until it is back, as
  before.
- **"Accepting" is not "has the latest policy".** For the OPA preset the gRPC listener opens
  after `/policy/policy.rego` has loaded, so a pass means a policy is in place (and a policy
  that does not compile means the probe never passes; see *Changing the OPA policy*). A
  bring-your-own sidecar that loads its policy after it binds the socket needs
  `startupProbe.override` with its own readiness test.
- **The grace period still applies.** The sidecar's shutdown shares the pod's
  `terminationGracePeriodSeconds` (`proxy.terminationGracePeriodSeconds`, 180 s). If the
  supervisor uses all of it, the kubelet kills both.
- **With `startupProbe.enabled: false`** the kubelet starts `proxy` as soon as the sidecar's
  process is running, not when it serves.

Check the order on a live pod. `authz` is listed under `initContainerStatuses`, and its
`started` turns true only when the probe passes. The `proxy` container's `startedAt` must
be later than that:

```bash
kubectl -n aether-system get pod <proxy-pod> -o jsonpath='{range .status.initContainerStatuses[?(@.name=="authz")]}authz started={.started} at {.state.running.startedAt}{"\n"}{end}{range .status.containerStatuses[?(@.name=="proxy")]}proxy at {.state.running.startedAt}{"\n"}{end}'
```

**Which `ext_authz` counters exist (#1325).** In Prometheus the filter's counters are
`envoy_http_<stat_prefix>_ext_authz_<name>_total` (`capture_http` is the capture
listener's prefix). A missing series means the event never happened on that proxy. It
does not mean the stat is filtered:

- Envoy's OTLP stats sink exports a counter only after it was first incremented. So
  `ok`, `denied` and `error` exist only for nodes whose proxy has run a check with that
  result. On talos-main that is the node carrying the canary. The first sample appears at
  its value, and `increase()` reads 0 for that first jump. An alert on these needs the
  "present now, absent 10 minutes ago" arm next to `increase()`:
  `sum(m unless m offset 10m) > 0`.
- `failure_mode_allowed` is incremented only when the filter runs fail-open
  (`proxy.authzSidecar.failureMode: ALLOW`, `--authz-sidecar-failure-mode-allow`). With the
  default `DENY` the series never exists.
- There is **no `timeout` counter**. Envoy's HTTP `ext_authz` filter does not define one
  (checked at the pinned Envoy, `ALL_EXT_AUTHZ_FILTER_STATS` in
  `source/extensions/filters/http/ext_authz/ext_authz.h`). A check that exceeds
  `proxy.authzSidecar.timeout` is counted in `error`. Do not gate on
  `…_ext_authz_timeout_total`: it reads as a clean zero forever.
- Neither the proxy's `stats_matcher` (an exclusion list,
  `charts/aether/templates/agent-proxy-configmap.yaml`) nor the collector drops any
  `ext_authz` stat.

The chart needs Kubernetes >= 1.29 for this and refuses to render with the sidecar enabled
on an older cluster. Kind e2e: `e2e/authz.sh` (nightly job `authz`), steps a to c. It evicts the OPA
image from the node and rolls the proxy under load. Against the 2.4.8 layout that gave
495 × 403 and 990 `ext_authz.error` in 4 s; against 2.4.9 it gave 0 and 0.

### Changing the OPA policy (#1383)

Since chart **2.4.19** the OPA preset's sidecar watches its policy
(`opa run --server --watch … /policy/policy.rego`). Changing
`proxy.authzSidecar.opa.policy` and running `helm upgrade` changes the ConfigMap
`aether-opa-policy` and nothing else: the proxy pods are not replaced and Envoy
is not hot-restarted.

**What was observed** (kind, Kubernetes 1.35.8, OPA 1.21.1, **one node**; these
are observations, not bounds):

| | Observed |
| --- | --- |
| `helm upgrade` returning to the new policy deciding | 55 s, 46 s, 64 s; the kubelet's delivery over eight changes was 27–64 s (the reload itself takes about 1 ms) |
| Proxy pod | same UID, `authz` and `proxy` restart counts unchanged, DaemonSet generation unchanged, Envoy `restart_epoch` unchanged |
| Requests through a reload | 8,607 of 8,607 answered 200, `ext_authz.error` +0 |

**It is unstaged and asynchronous.** A ConfigMap volume is refreshed by each
node's kubelet on its own schedule: the kubelet's sync period plus the delay of
its ConfigMap cache (watch, or a TTL, depending on the kubelet's configuration).
Nodes therefore get the new policy at different moments, in no order, and for a
while the fleet decides with two policies. Nothing bounds that window and nothing
reports its end: do not assume every node has converged after a minute; confirm
it per proxy pod (below). A DaemonSet roll used to apply a policy one node at a
time and stop at the first pod that failed to become Ready; that brake is gone.
The chart validates nothing, so validate the policy **before** you update it,
with the image the chart runs:

```bash
OPA_IMAGE="$(helm get values aether -n aether-system -a -o json | jq -r .proxy.authzSidecar.opa.image)"
# parses and compiles
docker run --rm -v "$PWD/policy.rego:/policy/policy.rego:ro" "$OPA_IMAGE" \
  check /policy/policy.rego
# the decision the sidecar queries is defined for ONE request you write down
# (request.json, below); prints it, exit 1 when undefined for that request
docker run --rm -v "$PWD/policy.rego:/policy/policy.rego:ro" \
  -v "$PWD/request.json:/policy/request.json:ro" "$OPA_IMAGE" \
  eval --fail -f pretty -d /policy/policy.rego --input /policy/request.json \
  'data.envoy.authz.allow'
# your own tests (*_test.rego next to the policy)
docker run --rm -v "$PWD:/policy:ro" "$OPA_IMAGE" test /policy
```

`request.json` is one request in the shape the sidecar receives from Envoy
(`input.attributes.request.http`, header names in lower case; a sidecar's
decision log shows real ones under `"input"`). Use a request the policy should
allow:

```bash
cat > request.json <<'EOF'
{
  "attributes": {
    "request": {
      "http": {
        "method": "GET",
        "path": "/",
        "host": "my-service.my-namespace.aether.internal:18081",
        "scheme": "http",
        "protocol": "HTTP/1.1",
        "headers": {
          ":authority": "my-service.my-namespace.aether.internal:18081",
          ":method": "GET",
          ":path": "/",
          "x-authz": "letmein"
        }
      }
    }
  },
  "parsed_path": [""],
  "parsed_query": {}
}
EOF
```

What each command proves, checked with OPA 1.21.1:

- `opa check` exits 1 for a policy that does not parse or compile, and 0 for
  anything else, including a policy that defines no `envoy.authz.allow`.
- `opa eval --fail --input request.json` says whether the decision is defined
  **for that one request** and prints it. The e2e policy (`default allow :=
  false`) prints `true` for the request above and `false` with another header
  value, exit 0 both times. A policy with no default prints `true` for a request
  that matches a rule and exits 1 (`undefined`) for one that matches none. A
  policy that does not define the decision exits 1 for every request. Without
  `--input` the policy is evaluated against no request, which rejects a correct
  policy that has no default and proves nothing about real requests.
- Only your own `opa test` cases cover behaviour: which requests are allowed and
  which are not. Neither command above does.

**A policy that does not parse or compile** (both measured: a `rego_parse_error`
and a `rego_type_error`):

- *A running sidecar* does not load it. It keeps deciding with the last good
  policy, does not exit, is not restarted, and `/health` stays 200. The mesh
  looks healthy.
- *A sidecar that starts* with that file on the node exits 1
  (`error: load error: … rego_parse_error`), because the last good policy lived
  only in the process that is gone. What follows depends on what restarted:
  - The `authz` container alone (an OOM kill, a crash): the kubelet restarts it
    into `CrashLoopBackOff`, the pod shows `Init:CrashLoopBackOff` and 1/2 Ready,
    and `proxy` keeps running with nothing behind the authz socket. Every check
    on that node is an `ext_authz` error: a 403 with `failureMode: DENY`, allowed
    unchecked with `ALLOW`.
  - A new proxy pod (a pod deleted, a node rebooted or added, a DaemonSet roll):
    `authz` never passes its startup probe, so `proxy` is never started and the
    pod stays in `Init:CrashLoopBackOff`. A surge roll stalls there with the old
    pod still serving. A deleted pod's Envoy waits for its successor for
    `successor_wait` (155 s at the defaults), then drains and exits, and the node
    has **no proxy**: mesh requests from that node fail to connect.

So a bad policy does nothing visible when it is applied and takes a node out
later. Check for it after every policy change (below).

**Confirming every node has the new policy, and seeing a failed reload.** OPA
exports no metric for a reload (its `/metrics` has only Go runtime and HTTP
request series), and it logs a failed one at level `info`. There are two ways to
see what each sidecar did; both are per proxy pod, because each node converges
on its own.

```bash
# 1. The latest watch events of every sidecar, successes included.
for p in $(kubectl -n aether-system get pods -l app.kubernetes.io/component=proxy -o name); do
  echo "== $p"
  kubectl -n aether-system logs "$p" -c authz --since=15m \
    | grep 'Processed file watch event' | tail -n 3
done
```

Read each pod's lines by their `time` and `err`:

- `{"duration":1779985,"err":null,"level":"info","msg":"Processed file watch event.","time":"2026-10-08T13:32:47Z"}`
  with a `time` after your update: this sidecar **loaded** the new policy.
- `{"duration":197736,"err":"1 error occurred during loading: /policy/policy.rego:7: rego_parse_error: unexpected eof token …","level":"info","msg":"Processed file watch event.","time":"2026-10-08T13:33:58Z"}`:
  this sidecar **rejected** it and is still deciding with the last good policy.
- No line, or only lines older than your update: the kubelet has **not
  delivered** the update to that node yet (or the lines are older than
  `--since`). That is not a success; repeat until every pod shows an event newer
  than the update. A sidecar that restarted after the update has no event either:
  it read the file when it started, and a running `authz` container means it
  loaded.

One ConfigMap update produces about five lines per pod, with the same `err` (one
per file-system event of the volume's symlink swap).

```bash
# 2. The policy a sidecar is deciding with, compared with the ConfigMap. Run it
#    on the node (the diagnostic API is a Unix socket in the pod's emptyDir; the
#    full path is longer than a socket address may be, hence the cd).
UID_=$(kubectl -n aether-system get pod <proxy-pod> -o jsonpath='{.metadata.uid}')
cd "/var/lib/kubelet/pods/$UID_/volumes/kubernetes.io~empty-dir/authz-socket" &&
  curl -s --unix-socket opa-api.sock http://opa/v1/policies | jq -r '.result[].raw'
kubectl -n aether-system get configmap aether-opa-policy -o jsonpath='{.data.policy\.rego}'
```

If they are the same, that node has converged. If they differ, either the update
has not reached that node yet or the sidecar rejected it; the watch events above
say which.

**Recovering.** Apply a policy that compiles; nothing needs a restart by hand.

- A running sidecar loads it when its node's kubelet delivers it (46 s in one
  measurement).
- A crash-looping `authz` container starts with it at the kubelet's next restart
  attempt. The back-off doubles up to 5 minutes, so recovery can take that long
  after the file is fixed: 8 s after `helm upgrade` in one measurement (the
  back-off happened to expire then), 125 s in another (a new pod on its sixth
  restart).

**Why the file and not the directory.** The sidecar is given
`/policy/policy.rego`. A ConfigMap volume holds each file three times
(`policy.rego`, `..data/policy.rego` and the timestamped directory both point
at), and OPA given `/policy` loads all three and refuses to start
(`rego_type_error: multiple default rules`). OPA watches the file's directory in
either case, which is how it notices the kubelet's `..data` swap. A `subPath`
mount would never be updated, so the mount is the whole volume.

Kind e2e: `e2e/authz.sh`, step d (nightly job `authz`): a policy change, a policy
that does not parse, and the first policy again, with the pod UID, the restart
counts and the Envoy epoch required to stay the same. The sidecar-restart and
new-pod cases above were measured by hand and are not in CI.

### A roll wedges both epochs after `starting workers` (#1050)

Symptom, per roll: the successor's last Envoy line is `all dependencies initialized.
starting workers`, the draining parent's last is `closing and draining listeners`, the
admin on 127.0.0.1:9901 stops answering, and about 30 s later the supervisor logs
`liveness watchdog fired; terminating for container restart` for **both** epochs and,
15 s after that, `drain deadline elapsed, killing envoy epoch` for both. Existing
connections keep serving (worker threads are fine); every new connection to or from the
node fails until the fresh epoch 0 comes up, and source nodes log `503 UC
…QUIC_TOO_MANY_RTOS` / `Network_blackhole_detected` toward the node's QUIC twins.

The cause is a deadlock between the two Envoys' **main threads** over the hot-restart
domain sockets
([analysis](https://github.com/bpalermo/aether/issues/1050#issuecomment-5875727632)).
Once the child asks the parent to drain, the parent forwards every QUIC/UDP datagram
that no parent session owns to the child, from its main thread, with a blocking
`sendmsg` with no timeout; the child's UDP listeners stay paused until the parent exits,
so for the whole `parentShutdownTime` every packet of every new QUIC connection takes
that path. Meanwhile the child asks the parent for its stats on every 5 s stats flush
with a blocking `recvmsg`, also with no timeout. When about 24 forwarded datagrams fill
the child's queue as it enters that `recvmsg`, the parent is parked in `sendmsg` and
never reads the stats request. Signals are handled on the main dispatcher, so neither
process honours SIGTERM, which is why the supervisor ends up killing both.

**The fix** is the carried Envoy patch in the aether-proxy image (#1060,
`proxy/bazel/patches/envoy-aether1050-hotrestart-nonblocking-forward.patch`): the
parent forwards UDP to the child with a non-blocking `sendmsg` behind a bounded queue,
and every child wait for a parent reply (stats, listen-socket hand-off, admin shutdown)
keeps draining the forwarded datagrams and is bounded, so neither main thread can park
on the other. It also covers the `duplicateParentListenSocket` (LDS add inside the
window) case the stats-only workaround could not. On kind the forced fault wedged 0 of 8
restarts with it; talos rev252 ran the e2e with the workaround off.

**The workaround**, `proxy.hotRestart.skipParentStats`, is now **off** by default (it was
on from #1056 until the patch landed). It passes Envoy `--skip-hot-restart-parent-stats`,
so the child never makes the stats call. Keep it as an emergency switch: if a roll wedges
again with the signature above, set it true and roll. The cost is that the parent's
gauges and its last ≤5 s of counter deltas are not merged into the child. Counters are
already per generation (#708), so dashboards built on `increase()`/`rate()` do not
change.

**What the supervisor does about it (#1058).** Two changes, so that a recurrence is
both shorter and visible sooner:

- *Early signal.* A hot-restart child (epoch > 0) whose admin stops answering for
  **10 s** after its first observed LIVE is logged once per epoch and counted. LIVE is
  the step that logs `starting workers`, and it is the anchor the #991 ready gate uses.
  The check covers streaks that begin before the parent is gone (first LIVE +
  `parentShutdownTime` + 2 s). It is not anchored on the fork, because a successor's
  xDS-gated init legitimately misses admin probes (#991). Silence before LIVE is still
  the handoff watchdog's job. On the #1050 timeline this fires about 20 s before the
  watchdog.

  ```
  hot-restart child silent   epoch=54 silentSeconds=10 sinceLiveSeconds=17.8 threshold=10s
  ```

  ```promql
  # Expected 0 per roll. Seeded at zero, so an empty result means the metric never arrived.
  sum by (node) (increase(aether_supervisor_child_silent_total[30m]))
  ```

- *No dead grace.* When the liveness watchdog fires and **both** epochs of the
  hot-restart pair have been silent on the admin for the watchdog's bound, the supervisor
  SIGKILLs straight away. It no longer sends a SIGTERM that a blocked main thread cannot
  take and then waits `drainTime` + 5 s (15 s) for it. The pair is every epoch the
  supervisor tracks, plus the cross-pod predecessor until the pod has gone Ready, plus a
  newer epoch the admin last answered as. So the old pod and the successor pod each kill
  their own Envoy at once. Answers are attributed per epoch, so a handoff watchdog on a
  child that never went LIVE while the parent still answers is one silent epoch, and keeps
  SIGTERM then grace, as does a single wedged epoch with no handoff in flight:

  ```
  liveness watchdog fired; terminating for container restart   error="admin watchdog: …" exitCode=1
  both hot-restart epochs silent; SIGKILLing now instead of SIGTERM + drain grace (a blocked main thread cannot take SIGTERM)   epochs=53,54 silentFor="53=41.8s 54=31.0s" bound=30s graceSkipped=15s
  killing wedged envoy epoch   epoch=54
  ```

  `drain deadline elapsed, killing envoy epoch` after a watchdog line now means the
  SIGTERM path was taken, with one epoch still answering.

**The container restarts in place.** A watchdog exit is `Run` returning an error, which
`proxy-supervisor` turns into `os.Exit(1)`. The kubelet restarts the `proxy` container
in the **same pod** (DaemonSet pods are `restartPolicy: Always`), so its `restartCount`
goes up by one and its `lastState.terminated.exitCode` is 1. Nothing re-execs in place,
and the DaemonSet does not replace the pod. The kind reproduction shows `restartCount 1`.
Only a pod that is already terminating (it has a `deletionTimestamp`) does not get its
container restarted. The talos reading of `restartCount 0` after #1050 is therefore not
what this code does. Read the count by container name, not by index. Before chart 2.4.9
`containerStatuses` held `authz` and `proxy` sorted by name, so with the ext_authz sidecar
enabled `[0]` was `authz`. Since #1275 `authz` is a native sidecar and is listed under
`initContainerStatuses`.

```bash
kubectl -n aether-system get pod <proxy-pod> -o jsonpath='{range .status.containerStatuses[?(@.name=="proxy")]}{.restartCount} {.lastState.terminated.exitCode} {.lastState.terminated.finishedAt}{"\n"}{end}'
```

Reproduce it on kind with `e2e/hotrestart-wedge.sh`: `WEDGE_SKIP_PARENT_STATS=false
WEDGE_FREEZE_S=6` wedged 4 of 4 restarts on 2026-09-28 against the unpatched image, and
0 against the patched one (see the header of the script). The per-roll soak gates are in
`e2e/soak/README.md`, "The hot-restart wedge gates (#1050)".

### A node stalls for seconds during a handoff (#1093)

Symptom: on some handoffs, for about 2–7 s, requests from every node to pods on the rolling
node take 1–3 s, the node's own `127.0.0.1:18081/-/-/live` (a `direct_response`, no
upstream) misses the prober's 2 s budget, and the mesh_dns tier times out on that node.
It is not #1085 (listeners are present, no `initial fetch timed out`) and not #1050 (the
admin keeps answering, `child_silent` stays 0).

What the 2026-10-01 soak established:

- **It is node-local and not specific to the handoff.** The same shape recurs at
  0.5–1 s on main-worker-04 and main-worker-05, dozens of times in 8 h, including in the
  no-roll window. The shape is destination-side h3 requests whose `upstream_service_time`
  to the local app is about 1 s on every pod at once. It is almost never seen on w01–w03.
  Envoy's worker watchdog (`envoy_server_worker_<n>_watchdog_miss_total`, ≥ 200 ms
  without a loop iteration) moved only on w04 and w05. A handoff is the tail of that
  distribution.
- **The successor's workers are the ones that stall.** At w04's 02:16 roll, workers 0
  and 3 of the new epoch missed the watchdog between 02:16:45 and :50, which is the
  stall. A request a w04 source received at 46.618 reached its w02 destination at
  49.099, 2.5 s later, and was then served in 6 ms. The new epoch's h3 connects took
  250 ms–2.5 s for 40% of them, against ≤ 100 ms for most of the parent's.
- **It is not CPU the profiler can see.** Pyroscope (on-CPU only) shows both Envoys at
  0.6–1.1 cores and the node at about 2.7 of 4 cores, with no unusual frame.

Whether the threads were starved of CPU, blocked, or busy is the open question. Since
#1093 the supervisor answers it. Every 100 ms (`--stall-sample-interval`, 0 disables it)
it reads `/proc/<envoy>/task/<tid>/{schedstat,stat,wchan}` for the main and `wrk:*`
threads of every epoch it runs. When a thread spends `--stall-threshold` (default
200 ms, Envoy's own watchdog miss bound) of a one-second window in one of the states
below, it logs one line per epoch for that window:

| class | meaning |
|---|---|
| `starved` | Runnable but waiting for a CPU (the schedstat runqueue delay). This is CPU contention or a node-level scheduling stall. |
| `blocked` | Asleep anywhere other than the idle `ep_poll`/`do_epoll_wait`. `wchan=` names where, e.g. `__futex_wait` (a lock) or `unix_wait_for_peer` (a full hot-restart socket). |
| `busy` | On a CPU for ≥ 90% of the window. The event loop is saturated. |

Example line:

```
envoy thread stall   epoch=119 pid=15 windowMs=1000 thresholdMs=200
  threads=["wrk:worker_3[starved] cpu=140ms runq=620ms blocked=0ms"]
  nodeBusyPct=71.2 nodeIrqPct=3.1 nodeSoftirqPct=9.8 nodeStealPct=0
  nodeHottestCPUIrqSoftirqPct=38.5 nodePSICPUSomeMs=640 nodePSIIRQFullMs=55
  trackedEpochs=1 handoffPeer=118
```

The kernel charges a runqueue wait when it *ends*, so `runq` can exceed `windowMs`: a 5 s
starvation is reported whole in the second it ended. The supervisor shares the proxy
container's cgroup, so it is starved along with Envoy and reports the stall afterwards. On
kind, a 6 s `cpu.max` throttle of the proxy container logged every worker as
`[starved] cpu=0ms runq=~5500ms` with `nodeBusyPct=7.2`: Envoy-only starvation on a
quiet node.

The `node*` fields come from `/proc/stat` and `/proc/pressure`, which are not
namespaced, so they describe the node for the same second. Read them like this:

- A stall that every process on the node shares shows high PSI cpu-some and a hot CPU.
- A stall only Envoy has shows `blocked` with a wchan, or `busy`, on a quiet node.

**Who had the CPU (#1392).** A line with a `[starved]` thread also names the top CPU
consumers on the node over the stall. `[blocked]` and `[busy]` lines do not carry them:
a blocked thread is not waiting for a CPU, and a busy one is the consumer.

```
envoy thread stall   epoch=210 pid=15 windowMs=1000 thresholdMs=200
  threads=["wrk:worker_0[starved] cpu=310ms runq=611ms blocked=0ms"]
  nodeBusyPct=99.1 … nodePSICPUSomeMs=1010 nodePSIIRQFullMs=12
  topCgroups="/podruntime/kubelet=1240ms /kubepods/burstable/pod0f3c1a2b-…=820ms /kubepods/besteffort/pod9d8c7b6a-…=610ms /podruntime/runtime=180ms /=95ms"
  topCgroupsOverMs=1000
  topProcs="unavailable: /proc shows only this pod's PID namespace (no hostPID)"
  trackedEpochs=1 handoffPeer=-1
```

(An illustration of the format, not a line from a cluster: the fields have not run on
a cluster yet. The cgroup names are shortened here; the line carries each pod's whole
UID, and a path longer than 64 bytes keeps its tail behind `...`.)

| field | meaning |
|---|---|
| `topCgroups` | The cgroups that used the most CPU time, largest first, as `<cgroup path>=<n>ms`. At most `--stall-top-consumers` entries (default 5, at most 20; 0 turns the consumer sampling off; the chart does not pass the flag, so the default applies). CPU time, not wall time: on a 4-core node one second holds up to 4000 ms. |
| `topCgroupsOverMs` | The interval the figures cover, ending when the line is logged. It is the stall, not the window, and it starts where the wait BEGAN: the kernel charges a wait when it ends, so a thread that waited 5 s is reported in the second its wait ended, and a 500 ms wait seen 100 ms into a window began 400 ms before that window. The supervisor works the start out for each starved thread (the tick it saw the wait on, minus the wait) and takes the consumers from the last one-second sample at or before the earliest such start, so the field is usually a whole number of seconds and can be longer than both `windowMs` and `runq=` (that 500 ms wait is reported with 2000). When the supervisor's own tick came late (it was waiting for a CPU too), the start is moved back by how late the tick was, because the wait may have ended that long ago; the interval then errs on the long side. Never less than the window. |
| `topCgroupsTruncated` | On the line, as `true`, only when the stall began before the oldest sample kept: the supervisor keeps 32 cgroup samples (about 32 s; fewer right after it starts) and 4 process scans. The figures then cover only the END of the stall, the last `topCgroupsOverMs` of it. Absent: the whole stall is covered. |
| `topProcs`, `topProcsOverMs`, `topProcsTruncated` | The same per process, as `<comm>(<pid>)=<n>ms`. Only when the proxy pod shares the host PID namespace, which the chart does not ask for; otherwise the field says so. |
| `unavailable: <reason>` | The value of `topCgroups` or `topProcs` when that source could not be read (no cgroup v2, only the container's own cgroup visible, a scan over its 250 ms budget, no earlier sample yet). The stall line is logged regardless. |

No figure is larger than the interval on every CPU of the node. That count is the node's
online CPUs (`/sys/devices/system/cpu/online`), not the CPUs the proxy container may run
on: under a cpuset those differ, and the smaller one would cut real usage down. The
`envoy thread-stall sampler running` line at startup carries it as `nodeCPUs`; when the
file cannot be read it says `unknown (...)` instead and the figures are not capped by a
CPU count at all (a cgroup is still held to what its parent used).

How to read `topCgroups`:

- The entries do not overlap. Each cgroup's figure excludes the cgroups below it that
  have their own entry, so `/kubepods/burstable` is what ran there outside every pod and
  `/` is what was charged to the root cgroup itself once every listed cgroup below it is
  taken out. That is commonly kernel threads, but a userspace task attached directly to
  the root is counted there too (every task is in some cgroup), so `/` alone does not
  name the consumer. A pod is one entry: its
  containers are not listed separately.
- On Talos the kubelet is `/podruntime/kubelet`, containerd `/podruntime/runtime`, the
  Talos services `/system/<name>`, and a pod `/kubepods/[burstable/|besteffort/]pod<uid>`.
  Name a pod from its UID with
  `kubectl get pods -A -o jsonpath='{range .items[?(@.metadata.uid=="<uid>")]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'`.
  The proxy's own pod is in the list like any other.
- A pod created in the last 10 s may not have its own entry yet: the supervisor lists
  the cgroup directories every 10 s and reads only the known ones in between, so the
  new pod's time is in its parent's entry (`/kubepods/burstable`) until then. The 10 s
  hold while the node is starved too: every tenth starved second the line is made from a
  fresh listing, so a pod created during a long incident appears on it. On that one line
  the new pod's figure is an upper bound (its usage since it was created, held to what
  its parent used in the interval); it is exact once the interval starts at or after
  that listing, which for a one-second stall is the next line. If that listing
  runs past the 250 ms budget, that one line says `unavailable` and the next reads the
  known cgroups again.
- `cpu.stat` is kept by the kernel, so the figures include processes that lived and died
  inside the interval (an exec probe, a container start), which a process list misses.

Grep it like the rest of the line, by message text:

```logsql
service.name:aether-proxy "envoy thread stall" "[starved]" "topCgroups=" k8s.node.name:<node>
```

```logsql
service.name:aether-proxy "envoy thread stall" "[starved]" "/podruntime/kubelet=" | stats by (k8s.node.name) count() lines
```

The second query counts the starved seconds in which the kubelet was one of the top five
consumers on each node. That is the question #1389 had to leave as an inference: on
one worker node the hot seconds sat on a 30 s phase that followed the kubelet and cAdvisor
scrapes, and nothing recorded CPU per consumer at one-second resolution to say the
kubelet was what burned it. With these fields each of those seconds names its consumers:
`/podruntime/kubelet` at the top with several hundred ms on the :12/:42 seconds confirms
the scrape, and a pod UID at the top instead names the second load that issue could not
identify.

What it costs, measured on a 20-thread x86 workstation (not on a Talos node): the cgroup
sample is one `cpu.stat` per cgroup down to pod level, read once a second whether or not
anything is starved (a delta needs a "before", and a stall is only known when it is
over). 134 cgroups took 2.6 ms and 402 system calls (open, read, close per file), so
about 0.3% of one core for a node with ~110 pods. Listing the directories is dearer
than reading the files (126 directories: about 7 ms) and is done every 10 s, starved or
not, together with one read of the node's online-CPU list. The
process scan is one `/proc/<pid>/stat` per process, about 22 µs each: 300 processes are
about 300 files, 907 system calls and 6.6 ms. It runs every 10 s for a baseline and once
more for each starved window, and not at all without the host PID namespace. A scan that
runs past 250 ms is abandoned and the field says so.

What the proxy pod can read. It is privileged, and the runtime gives a privileged
container the host's cgroup namespace, so `/sys/fs/cgroup` there is the node's whole
hierarchy with no extra mount. It does not set `hostPID`, so `/proc` lists only the
container's own processes; `/proc/stat` and `/proc/pressure` are not namespaced, which
is why the `node*` fields work. Per-process names would need `hostPID: true` on the
proxy DaemonSet.

The counter `aether_supervisor_envoy_thread_stalls_total{aether_supervisor_stall_class}`
is seeded per class. Grade it per roll and per node:

```promql
sum by (node, aether_supervisor_stall_class) (increase(aether_supervisor_envoy_thread_stalls_total[30m]))
```

```logsql
service.name:aether-proxy "envoy thread stall"
```

How to tell which class you have, and where to go from there:

| what the lines show | what it is | where to look |
|---|---|---|
| `[starved]` with a large `runq=`, `nodeBusyPct` in the high 90s and a high `nodePSICPUSomeMs` | The node is out of CPU. Every runnable thread waits, Envoy's included. | "Sizing nodes for a proxy hot restart" below. |
| `[blocked]` with a `wchan=` | A kernel path: a lock, a full socket, I/O. | The function `wchan=` names. |
| `[busy]` | One callback keeps an event loop on its CPU. | The profiler (Pyroscope) for that Envoy, over the same seconds. |

### Sizing nodes for a proxy hot restart

The measurements in this section come from talos-main (5 workers, 4 cores each, arm64)
on 2026-10-01 and 2026-10-02. They are readings from that cluster under the soak load,
not constants. Take the method, then measure your own nodes.

**What a handoff costs.** For the length of a hot restart two Envoy processes run on
the node: the successor's init (about 3–15 s, gated on xDS) plus
`proxy.hotRestart.parentShutdownTime` (15 s by default), during which the parent drains.
On talos-main a handoff cost about 10–13 CPU-seconds of Envoy CPU on top of steady
state, and up to about 1.9 cores in the 5 s after the successor started its workers,
on a proxy that used 0.8–0.9 core in steady state. Rule of thumb: **keep about one core
of headroom on each node for the roughly 20 s of a handoff.**

**What happens without it.** On the nodes that ran 81–85% busy in steady state, handoffs
pushed them to 99–100%. The stall lines classed every Envoy thread `starved` (0
`blocked`), parent and child alike. The prober's 2 s liveness probe timed out only in
the most starved handoffs. Of the 30 handoffs in one 8 h soak, the two with the most
starvation failed 4 probes of 719,979. Starvation there was 54.8k and 85.9k ms of `runq`,
summed over all threads in the bracket [parent drain − 5 s, parent exit + 5 s]. No
handoff at or below 41.8k ms failed one. Client traffic was not affected (k6: 0 failures
in 9,179,946 requests) because the failed attempts were retried. This is a threshold
effect of CPU oversubscription, not a functional bug: below the threshold a handoff costs
latency, above it a probe misses its budget.

**How to see it.** Count the stall windows per node and class over the roll, then read
the lines for the node that stands out (the fields are explained in the section above):

```promql
sum by (node, aether_supervisor_stall_class) (increase(aether_supervisor_envoy_thread_stalls_total[30m]))
```

```logsql
service.name:aether-proxy "envoy thread stall" | stats by (k8s.node.name) count() stalls
```

```logsql
service.name:aether-proxy "envoy thread stall" "[starved]" k8s.node.name:<node>
```

The supervisor's records are not field-parsed: the whole line sits in `_msg`, so there
is no `threads` field to filter on, and a filter has to match the message text. Select a
class with its bracketed phrase, `"[starved]"`, `"[blocked]"` or `"[busy]"`. The bare
word `blocked` matches every line, because each thread entry carries `blocked=<n>ms`.

A node whose steady-state count is several times its peers' is the one a handoff will
tip over. A starved line whose `nodeBusyPct` is near 100 and whose `nodePSICPUSomeMs`
is in the hundreds is this section. Rank the nodes by CPU requests and name what holds
them with the commands in §7 "Pre-flight: node headroom before a roll (#812)".

**What to size.**

- **Workload CPU requests.** The scheduler places pods by request, not by use. Pods with
  token requests (the talos test services request `10m`) are packed onto whichever node
  shows room, and on talos-main one node ended up with about 4× the steady-state
  starvation of its peers. Give workloads requests that reflect what they actually use,
  so the scheduler spreads them.
- **`proxy.resources.requests.cpu`** (chart default `500m`). Raising it to `800m` on
  talos-main cut steady-state starvation on the two hot nodes by 26–35%. It buys
  cgroup weight (the proxy wins more of a contended CPU), not capacity: the stall during
  a handoff was unchanged within noise. The proxy has no CPU limit on purpose, and
  should not get one. Remember that during a DaemonSet roll the surge pod's request and
  the departing pod's are both on the node, so the node needs room for two proxy
  requests at once.
- **Co-tenants.** Latency-insensitive pods on a hot node (profilers, collectors, batch
  jobs) use exactly the headroom a handoff needs. Move them to a quieter node, or keep
  them off the nodes that carry the mesh's busiest services.
- **`proxy.hotRestart.drainTime`** (default `10s`). Do not stretch it to spread the cost.
  At `30s` on talos-main the peak was no lower, there were more starved seconds, Envoy
  spent about 40% more CPU per handoff, and rolls took 70% longer. Keep the default.
- **`proxy.concurrency`** (default `0`: Envoy's own default, one worker per core unless
  the container has a CPU limit or a restricted CPU mask, see below). In an A/B, 2 workers
  instead of 4 cut handoff starvation per thread by about 27% and steady-state
  starvation by 57–81%. **Do not change it on a live mesh** until both prerequisites
  are deployed. On talos-main a 4→2 change crashed successors (`Mismatched worker
  index` in `HotRestartingChild::onForwardedUdpPacket`,
  [#1126](https://github.com/bpalermo/aether/issues/1126)), and the old pods'
  self-drain drained the new pods' Envoys
  ([#1127](https://github.com/bpalermo/aether/issues/1127)). Together they left two
  nodes not accepting new pods' connections for about 12 minutes. The crash is fixed by
  the carried Envoy patch `envoy-aether1126-forwarded-udp-worker-index.patch`, in proxy
  images built from it or later. The #1127 supervisor fix is the other prerequisite:
  the old pod's drain now reaches only its own Envoy (see "What the proxy supervisor
  does on SIGTERM"). A crashed successor still costs the old pod its graceful drain,
  because its Envoy has already handed the admin over, so it is SIGTERMed instead.

  **A count change is a drain + fresh start, not a hot restart (#1136).** Even without
  the crash, a hot restart between different counts is not hitless: the child attaches
  its QUIC connection-ID steering program (`CID % concurrency`) to the sockets it
  inherits, the program applies to the whole reuse-port group, and about half of the
  parent's live HTTP/3 connections are steered to a worker that does not own them
  (`Mismatched worker index. expected 3, actual 1`, then a stateless reset). On the
  2026-10-02 4→2 rollout that was 34 mismatched batches and 12 stateless resets fleet-wide;
  on kind, 14 mismatched lines and 5–10 `503`s per roll in either direction. So the new
  pod's supervisor reads the live predecessor's `command_line_options.concurrency` from
  `/server_info` (trusted only from a LIVE aether supervisor's Envoy at the heartbeat's
  epoch; anything else hot-restarts as before) and, when it differs from its own, logs
  `envoy worker count changes across this handoff; NOT hot-restarting` with both counts,
  drains the predecessor gracefully over `proxy.hotRestart.drainTime`, stops it, and
  starts a fresh Envoy at epoch 0. The pod goes Ready only when its own fresh Envoy is
  LIVE. The cost per node is the drain window plus a gap with no listeners while the
  fresh Envoy initializes: about 2 s on kind (requests waited up to 2.1 s on SYN
  retransmits; 0 failed), longer on a busy node, where a handoff's init takes 3–15 s,
  so expect some failed requests and prober misses on each node in turn. Count it with
  `aether_supervisor_handoff_mode_total{mode="fresh_after_drain"}` (expected once per
  node on the rollout that changes the count, 0 otherwise).
  `proxy.hotRestart.hotRestartOnConcurrencyChange: true` forces the old hot restart.
  Reproduce with `e2e/proxy-concurrency-change.sh`.

  **What "its own" count is when `proxy.concurrency` is `0` (#1442).** The supervisor
  then passes no `--concurrency`, and Envoy does not run one worker per core: it runs
  `max(1, min(online CPUs, CPUs in its affinity mask, its cgroup CPU limit))`, where the
  limit is the quota of the container's own cgroup (cgroup v2 `cpu.max`, or cgroup v1
  `cpu.cfs_quota_us` / `cpu.cfs_period_us`) divided by the period and rounded **down**:
  a `1500m` limit is one worker, `2500m` two. No ancestor cgroup is read, so a limit set
  on the pod as a whole and not on the proxy container does not lower the count
  (measured: a 1.5-CPU quota one level up left 20 workers), and
  `ENVOY_CGROUP_CPU_DETECTION=false` in the container's environment turns the cgroup
  term off. Measured on the pinned
  proxy on a 20-CPU machine: 20 workers unrestricted; 1, 2 and 3 under a mask of 1, 2
  and 3 CPUs; 1 under a quota of 0.5, 1, 1.5 and 1.99 CPUs, 2 under 2 and 2.5, 3 under
  3; an explicit `--concurrency 4` stayed 4 under a 2-CPU mask and under a 1.5-CPU
  quota. The supervisor computes the same number from the same sources (it forks Envoy,
  so both are in one container and see the same mask and cgroup), and the WARN of a
  count change says where its number came from (`successorConcurrencyFrom`, for example
  `envoy default: min(online CPUs 4, CPU affinity 4, cgroup CPU limit 2)`). Before
  #1442 it assumed one worker per online CPU, so a proxy with a CPU limit or a
  restricted mask and no explicit count compared, say, 4 with a predecessor's 2 and took
  the drain + fresh start on every roll: `fresh_after_drain` counting on every restart,
  not only on the rollout that changes the count, is the signature. Two consequences
  remain by design. Changing `proxy.resources.limits.cpu` across a whole-CPU boundary
  with `proxy.concurrency: 0` is a real count change and is handled as one. And when the
  online CPUs cannot be read the count is unknown and the supervisor hot-restarts, as
  for every other inconclusive check. `//agent/test/envoyargs` starts the pinned Envoy
  under an affinity mask (and, where the machine lets an unprivileged test make a
  cgroup with a quota, under a CPU limit) and fails when the supervisor's number is not
  Envoy's, so a proxy pin bump that changes the rule is caught there.
- **The node agent.** It has no CPU limit since #1119 (`agent.resources.requests.cpu`
  `200m`, `GOMAXPROCS=2`), because a CFS quota parked snapshot builds while they held
  the snapshot-cache mutex. Do not add one back to save headroom.

**Timeouts under starvation (#1128).** The destination's connect timeout to its local
app is 1 s (`meshconst.AppConnectTimeout`, #1122), so the `503 UF` for a pod in
teardown leaves the node while the pod's veth still exists and the source can retry it
elsewhere. A live app answers a loopback connect in microseconds, but on a starved node
the same 1 s timeout fires for live pods too: the two starved handoffs above produced
111 destination `503 UF` with `connection_timeout`. The source retried each one, at a
cost of at least 1 s of latency. That is the intended trade, not a fault, and the fix
is CPU headroom, not a longer timeout. Note also that 1 s is the kernel's initial SYN
retransmit timeout, so a single dropped SYN cannot be recovered inside it (tracked on
#1093).

### Reading a hot-restart overlap (#1333)

During a proxy roll two Envoy generations serve the node at once: the parent
drains while the child takes new connections. The stats cannot tell them apart;
the access log can.

**What the stats do during the overlap** (pinned Envoy; the first three were
also seen in worker-04's raw samples at the 2026-10-06 20:39Z roll):

- **The parent stops exporting when the child starts.** The child's start makes
  the parent shut its admin down, and that step cancels the parent's stat-flush
  timer. The parent does not flush at exit either. From then on every exported
  sample is the child's. Expect one missing flush interval at the switch.
- **The child's first counter sample contains the parent's delta.** The child
  pulls the parent's counter deltas and adds them to its own, so a counter keeps
  rising across the roll and nothing in it says which generation counted what.
- **`envoy_server_uptime` roughly doubles** while the parent lives. It is an
  accumulated gauge: the child's value plus the parent's. Every gauge that
  accumulates does the same (active connections, active requests), then drops
  back when the parent exits. A step down at parent exit is the merge ending,
  not a loss.
- **Connections are the one thing the gauges split.**
  `envoy_server_parent_connections` is what the parent still holds, and
  `envoy_server_total_connections` is both generations, so the child's
  connections are `envoy_server_total_connections - envoy_server_parent_connections`.
- **Histogram samples the parent records during the overlap are not exported.**
  This one is inferred from the Envoy source (histograms are not part of the
  parent-to-child transfer), not measured.

So a metric label naming the pod or the epoch separates nothing: there is only
ever one exporter, and its numbers already include the other generation's. It
would also create about 4,150 new series per node per roll.

**What the access log says instead.** Every HTTP access-log row
(`log_name:aether_access_logs`, both reporters) carries:

| field | is | reads `-` when |
|---|---|---|
| `proxy_epoch` | the hot-restart epoch of the Envoy process that wrote the row; the same number as `envoy_server_hot_restart_epoch`. Within one overlap the lower number is the parent | the proxy's supervisor predates the field (it exports `AETHER_RESTART_EPOCH` to each Envoy child) |
| `connection_id` | Envoy's id for the downstream connection the request rode. Unique within one Envoy process only: pair it with `node_name` and `proxy_epoch`. A small counter for a TCP connection, a 64-bit number for a QUIC one | never (`0` = no id) |
| `ds_cx_age_ms` | how old that connection was when the request started: from Envoy accepting it to the request's first byte. Small = the request paid for a new connection, large = reuse | never |
| `ds_hs_ms` | from accepting the connection to the end of its TLS handshake. A property of the connection, repeated on every row that rode it | the downstream is not TLS: every `reporter:source` row (the application speaks cleartext to its proxy) and a cleartext inbound |
| `us_tx_beg_ms` | from the request's first byte to the first byte sent upstream: routing, plus waiting for an upstream connection (a new upstream handshake shows up here) | nothing was sent upstream (local reply, no healthy upstream, connect failure) |

The L4 stream (`log_name:aether_l4_access_logs`) carries `proxy_epoch` only. An
L4 record is written when the connection closes, so there it names the
generation that held the connection.

```logsql
# Which generation served, or failed, the requests of a roll window on one node
log_name:aether_access_logs AND reporter:destination AND node_name:"main-worker-04" AND _time:[<start>, <end>]
  | stats by (proxy_epoch, response_code, response_flags) count()

# Requests that paid for a new inbound connection and a slow handshake
log_name:aether_access_logs AND reporter:destination AND ds_cx_age_ms:<50 AND ds_hs_ms:>=100
  | stats by (node_name, proxy_epoch, protocol) count(), max(ds_hs_ms)

# Failures per connection, not per request: N rows sharing one connection are one event
log_name:aether_access_logs AND reporter:destination AND response_flags:!"-"
  | stats by (node_name, proxy_epoch, connection_id) count()
```

**What these fields cannot tell you.** Time spent before Envoy read the packet.
Envoy takes no kernel receive timestamp, so `ds_cx_age_ms` and `ds_hs_ms` start
when a worker accepted the connection (for QUIC, when it created the session
from the first datagram it read), not when the peer sent its SYN or first
datagram. A request that waited in a socket queue on an established connection
while the workers were starved shows normal values on the destination row and a
long `duration_ms` on the source row. For that gap, compare the two sides'
`start_time` on one `x_request_id`, and read node CPU and run-queue data (see
"A node stalls for seconds during a handoff (#1093)" above).

The time-point names behind the three durations are checked against the pinned
Envoy by a live test (`//agent/test/mtlspool`, `TestAccessLogConnectionTiming`),
on an mTLS connection and on an HTTP/3 one. `envoy --mode validate` cannot do
it: Envoy accepts a time-point name it does not know and renders `-`.

### Envoy's UDP dropped-datagram counter after a hot restart (#1353)

**Do not gate or alert on it across a proxy roll.** The stat
`listener.<prefix>.udp.downstream_rx_datagram_dropped`, and the warning
`Kernel dropped N datagram(s)` in the proxy log, over-report after every hot
restart that follows a real drop.

**Why.** Envoy derives the count from `SO_RXQ_OVFL`, which the kernel keeps for
the lifetime of the **socket**. Envoy's own running total starts at 0 in each
**process**. A hot-restarted child inherits the parent's UDP sockets, so at its
first read it reports the socket's whole history as new drops. That first read
is when the parent exits.

**Measured** (kind, the pinned proxy image, during the #1332 experiment):

- A roll with 743 real kernel drops: the stat read 1486, and the child logged
  731 + 12 at parent exit.
- The next roll, with 0 real drops: the child still logged `Kernel dropped 273`
  and `21`, and the stat read 294.

So a non-zero value after a roll says only that this socket dropped a datagram
at some point since it was bound. It does not say the roll dropped anything.

**What to read instead.** The kernel's own counters in the pod's network
namespace, taken before and after the window you care about:

```bash
# On the node. PID is any process of the pod (its pause container will do).
PID=12345
# Per-socket drops (last column) for the QUIC inbound port:
nsenter --net="/proc/${PID}/ns/net" cat /proc/net/udp | awk 'NR==1 || $2 ~ /:4658$/'   # 0x4658 = 18008
# Namespace-wide receive-buffer overflows:
nsenter --net="/proc/${PID}/ns/net" nstat -az UdpRcvbufErrors
```

Both are cumulative as well, but they are read by you at two instants, so the
difference is the window's drops.

Nothing in this repository or in the talos-main alert rules uses the Envoy stat
today. A larger receive buffer is not the fix for drops during a starved
handoff: see #1332 (it removes the drops and changes neither the slow requests
nor the failures) and "Sizing nodes for a proxy hot restart" above.

### Source h3 requests die on a stateless reset at a destination's roll (#1054)

Symptom, per roll of a node's proxy: **source** proxies on other nodes log a few
`503 UC` lines toward the rolling node's pods with `response_code_details` containing
`QUIC_PUBLIC_RESET|FROM_PEER|Received_stateless_reset`, clustered at the moment the
draining parent exits (about `parentShutdownTime` after the successor started).

The cause ([mechanism](https://github.com/bpalermo/aether/issues/1054)): a source's h3
connection to the draining parent was still open when the parent exited. Its next packet
reached the child, which does not own the connection ID and answers with a QUIC stateless
reset; the source accepts it because the token is derived from the connection ID alone,
so parent and child mint the same one. The request in flight fails, and the source's
retry policy does not cover it (the request was already sent). Two things kept such a
connection alive past the parent: under Envoy's default `gradual` drain strategy the
GOAWAY is a coin flip per response, so a busy connection can dodge it for the whole drain,
and an idle connection gets no GOAWAY at all while the h3 pool's 30 s idle timeout
outlasts the 15 s parent-shutdown window.

**The fix** is two chart values:

- `proxy.hotRestart.drainStrategy` (default **`gradual`**; `immediate` is an opt-in)
  passes Envoy `--drain-strategy`. `immediate` puts a GOAWAY on every response from the
  start of the drain, but on talos-main (2026-09-28) it made the #1054 resets **worse**
  (4/8/0 per roll vs 1–3 per run under gradual): more parent connections close inside
  the drain window, and those closes are what sources then see reset. It is also
  server-wide (h2 and h3 reconnections bunch at drain start; LDS and pod-termination
  drains switch too). Three carried Envoy patches are the fix under `gradual`:
  #1064 (`envoy-aether1054-hotrestart-terminate-wait-h3-goaway.patch`: the child
  unpauses its UDP listeners only after the parent exits), #1066
  (`envoy-aether1054b-quic-time-wait-before-forward.patch`: the draining parent
  answers its own time-wait connections) and #1069
  (`envoy-aether1054c-paused-udp-listener-no-read.patch`, chart 1.0.11), the actual
  root cause: a **paused** child UDP listener still read the inherited socket when
  the parent forwarded it a CHLO, because the QUIC listener injects a read event to
  process the buffered handshake, and every packet it dequeued that way belonged to a
  parent connection and drew a stateless reset.
- `agent.eastWestQuicIdleTimeout: 8s` (the agent's `--east-west-quic-idle-timeout`) is
  the idle timeout on the `quic:` twins only; h1/h2 keep 30 s. It closes the connections
  that were idle when the drain started, before the parent exits. The chart refuses to
  render unless `eastWestQuicIdleTimeout + 5s < proxy.hotRestart.parentShutdownTime`, so
  lowering the parent-shutdown time needs the idle timeout lowered with it. The cost is
  one extra QUIC handshake for a (source, destination) pair that sits idle between 8 s
  and 30 s. Since #1087 the QUIC transport's own `idle_network_timeout` (8 s, negotiated
  to the minimum on both ends) also closes a twin connection with no open stream after
  8 s, so raising this flag above 8 s no longer lengthens reuse.

A request in flight at the parent's exit still dies, as it does on h2. The soak gate is
in `e2e/soak/README.md`, "The h3 stateless-reset gate (#1054)"; the kind leg is
`e2e/eastwest-quic-hotrestart.sh` with `HR_MODE=sparse` (two nodes, `EWQ_WORKER=1`).

### `504 UT` after exactly 15 s over a `quic:` twin to a terminating pod (#1087)

Symptom (before the fix): a few source-reporter `504 UT response_timeout` lines per
churn-heavy roll, each `duration_ms` ≈ 15000, `upstream_cluster` a `quic:` twin,
`upstream_host` an endpoint whose pod was terminating. The destination-reporter lines
for the **same `x_request_id`** show the request arrived: `503 UF` after 5 s, or code 0
after 3–5 s.

Cause: the destination pod's network went away (CNI DEL deletes the veth) after the
request was delivered and ACKed, so nothing the destination proxy wrote back could
leave the pod netns. QUIC has no RST, and the source had nothing in flight, so no PTO
or blackhole detector ran. The only timers left were the QUIC idle timeout (300 s from
the inbound listener's default) and QUICHE's 15 s keep-alive, and the 15 s route timeout
won. h2 had the same exposure for an ACKed request (no TCP keepalive or HTTP/2 PING was
configured); #1104 closed it, see the next section.

Since #1087 every twin carries `quic_protocol_options` with
`connection_keepalive.max_interval: 1s` and `idle_network_timeout: 8s`. While a request
stream is open the source PINGs after 1 s of silence; a live destination ACKs it, however
slow its application is, and a dead one is closed 8 s after the first unanswered PING.
The bound is about 10 s after the destination's last packet (9 s measured on kind).
It is 8 s and not shorter because of the #1093 node-local worker stalls (5–7 s
episodes): an idle deadline inside a stall would fail live requests, and the soak's
failure count is the same either way (those requests were delivered, so they are not
retryable), so only the hang gets shorter. Tighten it once #1093 is fixed. What you see now is a
`503 UC` with `response_code_details` starting
`upstream_reset_before_response_started{connection_termination` and mentioning
`QUIC_NETWORK_IDLE_TIMEOUT`, well under 15 s.

The request is **not retried**, GET or POST. It was sent, so it may have reached the
application, and the mesh retry policy retries only `connect-failure`, `refused-stream`,
`reset-before-request` and a 503 *response*. New requests to the dead endpoint fail the
QUIC handshake within the 2 s connect timeout and are retried on another endpoint.
The kind proof is `e2e/eastwest-quic-deadpeer.sh`.

### A source keeps picking an endpoint seconds after its drain mark (#1103)

Symptom: a request is sent to an endpoint whose destination agent logged
`pod terminating: endpoint marked draining ahead of shutdown` seconds earlier, and
fails (#1087's `504 UT`/`503 UC`). Join the source access-log line to the
destination agent's drain mark by `upstream_host` / pod IP.

The drain mark itself is fast. The registrar applies it to its snapshot and broadcasts
it the moment the RPC lands; the other replica picks it up through its etcd watch
(200 ms debounce). On 2026-10-01 the peer replica had it 0.15 s after the mark. A
source whose ADS stream to its node agent is up rebuilds and pushes EDS within the
snapshot cost (#1105). Envoy excludes an EDS `DRAINING` host from new selections
(`upstream_impl.cc` `setEdsHealthFlag` → `EDS_STATUS_DRAINING`;
`excludeBasedOnHealthFlag` puts it in the excluded set) and panic routing is off
(`healthy_panic_threshold: 0`), so nothing selects it once the update is applied.

**On the kubernetes registry backend (the chart default) the mark itself never leaves
the replica that received it (#1124).** That backend derives endpoints from Pods and
ignores writes, so the agent's DRAINING mark lives only in the receiving replica's
snapshot. Before #1124 the other replica listed the pod HEALTHY for its whole preStop
(the kubelet keeps a terminating pod Ready until its containers stop) and only polled
every 5 s, so every source agent attached to it kept sending new requests into the
dying pod. On kind with a 15 s preStop: 0.22–0.26 s mark → last request when the source
and destination agents were on the same replica, **14.9 s** (the full preStop) when
they were not. Since #1124 the backend lists a pod whose deletion was requested as
DRAINING while it is still Ready (UNHEALTHY once it is not), and the registrar syncs
from a managed-pod informer instead of only the poll, so every replica hears the drain
from the pod's `deletionTimestamp` within the 200 ms debounce (kind: cross-replica
0.46–0.60 s, same-replica 0.17–0.25 s). The registrar logs
`kubernetes registry initialized ... podWatch=true` and `registry supports change
notifications`. The agent's phase-2 UNHEALTHY (pool close ~1 s before SIGTERM) is
derived from the Pod the same way since #1144. Every replica lists a terminating,
still-Ready pod UNHEALTHY from `drain.PoolCloseAt`, which is the deletion request
(`deletionTimestamp` − `deletionGracePeriodSeconds`) plus the agent's own
`drain.PoolCloseDelay` (sleep preStop − 1 s, floor 2 s). A per-pod timer in the backend
wakes the sync at that moment. Because `deletionTimestamp` has one-second resolution,
this can land up to 1 s before the agent's own mark, never after it. Before #1144 the
other replicas saw UNHEALTHY only when the Ready condition dropped, after SIGTERM.
Since #1145 the receiving replica
holds an agent's write only until its next sync (the first listing that started after
the RPC; the informer or the 5 s poll), then serves the Pod-derived endpoint like every
other replica. Before, it kept the agent's version for the pod's lifetime whenever the
two differed in any field (the CNI path's health-check mode, for one). Find which
replica an agent is on with `conntrack -L -p tcp --orig-src <agent IP> --orig-dst <registrar Service IP>` on its node
(`e2e/drain-propagation.sh` does this per run).

What was slow is a source proxy that **had no ADS stream**: its own node agent was
restarting. While the stream is down the proxy routes on its last config, so it cannot
hear a drain mark however fast the registrar is. All five 2026-10-01 TRIPLE failures
were of this kind:

| source | source agent | drain mark (destination) | requests | proxy reconnected |
|---|---|---|---|---|
| main-worker-02 (proxy also rolling, parent epoch serving) | down 05:41:08, serving xDS 05:41:20.51 | 05:41:20.14 (w05) | 05:41:23.92, 24.04 | child CDS 05:41:22.51 |
| main-worker-04 | down 05:41:24.45, new pod 05:41:33.16, serving xDS 05:41:39.93 | 05:41:36.42 (w02) | 05:41:39.78–39.97 | **05:41:48.34**, 8.4 s after the agent was serving |

The 8.4 s is Envoy's xDS reconnect backoff: fully jittered exponential, 500 ms base,
30 s cap (`xds_manager_impl.cc` `SubscriptionFactory::RetryInitialDelayMs` /
`RetryMaxDelayMs`; `backoff_strategy.cc` returns `random() % interval` and doubles the
interval). The longer the agent was away, the longer the proxy waits after it is back.
Since #1103 the bootstrap's `ads_config` carries
`retry_policy.retry_back_off {base_interval: 0.1s, max_interval: 1s}`, so the proxy
reconnects within 1 s of the agent serving. Checked by
`//agent/test/envoy_validate:envoy_validate_test` (`TestNodeProxyADSReconnectBackoffIsBounded`)
and `//charts/aether:aether_proxy_bootstrap_ads_reconnect_backoff_test`; the kind proof
is `e2e/drain-propagation.sh`.

The fast reconnect needs the agent's half of #1103. A restarted agent holds its own
SVID before the SPIRE bridge has re-delivered the pods' certificates, and until then
the #1049 gate keeps those identities' `quic:` twins out of the snapshot. A proxy that
connects to that snapshot is told to remove the twins it holds (`cds: response
indicates 0 added/updated cluster(s), 1 removed cluster(s)` right after the reconnect)
and the requests its routes still send to a twin fail until the certificate's snapshot
re-adds it. On kind, with the 1 s cap and main's agent, that happened on 8 of 18
restarts, for 1–9.6 s each, and once cost 56 × 503. The 30 s default mostly hid it by
reconnecting late. The agent now opens its xDS socket only once the snapshot carries
every local certificate (bounded at 5 s; `local workloads' client certificates
delivered; serving the complete snapshot`, or a WARN with `awaiting_client_cert` on
timeout). On talos-main the certificates already arrive before the registry load
finishes (all five TRIPLE agents on 2026-10-01: 0.3–2.6 s before), so the wait costs
nothing there.

**What it does not cover:** the agent's own outage. A drain mark that lands while the
source agent is down is heard only once the new agent serves xDS: on w04 that was
15.5 s after the old agent stopped (8.7 s pod replacement, then 6.8 s to identity,
the registrar watch and the first registry load). The two-phase drain gives the
destination only its preStop window, so a source agent restart that overlaps a
destination pod's deletion can still send it requests.

To see whether a proxy is blind right now: `control_plane.connected_state` on its admin
(`0` = no ADS stream). On reconnect the proxy logs `cds: response indicates ...`, and
the agent logs `fresh xDS stream re-stated QUIC twins` when the proxy holds twins.

### Stored vs in place vs applied: registrar snapshot lag and divergence (#1193)

An endpoint change passes through three places: **stored** (the external
registry: etcd), **in place** (a registrar replica's snapshot, rebuilt by its
sync loop), and **applied** (each agent's watch cache). Since #1193 the
registrar's snapshot version carries the etcd revision the snapshot was listed
at, which makes the three comparable across processes and replicas, and always
the content hash, which alone names the contents:

| Version | Meaning |
|---|---|
| `<rev>.<hash>` | exactly the etcd listing at revision `rev` |
| `<rev>+<hash>` | the listing at `rev` plus something not yet in it: a write-behind intent still overlaid, or an agent RPC applied since the last sync |
| `hash:<hash>` | the backend has no revision (kubernetes). The version is content-addressed |

`<hash>` is 16 hex digits of a sha256 over the snapshot's endpoints. The
revision is never trusted for equality: a rebuilt etcd restarts its revisions at
1, and two binaries can decode one stored value differently.

```promql
# in place: how far each registrar replica's snapshot trails the store
aether_registrar_store_revision - aether_registrar_snapshot_revision

# applied: how far the fleet's agents trail the store (worst agent)
max(aether_registrar_store_revision) - min(aether_agent_registry_last_version)

# divergence: more than one endpoint set at ONE revision, with no write-behind
# intent pending. MUST be empty. See "Divergence rule" below.
count by (job, revision) (
  count_values by (job, revision) (
    "hash",
    aether_registrar_snapshot_content_hash
    * ignoring (revision) group_left (revision)
    count_values without () ("revision", aether_registrar_snapshot_revision)
  )
) > 1
unless on (job) (max by (job) (aether_registrar_writebehind_queue_depth) > 0)
```

The replica-lag line settles at 0 between changes. A short climb during churn
is the watch-to-sync debounce (200 ms). A replica that stays behind has a
stalled sync loop: check `aether_registrar_sync_errors_total`.

The agent line settles to 0 between changes (#1241). An agent learns a revision
from an event that reaches it: the last event of a batch it is sent, the
`SNAPSHOT_COMPLETE` of a (re)connect, or a **version marker**. Two kinds of store
write move the revision without sending an agent any endpoint event:

- a write that changes no contents: a write-behind flush landing in etcd after
  the RPC already applied it, a re-Put of an identical endpoint;
- a change to a service outside a demand-scoped agent's watch filter.

At the end of every sync cycle the registrar sends each watch stream that is
not already at the snapshot's version a version-only `SNAPSHOT_COMPLETE`
carrying it (`Broadcaster.MarkVersion`). The cycle is the throttle: a burst of
store writes is one debounced sync (200 ms), and a cycle that moved nothing sends
nothing. Markers are counted in
`aether_registrar_broadcast_events_total{aether_event_type="EVENT_TYPE_SNAPSHOT_COMPLETE"}`;
the sync span carries `aether.sync.version_markers`. A marker is handed out
under the same rule as a batch's version (#1203): the registrar holds every
publication off while it sends them, so every batch the version includes is
already ahead of the marker on the stream.

So a gap that persists past a sync cycle (the poll interval at worst) is lag:
check `aether_agent_registry_reconnects_total` and `watch_errors_total` on that
agent (and `watch_token_drops_total`, the reconnects it chose to make full
resends, #1269, and `watch_resends_abandoned_total`, the resends cut before
their marker, #1334), and `aether_registrar_broadcast_dropped_events_total` (a full stream skips its
marker and is retried next cycle; a dropped endpoint event force-resyncs it).

Skew: a registrar older than #1241 sends no markers, and against it the line is
only an upper bound (on kind, both agents sat one revision behind for 11 s with
identical contents). An agent older than #1241 handles a marker the way it
handles any `SNAPSHOT_COMPLETE` after its first: it adopts the version and
re-derives its xDS from an unchanged cache (one debounced refresh per sync cycle
that moved the version, until the agent DaemonSet has rolled). Alert on this
line only once both are on #1241; content-hash agreement (below) is alertable
regardless.

**Divergence rule.** Two replicas reporting the **same**
`aether_registrar_snapshot_revision`, with no write-behind intent pending
(`aether_registrar_writebehind_queue_depth == 0` on both), must report the
**same** value of `aether_registrar_snapshot_content_hash`. The same revision
with a different hash is a bug: the listing is a pure function of the revision,
so two replicas that disagree are not serving what the store holds.

`aether_registrar_snapshot_content_hash` (#1329) carries the hash as the gauge's
**value**: the first 13 hex digits of the `<hash>` in the snapshot version, as an
integer. That is 52 bits. A float64 holds every integer up to 2^53 exactly, so 53
would fit; 52 is a whole number of hex digits, which keeps the value readable
against a version string:

```sh
printf '%013x\n' 20015998343868   # -> 0123456789abc, the start of <hash>
```

The 12 bits it drops do not matter for this use. The gauge answers one question,
"do the replicas at this revision agree", between a handful of replicas; two
replicas serving different contents report the same value with probability
2^-52 (about 2.2e-16). It is not an identifier to look contents up by, and the
resume token still compares all 16 digits. Each replica has one series, with no
label that changes, in every version form (`<rev>.<hash>`, `<rev>+<hash>`,
`hash:<hash>`).

How the expression above works:

- `count_values without () ("revision", aether_registrar_snapshot_revision)`
  turns each replica's revision into a `revision` label. `without ()` keeps all
  of the series' labels, so the expression does not depend on the name of the
  replica label (`node` carries the pod name on talos-main).
- Multiplying the hash gauge by that (`ignoring (revision) group_left
  (revision)`) gives one series per replica: value = hash, labelled with the
  replica's revision. A replica with no revision has no match and drops out.
- `count_values by (job, revision) ("hash", ...)` is one series per distinct
  hash at one revision, and the outer `count` is how many there are. A replica
  that lags is at another revision and is not compared.
- `unless ... > 0` is quiet only when some replica reports a pending intent.
  `aether_registrar_writebehind_queue_depth` is recorded only when an intent is
  queued or flushed, so on a registrar that never queued one the series is
  missing; `unless` on a missing series changes nothing, where `and on() (... ==
  0)` would have made the expression unable to fire.

**Timestamps do not matter to it.** The registrar observes the revision and the
hash in one callback from one read of the snapshot, so the pair in an export
always belongs to the same contents. The OTel SDK stamps each instrument's data
point separately (`timestamp()` of two registrar gauges differed by at most 4 ms
over 24 h on talos-main), both travel in one OTLP request, the collector's batch
processor does not split a request, and Prometheus's OTLP receiver commits a
request as one append. The expression matches the two metrics on labels, so it
needs none of that to be exact. A pipeline that did deliver them separately
could pair a new revision with the previous hash for one evaluation, on one
replica; the alert's `for: 3m` (kept from #1328 for the RPC-between-syncs
window) covers it.

To read the pairs by hand:

```promql
# one row per replica: value = the hash, revision label = where it serves it
aether_registrar_snapshot_content_hash
  * ignoring (revision) group_left (revision)
  count_values without () ("revision", aether_registrar_snapshot_revision)
```

The alert is `AetherRegistrarSnapshotDiverged`
(`docs/observability/registrar-alerts.yml`, `for: 3m`). Its promtool tests are in
the GitOps repo (`clusters/talos-main/prometheus/rules_test.yaml`).

**Deprecated: `aether_registrar_snapshot_content{content_hash}`.** Until #1329
the hash was a label on an info gauge (always 1). A content change ended one
series and started another, OTLP carries no staleness marker for a series that
is no longer exported, and Prometheus kept returning the superseded hash for its
5-minute lookback, so each replica showed two hashes after every change (#1322).
Counting hashes over the bare selector read that as a divergence: on the 8 h
soak of 2026-10-06 (66 revisions, none diverged) it was true at 108 of 1,920
15-second steps. #1328 worked around it by keeping, per replica, the series
whose `timestamp()` is the newest. The value gauge has no label to go stale and
needs no such filter.

The labelled metric is still exported by the release that introduced the value
gauge and is removed in the release after it. That one release exists for the
upgrade. A rule on the new metric cannot see a replica still on the older image
(it would compare the new replicas among themselves and miss a divergence
against the old one), so while both images can run, keep the #1328 expression as
a second arm. Both arms return `{job, revision}`, so `or` yields one alert:

```promql
(
  count by (job, revision) (
    count_values by (job, revision) (
      "hash",
      aether_registrar_snapshot_content_hash
      * ignoring (revision) group_left (revision)
      count_values without () ("revision", aether_registrar_snapshot_revision)
    )
  ) > 1
  or
  # transitional (#1328): every replica, old image or new, still exports this
  count by (job, revision) (
    count_values by (job, content_hash) (
      "revision",
      aether_registrar_snapshot_revision
      * ignoring (content_hash) group_right ()
      (
        aether_registrar_snapshot_content
        and
        (
          timestamp(aether_registrar_snapshot_content)
          == ignoring (content_hash) group_left ()
          max without (content_hash) (timestamp(aether_registrar_snapshot_content))
        )
      )
    )
  ) > 1
)
unless on (job) (max by (job) (aether_registrar_writebehind_queue_depth) > 0)
```

Order of work: deploy that rule (it is correct against any mix of images),
upgrade the registrar, then drop the second arm, and only then take the release
that removes `aether_registrar_snapshot_content`. Check before dropping it:
`count(aether_registrar_snapshot_content_hash) == count(aether_registrar_snapshot_revision)`
(every replica that reports a revision reports the value gauge).

Notes:

- **kubernetes backend: the version is content-addressed, and the lag metrics
  are etcd-only.** A kubernetes listing is not a function of the Pod list's
  `resourceVersion`: endpoint health derives from the clock (the drain
  pool-close deadline), locality comes from a separately cached node list, and
  the list RV moves on every write anywhere in the cluster. So that backend does
  not implement `registry.RevisionedLister`, and `store_revision`,
  `snapshot_revision` and the agent's `last_version` are not reported. The
  content-hash gauge is still exported there, but the divergence expression above
  returns nothing there: it compares replicas at one revision, and there is no
  revision. Two kubernetes-backend replicas can also differ for a moment
  legitimately, because health derives from each replica's clock. Read each
  replica's current hash straight from
  `aether_registrar_snapshot_content_hash` (it is reported on this backend too:
  it names the contents, with or without a revision) and treat only a
  disagreement that persists across several sync cycles as a finding. It is not
  alerted on.
- `aether_registrar_snapshot_version` keeps its pre-#1193 series name but is now
  the snapshot **generation**: a per-process count of content changes. It moves
  only when the served endpoint set changes, and it is not comparable across
  replicas.
- **Reconnect outcomes**, counted by `aether_registrar_watch_starts_total{resume}`:
  - `current`: the agent's `last_version` is the current version. It gets only
    `SNAPSHOT_COMPLETE`.
  - `renamed`: the hash matches but the name differs, for example because the
    revision moved with no content change. It gets the service catalog plus
    `SNAPSHOT_COMPLETE` carrying the current version. No endpoint events are
    sent.
  - `extended`: a dependency-set **growth** (#1239), below. It gets only the
    added services' endpoints, plus the catalog when its token was renamed.
  - `resent`: anything else. It gets the full snapshot.
- **Dependency-set changes** (#1239). An agent's watch is filtered to its
  dependency set, and a server-streaming RPC cannot change its filter, so every
  change of the set (a TCP service appearing or disappearing anywhere changes it
  on every node; `service left dependency set` in the agent log) re-opens the
  stream. Before #1239 that re-open always dropped the resume token and every
  agent was `resent` its whole filtered snapshot. Now the agent keeps the token
  for the services its cache still holds:
  - The set **shrank** (or is unchanged): it sends `last_version` as on any
    reconnect, and gets `current` or `renamed`. The version names the
    registrar's whole snapshot, so a token that is current is current for any
    subset of the services it was earned on.
  - The set **grew**: the added services were never delivered at that version,
    so `last_version` would wrongly be answered `current`. The agent leaves it
    empty and sends `partial_resume {version, services it holds}` instead. A
    registrar that knows the field answers `extended` when the version names its
    current contents, and `resent` otherwise. One that predates it sees no token
    and resends: a one-release skew in either direction is safe, it only costs
    the resend.
  - A service that leaves and re-enters the set while one stream is open,
    including while its initial exchange is still arriving, is not held (its
    endpoints were purged when it left, and the stream keeps delivering only
    what came after), so it is re-requested.
  - A resend of a filter with no endpoints carries no `FULL_SNAPSHOT` to clear
    the cache with. The agent clears it at the marker when the marker's content
    hash differs from the token it presented (`renamed` always has an equal
    hash), or when it presented none.
  `resent` therefore still counts every agent and registrar restart and every
  stale token (a change landed while the stream was being re-opened), and on a
  registrar older than #1239 every growth. A `resent` step on every node at
  each dependency-set change, with agents on #1239, is a bug.
- The version is sent only where the receiver holds everything it names: on
  `SNAPSHOT_COMPLETE`, and on the last event of each broadcast batch per watcher
  (#1203). The agent also drops its token when a resend starts clearing its
  cache, so a stream cut mid-snapshot resends in full. Agents older than #1204
  keep the old token across a cut resend, so until the agent DaemonSet has rolled
  they are exposed to an empty cache on a reconnect that matches it. The window
  is milliseconds per reconnect.
- A stream cut **mid-batch** (#1269) leaves the batch's unversioned prefix
  applied while the token still names the version before the batch. Presenting
  that token is safe only while the registrar has moved on (its hash differs, so
  it resends). If its contents return to the token's hash, a change and its exact
  reversal or a reconnect to a peer replica that never got the change (a lost
  write-behind write), it answers `current`, `renamed` or `extended` and the
  prefix stays in the cache for good. So the agent drops its token, and the
  services it holds, whenever a stream ends after a live event that carried no
  version (a batch's versioned last event, a version marker or the initial
  `SNAPSHOT_COMPLETE` ends that state). The next stream is `resent`, logged as
  `watch stream ended inside a batch; requesting a full snapshot on reconnect`
  and counted in `aether_agent_registry_watch_token_drops_total{reason="midbatch"}`.
  That costs one resend per mid-batch cut, whatever ended the stream: a
  failure, a server drain, or a dependency-set change whose cancellation landed
  inside a batch. Read the counter after a soak, against
  `aether_registrar_watch_starts_total{resume="resent"}`: the suspected cost is a
  batch whose own event changes the dependency set (a TCP service's
  `SERVICE_ADDED`/`SERVICE_REMOVED` wakes the xDS cache, which re-asserts the
  filter and cancels the stream before the rest of the batch arrives), which
  would turn #1239's `current`/`extended` re-opens back into resends exactly at
  dependency-set changes. A `midbatch` step on every node at each such change
  is that case. A registrar older than #1203 versions every event, so its
  streams never end inside a batch. Agents older than #1269 keep the token and
  stay exposed to this case until the DaemonSet has rolled.
- A stream that ends **before its first `SNAPSHOT_COMPLETE` with no resume
  token** (#1334) abandons the full snapshot the registrar was sending it, and
  the next stream is `resent` in full again: two `resent` for one cause. It is
  not a token drop (there was no token to drop, so `watch_token_drops` stays
  flat) and it is correct (the abandoned half is never announced as complete;
  the second resend clears and rebuilds the cache). It is only wasted work, and
  it was invisible: on the 2026-10-06 soak it showed as 6 `resent` at an agent
  roll where 5 were expected (#1324). The agent now logs it at INFO as
  `watch stream ended before its first SNAPSHOT_COMPLETE with no resume token;
  the snapshot it was being sent is abandoned and the next stream is resent in
  full` and counts it in
  `aether_agent_registry_watch_resends_abandoned_total{reason}`, where `reason`
  is what ended the stream:
  - `filter_change`: the dependency set changed while the snapshot was arriving
    and the agent re-opened the stream to assert the new filter. This is the
    #1324 case. At start-up the first stream already carries the local pods'
    dependency set (the xDS `PreListen` asserts it ahead of the identity hold),
    but the set keeps growing for a few seconds as the agent's informers sync
    (the mesh-Service projection's TCP services, GAMMA and L4 route backends,
    chain filters, imported config) and, on a surge standby, at the takeover
    (the previous agent's persisted observed upstreams, the overlap's CNI
    ADD/DEL, then the clusters the reconnecting proxy reports it holds). A
    change that lands after the marker costs an `extended` or `current`
    re-open; one that lands inside the first exchange costs this.
  - `server_drain`, `eof`, `forced_resync`, `error`: the registrar drained,
    closed, force-resynced or failed the stream mid-snapshot.
  The same line carries `eventsReceived` and `openFor` (how far the stream got),
  `filterServices` and `nextFilterServices` (the abandoned stream's filter size
  and the one that superseded it) and `tokenPresented` (true when the stream
  did present a token, the registrar resent past it, and the cut landed inside
  that resend: #1203 drops the token at the first `FULL_SNAPSHOT`). A stream
  that ends before its marker with its token intact is not counted: nothing
  was being resent, and the next stream resumes.

  Read it against the registrar's resends over the same window:

  ```promql
  # Abandoned resends per node, by what ended the stream. max_over_time, not
  # increase(): the typical one happens in an agent's first second, so its
  # series is born at 1 and increase() reads 0 for it.
  sum by (node, reason) (max_over_time(aether_agent_registry_watch_resends_abandoned_total[30m]))

  # Full resends the registrar served in the window.
  sum(increase(aether_registrar_watch_starts_total{resume="resent"}[30m]))
  ```

  Each abandoned resend accounts for at most one `resent` beyond the expected
  ones (one per agent start, one per agent per registrar it reconnects to with a
  stale token, one per `watch_token_drops`); "at most" because a stream
  cancelled before the registrar began serving it was never counted there. A
  non-zero `filter_change` at agent rolls is the known start-up race and costs
  one filtered snapshot send per count. `filter_change` on every node at every
  roll, or outside rolls, means dependency-set changes are landing inside
  resends as a rule (check `eventsReceived`/`openFor`: a registrar that takes
  seconds to finish an initial exchange widens the window). Any other reason
  that keeps counting is a registrar that cannot complete an initial exchange:
  read it with `aether_agent_registry_watch_errors_total` and the registrar's
  own log.
- Publications (an RPC's or a sync's snapshot change plus its broadcast) are
  serialized: every watcher receives batches in the order they changed the
  snapshot, each one contiguous. Before the #1239 review they ran concurrently,
  and two on one endpoint (a register and an unregister, or two agents
  registering it during a surge roll) could reach a watcher in reverse, leaving
  it holding the endpoint under an older version (healed only by a resend on its
  next reconnect).

### `504 UT` after exactly 15 s over an h2 mesh cluster to a terminating pod (#1104)

The h2 sibling of #1087. Since QUIC went unconditional, h2 carries the GAMMA
weighted split (by design, #961), waypointed (cross-cluster) traffic and the
edge -> mesh hop; all of them are `NewServiceCluster`. A request the destination
proxy had received (TCP-ACKed) whose pod veth was then deleted got no FIN and no
RST (the node-shared destination proxy keeps its sockets in the dead netns), the
source had no unacked bytes, and nothing was configured to probe, so it waited for
the 15 s route timeout.

Since #1104 every h2 mesh cluster carries
`http2_protocol_options.connection_keepalive {interval: 1s, interval_jitter: 15%, timeout: 8s}`.
Envoy PINGs every connection (open streams or not) about once a second; the
destination proxy's codec answers, never the application, so a slow application is
not touched (kind: a 10 s handler on the live backend answers 200). An unanswered
PING closes the connection 8 s later: at most ~9.2 s after the peer's last frame
(8.1-8.95 s after the cut on kind). It is 8 s, not less, for the same #1093 reason as
the twins: a destination worker stalled for 8 s or more cannot answer the PING. What you
see now is `503 UC` (upstream connection termination)
and `cluster.<svc>.http2.keepalive_timeout` incrementing. The request is **not
retried**, GET or POST: the close is a `ConnectionTermination` reset of a request
already sent, which `reset-before-request` excludes. The PING is NOT on the app hop,
authz, xDS or collector clusters (their h2 peer is not an aether proxy; a gRPC server
answers frequent PINGs with `GOAWAY too_many_pings`). The kind proof is
`e2e/eastwest-quic-deadpeer.sh verify-h2`.

### An HTTP/1.1 client gets a reset on a reused keep-alive connection (#1350)

The proxy's HTTP connection managers have two downstream idle timeouts, chosen by who
the downstream is (Go constants in `agent/internal/xds/proxy/networkfilter.go`; not a
chart value):

| Listener | Downstream | Idle timeout |
|---|---|---|
| mesh inbound (18008, TCP and QUIC; stat prefix `inbound`) | a peer proxy | **5 min** (`peerFacingIdleTimeout`) |
| per-pod capture chain and per-pod outbound (18001 / 18081; `capture_http`, `outbound_http`) | the pod's application | **1 h** (`appFacingIdleTimeout`) |
| edge gateway listeners | an external client | **1 h** (`edgeDefaultIdleTimeout`; per-Gateway listeners take `EdgeConfig` `idleTimeout`) |

The rule on the app-facing side: **the server's idle timeout must exceed the client's,
because HTTP/1.1 has no GOAWAY.** A server that closes an idle kept-alive connection
cannot tell the client first, so a request the client writes onto that connection at
the same instant is lost. A client that retries on a reused-connection failure (Go's
`net/http`, so k6; browsers) hides it; one that does not (an Envoy-based client, many
plain HTTP/1 pools) reports a reset. With 1 h on the proxy, an ordinary client (idle
timeout 30-120 s) always closes first. HTTP/2 and HTTP/3 are not affected: the idle
timeout drains them with a GOAWAY.

The inbound side is the other way round on purpose. Its client is a peer proxy that
idles its pools out at 30 s (`config.UpstreamIdleTimeout`), so 5 min already exceeds
it, and it is short because the 1 h default once let a peer's leaked connections pin
~7k inbound connections on one listener.

Before #1350 both sides had 5 min, and the signature was: one client-side
`connection_termination` / reset before headers in a few hundred thousand HTTP/1.1
requests, **no access-log row on either side** (the request never reached the HTTP
layer), and `envoy_http_capture_http_downstream_cx_idle_timeout_total` ticking on the
source node within seconds of it. If you see that again:

- On a proxy with the fix, `…_capture_http_downstream_cx_idle_timeout_total` and
  `…_outbound_http_downstream_cx_idle_timeout_total` should barely move. If they
  move, either a client really does hold a connection idle for an hour, or the node
  proxy is under memory pressure: its overload manager (`proxy.overload`, on by
  default) scales this very timer down toward 2 s between 85 % and 95 % of the heap
  budget and disables keep-alive at 90 %. Check Envoy's
  `overload.envoy.overload_actions.reduce_timeouts.scale_percent` and
  `overload.envoy.overload_actions.disable_http_keepalive.active` first.
- A load driver or client you control should set its own idle timeout **below** the
  proxy's, and retry an idempotent request on a reused-connection failure.

The cost of 1 h: a connection an application leaks (opens and never closes or uses)
stays on the proxy for up to an hour instead of 5 minutes. Nothing caps downstream
connections per listener; the overload manager above is what reclaims them. Watch
`envoy_http_capture_http_downstream_cx_active` per node if a workload is suspected of
leaking.

The gate is `TestDownstreamIdleTimeoutFollowsWhoTheDownstreamIs` in
`//agent/test/envoy_validate`: it reads both values off the generated config and
fails on an HTTP connection manager it cannot classify. There is no timing test: with
a 1 h timeout the race cannot be reproduced in CI.

### Envoy SIGBUS/SEGV in `QuicConnection` after an h3 cluster removal (#1074)

Symptom: the proxy container restarts with a SIGBUS or SIGSEGV whose backtrace ends in
`quic::QuicConnection::OnCanWrite` / `CanWrite` (or a QUIC alarm), shortly after a
CDS push removed a `quic:` twin while requests were still in flight on it. Seen on
talos-main w01 when the pre-#1073 fetch-window prune removed held twins.

Cause: the per-cluster `PersistentQuicInfoImpl` (connection helper and clock, alarm
factory, QUIC config) was owned by the worker's thread-local `ClusterEntry`, while the
HTTP/3 pools held it by reference and their connections kept raw pointers into it. A
CDS removal destroys the entry after only *draining* its pools, so a pool with live
streams outlived the info and its next write or alarm read freed memory.

Fix: the carried patch `envoy-aether1074-quic-persistent-info-lifetime.patch` (#1077;
upstream as envoyproxy/envoy#47893): the info is shared, and every HTTP/3 pool and
connectivity grid holds a reference. It is in the proxy pinned by chart **1.0.13**
(`71be75f…`, #1078). A proxy older than that is exposed on any h3 cluster removal
with streams in flight, which includes rolling the chart back to 1.0.11 without
pinning `proxy.image` (see "If UDP:18008 is blocked" above).

### The node agent is OOMKilled (exit 137)

Symptom: agent pods restart with exit code 137 and the reason is `OOMKilled`,
typically at churn peaks (many pod ADD/DEL, snapshot rebuilds):

```bash
kubectl get pod -n aether-system -l app.kubernetes.io/component=agent \
  -o custom-columns='POD:.metadata.name,NODE:.spec.nodeName,RESTARTS:.status.containerStatuses[0].restartCount,LAST:.status.containerStatuses[0].lastState.terminated.reason,EXIT:.status.containerStatuses[0].lastState.terminated.exitCode'
```

The check: compare the Go runtime's memory with the container limit and with
`GOMEMLIMIT`.

```bash
# the limit and the GOMEMLIMIT the chart rendered (bytes)
kubectl get ds -n aether-system aether-agent -o jsonpath='{.spec.template.spec.containers[?(@.name=="agent")].resources.limits.memory}{"\n"}{.spec.template.spec.containers[?(@.name=="agent")].env[?(@.name=="GOMEMLIMIT")].value}{"\n"}'
```

```promql
# peak Go runtime memory per agent over the incident window, MiB
max by (node) (max_over_time(go_memory_used_bytes{job="aether-agent"}[1h])) / 1048576
```

Read it as follows:

- `GOMEMLIMIT` must be **below** `limits.memory`. Since chart 2.4.2 the chart
  renders it at 90 % of the limit (`aether.goMemLimit` in `_helpers.tpl`).
  Before that it was a `resourceFieldRef` on `limits.memory`, which is the
  whole limit: the heap could fill the cgroup before the GC tightened. If it
  is a `valueFrom` again, or absent while a limit is set, that is the bug.
- The cgroup holds more than the Go runtime. The exec probes (proposal 041:
  `/agent-ready`, readiness every 2 s and liveness every 10 s) are processes
  in the agent container's cgroup, ~3 MiB RSS each (measured), plus runc's
  exec helper, plus kernel socket buffers and page cache. Count on ~10 MiB
  beyond `go_memory_used_bytes`.
- Peaks near `GOMEMLIMIT` with steady growth between churn waves point at a
  leak: capture a heap profile (Pyroscope) before raising anything. Flat peaks
  (the 2026-10-04 soak: 44-61 MiB per agent, the same as the passing 10-03
  soak) mean the limit is simply too small for the load.

The knobs: `agent.resources.limits.memory` (default 128Mi since 2.4.2; was
64Mi, which the 2026-10-04 soak OOMKilled 3 times on 2 of 5 nodes within ~1 h
of churn) and `agent.resources.requests.memory` (96Mi). `GOMEMLIMIT` follows
the limit at 90 % with no separate setting. Raising the request also raises
what a surge roll needs per node (see *Agent surge roll (proposal 041)*).

### The agent reports an unrepairable conflist

Symptom: `AetherCNIConflistUnchained` fires for a node, the agent there is NotReady and
the startup taint is held (proposal 033, #667), and the agent logs

```
aether is not chained in the active CNI conflist and no known-good entry could be
recovered; cni-install must run
```

Aether is a **chained** plugin in another CNI's conflist, so this node issues no CNI ADD
to the agent at all: every pod that starts on it comes up unmeshed. The re-assert loop
re-appends the entry it last **observed**, and this message means it has none — the
strip landed before its first check (the priming window, #680).

Check the durable entry `cni-install` leaves beside the conflist; the loop primes from it
and repairs within a check (~2.5s) when it is present and valid:

```bash
# On the node (talosctl, or a debug pod with /etc/cni/net.d mounted):
ls -la /etc/cni/net.d/                     # .aether-cni-entry must be there
cat /etc/cni/net.d/.aether-cni-entry       # one JSON object, "type": "aether-cni"
grep -c aether-cni /etc/cni/net.d/*.conflist
```

- **Present and valid, agent still refusing** — look for
  `primed known-good entry from durable file` in the agent log. Its absence with the
  file present means the agent is reading a different directory: compare its
  `--mounted-cni-net-dir` (log line `CNI conflist re-assert loop started dir=…`) with
  where `cni-install` wrote.
- **Missing** — the node predates #680, or `cni-install` failed to write it (search the
  init container's log for `failed to write the durable aether CNI entry`). Recreate the
  agent **pod** on that node (`kubectl -n aether-system delete pod aether-agent-…`): only the
  init container renders the entry, so restarting the container is not enough.
- **Present but garbage** — the agent logs `the durable aether entry is unusable` and
  ignores it, by design; recreate the agent pod to have `cni-install` rewrite it.

Never hand-write either file: `cni-install` is the durable entry's only writer, and the
conflist belongs to the primary CNI plus the re-assert loop.
### The agent is stuck waiting for SPIRE

Symptom: the agent logs, once per retry,

```
waiting for the SPIRE Workload API to issue this workload's SVID socket=/run/secrets/workload-spiffe-uds/socket attempt=7 elapsed=41.2s warnAfter=2m0s error=...
```

**That line is the fix working, not the failure** (issue #740). Startup no longer dies
when the Workload API is not serving yet: the agent programs everything that does not
need identity, keeps `/healthz` and `/readyz` answering, and folds the SVID in when it
arrives. The signature of a healthy wait is the waiting lines plus **0 restarts** and
**no** `failed to create SPIRE Workload API source` anywhere. Before #740 that error was
followed by `exit 1`, which is how a cold boot produced 2-4 restarts per node.

What to check, in order:

```bash
# 1. Is it still waiting, or did it resolve? (arrival is one INFO line)
kubectl -n aether-system logs ds/aether-agent | grep -E "waiting for the SPIRE Workload API|obtained this workload's SVID|resolved workload trust domain"

# 2. Restarts must be zero — a restarting agent is a DIFFERENT problem
kubectl -n aether-system get pods -l app.kubernetes.io/name=aether-agent

# 3. The readiness gate and its dwell
# (chart >= 2.3.0: health is a pod-local socket; the probe prints the verbose
# body, naming the failing check, and exits 1 when not ready)
kubectl -n aether-system exec ds/aether-agent -c agent -- /agent-ready --path=/readyz
```

#### The readiness semantics are NOT the same on every component

This is the first thing to get straight, because the same `spire-svid` check
deliberately answers differently on the agent and on the three Deployments:

| Component | Workload | Dwell before NotReady | What NotReady does |
| --- | --- | --- | --- |
| `aether-agent` | DaemonSet | **2 m** (the Go constant `spire.NotReadyDwell`, not a chart value) | The controller's node-taint guard re-arms `aether.io/agent-not-ready:NoSchedule` on this node |
| `aether-registrar` | Deployment | **0** (the Go constant `spire.ServiceNotReadyDwell`) | This replica leaves the registrar Service's endpoints |
| `aether-controller` | Deployment | **0** | This replica leaves the webhook Service's endpoints |
| `aether-edge` | Deployment | **0** | This replica leaves the LoadBalancer's endpoints |

The agent's dwell is **not** a general tolerance for slow SPIRE. It exists solely
because NotReady on the agent DaemonSet is the signal the node-taint guard consumes, and
a taint is a fleet-level action a 40s SPIRE hiccup must not trigger. Nothing behind a
Service has that coupling: there, NotReady removes one replica from one endpoint set,
which is precisely the right handling of a pod that cannot complete an mTLS handshake,
so those three go NotReady the instant they are known to lack an identity.

That asymmetry is a fix, not an inconsistency. On the rev210 upgrade roll
(2026-09-07 20:03:45Z) the registrar carried the agent's 2 m dwell, so a registrar Pod
was Ready — and in its Service's endpoints — before it had an SVID, and an agent on
`main-worker-01` that dialled it logged
`transport: authentication handshake failed: x509svid: could not get X509 bundle`.

The `--spire-wait-warn-after` flag is a **logging** threshold only; since #740 PR 4 it no
longer drives readiness on any component. On the agent the two values coincide at 2 m.

`readyz?verbose` reads `spire-svid ok` for the first **2 minutes** of an agent's wait
(the dwell above) and then fails. It prints
**`spire-svid failed: reason withheld`** — controller-runtime redacts checker errors, so
the endpoint never shows the reason. The reason is in the **log**, once per transition:

```bash
kubectl -n aether-system logs ds/aether-agent | grep -E "readiness (failing|passing)"
# spire-svid readiness failing   reason="no SPIRE SVID after 2m10s (socket /run/secrets/…)"
# spire-svid readiness passing
```

The dwell is deliberate: the controller's node-taint guard re-arms
`aether.io/agent-not-ready:NoSchedule` after 30s of NotReady, so a gate that failed
immediately would turn a brief SPIRE hiccup into a fleet-wide taint. Past the dwell the
NotReady is the point: stop scheduling pods onto a node that cannot give them an
identity. Since #740 the agent's own taint remover **holds** that taint while any
readiness gate is failing, instead of clearing it 50ms after the guard arms it — so
**spire-server, spire-agent and the SPIFFE CSI driver must tolerate the taint**
(`spire.waitWarnAfter`'s note in `values.yaml`; applied on talos-main in
k8s-talos-main #45). Without those tolerations the outage fences out its own cure.

**The node keeps serving.** While the agent has no SVID it does NOT open the xDS socket
— it logs `holding xDS until this agent has an SVID; Envoy keeps its current
configuration` every 15s with a rising `elapsed`, and Envoy goes on serving the last
config it was given. This matters because the alternative is worse than the crash loop
#740 replaced: a live agent that cannot reach the registrar publishes a **local-only**
snapshot, which REPLACES a complete one and strips every cross-node endpoint. On
2026-09-07 that failed ~95% of one node's mesh probes for a whole 6m41s outage. If you
see `registry unavailable for initial snapshot; starting with local-only config` while
SPIRE is down, that is the bug — the hold is missing.

Recovery is announced only when **both** halves of the identity exist (the SVID and the
bundle for its trust domain), and the registrar client is kicked out of its gRPC
backoff the moment they do:

```bash
kubectl -n aether-system logs ds/aether-agent | grep -E "identity acquired"
# identity acquired; generating the initial snapshot                      held=6m41.2s
# identity acquired; reconnecting the registrar client immediately …
```

**Acquiring the SVID is not the same as being able to use it.** Every handshake made
before the SVID landed failed, so the registrar client's gRPC connection is still
carrying that failure for the second or so it takes to redial — and a snapshot built in
that window has no cross-node endpoints, which is the same local-only outage as above by
another route. Since #740 PR 5 the initial snapshot therefore waits for a registry that
actually **answers**, not merely for its readiness latch, and says so:

```bash
kubectl -n aether-system logs ds/aether-agent | grep -E "registry connected|not yet re-established|local-only"
# registry connected; generating the initial snapshot                     waited=1.106s
# registrar connection not yet re-established after identity; retrying    reconnecting=true …
```

`registry connected` is the healthy end of every agent start; `waited` is normally a few
milliseconds and rises to about a second when the start raced identity. The
`not yet re-established` INFO is that race being named correctly — it is **our own**
connection catching up, not the registrar, so do not go reading registrar logs for it.
Reserve that for `registrar has no identity yet; retrying`, which is only ever logged
once this agent's identity has been settled for more than a few seconds. On the rev211
deploy roll (2026-09-07 20:47Z) both lines read `registrar has no identity yet`, and
`main-worker-02` published an endpoint-less snapshot and logged **316** prober
`http_error` in ~30s while both registrar replicas had been Ready for 43 seconds. The
wait is bounded by the same 15s budget as before, so a registrar that is genuinely
unreachable still ends at `registry unavailable for initial snapshot; starting with
local-only config` rather than stalling the node.

The upstream cause is almost always spire-server or the node's spire-agent, not aether:

```bash
kubectl -n spire-server get pods                  # spire-server-0 Running and 1/1?
kubectl -n spire-system get pods -o wide          # this node's spire-agent Running?
kubectl -n spire-server logs spire-server-0 | tail -50
```

Metrics for the same question, fleet-wide:

```promql
# nodes without an SVID right now (0 = waiting)
min by (node) (aether_agent_spire_svid_ready)
# how long the wait took on the last boot
histogram_quantile(0.99, sum by (le) (rate(aether_agent_spire_wait_seconds_bucket[15m])))
# must stay flat at 0: a source that connected but could not serve an SVID
sum(increase(aether_agent_spire_source_restarts_total[1h]))
```

While the wait is on, the registrar watch stream logs
`watch stream deferred until this agent has an SVID` at INFO and counts no
`watch_errors` — the agent cannot handshake without a certificate, and that is not a
registrar fault. Envoy keeps serving on the certificates and the configuration it
already holds. A **new** pod on that node is still registered and still gets its CNI
redirect, but nothing is pushed to Envoy until the SVID lands (and it could not have
meshed without an identity anyway) — which is why the node reports NotReady and stays
tainted for the duration.

#### A pod has no SDS certificate: the SPIFFE Broker Endpoint

Distinct from the wait above, and the failure mode is per-pod rather than
per-node. Since proposal 036 the agent does **not** mint pod SVIDs from a
selector set it builds itself; it presents a **Kubernetes object reference**
(`pods`/`core`, namespace + name + UID) over the SPIRE agent's SPIFFE Broker
Endpoint (`--spire-broker-socket`, mounted from
`/run/spire/agent/sockets/csi.spiffe.io/broker`), and SPIRE resolves and attests
the pod itself. A reference resolves **at request time**, which is a class of
failure the delegated path did not have.

```promql
# references that did not resolve — a handful per pod creation is the expected
# CNI-ADD-beats-the-kubelet-list race; a sustained rate is not
sum by (node) (rate(aether_agent_spire_broker_reference_not_found_total[5m]))
# the provider refused us: ALWAYS a policy/config problem, never transient churn
sum by (node) (rate(aether_agent_spire_broker_permission_denied_total[5m]))
```

Both counters are seeded at zero, so "no series" means the agent is too old, not
that the value is zero (#717).

| Symptom | Cause | Fix |
|---|---|---|
| `has not resolved this pod yet` at INFO, converging in seconds | The CNI ADD beat the pod into the kubelet's pod list. Expected. | Nothing. The subscription retries on the jittered backoff. |
| The same line escalating to **WARN** (>30s on one reference) | The pod is gone, the UID does not match what SPIRE resolved, or `podReferenceScope` is wrong for this topology. | Check the pod still exists and its UID matches; check `spire-agent`'s `experimental.broker` block lists this agent. |
| `denied this agent the pod's identity` + `permission_denied` climbing | SPIRE's `access_policy` is `enforced` without the `impersonate-via-spire` RBAC grant, or the agent's SPIFFE ID is not in the k8s attestor's broker list (`broker "…" is not configured`). | Set `workloadAttestors.k8s.brokerAPI.accessPolicy=permissive`, or grant the RBAC. **`accessPolicy: auto` is not safe here** — it resolves to `enforced` for our reference type. |
| `rejected the request as malformed; not retrying` | A bug in the agent (missing security header, bad reference). Never self-heals. | File it; the subscription is dead until the agent restarts. |
| Every subscribe fails `Unavailable` on a healthy SPIRE agent | The agent's SPIFFE ID is not in `spire-agent.brokerAPI.brokers.*` — an unauthorised broker is rejected at the **TLS layer**, which gRPC reports as `Unavailable`, not `PermissionDenied`. | Fix `idTemplate` to the agent's real ServiceAccount ID. |

**Is rotation happening?** The counters above only count failures; the healthy
path has its own pair, also seeded at zero per attribute set:

```promql
# pod SVIDs rotated per node — expect about one per managed pod per SVID
# half-life (2h at the default 4h TTL). `initial` is every pod after an AETHER
# agent restart (a new process has served nothing yet). `unchanged` is the same
# certificate redelivered: a stream that dropped and re-subscribed while the
# SPIRE agent stayed up.
sum by (node) (increase(aether_agent_spire_svid_updates_total{aether_spire_identity="pod", aether_spire_update="rotated"}[3h]))
# the agent's own SVID (identity="node"), and the trust-bundle inputs:
# bundle="own", update="rotated" is a trust-ROOT change — SPIRE's 24h signing-CA
# rotation does not move it, because the bundle is the upstream root.
sum by (node, aether_spire_bundle, aether_spire_update) (increase(aether_agent_spire_bundle_updates_total[24h]))
```

**`rotated` is "the certificate changed", not "the TTL ran down".** A restarted
SPIRE agent re-attests and mints fresh SVIDs for every pod on its node, so a
`spire-agent` restart moves `rotated` by that node's pod count (+1 for the node
SVID) and leaves `unchanged` at zero — measured on rev219: one `spire-agent` pod
delete, `pod/rotated` 0 → 7, `node/rotated` 0 → 1. That is correct, and it means
a rotation-cadence reading has to exclude the minutes in which that node's
`spire-agent` restarted (`kube_pod_start_time{namespace="spire-system"}`), exactly
as proxy-roll minutes had to be excluded from the Envoy gauges this replaced.

A pod whose `rotated` count stays at zero past its SVID half-life while its
neighbours rotate is holding a certificate that will expire; the agent also logs
`pod SVID rotated` / `node SVID rotated` at INFO. Before these existed a rotation
could only be inferred from Envoy's `envoy_sds_*_version` gauges, with every
proxy-roll minute excluded by hand (a new Envoy changes every gauge once).

Trust bundles do **not** come from the broker (its bundle RPC also needs a
workload reference, which a node with no managed pods does not have). They are
built from the agent's **own** Workload API bundle plus the union of the
`federated_bundles` each pod stream carries, so a node with zero pods still
serves validation contexts — if it does not, the agent has no identity yet and
the section above is the one you want.

#### The controller, registrar or edge is stuck waiting for SPIRE

All four binaries wait the same way, log the same two lines, and register the same
`spire-svid` readiness gate, so the commands above work verbatim against
`deploy/aether-controller`, `deploy/aether-registrar` and `deploy/aether-edge`.

Two things differ, and both matter. The **dwell is 0** on all three Deployments (see the
table above): they go NotReady the moment they are known to have no identity, and leave
their Service's endpoints, rather than sitting Ready for 2 minutes absorbing dials they
cannot handshake. And the **metric namespace** differs:
`aether_controller_spire_*`, `aether_registrar_spire_*`, `aether_edge_spire_*`
(**not** `aether_agent_spire_*` -- a false zero if you query the wrong one).

What each component does while it waits:

- **Controller** -- the admission webhooks cannot complete a TLS handshake, so the
  apiserver **fails open**: both webhook configurations are `failurePolicy: Ignore`,
  so `MeshConfig`/`HTTPFilter`/`EdgeConfig`/`EndpointPolicy`/`HTTPRoute` creations are
  admitted **unvalidated** and pods are **not** mutated (no mesh-domain `ndots`
  injection) for the duration. The caBundle injector logs
  `webhook caBundle injection deferred until this workload has an SVID` at INFO and
  injects on the first SVID. Symptom of the wait having ended: one
  `injected SPIRE trust bundle into webhook caBundle`. The replica is NotReady for the
  whole wait and leaves the webhook Service's endpoints, which is what makes the
  fail-open immediate instead of a 10s webhook timeout per request.
- **Registrar** -- agents cannot handshake, so their watch streams retry (they log
  `watch stream deferred until this agent has an SVID`, not errors). While the SVID is
  pending the registrar authorizes **nothing**; the trust domain it authorizes against
  is read from its own SVID at handshake time and announced once, as
  `resolved workload trust domain from SPIRE`. The replica is NotReady for the whole
  wait and leaves the registrar Service's endpoints, so agents dial one that can serve
  instead of one that will reject the handshake. An agent that dials it anyway (a
  single-replica registrar, or the endpoint update racing the dial) logs
  `registrar has no identity yet; retrying` at **INFO** and counts no `watch_errors`;
  the cluster-refresh path logs
  `cluster refresh deferred: the registrar has no identity yet, keeping the current
  snapshot` at **WARN** and counts no `refresh_errors`. Both are transients of the
  server's startup, and both used to be ERRORs (#740 PR 4).
- **Edge** -- ingress keeps serving on the certificates its Envoy already holds. The
  control plane starts with seeds (mesh domain, empty SPIFFE ID), so newly loaded
  clusters carry **no upstream mTLS** until the SVID lands; the arrival logs
  `resolved edge identity from SPIRE` and pushes a new snapshot, so no restart is
  needed. The replica is NotReady for the whole wait and leaves the LoadBalancer's
  endpoints; a second replica goes on serving ingress.

```bash
for d in aether-controller aether-registrar aether-edge; do
  echo "== $d"
  kubectl -n aether-system logs deploy/$d | grep -E "waiting for the SPIRE Workload API|obtained this workload's SVID"
done
```

Before #740 each of these exited with `failed to create SPIRE Workload API source`
(the controller: `failed to open SPIRE Workload API source`) and crash-looped. Seeing
that error at all now means an OLD image.

**Expected side effects while an agent holds xDS (#743).** The hold keeps the xDS socket
closed so Envoy keeps its last configuration; from Prometheus that is
`envoy_control_plane_connected_state == 0` with `envoy_server_live == 1` on that node, and
`EnvoyControlPlaneDisconnected` fires for the duration. It resolves by itself when the
identity lands (`identity acquired; generating the initial snapshot held=…`). A firing
`EnvoyControlPlaneDisconnected` with `aether_agent_spire_svid_ready == 0` on the same node
is this case, not a broken agent. Attestation-to-Ready is bounded by the wait loop's 30 s
backoff cap; the node taint is armed ~30 s after NotReady and stays until readiness returns.

**Reproducing the SPIRE-outage path on purpose.** Scaling `spire-server` to 0 alone does
not reproduce it (the node's spire-agent serves from cache; measured: 0 prober delta).
Delete that node's spire-agent pod and then its aether-agent pod. For a fleet test, do not
use `kubectl rollout restart ds/aether-agent`: the DaemonSet is `maxUnavailable: 1`, so the
rollout stalls as soon as the first replaced agent goes NotReady — delete the agent pods
instead. Keep each window under 10 minutes (SVID TTL 4 h) and recover with
`kubectl -n spire-server scale statefulset spire-server --replicas=1`.

#### The pod-mutating webhook's caBundle is empty (SPIRE-served webhook, #1411)

With `controller.webhook.spire=true` both webhook configurations render with an
empty `caBundle` and the controller's leader fills them in. For the pod-mutating
one it needs three things that the chart now derives from one condition
(`controller.namespaceInjection` **or** `controller.injectPodNdots`): the
configuration itself, `--mutating-webhook-config-name` on the controller, and the
`mutatingwebhookconfigurations` rule in the controller's ClusterRole.

Charts up to 2.4.17 rendered the rule only with `injectPodNdots=true`. A release
with `controller.webhook.spire=true`, `namespaceInjection=true` and
`injectPodNdots=false` therefore told the controller to patch an object it could
not read. The default self-signed webhook, and any release with `injectPodNdots`
on (the default), was never affected. To check a release:

```bash
NS=aether-system
SA=$(kubectl -n "$NS" get deploy -l app.kubernetes.io/name=aether-controller \
  -o jsonpath='{.items[0].spec.template.spec.serviceAccountName}')
# Expect "yes" for each verb when the webhook is SPIRE-served and the
# MutatingWebhookConfiguration exists.
for verb in get list watch update; do
  printf '%s: ' "$verb"
  kubectl auth can-i "$verb" mutatingwebhookconfigurations.admissionregistration.k8s.io \
    --as="system:serviceaccount:$NS:$SA"
done
# Every entry of every aether webhook configuration must show a non-zero length.
kubectl get mutatingwebhookconfiguration,validatingwebhookconfiguration \
  -l app.kubernetes.io/name=aether-controller \
  -o jsonpath='{range .items[*]}{range .webhooks[*]}{.name}{" caBundle bytes: "}{.clientConfig.caBundle}{"\n"}{end}{end}' |
  awk '{ printf "%s %s %s %d\n", $1, $2, $3, length($4) }'
```

An entry at `0` is a webhook the apiserver cannot call; with
`failurePolicy: Ignore` it is skipped silently, so pods in an
`aether.io/managed` namespace come up outside the mesh and without the
identity gate.

What the controller's leader does when it cannot read or write a webhook
configuration (since #1431; reproduced in the controller's tests against a real
informer cache, not on a cluster):

- It logs `webhook caBundle injection failed` at ERROR, once per attempt, with
  the object in `kind` and `webhookConfig`, the step in `reason` (`get`,
  `update`, or `trust_bundle` when SPIRE gave an SVID but no bundle), the
  apiserver's error, and `retryIn`.
- It counts the attempt in `aether.controller.webhook.cabundle_injection_failures`
  (Prometheus: `aether_controller_webhook_cabundle_injection_failures_total`),
  by `kind` (`validating`, `mutating`) and `reason`. Expected to stay 0.
- It retries with a backoff that doubles from 1 s to 1 min, so a permission that
  appears is picked up within a minute, with no restart and no trust-bundle
  rotation needed.
- The two configurations are independent: the one that works keeps following
  SPIRE trust-bundle rotations while the other fails.
- The leader **stays Ready**. Leaving the webhook Service would not help the
  webhook whose caBundle is missing (the apiserver cannot verify any replica
  for it) and would take the working webhook's endpoint away. The log line and
  the counter are the signal, not readiness.

The injector reads the two objects from the apiserver directly, by name, and
needs only `get` and `update` on them; the `list` and `watch` the chart also
grants are no longer used by it.

A controller built before #1431 behaved differently in the same state: it read
through the manager's cache, whose informer could never list the configuration,
so the read waited instead of failing. That leader logs client-go's
`Failed to watch ... mutatingwebhookconfigurations is forbidden` lines and
**no** `webhook caBundle injection failed` line, stops refreshing the
*validating* webhook's caBundle on a trust-bundle rotation too, and is NotReady
on its `cache-sync` check. Adding the rule lets its informer sync on the next
retry; if the bundle does not appear within a few minutes, restart the
controller (a decision for the cluster's owner; the webhooks fail open while no
replica answers):

```bash
kubectl -n "$NS" rollout restart deploy -l app.kubernetes.io/name=aether-controller
```

### Grepping the outbound identity bindings during a soak (issue #638)

`ssl_fail_verify_san` bursts a few tens of seconds into a fresh proxy generation point at
the node's **netns → SPIFFE ID index** (`localWorkloads`), which is the whole of the
(source pod → outbound cluster → SDS client-cert secret) binding: every mTLS-injected
outbound cluster carries one transport-socket match per local identity, *named by* the
SPIFFE ID whose SDS secret it fetches, plus one matcher mapping each source pod's netns
to one of those names. The agent logs that binding at **INFO**, `outbound identity
binding`, only when a `(source pod, cluster)` pair re-binds or is seen for the first time
— so steady state is silent and a startup re-bind is exactly the handful of lines you
want. A binding whose bound secret is not the owning pod's own SPIFFE ID additionally
logs **WARN** `outbound cluster bound to a foreign identity` and increments
`aether_agent_identity_outbound_binding_mismatch_total`; an identity mapping no pod owns
any more (a missed CNI DEL) logs WARN `outbound identity mapping has no owning pod`.
Above 200 changed pairs in one snapshot a single `outbound identity bindings changed`
summary replaces the per-pair lines, keeping the distinct source→identity transitions.

```bash
# Every re-bind on one node, around the roll.
kubectl -n aether-system logs ds/aether-agent --since=10m \
  | grep -E 'outbound identity (binding|bindings changed|mapping)|foreign identity'

# The alarm, in VictoriaLogs (field syntax; never `| stats`, it false-zeroes).
_stream:{service.name="aether-agent"} AND "outbound cluster bound to a foreign identity"

Note: the stream labels on these logs are the dotted OTel resource fields (`service.name`,
`k8s.pod.name`, `k8s.namespace.name`). A selector on `k8s_container_name` matches nothing and
returns a FALSE ZERO — control-test any negative by dropping the selector.

# Same, as a counter: zero at steady state, any increase is the #638 defect.
# increase(), never a raw read — the raw value is per-process (an agent restart
# resets it) and an instant query lands between samples and false-zeroes.
sum by (node) (increase(aether_agent_identity_outbound_binding_mismatch_total[1h]))
```

Join the `snapshot_version` on the WARN with the first
`envoy_cluster_ssl_fail_verify_san_total` sample of the new proxy generation: a WARN at
(or just before) that timestamp proves the source-side binding was wrong; the absence of
one over a window that *did* produce SAN failures rules the agent-side index out and
moves the search to the destination proxy's inbound chain selection.

### Attributing an `ssl_fail_verify_san` event to the TERMINATING node (issue #638, inbound side)

**The inversion.** Envoy's `default_validator.cc:332` message
`verify cert failed: SAN matcher, certificate SANs are [...]` prints the SANs of the
certificate being *validated* — the one the **peer** presented. So
`envoy_cluster_ssl_fail_verify_san_total` is a **client-side** counter about the
**server's** certificate, and every #638 ledger join that read it as "the restarting
proxy presented X as its client identity" had it backwards. The wrong identity belongs to
a **server** certificate: the inbound filter chain → SDS server-secret binding of
whichever proxy **terminated** the connection. For same-node traffic that is the
restarting proxy itself, which is the observed "node-wide constant per time slice" shape.
The outbound discriminator above watches the client side and structurally cannot fire for
this.

**The inbound discriminator.** On every snapshot the agent reads each local pod's inbound
listener back out of the snapshot it just handed Envoy, extracts each filter chain's
`DownstreamTlsContext.tls_certificate_sds_secret_configs[0].name`, and compares it with
the SPIFFE ID of the pod that listener entry belongs to:

- **INFO `inbound identity binding`** — `chain` (`<listener>/<chain>`), `pod`,
  `pod_spiffe_id`, `secret` (the server certificate the chain will present),
  `previous_secret` (empty on a first bind), `secret_served`, `snapshot_version`. Emitted
  only on a first bind or a change; steady state is silent.
- **WARN `inbound chain bound to a foreign identity`** + counter
  `aether_agent_identity_inbound_binding_mismatch_total` — the chain would terminate mesh
  mTLS for its pod while presenting **another workload's** SVID.
- **WARN `inbound chain references a secret absent from the snapshot`** — the chain
  reached Envoy before its own SVID did (the other candidate mechanism). Reported, not
  counted.
- Above 200 changed chains in one snapshot a single `inbound identity bindings changed`
  summary replaces the per-chain lines.

The binding holds **by construction** inside `proxy.NewInboundListener` (the chain's
secret name and the chain's own name both come from the same `CNIPod`). That is the
point: **if the counter stays 0 through a #638 event while the INFO lines show the chains
re-binding correctly, the mis-binding is not in the agent's snapshot — it is in Envoy's
SDS/secret lifecycle across the hot restart**, and the agent-side line of enquiry closes.
A non-zero counter names the pod, both identities and the snapshot version.

```bash
kubectl -n aether-system logs ds/aether-agent --since=10m \
  | grep -E 'inbound identity (binding|bindings changed)|inbound chain '
```

```
# VictoriaLogs (field syntax; never `| stats`, it false-zeroes).
_stream:{service.name="aether-agent"} AND "inbound chain bound to a foreign identity"
```

```promql
# Zero at steady state; any increase is an agent-side inbound mis-binding.
# increase(), never a raw read: the value is per-process and an instant query
# lands between samples and false-zeroes.
sum by (node) (increase(aether_agent_identity_inbound_binding_mismatch_total[1h]))
```

#### The unpinned-cluster signal (#832)

The two discriminators above ask *"is the identity we present the right one"*. This one
asks *"are we checking the identity we are handed at all"*. A mesh cluster whose upstream
validation context carries no `match_typed_subject_alt_names` authenticates **any**
workload in the trust domain, so a foreign endpoint in its load assignment produces a
clean handshake and a delivered request instead of the `ssl_fail_verify_san` rejection
that caught #829. It is the fail-**open** direction.

There are four signals, and one vocabulary (`reason`) joins them:

| Signal | Says | Shape |
|---|---|---|
| WARN `mesh clusters published with no server-identity SAN pin` | **which** clusters, and why each | one line per `reason` per snapshot, first 20 names per line |
| counter `aether_agent_identity_cluster_unpinned_total{reason}` | **that** a snapshot went out with unpinned clusters | adds the unpinned count on every snapshot; seeded at zero per reason |
| gauge `aether_agent_snapshot_tls_clusters{pin,reason}` | **how many** clusters are pinned and unpinned **now**, per reason | written on every snapshot, zeros included |
| gauge `aether_agent_xds_acked_tls_clusters{pin,reason}` | the same, for the last snapshot whose cluster update the **proxy acknowledged** | written on every cluster ACK; absent before the first |

None of the metrics carries a cluster name: the names are in the log line only.

**The line (#1424).** Each cluster is named under the reason that emptied *its* pin, so a
snapshot logs at most one line per reason (three), each with at most 20 names followed by
`...`. `count` is that reason's exact number, `unpinned` and `pinned` are the snapshot's
totals, `trust_domain` is the trust domain in force when the snapshot was set. Agents
before #1424 logged one line with one `reason` for every cluster it named, chosen from the
trust domain at the time of the report (`trust domain not yet known` or `service endpoints
carry no namespace metadata`); a cluster listed there may have had the other cause.

| `reason` | What happened | Transient? | What to do |
|---|---|---|---|
| `trust_domain_unknown` | The pin was rendered while the agent did not know its trust domain, so there was no identity to name. The deliberate lesser evil over `spiffe:///ns/…`, which no certificate can satisfy and cost rev222 four endpoints (#815/#819). Every mesh cluster on the node has this reason at once. | Yes: it ends with the first snapshot after the agent learns its trust domain. | Nothing if it lasts a snapshot or two after an agent start. **If it persists it is the bug**: check that the agent has its own SVID (the agent's identity readiness, #740) and whether `trust_domain` on the line is still empty. A non-empty `trust_domain` under this reason means the trust domain was learned and the pins have not been re-rendered yet; the next snapshot does it. While it lasts the node publishes these clusters with **no TLS at all** (a peer's mesh inbound refuses them), not with an unpinned TLS context. |
| `no_namespace_metadata` | None of the service's endpoints carries a Kubernetes namespace in the registry, so the expected SPIFFE ID (`spiffe://<td>/ns/<ns>/sa/<svc>`) cannot be built. Once the node has its own SVID the cluster is published **with TLS and without a pin**. Before that (the first moments of an agent, or a node whose SVID never arrives) no mesh cluster carries TLS at all: an HTTP cluster goes out bare and the TCP floor cluster is withheld, and such an entry is still named and counted under this reason, because the pin is what is missing once TLS is there. The line alone does not tell the two apart: the gauge's `pin="pinned"` series does (zero on a node that publishes no TLS yet). | No. It lasts as long as the registry serves those endpoints. | This is the mTLS validation gap. Find who registered the endpoints (`clusters` on the line names the service) and why their `kubernetes_metadata.namespace` is empty; a destination that is not a Kubernetes workload with a SPIFFE identity of that shape cannot be pinned. |
| `pin_not_rendered` | The entry reached a snapshot without its pin ever having been rendered. | No code path does this. | An agent defect: file it with the line. |

The UDP floor's `udp:<svc>` clusters are never named and never counted, in any of the
four signals. They carry no transport socket at all (UDP rides the mesh in plaintext,
proposal 038), so they have no pin to lose: plaintext by design, not unpinned. Agents up
to chart 2.4.16 did name them, once per snapshot on every node with a UDP service in
scope, so on those versions the counter is never at rest and a line whose `clusters` is
only `udp:…` is not an authentication event (#1393).

**The gauges (#1425).** The counter cannot answer "how many clusters are unpinned now":
it grows by the unpinned count on *every* snapshot, so its rate is clusters times
snapshots. `aether_agent_snapshot_tls_clusters` is the count itself. It has four series
per agent whatever the size of the mesh: `pin="pinned"`, and `pin="unpinned"` once per
`reason`. A reason that no longer applies reads `0`; a cluster that gains its pin moves
from its `unpinned` series to `pinned`; a cluster that is removed leaves both. It counts
cluster *entries*: an HTTP service is three (its default cluster and two port aliases), a
QUIC twin is not counted apart from the cluster it is derived from, and an entry whose
pin is rendered is in neither series while the node cannot publish TLS for it (no node
SVID yet): it is not a pinned TLS cluster until one is published.

```promql
# N clusters unpinned for reason R on this node, now. This is the alert.
sum by (node, reason) (aether_agent_snapshot_tls_clusters{pin="unpinned"}) > 0

# The positive answer: TLS clusters were published, and all of them pinned.
sum by (node) (aether_agent_snapshot_tls_clusters{pin="pinned"}) > 0
  and sum by (node) (aether_agent_snapshot_tls_clusters{pin="unpinned"}) == 0

# Did it happen at all in a window, however briefly (a gauge is sampled; the counter
# is not). Seeded at zero, so a live zero is a real series (not an absent one).
sum by (node, reason) (increase(aether_agent_identity_cluster_unpinned_total[1h]))
```

**Published is not held.** The gauge above is what the agent *published*.
`aether_agent_xds_acked_tls_clusters` has the same series and is written when the proxy
**acknowledges** a cluster update: it then takes the values of the snapshot that update
was built from. It is the closest the agent gets to what the proxy holds without asking
the proxy's admin interface.

- The two agree at rest. They differ for the moment an update is in flight.
- They **stay** different while the proxy rejects cluster updates: a NACK acknowledges
  nothing, so the acknowledged gauge stays on what the proxy last accepted
  (`aether_agent_xds_nacks_total{aether_xds_type_url=~".*Cluster"}` moves, and the agent
  logs `envoy NACKed delta response`). Published `unpinned == 0` with acknowledged
  `unpinned > 0` reads: *the agent has pinned it, the proxy still holds the unpinned
  cluster*.
- **Absent is "not known", not zero.** Nothing is written before the first cluster ACK
  the agent process sees. An agent that restarts against a proxy already holding exactly
  the current clusters owes it no cluster update (delta xDS sends differences), so there
  is no ACK and no series until a cluster next changes. In that state the proxy holds
  what the published gauge shows.
- During a proxy hot restart two generations are connected; the gauge shows the last
  ACK from either.
- The agent logs the acknowledged state only when it **changes**: WARN `proxy
  acknowledged mesh clusters with no server-identity SAN pin` (with `unpinned`, `pinned`,
  one count per reason and `snapshot_version`, which joins it to the snapshot's own line
  and its cluster names), and INFO `proxy acknowledged mesh clusters, all with a
  server-identity SAN pin` when it returns to none.

```promql
# What the proxy last acknowledged, where it differs from what is published.
sum by (node, reason) (aether_agent_xds_acked_tls_clusters{pin="unpinned"})
  != sum by (node, reason) (aether_agent_snapshot_tls_clusters{pin="unpinned"})
```

```
# VictoriaLogs (field syntax; never `| stats`, it false-zeroes).
_stream:{service.name="aether-agent"} AND "published with no server-identity SAN pin" AND reason:no_namespace_metadata
```

The config-shape half is a build-time gate: `//agent/test/envoy_validate` asserts every upstream
TLS context in a generated bootstrap carries a non-empty `match_typed_subject_alt_names`.
`envoy --mode validate` **accepts** an unpinned context, so validation passing says nothing
about it.

#### The ledger join, with the terminating-node column

The join that has been missing: each failing request must be attributed to the node whose
proxy **served** it, then compared with the node whose proxy was restarting.

1. **Pull the events.** Field syntax only — a bare phrase search or `| stats` returns a
   documented FALSE ZERO. Control-test every negative by dropping the reason field (normal
   200s must come back).

   ```
   log_name:aether_access_logs
     AND upstream_transport_failure_reason:"CERTIFICATE_VERIFY_FAILED"
   ```

   Do not prefix it with `_stream:{service.name="aether-proxy"}`. The access-log records
   carry an empty `_stream` and no `service.name`, so that prefix matches nothing (0 of
   20,289,652 records over 2026-10-01 00:40:30–08:40:30Z).

   The identity in `certificate SANs are [spiffe://…]` on these lines is the **server's**.
   Keep `upstream_host`, `upstream_cluster`, `pod_name`/`pod_namespace` (the local pod this
   hop serves — the *client* side here), `response_flags` and the timestamp.

2. **`upstream_host` → pod → node.** `upstream_host` is `<pod IP>:18008`. Resolve the IP
   to its pod and that pod's node:

   ```bash
   kubectl get pods -A -o wide --field-selector status.phase=Running \
     | awk '$7=="<upstream-ip>" {print $1, $2, $8}'   # ns name node
   ```

   For an IP that is already gone, use the agent-side record on each node
   (`aether_agent_storage_pods` is the per-node count; the entries themselves are the
   agent's protojson store) or the pod-IP index in the run record. **That node is the
   terminating node** — its proxy holds the inbound listener whose chain presented the
   certificate the client rejected.

3. **Compare with the restarting node.** The proxy generation boundary per node:

   ```promql
   max_over_time(envoy_server_hot_restart_epoch[8h])          # step, per node
   max_over_time(envoy_cluster_ssl_fail_verify_san_total[8h]) # never an instant query
   ```

   `max_over_time` is mandatory — the series ages out with the proxy generation and an
   instant query at grade time returns **no series at all** (that trap has produced two
   premature "zero" readings on this issue).

4. **Read the verdict.**
   - Terminating node **==** the restarting node → same-node termination by the restarting
     proxy. Grep that node's agent for the WARNs above in the same window; a hit localises
     the defect to the agent's snapshot, a miss to Envoy's SDS lifecycle.
   - Terminating node **!=** the restarting node → the server was a remote proxy that
     itself shows no counter (the counter is client-side). Grep *that* node's agent log.
   - Terminating nodes **scattered across many nodes** for one presented identity → the
     identity was not bound per-server, and `upstream_host` is not the TLS-terminating
     peer; record it and re-open the transport path.

#### L4 hops: the source / destination / SAN join from the L4 access log (#1023)

The ledger above reads the HTTP access log, which has no record of an L4 hop: until
#1023 an L4 `ssl_fail_verify_san` tick had no source, no destination and no SAN on
record, and the #1007 event had to be inferred from per-pod connection counters and the
chain config. Every capture TCP/TLS chain now writes one record per connection on the
`aether_l4_access_logs` stream (see "L4 stat keys and the L4 access log" below), and a
SAN rejection is a connection with a response flag, so it is always logged, never
sampled away:

```
log_name:aether_l4_access_logs
  AND upstream_transport_failure_reason:~"CERTIFICATE_VERIFY_FAILED"
```

Control-test the negative the same way: drop the reason field and the clean
connections of the same chains must come back. Each line is the whole join:

| field | answers |
|---|---|
| `node_name` (resource attribute), `pod_name`, `pod_namespace`, `source_netns`, `source_spiffe_id` | **source**: which pod dialled, from which netns, presenting which identity |
| `filter_chain_name` | which capture chain took it: `cap_tcp_*` (floor, per-port, any-port shim), `cap_tls_*` (TLSRoute SNI), `cap_tcp_blackhole` |
| `downstream_local_address` | what the client **dialled** (the restored VIP:port) |
| `upstream_cluster` | which L4 cluster was chosen, as its **stat key** (`tcp_<ns>/<svc>[_<port>]`, the `aether_cluster` the tick landed on): Envoy's `%UPSTREAM_CLUSTER%` renders the cluster's `alt_stat_name`, not its config name `tcp:<fqdn>[:<port>]` |
| `upstream_host` | the **intended destination** endpoint, `<pod IP>:18008` → pod → node as in step 2 above |
| `requested_server_name` | SNI (`-` on the floor; the port on a per-port cluster; the hostname on a TLSRoute chain) |
| `upstream_transport_failure_reason` | the rejection, with the **presented** SAN where the pinned proxy prints it (`certificate SANs are [...]`) |
| `upstream_peer_uri_san` | the verified server identity on a **successful** connection (`-` on a rejection: the handshake never completed) |

Verdict, per line:

- Presented SAN **is** the identity of the pod at `upstream_host` → the connection reached
  the endpoint it dialled and the **pin** is wrong (check `sanURIs`, `cache/mtls.go`).
- Presented SAN is **another** workload's, and that workload has a pod on the **source's
  node** → a local pod's inbound terminated the connection, not `upstream_host`: the
  cross-pod landing below (#1007/#1022). The node's newest mesh pod at that minute is
  the usual suspect; its `in_tcp_<pod>` counter confirms it.
- Presented SAN is another workload's on a **different** node → `upstream_host` was not
  the terminating peer; record it and re-open the transport path, as for HTTP.

### L4 stat keys and the L4 access log (#1023)

**Stat keys.** Every L4 cluster reports under its own `alt_stat_name`, which the proxy
bootstrap's `aether.cluster` stats tag turns into the `aether_cluster` label:

| cluster (config_dump name) | `aether_cluster` |
|---|---|
| HTTP default `<svc>.<ns>.<domain>`, its port aliases and per-port clusters | `<ns>/<svc>` (unchanged) |
| QUIC twin `quic:<fqdn>@<ns>/<sa>` | `<ns>/<svc>@<ns>/<sa>` (unchanged, #960) |
| TCP floor `tcp:<fqdn>` | `tcp_<ns>/<svc>` |
| TCP per-port or primary-port alias `tcp:<fqdn>:<port>` | `tcp_<ns>/<svc>_<port>` |
| UDP floor `udp:<fqdn>` | `udp_<ns>/<svc>` |

Before #1023 every L4 row reported as `<ns>/<svc>`, so
`aether_cluster="aether-test/mixed-svc"` mixed the HTTP cluster with the `:9000` TCP
cluster, and a tick could not be assigned to a cluster kind (#1007).

- **`_`, not `:`.** Envoy sanitizes every stat name and tag value
  (`Stats::Utility::sanitizeStatsName` rewrites `:` to `_`), so a `tcp:…:9000` key would
  be exported as `tcp_…_9000` anyway, and a selector written as `tcp:.*` would match
  nothing, forever. The key aether writes is the label you query. Namespaces and service
  names are DNS labels, so `_` is unambiguous.
- **No `tls_` key.** A TLSRoute SNI chain routes to its backends' **per-port**
  `tcp:<fqdn>:<port>` clusters (the primary-port alias when the backendRef names the
  primary port), never the floor: every backendRef is resolved by its port, and Gateway
  API requires a port on a Service backendRef. So its connections count under
  `tcp_<ns>/<svc>_<port>`, and a selector anchored on the floor key
  (`^tcp_<ns>/<svc>$`) reads **nothing** for a TLSRoute backend (#1044; on kind,
  `upstream_cluster=tcp_aether-test/l4tls-a_9443`). The chain itself is told apart in the
  L4 access log (`filter_chain_name` = `cap_tls_*`).
- **Cardinality** is one key per cluster (services × raw-TCP ports), never per endpoint
  or per source.
- **HTTP queries are unaffected**: an exact `<ns>/<svc>` or `<ns>/<svc>(@.*)?` selector
  cannot match an L4 key. `//agent/test/envoy_validate` runs the chart's own tag regex over the
  keys, after Envoy's sanitization, and pins that.

```promql
# L4 only, every kind
sum by (node, aether_cluster) (rate(envoy_cluster_upstream_cx_total{aether_cluster=~"tcp_.*|udp_.*"}[5m]))
# One service's L4 clusters: the floor and every port
sum by (aether_cluster) (rate(envoy_cluster_upstream_cx_total{aether_cluster=~"tcp_aether-test/mixed-svc(_[0-9]+)?"}[5m]))
# A TLSRoute backend: always port-qualified (the _<port> suffix is required)
sum by (aether_cluster) (rate(envoy_cluster_upstream_cx_total{aether_cluster=~"tcp_aether-test/l4tls-a_[0-9]+"}[5m]))
# Client-side SAN rejections on L4 clusters (the soak gate; never an instant query)
max by (node, aether_cluster) (max_over_time(envoy_cluster_ssl_fail_verify_san_total{aether_cluster=~"tcp_.*"}[8h]))
```

A dashboard or alert that selected an L4 service by its bare key
(`aether_cluster="aether-test/tcp-echo"`) reads **no series** from a #1023 proxy onward.
Move it to the `tcp_` key.

**The L4 access log.** Every `tcp_proxy` chain on a capture listener carries an OTel
access logger on the stream **`log_name=aether_l4_access_logs`**, next to the HTTP stream
`aether_access_logs`. That covers the TCP floor and its port spellings, per-port chains,
TCPRoute-weighted chains, the any-port shim, TLSRoute SNI chains and the scoped-mode
`cap_tcp_blackhole`.

- **Same switch, same sink.** It is on exactly when the HTTP log is (MeshConfig
  `accessLogsEnabled`; no new flag) and ships to the same `otel_collector` cluster.
- **Connection-level.** One record when the downstream connection **closes** (no
  `access_log_options` flush interval). Logged: every connection with a response flag
  (`UF`, `UH`, `UO`, `NR`, …, which is where a SAN rejection or a blackholed flow lands)
  plus the HTTP log's success sample (`aether.access_log.sample`, the same runtime key).
- **Not logged:** the redirect-all passthrough `DefaultFilterChain` (all non-mesh egress),
  the UDP capture listener (no per-datagram log), and the inbound side.
- **A separate stream** because the shapes differ: no method, path, authority, status or
  request id. It deliberately carries **no `reporter`** attribute. The collector's
  identity counters (`aether_access_log_*`, k8s-talos-main otel-collector values) select
  on `reporter`, because `log_name` is a resource attribute their transform cannot see,
  and an L4 record must not enter the HTTP request counters.
- **Fields:** `pod_name`, `pod_namespace`, `source_netns`, `source_spiffe_id` (the source),
  `filter_chain_name`, `downstream_local_address` (the dialled VIP:port),
  `downstream_remote_address`, `upstream_cluster`, `upstream_host`,
  `upstream_local_address`, `requested_server_name` (SNI), `upstream_peer_uri_san`,
  `response_flags`, `upstream_transport_failure_reason`,
  `connection_termination_details`, `start_time`, `duration_ms`, `bytes_received`,
  `bytes_sent`, `proxy_epoch` (the proxy generation that held the connection; see
  "Reading a hot-restart overlap"). The node is Envoy's resource attribute `node_name`.

```
# Every failed L4 connection
log_name:aether_l4_access_logs AND response_flags:!"-"
# One service's L4 traffic
# (upstream_cluster is the stat key, tcp_<ns>/<svc>[_<port>], never the tcp:<fqdn> config name)
log_name:aether_l4_access_logs AND upstream_cluster:~"^tcp_aether-test/tcp-echo(_[0-9]+)?$"
# TLSRoute chains only
log_name:aether_l4_access_logs AND filter_chain_name:~"^cap_tls_"
# One TLSRoute backend (per-port key: tcp_<ns>/<svc>_<port>, never the bare floor key)
log_name:aether_l4_access_logs AND filter_chain_name:~"^cap_tls_" AND upstream_cluster:~"^tcp_aether-test/l4tls-a_[0-9]+$"
```

### Cross-pod L4 landings (#1007/#1022)

**Symptom.** A node proxy's outbound L4 connection to `tcp-echo` or `mixed-svc` is
rejected with `ssl_fail_verify_san`: it reached the inbound `:18008` listener of an
**unrelated pod on the same node** (always the node's newest mesh pod), which presented
its own SVID. The soak's `mp-dialer` shows it as one failure on every L4 leg at once.

**Two defects, one proof order.**

- **(b) #1022, Envoy.** `Network::Utility::execInNetworkNamespace` recorded the
  namespace to return to from `/proc/self/ns/net`, which is the **main thread's**
  namespace. `setns()` is per thread, so a worker calling it while the main thread was
  briefly inside a pod netns (health-check connects every 5 s per pod, listener socket
  creation) "restored" itself **into** that pod's netns and stayed there, creating its
  later upstream sockets inside the pod where redirect-all capture diverted them. The
  proxy carries `proxy/bazel/patches/envoy-aether1022-exec-in-netns-thread-self.patch`
  (`/proc/thread-self/ns/net`, fallback `/proc/self/task/<tid>/ns/net`).
- **(a) #1007, aether.** The capture listener's `use_original_dst: true` hands a
  diverted connection to "the listener bound to its original address", looked up by the
  address string only (`0.0.0.0:18008`, no netns), so the most recently added pod's
  inbound listener wins.

Fixing (a) alone **hides** (b): the leaked connection would then leave through the
right endpoint's ORIGINAL_DST from a pod IP and succeed silently, and this counter would
go quiet for the wrong reason. So (b) is proven on talos first, with (a) still in place.

**The proof signal: TCP-floor connections on pods that serve no raw-TCP port.** The
inbound listener's DEFAULT chain is the TCP floor (`in_tcp_<pod>`, stat prefix
`inboundTCPFloorStatPrefix` in `agent/internal/xds/proxy/ingress.go`). Only a pod whose
primary port is raw TCP can legitimately receive a connection there; per-port raw-TCP
chains are `in_tcp_<pod>_<port>` and are excluded. The pod name is part of the METRIC
NAME, so select by `__name__` pattern and read the RAW counters (a series is born on the
first stray connection):

```promql
# Stray landings: default floor chain of every pod except tcp-echo (TCP-primary);
# the per-port chains (…_<port>_downstream_cx_total) are legitimate and excluded.
sum by (__name__) ({__name__=~"envoy_tcp_in_tcp_.*_downstream_cx_total",
                    __name__!~"envoy_tcp_in_tcp_tcp_echo_.*|envoy_tcp_in_tcp_.*_[0-9]+_downstream_cx_total"})

# The same over a window, with the pod lifted into a label (the soak gate, #1023).
# max_over_time drops __name__, so the label is taken first, inside a subquery.
max by (node, pod) (max_over_time((label_replace(
  {__name__=~"envoy_tcp_in_tcp_.+_downstream_cx_total",
   __name__!~"envoy_tcp_in_tcp_(tcp_echo_.+|.+_[0-9]+)_downstream_cx_total"},
  "pod", "$1", "__name__", "envoy_tcp_in_tcp_(.+)_downstream_cx_total"))[8h:1m]))

# The client side: SAN rejections on the L4 clusters, per node and per L4 cluster
# (never an instant query). From #1023 on, the tcp_ keys hold ONLY L4 clusters, and
# aether_cluster names the floor or the port the misdirected connection was dialled on.
max by (node, aether_cluster) (max_over_time(envoy_cluster_ssl_fail_verify_san_total{aether_cluster=~"tcp_.*"}[8h]))
# ...on a pre-#1023 proxy the same ticks read under the bare keys, mixed with HTTP:
max_over_time(envoy_cluster_ssl_fail_verify_san_total{aether_cluster=~"aether-test/(tcp-echo|mixed-svc)"}[8h])

# The landing pod's inbound sees the client abort after its SAN check
max_over_time(envoy_listener_inbound_ssl_connection_error_total[8h]) > 0   # by aether_pod, node
```

The exclusion list is the soak's: `tcp-echo` is its only TCP-primary workload. The
stat keys cannot derive it (an HTTP-primary service with a raw-TCP port, like
`mixed-svc`, has a `tcp_` floor key too), so a cluster with another TCP-primary
workload adds it to the `__name__!~` alternation. A pod whose 5-character hash suffix
happens to be all digits is excluded along with the per-port chains; that is rare
(about 0.1% of pods) and errs toward a missed landing, not a false one.

Attribute a tick by joining on node and minute. Since #1023 the L4 access log carries the
whole join in one record (see "L4 hops" above): the source pod, the dialled VIP:port,
the chosen `tcp:` cluster, the intended `upstream_host` and the rejection. Before it,
the join was three counters: the `verify_san` +1 on node N, a new or incremented
`in_tcp_<pod>` series for a pod on N, and that pod's `inbound_ssl_connection_error`
climbing in the same minute (the 2026-09-27 16:37Z w04 event in #1007 is the worked
example).

**Reading it.**

| build | expected |
|---|---|
| rev242 and earlier (no thread-self patch) — the negative control | non-zero on svc-1..5, prober, k6-soak-loader, udp-dialer (and echo, uds-cr-echo, udp-echo); ~1 burst per node per hour; `verify_san` ticks on `tcp-echo`/`mixed-svc` (23 over the rev242 soak) |
| rev243 (unpatched, 1h47m generation, 2026-09-27 22:53Z–09-28 00:39Z) | 6 stray floor connections, all on `prober` pods (w05 2, w03 3, w04 1), and 1 `verify_san` on w03 `tcp-echo` |
| first proxy with the #1022 patch, #1007 still unfixed | **no new series and no increments** after every node's proxy has rolled onto it (series from older generations age out with them) |

A landing that persists on the patched proxy **refutes** #1022 as the (only) cause:
something else moves node-proxy sockets into pod netns — keep #1007 unmerged and
re-open the attribution. Only once the patched proxy reads zero for a full soak does
the #1007 capture fix merge; after it, this counter no longer discriminates (b).

### Known-unexercised code paths

Recovery branches that have never run in production. **This is the system working, not a
backlog** — it is recorded so nobody mistakes "no data" for "untested logic", and so nobody
re-attempts a trigger that is known not to be forceable. Retired from #813, which was an
umbrella that could not close.

| path | why it has never fired | how it is covered |
|---|---|---|
| snapshot stale-netns guard, `aether_agent_snapshot_stale_netns_skipped_total` (#798) | a replacement agent returns in ~5 s, inside the 60 s `NetnsUnpinDelay` | would need an agent held down > 60 s on a node with a terminated app pod there |
| CNI DEL `agent unreachable` WARN, the `<pin>.delfail` give-up path, the pin unlink (#796) | same — the agent comes back too fast | unit-tested |
| supervisor `drain_fallback` (#797) | the normal drain path always wins | `aether_supervisor_shutdown_branch_total{branch="drain_fallback"}` has never been non-zero anywhere |
| edge xDS "registry unreachable … while this workload waits for its first SVID" (#807) | both edge pods reach the registrar in ~3 s | unit-tested only |
| `svid_updates{update="unchanged"}` (#806) | a SPIRE agent restart re-mints, so it counts `rotated` | would need a Broker stream that drops while the SPIRE agent stays up |
| orphan prune → SVID unsubscribe (#804) | **not forceable from a harness** — the runtime re-issues CNI DEL at sandbox removal and a restarted agent serves it before the first sweep pass (measured 2026-09-19) | proven once in production (2026-09-18); the deterministic check is the in-process test from #805 |

Two cautions when tempted to "test" one of these:

- **Contriving the precondition proves the branch compiles, not that it behaves.** Forcing
  `drain_fallback` or the #807 WARN by hand exercises the code under conditions you invented,
  which is the same trap as a test you author both sides of.
- **#804 in particular should not be re-attempted from a harness.** That was measured and
  ruled out; the in-process test is the coverage.

Genuine test debt is tracked separately — see #868 for L4 route e2e coverage, which is a
shipped default-on feature with no end-to-end test at all.

### A pod is stuck `Terminating`, or a node has stale netns entries (#245, #796)

Two related symptoms, one mechanism: the pod's CNI DEL never completed.

**Symptom A — the sandbox will not tear down.** `kubectl describe pod` shows repeated
`KillPodSandbox` failures, the pod sits `Terminating` for minutes, and — the part that
hurts — it keeps its **CPU request** the whole time. On a node that is already tight this
starves the replacements for that node's own DaemonSets (`0/6 nodes are available:
1 Insufficient cpu`), so the agent, proxy and mesh-dns cannot come back and the node goes
unmanaged. Priority does not help: the scheduler refuses to preempt on a node that has a
terminating pod (`preemption: not eligible due to a terminating pod on the nominated
node`). That is the #796 outage — 12m47s on worker-03.

**Since #796 the plugin does not enter that loop when the agent is simply gone.** CNI DEL
classifies the failure:

- **Nothing answered** (the agent's socket is missing, or the RPC came back
  `Unavailable`/`DeadlineExceeded`/`Canceled`) → the DEL logs WARN `agent unreachable at
  CNI DEL; unpinning on the normal delay and letting the ghost sweep reconcile`, skips the
  DEL readiness probe, schedules the usual detached unpin, and **returns success**. The
  sandbox tears down, the CPU request is released, and the node keeps its agent.
- **A live agent answered with an error** → unchanged: the DEL fails, containerd retries,
  the netns stays pinned. That loop is now bounded by `netns_del_give_up_after_seconds`
  (default 5m, tracked in a `<pin>.delfail` marker beside the netns pin, since the plugin
  process lives for exactly one CNI call). Past the bound it degrades to the first case
  and logs WARN `CNI DEL has been failing past the give-up bound`.

So a pod still stuck `Terminating` means either a **live** agent that keeps refusing the
removal (grep that node's agent for the RemovePod error) or a failure outside aether
(the primary CNI, the runtime). Confirm which before force-deleting:

```bash
# Is the node's agent even there? (the unreachable case should no longer stick)
kubectl -n aether-system get pods -o wide --field-selector spec.nodeName=<node>
# The plugin's own verdict, on the node:
journalctl -u containerd | grep -E 'agent unreachable at CNI DEL|give-up bound'
# Node pressure, which is what turns one stuck DEL into an outage:
kubectl describe node <node> | sed -n '/Allocated resources/,/Events/p'
```

Force-deleting the pod (`--grace-period=0 --force`) clears it in seconds and is the right
escape hatch, but it leaves exactly the state of symptom B.

**Symptom B — stale storage entries for pods that no longer exist.** The agent's host
registry (`/var/lib/aether/registry/<containerID>.json`) still lists a pod whose netns is
gone. This is expected after the unreachable path above: the plugin completed the DEL
without the agent, so nothing deregistered the pod. **It self-heals — do not hand-edit
the registry.**

- At startup, `LoadListenersFromStorage` skips any pod whose netns is missing, so the
  stale entry never reaches the snapshot (`skipping pod with missing network namespace`).
- On **every** snapshot generation the same check runs again (#717): the pod's listeners
  and its per-pod app/health clusters are left out, logged once per pod as
  `skipping pod with missing network namespace in snapshot generation`, and counted in
  `aether_agent_snapshot_stale_netns_skipped_total`.
- The ghost sweep (60s) prunes the entry, drops its listeners and per-pod clusters, and
  deregisters the endpoint. It counts the prune in
  `aether_agent_ghost_sweep_stale_pruned_total` (OTel instrument
  `aether.agent.ghost_sweep.stale_pruned`).
- CNI GC unpins orphan netns pins and removes orphan `.delfail` markers as a backstop.

```promql
# Prunes per node over the last hour. A burst right after an agent comes back on a node
# is the expected #796 reconciliation; a series that never stops is a defect.
sum by (node) (increase(aether_agent_ghost_sweep_stale_pruned_total[1h]))
# Stale entries the snapshot generator had to step over. Non-zero is normal for a minute
# after a DEL the agent missed; still climbing an hour later means the sweep is not
# pruning. Both counters are seeded, so a flat 0 is a real reading.
sum by (node) (increase(aether_agent_snapshot_stale_netns_skipped_total[1h]))
# Entries the agent is tracking per node -- must settle at the node's managed pod count.
aether_agent_storage_pods
```

> The metric families are `aether_agent_ghost_sweep_*` and `aether_agent_snapshot_*`.
> Querying `aether_ghost_sweep_*` or the dotted OTel spelling returns a **false zero**.

**Why the snapshot-time skip matters more than the crash ever did.** A stale netns on the
*listener* side does not crash the pinned proxy — the listener is rejected
(`listener_manager.listener_create_failure`, `listener_manager.lds.update_rejected`, the
log line `failed to open netns file <path>: No such file or directory`, and
`lds.version_text` frozen at the last good version) and everything else keeps serving. But
a **hot-restart successor** asked to create that listener NACKs the whole LDS response and
comes up with **zero listeners** — every listener on the node, not just the stale one —
because Envoy jumps into the netns *before* it asks the parent for the socket, so the
inheritance that would have worked never happens. The port then dies when the parent
finishes draining. That is why the agent filters stale pods out of every generation rather
than relying on the sweep's ≤60s window being quiet. An in-place LDS *modify* of such a
listener is silently accepted (the socket factory is cloned and the netns is never
reopened), so a dangling path can hide across arbitrarily many updates and only surface at
the next roll — do not read "LDS is being accepted" as "no stale netns".

```promql
# After a proxy roll on a node that had a stale entry: this must not go to zero.
envoy_listener_manager_total_listeners_active
max_over_time(envoy_listener_manager_lds_update_rejected[1h])   # never an instant query
max_over_time(envoy_listener_manager_listener_create_failure[1h])
```

**Why the stale entry is no longer dangerous.** It used to be: Envoy 1.38 dereferenced a
nullptr when a dial (or a cold-start health checker) opened a netns that had vanished, so
one stale entry could make the node proxy unbootable — the whole reason CNI DEL blocked
on the agent's ACK. The pinned proxy snapshot (`1.40.0-dev.20260926.726d7ac`, and every pin since `20260904.13144fb`) carries
envoyproxy/envoy#45975 (the pool dial returns a clean `LocalConnectionFailure`, a `UF` for
that request) and #46503 (the active TCP/HTTP/gRPC health checkers record a `NETWORK`
failure instead of crashing). A stale per-pod cluster now costs at most one failed request
plus an unhealthy host until the sweep prunes it. envoyproxy/envoy#45976 (opt-in netns
validation at config load) is in the snapshot too and stays **off** — it would turn a
stale pod into an LDS/CDS NACK, which is worse. The netns pin and its 60s unpin delay
stay: a hot-restart successor re-creating the pod's listeners and dials deferred 10-13s
past removal still need a live netns to *succeed* rather than merely fail cleanly.

### One node agent looks several times hotter than its peers in Pyroscope (#1131)

**Symptom.** In Pyroscope (`process_cpu`, `service_name="aether-agent"`, grouped by
node) one `aether-agent` pod shows 2–5× the CPU of the others while doing the same
work. Which agent is hot changes when agents restart and is fixed for the life of the
process.

**Resolution (2026-10-03).** It is a sampling artifact, not a hot agent. The kernel's
own per-process counter, `aether_agent_sched_cpu_time_seconds_total` (schedstat
`sum_exec_runtime`, exported by the agent since chart 2.2.4), put the five agents
within 1.16–1.24× of each other over the same hours in which Pyroscope showed 2.5× —
and named a different node as the hot one. The two sources agree on the fleet total
(the profiler's ~10 % surplus is softirq time it attributes to the process); only
the split between pods is wrong. The profiler sampled at 20 Hz, exactly 50 ms, which
divides the periods of everything a mostly idle agent does (kubelet readiness 2 s and
liveness 10 s, controller-runtime's 500 ms and 10 s queue timers, Go's
round-millisecond tickers): the sample instants line up with one process's timer
bursts and miss another's, so identical work is charged many times more to one pod,
and the phase only changes on restart. The flame-graph "excess" was always strictly
periodic work (the :8082 health handler, the queue tickers) with identical request
rates on every node, and other low-activity timer-driven pods (uds-csi, the MetalLB
speaker) show the same spread.

**Do not compare mostly idle processes by Pyroscope per-pod `process_cpu`.** Use the
kernel counters, which need no profile:

```promql
# Hot-agent ratio: max / min agent CPU across nodes, 30 m windows. ~1.2× is normal.
max(rate(aether_agent_sched_cpu_time_seconds_total[30m]))
  / min(rate(aether_agent_sched_cpu_time_seconds_total[30m]))
```

If this ratio is genuinely high, the agent's scheduler counters tell "more wakeups"
from "costlier wakeups". They are read from `/proc/self/task/*/{schedstat,status,sched}`,
summed over every thread, and monotonic even when threads exit; they are observable
instruments, so reading them adds no timer.

| Metric (Prometheus name) | Source | What it tells you |
| --- | --- | --- |
| `aether_agent_sched_timeslices_total` | schedstat field 3 | Times a thread was put on a CPU. Its rate is the **wakeup rate**. |
| `aether_agent_sched_run_delay_seconds_total` | schedstat field 2 | Time spent runnable but waiting for a CPU. Divided by timeslices, it is the **wait per wakeup**. |
| `aether_agent_sched_cpu_time_seconds_total` | schedstat field 1 | CPU time on a CPU. Divided by timeslices, it is the **CPU per wakeup**. |
| `aether_agent_sched_context_switches_total{aether_sched_switch="voluntary"\|"involuntary"}` | `/proc/…/status` | Voluntary switches (blocked or slept) follow the wakeup rate. Involuntary ones (preempted) point at contention. |
| `aether_agent_sched_migrations_total` | `se.nr_migrations` in `/proc/…/sched` | CPU migrations. **Absent** on a kernel without `CONFIG_SCHED_DEBUG`. |
| `aether_agent_sched_threads` | task count | Live OS threads. A hot agent with many more threads is a different problem. |
| `go_schedule_duration_seconds_bucket` | Go `runtime/metrics` `/sched/latencies` | How long runnable goroutines waited for a P. This is the Go-level twin of `run_delay`. |

Every series carries `job="aether-agent"` and the `node`, both from the resource
attributes. No series is seeded: an observable counter reports its cumulative value
on every collection, so a missing series means the agent is not exporting it, not a
value of zero.

```promql
# wakeups per second, by node
sum by (node) (rate(aether_agent_sched_timeslices_total[30m]))
# mean run-queue wait per wakeup (seconds), by node
sum by (node) (rate(aether_agent_sched_run_delay_seconds_total[30m]))
  / sum by (node) (rate(aether_agent_sched_timeslices_total[30m]))
# CPU cost per wakeup and migrations per wakeup
sum by (node) (rate(aether_agent_sched_cpu_time_seconds_total[30m]))
  / sum by (node) (rate(aether_agent_sched_timeslices_total[30m]))
sum by (node) (rate(aether_agent_sched_migrations_total[30m]))
  / sum by (node) (rate(aether_agent_sched_timeslices_total[30m]))
# Go scheduler p99 latency, by node
histogram_quantile(0.99, sum by (node, le) (rate(go_schedule_duration_seconds_bucket{job="aether-agent"}[30m])))
```

More timeslices per second at the same CPU per timeslice means a remaining ticker;
the same timeslice rate with more CPU, run delay or migrations per timeslice means
CPU placement or idle states, not the agent's code.

**What #1131 changed anyway (#1146).** The agent's controllers use a queue with no
metrics (`common/ctrlqueue`), so each one no longer runs a 500 ms
`updateUnfinishedWorkLoop` ticker (nothing scraped the agent's `workqueue_*` series),
and `--debug` no longer enables controller-runtime's V(2)+ logging: its verbosity is
capped at V(1) (`manager.ControllerRuntimeMaxVerbosity`), so the V(5)
`workqueue_items … items=[]` dump every 10 s per queue is gone. aether's own debug
and trace lines are unchanged. These stand as cleanups; they did not, and could not,
move the Pyroscope ratio.

**Profiler side.** The eBPF profiler's sample rate should not divide the common timer
periods: 19 Hz costs the same as 20 and does not; 97/99 Hz is the conventional choice
if more overhead is acceptable. After the change the agents' Pyroscope max/min ratio
should collapse towards the schedstat ~1.2×.

### A UDS workload is not delivered: `aether_agent_uds_resolve_failures_total{reason}`

A pod that asks for UDS delivery (annotation or `EndpointPolicy`) but whose
socket the agent cannot resolve falls back to TCP loopback; a UDS-only app then
stays **unpromoted** (callers see no endpoint, never a blackhole). The agent
counts it once per pod per reason and logs one ERROR line naming the pod
(`failed to resolve the pod's UDS socket; …`, fields `reason`, `pod`,
`namespace`, `socket`, `source`=`annotation|endpointpolicy`).

| `reason` | Meaning | Fix |
|---|---|---|
| `not_csi` | The named volume exists but is not `csi: {driver: csi.aether.io}` — almost always an `emptyDir` left over from before chart 2.0.0 | Switch the volume source and add `securityContext.fsGroup` (see §7 *Upgrading to chart 2.0.0*) |
| `volume_not_declared` | The pod declares no volume of that name (typically an `EndpointPolicy` drifted from its Deployment) | Fix the policy's `udsSocket` or the pod spec |
| `bad_file` | The file part is not a single clean path segment | Fix the value |
| `path_too_long` | `<uds-csi-root>/<uid>/<file>` over 107 bytes (file over 54 bytes at the default root) | Shorten the socket file name |
| `multiple_csi_volumes` | The pod declares two `csi.aether.io` volumes | Keep one |
| `no_uid` | The stored pod record has no UID (written by a pre-034 agent) | Restart the pod |
| `bad_request` | The value is not `<volume>/<file>` | Fix the value |
| `disabled` | The chart runs with `udsCsi.enabled: false` (`--uds-csi-root=`) | Enable it |

A pod stuck in `ContainerCreating` with `FailedMount: driver name csi.aether.io
not found in the list of registered CSI drivers` is on a node where the
`aether-uds-csi` DaemonSet is not (yet) running; one with `FailedMount … requires
the pod to set securityContext.fsGroup` needs an fsGroup. A deleted UDS pod stays
`Terminating` while the plugin is down on its node (the kubelet cannot
unpublish); it finishes once the plugin is back.

### A pod started uncaptured, or a pod start is slow in the CNI plugin (#1166)

The CNI plugin binary exports no telemetry of its own. Until #1166 it linked the
OTel SDK and the OTLP exporters (18 of its 37 modules, 3.2 MB of its 18.7 MB) and
flushed them before every exit, which cost every pod ADD/DEL ~6 ms with a reachable
collector and **2 s** with an unreachable one — the #950 state talos-main was in for
weeks. Nothing queried its spans. What only the plugin can see now reaches the
collector through the agent:

- **Capture divert** (the nftables table + policy routing it installs AFTER AddPod
  answers): the plugin sends it on `ReportAddResult`, and the agent counts
  `aether_cni_operations_total{aether_cni_operation="capture_divert",aether_cni_result=…}`
  (same name and labels the plugin exported) and logs
  `CNI plugin failed to install the transparent-capture divert; POD IS RUNNING UNCAPTURED`
  at WARN with the pod. A non-zero `error` series is a pod the mesh silently does
  nothing for. The `success` series is the existence proof that reporting works.
- **Timings**, as attributes on the agent's RPC spans (`--trace-export`):
  `aether.cni.plugin.pre_call_seconds` on `AddPod`/`RemovePod` (netconf parse, PID/CRI
  lookup, netns pin — before the agent's own span starts), and on `ReportAddResult`
  `aether.cni.plugin.{readiness_probe,capture_divert,total}_seconds` plus
  `…readiness_probe_error` / `…capture_divert_error`.
- ADD/DEL counts and latency as the agent served them: otelgrpc's `rpc.server.*`
  metrics for `aether.cni.v1.CNIService/AddPod` and `/RemovePod`.

Still node-local only: an ADD that never reached the agent (agent down). Its trace is
the kubelet's `FailedCreatePodSandBox` event and the plugin log,
`/var/log/aether-cni/plugin.log` on the node (`talosctl -n <node> read …`).

### A pod never becomes routable: what HEALTHY means since #815

An endpoint is advertised to the mesh only after the node agent's liveness loop
promotes it to `HEALTH_HEALTHY`. Since issue #815 the agent probes **two**
independent facts about each local pod, on **two separate gateway paths**:

| Gateway path | Probe cluster | What it proves | Transport |
|---|---|---|---|
| `/healthz/health_<pod>` | `health_<pod>` | the application answers (HTTP GET on the readiness path, or a raw TCP connect for a `protocol: tcp` pod) | cleartext, in the pod's netns |
| `/healthz/inboundready_<pod>` | `inboundready_<pod>` | the pod's **mesh inbound listener** is listening, has loaded the pod's own SVID, and it verifies as that pod | mTLS, node identity → `127.0.0.1:18008` in the pod's netns, SAN-pinned to the pod's SPIFFE ID |

Each path reflects **its own cluster only**. `/healthz/health_<pod>` means
exactly what it meant before #815. A `404` on either path means "not programmed"
— for the inbound path that is the normal, expected answer for an **ungated**
pod, and it is never read as unhealthy.

`inboundready_<pod>` exists because `health_<pod>` has no SDS dependency at all,
while the inbound listener does not `listen()` until its SVID arrives — so an
endpoint used to be advertised HEALTHY while its mesh port still returned
ECONNREFUSED (measured: promotion p50 5.8 s against an SVID at 6.1–8.4 s). It is
**not emitted** when SPIRE is disabled (the inbound listener is cleartext, so
there is nothing to prove), before the node SVID has been served (no client
certificate to present), in edge mode, or for a pod with no netns.

> **Why the two paths are separate, and not ANDed.** The first attempt (#819)
> put both clusters behind the single `/healthz/health_<pod>` path. The agent
> could then only see their conjunction, so "the application is fine but the
> mesh inbound never came up" was indistinguishable from "the application died".
> On 2026-09-19 main-worker-03 published inbound listeners with an EMPTY trust
> domain, the conjunction went 503, and four **already-serving** endpoints were
> demoted and never re-promoted. A signal meant to gate a *first* promotion had
> become a permanent trap.

#### The promotion rules

| application probe | inbound-readiness probe | endpoint already serving? | agent's decision |
|---|---|---|---|
| fails | any | any | **UNHEALTHY** (warm-up grace and the demote streak still apply) |
| passes | `404` (ungated) | any | **HEALTHY** — no gate is programmed for this pod |
| passes | passing | any | **HEALTHY** — application up *and* mesh inbound proven |
| passes | never passed this epoch | **yes** | **HEALTHY** — *can't-tell*: an already-serving endpoint is never pulled on the TLS probe alone. WARN + `inbound_gate_held_pods` |
| passes | never passed this epoch | **no** | **held un-promoted** — this is the hole the gate closes. WARN after 60 s |
| passes | passed earlier, failing now | any | **UNHEALTHY** after the demote streak — expired SVID, or the listener lost its secret |

"Already serving" means the endpoint **is or has been advertised HEALTHY** in
the registry, not merely that the application probe passed once. Active-mode
endpoints register HEALTHY at CNI ADD and so qualify immediately; EDS-mode
endpoints register UNHEALTHY and qualify only after their first promotion. This
survives an Envoy epoch change on purpose — after a proxy roll the endpoint is
still advertised, which is exactly why the unproven probe must be can't-tell.

#### Metrics

| Metric | Meaning |
|---|---|
| `aether_agent_liveness_inbound_gate_pods{aether_gate_state="gated"\|"ungated"}` | local pods with / without the gate programmed, recorded every liveness pass (5 s) |
| `aether_agent_liveness_inbound_gate_held_pods` | pods currently held un-promoted, or serving only on the can't-tell fallback |
| `aether_agent_liveness_health_transitions_total{aether_health_from,aether_health_to}` | endpoint health transitions; **seeded at 0** for HEALTHY→UNHEALTHY and UNHEALTHY→HEALTHY so "zero demotions" is gradeable |

```promql
# Is the gate on at all on every node? A standing ungated count with SPIRE
# enabled means the gate is missing, not that the pods are fine.
max_over_time(aether_agent_liveness_inbound_gate_pods{aether_gate_state="ungated"}[5m]) > 0

# Anything held by the TLS probe right now.
max_over_time(aether_agent_liveness_inbound_gate_held_pods[5m]) > 0

# Demotions, per node. This series now exists even when it is zero.
increase(aether_agent_liveness_health_transitions_total{aether_health_from="HEALTH_HEALTHY",aether_health_to="HEALTH_UNHEALTHY"}[10m])
```

The agent also logs the gate state once per change (and once at startup):

```
inbound-readiness promotion gate ACTIVE: HEALTHY requires an mTLS handshake with the pod's own inbound listener  gated_pods=5
inbound-readiness promotion gate NOT active for every local pod; those pods keep the application probe alone  gated_pods=0 ungated_pods=5 missing="no node SVID yet (…)"
```

and one rate-limited WARN per held pod (once per 5 min, after a 60 s grace):

```
liveness: pod held by the inbound-readiness probe  pod=svc-1-… probe_cluster=inboundready_svc-1-… gateway_path=/healthz/inboundready_svc-1-… why="held un-promoted: the inbound mTLS probe has never passed (inbound listener has no certificate, or its SDS secret name is not served)"
```

#### Named failure: `envoy_sds_spiffe_ns_*` — listeners built with an EMPTY trust domain

**Signature.** Envoy subscribes to SDS secrets named `spiffe:///ns/<ns>/sa/<sa>`
— note the **missing trust domain** between the second and third slash. The
agent never serves that name, so the secret never resolves, the inbound listener
never gets a certificate, and the pod is unreachable on the mesh. The healthy
form is `spiffe://aether.internal/ns/…`.

```promql
# Any proxy subscribing to a trust-domain-less secret. Should be EMPTY, always.
{__name__=~"envoy_sds_spiffe_ns_.*"}

# The specific shape seen on main-worker-03, 2026-09-19:
envoy_sds_spiffe_ns_aether_test_sa_.*_init_fetch_timeout_total == 1
# with update_attempt == 1 and NO update_success.
```

The agent says the same thing directly — grep its log for:

```
inbound chain bound to a foreign identity   bound_spiffe_id=spiffe:///ns/aether-test/sa/svc-1
                                            pod_spiffe_id=spiffe://aether.internal/ns/aether-test/sa/svc-1
                                            secret_served=false
```

**Cause.** The node agent's trust domain is late-bound (#740). #819 had the
listener-regeneration paths sample the cache's copy *before* blocking on
`listenerMu`, while `LoadListenersFromStorage` published the listener map
*before* recording the trust domain. A reconciler that started inside that
window (~700 ms on main-worker-03) read `""`, waited out the load on the lock,
and then rewrote every per-pod listener with the malformed name.

**It cannot recur by construction**, and every layer is asserted by a test:
`proxy.SpiffeIDFromPod` returns `""` rather than `spiffe:///`; builders that
need a real identity refuse with `proxy.ErrNoTrustDomain`; the cache's trust
domain is a single atomic value read at the point of use, inside the same
`listenerMu` section that writes the listeners; and it is recorded before any
listener is published.

**If you see it anyway:** the pods are not repairable in place — the malformed
config is already in Envoy. Roll the node's agent (which republishes every
listener from the now-known trust domain); if that does not clear it, roll the
workloads. Then reopen #815 with the agent's first 2 s of log, which contains
the ordering.

#### Reading a pod that is stuck un-promoted

Both probe clusters keep their per-pod stats (they must — the `health_check`
filter answers by READING their membership gauges; never exclude those, see the
2026-06-11 outage note in
`charts/aether/templates/agent-proxy-configmap.yaml`). The cluster name is
lifted into the `aether.cluster` tag by the proxy's `stats_tags`, so:

```promql
# Which local pods' inbound listeners are not serving their own certificate?
# 5s scrape at a coarser step reads as gaps — always max_over_time.
max_over_time(envoy_cluster_membership_healthy{aether_cluster=~"inboundready_.*"}[5m]) == 0

# The app probe, for comparison. App down => health_ is 0 too; inbound-only
# failure => health_ is 1 and inboundready_ is 0.
max_over_time(envoy_cluster_membership_healthy{aether_cluster=~"health_.*"}[5m])
```

If `inboundready_<pod>` alone is 0, the handshake is the problem, and the probe
cluster's own SSL counters say which half (these are deliberately NOT excluded
from the stats matcher, precisely for this):

```promql
increase(envoy_cluster_ssl_fail_verify_san{aether_cluster=~"inboundready_.*"}[5m])   # wrong pod answered on :18008
increase(envoy_cluster_ssl_connection_error{aether_cluster=~"inboundready_.*"}[5m])  # handshake failed / no certificate yet
```

Ranked causes, most to least common:

1. **The pod's SVID has not arrived.** Normal for the first 6–8 s after CNI ADD;
   a *never-promoted* pod simply waits, and an *already-serving* one keeps
   serving. Persistent means SPIRE — check the agent for `SubscribeToX509SVID`
   retries and see "The agent is stuck waiting for SPIRE".
2. **The listeners were built with an empty trust domain.** See the named
   failure above. `envoy_sds_spiffe_ns_*` distinguishes it from (1) in one query:
   in (1) the secret name is correct and merely unserved; here it is malformed.
3. **The node SVID has not arrived.** Then the probe cluster is not emitted at
   all, `inbound_gate_pods{aether_gate_state="ungated"}` is nonzero, the agent
   logs the "NOT active" line naming this precondition, and the node's outbound
   mTLS is degraded anyway. Same SPIRE check.
4. **`fail_verify_san` on the probe.** Something other than the expected pod is
   answering `:18008` in that netns — the #638 stale-endpoint / recycled-pod-IP
   shape. See "Attributing an `ssl_fail_verify_san` event".
5. **A stale netns entry.** The pod is gone but its entry survives until the
   ghost sweep; the probe fails cleanly (a `NETWORK` health-check failure, not a
   crash, on the pinned snapshot). See the stale-netns section.

#### The gate is silently absent

`inbound_gate_pods{aether_gate_state="ungated"}` standing above zero with SPIRE
enabled means those pods have **no** `/healthz/inboundready_<pod>` path and are
being judged on the application probe alone. That is a safe degradation, not an
outage — but it means the premature-promotion hole is open again, so it is worth
an alert. main-worker-05 ran a whole agent lifetime like that on 2026-09-19 with
no signal whatsoever; the agent now says which precondition is missing at
startup and on every change, and the probes are reconciled on every snapshot
rather than off one-shot triggers, so a missed event can no longer strand them.

#### After a proxy roll

A new Envoy epoch starts every host failed and must re-fetch each pod's SDS
secret before the inbound probe can pass. Every pod on the node is then in the
can't-tell row (application up, TLS unproven, already serving), so **no endpoint
is demoted** — that is the rule, not a timing budget.

Two guards remain and still earn their place:

- **`livenessDemoteStreak` (3 observations, 15 s).** The proxy supervisor's
  two-epoch overlap keeps the gateway socket answering across a hot restart, so
  the agent may never observe an unreachable tick and never re-arm. The new
  epoch's *application* probe also starts FAILED for up to two ticks. The streak
  covers that, and absorbs a single transient failure of a TLS probe that has
  already passed this epoch.
- **The gateway re-arm.** A proxy *container* restart does take the socket away;
  the first tick that reaches the gateway again re-arms the warm-up grace and
  clears this epoch's `tlsPassed` marks for every pod.

A node-wide demotion wave aligned with a proxy roll is therefore a bug, not a
tuning problem — capture `aether_agent_liveness_health_transitions_total` and
the agent log and reopen #815.

### Pod held in Init by aether-identity-ready (#1053)

The section above is the **inbound** half of "no traffic before identity": an
endpoint is not advertised until an mTLS handshake with the pod's own inbound
listener proves its SVID is loaded. Since #1053 the **egress** half is
symmetric: the controller's pod-mutating webhook injects the
`aether-identity-ready` init container, first in line, into every mesh pod it
admits (`controller.webhook.identityGate.enabled`, default on; never with
`spire.enabled=false`). It asks the SPIRE Workload API — a `csi.spiffe.io`
volume mounted into the init container only — for the pod's X.509 SVID and
exits 0 once SPIRE has issued it. Until then the app containers do not start,
so the app cannot send before the node proxy has a client certificate for it.

Without it, a pod that sends in its first seconds gets `503 UF`
`upstream_reset_before_response_started{connection_timeout}` (and `UO`/`URX`
once the pending queue overflows): the Broker API subscription is open, but
SPIRE delivers the initial SVID only after its registration entry is created
and synced to the node's spire-agent — `processed SVID update svids=0` twice,
then `svids=1 update=initial` 7.46 s after the subscribe on talos-main
(2026-09-28, #1053). Why not block the CNI ADD instead: that makes SPIRE a hard
dependency of sandbox creation, and the ADD races SPIRE's own pod-list
attestation. Gating the app container keeps the sandbox SPIRE-independent.

Symptom of a gate that does not release: `kubectl get pod` shows
`Init:0/N` (N counts the gate) and the pod never starts.

```bash
# What it is waiting for — one WARN line every 10s, with the socket and the last error
kubectl -n <ns> logs <pod> -c aether-identity-ready
#   level=WARN msg="still waiting for SPIRE to issue this pod's SVID; ..." socket=/run/secrets/workload-spiffe-uds/socket elapsed=40s attempts=78 last_error="rpc error: code = PermissionDenied desc = no identity issued"

# On release (normal: a few seconds after pod creation)
#   level=INFO msg="identity ready: SPIRE issued this pod's SVID; releasing the pod's containers" spiffe_id=spiffe://aether.internal/ns/<ns>/sa/<sa> elapsed=6.9s
```

Read `last_error`:

| `last_error` | Meaning | Fix |
|---|---|---|
| `PermissionDenied … no identity issued` | spire-agent is up and attested the pod, but **no registration entry matches it** | Check the `ClusterSPIFFEID` `podSelector`/`namespaceSelector` covers the pod (`kubectl get clusterspiffeid -o yaml`); an entry keyed on `k8s:container-name`/`k8s:container-image` never matches the init container. Nothing matching also means the node agent's Broker API subscription gets nothing: the pod could not have spoken mTLS anyway |
| `Unavailable … connect: no such file or directory` / `connection refused` | No Workload API on the node | spire-agent not running on this node, or the SPIFFE CSI driver is not (`kubectl get pods -A -o wide | grep -E "spire-agent|spiffe-csi"`); a `FailedMount … csi.spiffe.io` event on the pod means the driver is missing entirely |
| `DeadlineExceeded` | The spire-agent accepted the call but did not answer within 15 s | spire-agent overloaded or wedged; check its log |

It fails **closed** on purpose (no default timeout): a pod whose identity never
comes cannot talk to the mesh, and `Init` with a reason is a better failure than
Running with 503s. Escape hatches, narrowest first: annotate the pod
`aether.io/identity-gate: "false"` (the webhook skips it; its first requests may
fail until the SVID lands), set `controller.webhook.identityGate.timeout` (the
init container exits 1 after that long and the kubelet retries it with backoff —
or fails the pod if its `restartPolicy` is `Never`), or disable the gate
chart-wide. The webhook is `failurePolicy: Ignore`, so pods admitted while no
controller replica answers get no gate (and no mesh label) at all.

The Workload API answers as soon as the node's spire-agent holds the pod's
entry; the node agent's Broker API stream receives the same entry from the same
spire-agent cache. The proxy fetches the client certificate on demand at connect
time (#842/#843), so a connect in the sub-second gap before the node agent
publishes the SVID waits for it (within `connect_timeout`) rather than failing: the gate has no settle
delay.

### #815 release two: every pod event used to re-warm every cluster on the node

> ⚠ **SUPERSEDED BY #842 — see "per-connection certificate selection" below.**
> The `transport_socket_matcher` this section is about no longer exists, and
> with it the `envoy_cluster_match_count_total` queries here measure nothing.
> The *history* is still the right context for the re-warm behaviour and for the
> failure vocabulary, so it is kept; the **queries and the stat names are
> stale**. Do not build an alert from this section.

**What changed.** Every mesh service cluster carries a per-source upstream-mTLS
`transport_socket_matcher`. Its `exact_match_map` used to be keyed by the source
pod's **netns path**, which is unique per pod — so one CNI ADD or DEL rewrote a
field of **every** mesh cluster on that node. Under delta-xDS each rewritten EDS
cluster then re-warmed for the full 15 s EDS `initial_fetch_timeout` (delta
sends no EDS request for a name the old cluster already watches, so no CLA
arrives), and the warming→active swap destroyed the old `ClusterEntry` on every
worker, which `drainConnPools()`-es **every upstream pool on the node**. The
next request to every endpoint then paid a fresh TCP + mTLS handshake. Measured
on talos-main (rev220 and again on rev224): one new pod → 20–24 clusters warming
for 15 s; two new pods on one node → 30 clusters for 30 s; nodes with no new pod
→ 0. That handshake storm is the latency excursion after a service roll, and it
cost an 8 h soak its largest error episode.

The map is now keyed by the **source SPIFFE ID** (`aether.source.spiffe_id`
filter state; since #842 the matcher is gone and the certificate mapper reads
the same ID from `envoy.tls.certificate_mappers.on_demand_secret`, and #1165
stopped stamping `aether.source.spiffe_id`), which is per **ServiceAccount**. The thing the matcher selects
was always per-ServiceAccount — the `transport_socket_matches` entry name *is*
the SPIFFE ID — so nothing about certificate selection changed; the key simply
stopped carrying per-pod state.

#### What still re-warms, and what is now free

| event | before | after |
|---|---|---|
| pod ADD/DEL, ServiceAccount already on the node | every cluster, 15 s each | **nothing** |
| rolling update, replacement lands on the same node | every cluster, twice | **nothing** |
| FIRST pod of a ServiceAccount arriving on a node | every cluster | every cluster, once |
| LAST pod of a ServiceAccount leaving a node | every cluster | every cluster, once |
| node SVID rotation (same SPIFFE ID) | nothing | nothing |
| registry change to the cluster itself | that cluster | that cluster |

A scale-to-zero-and-back (the soak's SHRINK step) still costs one re-warm per
direction per node that held a pod — not one per pod. **Keeping a departed
ServiceAccount's entry alive for a grace period was considered and rejected:**
the retained entry would name an SDS secret the agent stops serving when the
pod's SVID subscription ends, and while Envoy's *delta* SDS keeps the last known
secret on a removal (`sds_api.cc` ACKs and ignores it), a **new Envoy epoch**
after a hot restart would not — every service cluster on the node would then
warm for its SDS `initial_fetch_timeout` on every proxy roll and come up with
`upstream_context_secrets_not_ready`, which is the same defect on a worse path.
A retained entry is also unreachable by construction (no listener stamps a
departed pod's identity), so it would buy byte-stability and nothing else.

#### Verifying the fix

The re-warm is invisible at a 60 s step — the proxy scrape is 5 s and the whole
event lasts 15 s. Always `max_over_time` at a 5 s step:

```promql
# THE measurement. Add a pod to a ServiceAccount that already has one on the
# node: this must stay 0 on every node. Before the fix it read 20-33 for 15 s.
max_over_time(envoy_cluster_manager_warming_clusters[1m])

# The same event from the other side: cluster rebuilds per node.
increase(envoy_cluster_manager_cluster_modified[20m])
```

`cluster_modified` running at tens per node per 20 minutes against a handful of
pod events is the pre-fix signature.

#### The failure signature: a connection took the `on_no_match` path

`on_no_match` presents the **node** identity. That is deliberate (some upstream
connections legitimately have no source pod), so it is not an error — but a
*workload's* traffic arriving as the node is the thing that says the
filter-state key is not reaching the matcher.

Envoy emits one counter per transport-socket match, named by the match:

```
cluster.<cluster>.<match_name>.total_match_count
cluster.<cluster>.default.total_match_count
```

Every match on a mesh service cluster is named by a SPIFFE ID, so the chart
lifts that into an attribute (`stats_tags` → `aether.transport_socket_match`,
added with this release). Without it the SPIFFE ID lands in the exported metric
**name**, one family per ServiceAccount, permanently in Prometheus' name index.
Because the node identity is only reachable through `on_no_match`, **the node
identity's counter on a service cluster is the no-match counter**:

```promql
# Connections that selected the NODE identity on a mesh service cluster.
#
# TWO traps, both hit during the rev225 validation (2026-09-19):
#  - the exported family is envoy_cluster_match_count_total — Prometheus' OTLP
#    ingest moves "total" to the end; envoy_cluster_total_match_count does not exist;
#  - on a node agent the "node identity" is the AGENT'S OWN SVID,
#    spiffe://<td>/ns/<agent-namespace>/sa/<agent-serviceaccount>
#    (spiffe://aether.internal/ns/aether-system/sa/aether-agent on talos-main) —
#    NOT spiffe://<td>/node/<node>. A `/node/` selector matches nothing and reads
#    as a clean zero. Take the value from the agent log line `served node SVID`.
#
# It is 0 over any quiet window and ticks only when a cluster's endpoints change
# (note 2): during a pod ADD the increments sit entirely on THAT service's cluster.
# Growing with traffic across clusters = workloads are presenting the agent's identity.
sum by (node, aether_cluster) (increase(envoy_cluster_match_count_total{
  aether_transport_socket_match="spiffe://aether.internal/ns/aether-system/sa/aether-agent"}[5m]))

# The healthy case, for contrast: per-ServiceAccount selection actually happening
# (every identity EXCEPT the agent's).
sum by (node, aether_transport_socket_match) (increase(envoy_cluster_match_count_total{
  aether_transport_socket_match!="spiffe://aether.internal/ns/aether-system/sa/aether-agent"}[5m]))
```

Three things about that counter, all confirmed against the pinned proxy's
sources — get them wrong and the query lies:

1. **`default.total_match_count` stays at zero and proves nothing.** Envoy's
   matcher treats `on_no_match` as a *match*, so an `on_no_match` that names a
   socket increments that socket's named counter, never `default`. `default` is
   reached only when there is no `on_no_match` at all, or when the action names
   a socket absent from `transport_socket_matches` (which also logs
   `Transport socket '<name>' not found, using default` at warn — there is no
   stat for that misconfiguration).
2. **There is a small constant offset.** The counter increments once per upstream
   connection creation *and* once per `HostImpl` construction, and the
   construction call passes no filter state, so it lands on `on_no_match`.
   Expect the node-identity counter to tick up by roughly the endpoint count on
   every EDS change even when everything is correct. Judge it against the
   connection rate, not against zero.
3. **It only counts connections at all because the matcher uses the filter-state
   input.** Envoy re-resolves per connection only when
   `usesFilterState() && !downstreamSharedFilterStateObjects().empty()`. A
   cluster without the matcher (SPIRE off, or before the node SVID) resolves
   once per host and the counter is meaningless there.

The authoritative cross-check is on the **receiving** side — which verified
identity did the destination actually see — and since #824 the access log carries
it. Every other identity field in a line is something the control plane baked
into the emitting pod's own config, so a line can say who it *thinks* it is; these
two are the certificate the other end presented and this proxy validated:

| field | on | is |
|---|---|---|
| `downstream_peer_uri_san` | `reporter="destination"` (the per-pod inbound listener) | the CALLER's verified SPIFFE ID |
| `upstream_peer_uri_san` | `reporter="source"` (outbound/capture) | the SERVER certificate the destination proxy presented |

```logsql
# Mesh traffic arriving as the node agent instead of a workload — the signature of
# the cluster matcher falling to on_no_match (#815). Expect ZERO rows.
log_name:"aether_access_logs" AND reporter:"destination"
  AND downstream_peer_uri_san:"spiffe://aether.internal/ns/aether-system/sa/aether-agent"

# The #638 cross-wiring from the client side: which server identity did we accept?
log_name:"aether_access_logs" AND reporter:"source" AND upstream_peer_uri_san:*
```

Both render `-` where the hop is not mTLS (SPIRE disabled, cleartext inbound, the
loopback hop to the local application), so a `-` is not a finding on its own —
check `reporter` and whether that hop is meant to be mTLS. For routes behind
ext_authz the OPA decision log's
`metadataContext.filterMetadata["aether.source"].spiffeId` remains available, but
it is the source's *claim* about itself and a weaker control than the fields
above. The failure signature is a workload's traffic arriving with the **agent's**
identity. The agent's own #638 discriminator
(`agent/internal/xds/cache/identitybinding.go`) still names the
netns→identity index behind it and WARNs on `outbound cluster bound to a foreign
identity`.

> **Why a wrong cert cannot leak across sources through pooling — now
> demonstrated, not derived (issue #831).** Envoy folds a downstream
> filter-state object into the upstream connection-pool hash key only if the
> object implements `Hashable`; `set_filter_state`'s `envoy.string` factory
> builds a `Router::StringAccessorImpl`, which does not, and a pool freezes the
> transport-socket options of whichever connection allocated it. The source
> SPIFFE ID therefore contributes **zero bytes** to the pool key. Node service
> clusters set `connection_pool_per_downstream_connection: true`
> (`proxy.NewServiceCluster`, `NewTCPServiceCluster`), which mixes the
> downstream connection id into the hash instead — one pool per downstream
> connection, so there is nothing to share.
>
> `//agent/test/mtlspool` runs the real pinned proxy with two source ServiceAccounts
> on one node and measures the identity the destination verifies. With the flag
> on, each source is verified as itself on its own upstream connection. With it
> off — the only change — **every** request from the second source is verified
> as the first, multiplexed onto one HTTP/2 connection.
>
> So this is **security configuration, not a throughput knob**. Do not turn it
> off while the matcher reads a non-hashable filter-state key: the result is a
> silent workload-to-workload authorization failure (both identities are valid
> mesh workloads, and the destination's validation context pins no client SAN,
> so it cannot object). The edge proxy sets it false only because it has exactly
> one identity. `TestNodeProxyPerSourceClustersPoolPerDownstreamConnection`
> (`agent/internal/xds/cache/`) fails the build if any per-source cluster loses
> the flag.
>
> Since #831 the source access log also carries `source_spiffe_id` — the
> identity the control plane stamped for that hop. Joined to the destination
> line's `downstream_peer_uri_san` on `x_request_id` the two must be equal; a
> disagreement is either a matcher miss (the node/agent SVID appears at the
> destination) or a pooling leak. Both were previously invisible.
>
> ```logsql
> # The claimed source identity, per hop. "-" means the chain stamped none at
> # all, i.e. the trust domain was still unknown when it was generated (#819).
> log_name:"aether_access_logs" AND reporter:"source" AND source_spiffe_id:*
> ```

#### Long-lived connections across the upgrade

A connection accepted **before** its listener started stamping the key carries
only the old key for its whole life, and its new upstream connections would take
`on_no_match`. Release one changed the `filters` list on every mesh-originating
filter chain, and Envoy's filter-chain reuse index hashes the whole `FilterChain`
proto — so those chains were replaced and their connections drained. The drain
is the server-wide `--drain-time-s`, which the supervisor sets from
`proxy.hotRestart.drainTime` (**10 s**, not Envoy's 600 s default), after which
the remaining connections are force-closed with `filter_chain_is_being_removed`.
A proxy roll settles it outright. So on a cluster that has been through release
one, no pre-release-one connection can still exist.

#### SPIRE off

No matcher is injected at all: the node SVID is only ever set by the SPIRE
bridge, and without it every service cluster is emitted with a plain transport
socket. Release two is a literal no-op there
(`TestServiceClusterBytesUnaffectedByPodChurnWithSpireOff`).

The edge proxy and the east/west waypoint tunnel are unaffected — the edge has a
single identity and takes the non-matcher branch, and the waypoint's two-level
matcher only changed which key its inner sub-trees read.

### #842: per-connection certificate selection (and the alert it breaks)

**Read the #815 section above first** — this replaces the mechanism it
describes, and the two share a failure vocabulary.

**What changed.** A mesh service cluster no longer carries
`transport_socket_matches` or a `transport_socket_matcher`. It carries **one**
transport socket whose `custom_tls_certificate_selector` resolves the client
certificate per connection:

```
envoy.tls.certificate_selectors.on_demand_secret
  config_source:      its OWN gRPC SDS stream to agent_xds  <-- NOT `ads: {}`
  certificate_mapper: envoy.tls.upstream_certificate_mappers.filter_state_override
                        default_value: <the node SVID>
  prefetch_secret_names: [<the node SVID>]
```

> ⚠ **THE SELECTOR MUST NOT FETCH OVER `ads: {}`. THIS IS WHAT TOOK THE MESH
> DOWN.** It shipped that way as rev228 on 2026-09-20 and every mesh upstream
> mTLS connection on the fleet stopped completing within seconds of the new
> proxies taking the config. Full account below under **"The rev228 outage"**.
> If you are reading this because you are about to "tidy up" the one SDS
> reference on this proxy that is not `ads: {}` — don't. Read that section.

The mapper reads the filter-state object named — exactly, and not
configurably — `envoy.tls.certificate_mappers.on_demand_secret` off
`TransportSocketOptions::downstreamSharedFilterStateObjects()` and returns its
string **as the SDS secret name**. Every mesh-originating listener chain stamps
the source pod's SPIFFE ID there, and aether's SDS secret names *are* SPIFFE
IDs, so the two join with no lookup table in between.

Two things follow, and they are the point of the change:

- **A mesh cluster's bytes no longer depend on the node's workloads at all.**
  #815 release two made them invariant under pod churn *within* a
  ServiceAccount; the first pod of a new ServiceAccount arriving, and the last
  one leaving, still rewrote every mesh cluster on the node and cost one 15 s
  EDS re-warm each. Those are free now. **No pod event changes a service
  cluster.**
- **`connection_pool_per_downstream_connection` is gone from every mesh
  cluster.** The object is stamped with the `envoy.hashable_string` factory, so
  the source identity reaches
  `CommonUpstreamTransportSocketFactory::hashKey` and upstream pools partition
  per (host, **source identity**) instead of per downstream connection. Pods
  sharing a ServiceAccount now share an upstream HTTP/2 connection; pods that do
  not, cannot.

> ⚠ **THE FACTORY IS SECURITY CONFIGURATION.** `envoy.string` builds a
> `Router::StringAccessorImpl`, which is not `Envoy::Hashable`; `hashKey` folds
> a shared filter-state object into the pool key only behind a `dynamic_cast`
> to `Hashable`. Reverting the factory while the flag stays off re-opens the
> #831 leak — a pooled connection carries another workload's client
> certificate, the handshake succeeds, and the destination stamps the wrong
> identity into XFCC. It fails **open**. `//agent/test/mtlspool` reproduces it as a
> negative control and would go green-for-the-wrong-reason if the pair were
> broken; read its `TestSharedPoolLeaksSourceIdentity` before touching either.

> ⚠ **`max_session_keys` must stay 0.** A client context supports a custom
> certificate selector only with session resumption off (`tls.proto`): a cached
> session is keyed without reference to which on-demand certificate produced it,
> so a resumed one would carry a certificate the selector did not choose. #834
> already set it to 0 for #829; it is now load-bearing for two reasons.
> Note the upstream factory does **not** enforce this the way the downstream one
> does — Envoy will accept a non-zero value here without complaint.

#### The rev228 outage: why the selector gets its own SDS stream

**Symptom.** Every mesh **upstream** mTLS connection stops completing, at the
instant the proxies take the config. Inbound is fine, the local data path is
fine, same-node upstreams fail too. Requests **pause**; they do not fail:

| what you would normally check | what it said |
|---|---|
| `envoy_cluster_ssl_connection_error_total` | no new series |
| `envoy_cluster_ssl_fail_verify_san_total` | 0 series |
| access log `upstream_transport_failure_reason != "-"` | **0** records |
| access log, source reporter | `upstream_host` SELECTED, `upstream_peer_uri_san="-"`, `response_flags=DC`, `duration_ms≈1980` |

**A green "no handshake errors" dashboard is not evidence of health here.** The
only signals that move are end-to-end ones, and the two that name the mechanism:

- `envoy_cluster_on_demand_secret_cert_requested_total` climbing while
  **`envoy_cluster_on_demand_secret_cert_updated_total` stays absent**. Absent
  means zero: Envoy does not export a counter that never incremented.
  `cert_updated` is bumped only by the completion callback of the selector's
  `setContext`, which is reachable only from a secret actually arriving. Zero
  therefore proves **no SDS payload ever reached the selector**.
- `envoy_sds_spiffe_<mangled name>_init_fetch_timeout_total` climbing, for the
  node SVID and for workload SVIDs. That is the 15 s `initial_fetch_timeout`
  on a subscription that never got an answer.

**`cert_requested == cert_active` is NOT a health signal.** `cert_active`
counts live secret *subscriptions*, not applied certificates, and it is
incremented on the same line as `cert_requested`. Equality just means nothing
has been removed. On the fleet it read equal on four of five nodes while the
mesh was entirely down. `cert_updated` is the only success signal the selector
has.

**Mechanism.** Envoy keys a secret provider on
`hash(ConfigSource) + "." + name + warm` (`secret_manager_impl.h`). The
on-demand selector always asks with `warm=false`; every statically named SDS
reference asks with `warm=true`. So **one secret name gets two independent
`SdsApi` objects with two independent watches** — and on a node proxy that is
the normal case, not an edge case:

- every local pod's SVID is already named statically by that pod's own inbound
  listener (`DownstreamTransportSocket`), and
- the node SVID — the selector's `default_value` *and* its
  `prefetch_secret_names` — is already named statically by every
  `inboundready_<pod>` probe cluster.

The selector is therefore always the **second** subscriber. On the delta-ADS
mux, Envoy's `WatchMap` deduplicates subscription interest per (type_url,
resource name) across watches, so the second watch contributes nothing to
`resource_names_subscribe`. No request goes out. A delta control plane —
correctly — sends only what changed, which is nothing. The second watch never
receives the resource.

And nothing recovers it: `SdsApi::onConfigUpdateFailed` only calls
`init_target_.ready()`. It does **not** notify the parked certificate-selection
callback. So `doSelectTlsContext`'s `Pending` is never resolved and the
handshake stays suspended until the cluster's `connect_timeout` — or, sooner,
until the downstream client gives up (`DC`).

**Fix.** The selector gets its own `api_config_source` to the `agent_xds`
cluster, which means its own mux and its own watch map, so its subscription can
no longer be deduplicated against a static one. It is state-of-the-world rather
than delta on purpose: a SotW response carries every requested resource and
`GrpcMuxImpl::addWatch` queues a request unconditionally, so the failure is
structurally impossible on that stream even if a static reference ever lands on
it. The agent already serves `SecretDiscoveryService` on the same socket as ADS
(`common/xds/xds.go`), so there is no new bootstrap cluster.

**What it costs.** Envoy builds a mux per non-ADS subscription, so this is one
gRPC stream **per secret name the selector resolves** — per identity actually
originating traffic on the node, appearing lazily on first use — each carrying
exactly one resource. `max_concurrent_streams: 10` on the chart's `agent_xds`
cluster does **not** cap them: on an upstream cluster that field is what Envoy
advertises for *peer*-initiated streams. The governing limit is the agent's
`grpc.MaxConcurrentStreams(1000)`.

> ⚠ **Perturbing the ConfigSource hash is NOT a fix.** Adding an
> `initial_fetch_timeout` to `ads: {}` gives you a different provider key while
> leaving the mux — and therefore the watch map that does the deduplication —
> shared. The *stream* has to be separate.

> ⚠ **Never point a statically named secret at the selector's stream.** That
> puts a `warm=true` and a `warm=false` provider for one name back on one mux
> and resumes the starvation, silently.
> `TestMeshCertSelectorSDSSourceIsNotSharedWithAnyStaticSecret`
> (`agent/internal/xds/proxy`) fails the build if any builder does.

**The gate.** `//agent/test/mtlspool`'s
`TestOnDemandCertificateResolvesWhenAlreadyStaticallyReferenced` runs the pinned
proxy against a real delta-ADS control plane with the source identity referenced
*both* ways, and asserts the request **completes within a bound**. It was red
against the shipped configuration for exactly this reason. The earlier harness
could not have caught it: it rewrote every SDS config source to a bespoke
`api_config_source` because it had no control plane, which accidentally *was*
the fix.

**Diagnosing it again, in order:** `cert_updated` flat while `cert_requested`
climbs → `sds.<name>.init_fetch_timeout` → `/config_dump?resource=dynamic_warming_secrets`
(the starved names sit there with `version_info: "uninitialized"`).

⚠ **`cert_requested` and `cert_active` cannot be compared across a hot restart.**
Two live Envoy epochs both export; when the parent's series goes away the summed
GAUGE (`cert_active`) drops to the child's alone while the COUNTER
(`cert_requested`) keeps the merged total, because StatMerger transfers gauges
absolute and counters as deltas. On rev228 that produced a w05 reading of
`requested=55, active=28` that looked like 27 secret removals and was not.
Check `envoy_server_live` and `envoy_server_hot_restart_epoch` before reading
anything into the pair.

**Rotation.** An SVID rotation re-resolves a secret the selector already holds,
and it must arrive on the selector's own stream. New upstream connections then
present the new certificate; connections already established keep the one they
handshook with, which is correct. SPIRE rotates on a ~4 h TTL, so a 60–75 minute
deploy validation crosses no rotation while an 8 h soak crosses about two — i.e.
a rotation defect would first appear as a soak going quiet several hours in,
with no error anywhere. `//agent/test/mtlspool`'s
`TestRotatedSVIDIsPickedUpOverTheSelectorStream` is the build-time gate for it:
it republishes every SVID under a new snapshot version and requires the
destination to verify a different certificate SERIAL for the same SPIFFE ID
within a bound, with `cert_updated` moving and every request completing
throughout.

#### THE ALERT THIS BREAKS — cross-repo

`AetherClusterIdentityNoMatch` (k8s-talos-main, GitOps #109) queries

```promql
sum by (node, aether_cluster) (increase(envoy_cluster_match_count_total{
  aether_transport_socket_match="spiffe://aether.internal/ns/aether-system/sa/aether-agent"}[5m]))
```

**That series no longer exists.** `cluster.<c>.<match>.total_match_count` is
emitted per `transport_socket_matches` entry, and mesh clusters now have none.
The rule does not error — it matches nothing and evaluates to a clean zero, so
it looks permanently healthy. **This is a vacuous gate and must be removed or
repointed in the same window this ships**, or the fleet loses its "workload
traffic is presenting the agent identity" signal without anyone noticing.

**The replacement, in order of strength:**

1. **The destination-side access log — already the authoritative check.** It
   measures what the other end actually verified, not what this proxy intended:

   ```logsql
   # Mesh traffic arriving as the node agent instead of a workload. Expect ZERO.
   log_name:"aether_access_logs" AND reporter:"destination"
     AND downstream_peer_uri_san:"spiffe://aether.internal/ns/aether-system/sa/aether-agent"
   ```

   The runbook already called this "the authoritative cross-check" while the
   counter existed; it is now the primary.

2. **The source-side access log's `source_spiffe_id`.** Absent (`-`) is
   *precisely* the condition that makes the mapper fall back to `default_value`,
   so it is the direct successor to the no-match counter — and it is per
   request, not per connection. Since #1165 the attribute reads the mapper's
   own key rather than the `aether.source.spiffe_id` copy stamped beside it,
   so "absent here" and "the mapper missed" are the same lookup, not two that
   happened to be stamped together:

   ```logsql
   log_name:"aether_access_logs" AND reporter:"source" AND source_spiffe_id:"-"
   ```

3. **`cluster.<c>.on_demand_secret.cert_requested` / `.cert_updated` /
   `.cert_active`** (the selector's own stats, from
   `ALL_CERT_SELECTION_STATS`). These say the on-demand path is *working* — a
   `cert_active` gauge of 1 on a node with several ServiceAccounts sending
   traffic means only the default is ever being fetched. They do **not**
   identify which identity, so they are a liveness signal for the mechanism, not
   a replacement for (1) or (2).

The agent's own discriminator
(`agent/internal/xds/cache/identitybinding.go`) is unchanged and still WARNs
`outbound cluster bound to a foreign identity` on the control-plane side.

#### First-connection handshake pause

On-demand SDS **pauses the handshake** on the first connection per (secret name,
worker thread, cluster) while the SDS response lands. The fetch is from the
node-local agent over its ADS UDS with the secret already in the snapshot, so it
is a local round trip.

The cluster itself does **not** warm on any of it, and that is a real change in
the other direction. A statically referenced SDS certificate is fetched through
a *warming* provider, whose init target fires only once the secret arrives — so
a client certificate the agent failed to serve used to hold its cluster in
warming for `initial_fetch_timeout` and then bring it up with
`upstream_context_secrets_not_ready`. The on-demand path creates its providers
with `warm=false` (`SecretManagerImpl::DynamicSecretProviders::findOrCreate`:
"the warming target only fires after a secret is fetched, while non-warming one
pre-fetches"), so **no mesh cluster blocks on a client-certificate secret any
more** — including the prefetched node SVID. The cost moves from cluster
warming, which is node-wide, to one paused handshake, which is not.

It is bounded and amortised: `prefetch_secret_names` carries the node SVID, so
the no-filter-state path (health checks, anything the proxy originates itself)
never pauses at all; a workload identity pays it once per cluster per worker,
and the per-identity pool then keeps that connection alive. Set against what it
replaces — `connection_pool_per_downstream_connection` forced a **full mTLS
handshake on every downstream connection**, measured at ~25 ms of a 29 ms p50
hop (#735) — this is strictly cheaper.

**Workload identities are deliberately NOT prefetched.** Listing them would put
the node's identity set back into every cluster's bytes, which is exactly the
churn #815 and this change removed.

> If a secret name is never served, the handshake stays paused rather than
> failing fast; the request then ends on its route timeout. That cannot happen
> for an identity the agent stamped (it stamps only identities it serves), but
> it is the shape to look for if `on_demand_secret.cert_requested` climbs
> without `cert_updated` following.

#### What is unchanged

- **The server-identity SAN pin** (`match_typed_subject_alt_names`) and the
  SDS-rotated trust bundle. They were never per-source.
- **The `inboundready_<pod>` probe cluster.** It has no selector: it keeps its
  own statically named certificate (the node SVID) and its own
  `MaxSessionKeys: 0` (#836/#840). A health checker carries no filter state, so
  had it used the selector it would have taken `default_value` on every probe —
  the same node SVID, by a longer route. Leaving it alone also preserves the
  #836 boundary: nothing the probe does may be observable by application
  traffic.
- **The edge proxy.** One identity, no source filter state, and its SDS comes
  straight from the SPIRE agent rather than the agent's ADS stream. A selector
  would add a handshake pause and buy nothing, so `EdgeUpstreamTransportSocket`
  keeps its statically named certificate.
- **The waypoint split** (proposal 019, off by default). SNI is a property of
  the transport socket, not of the certificate, so a waypoint-enabled cluster
  still carries two sockets — but they are now a fixed two (`local`, `waypoint`)
  selected by endpoint metadata, independent of the node's identity set, and
  their match names are bounded constants rather than SPIFFE IDs.
- **SPIRE off.** No node SVID means no `default_value`, so no selector and no
  transport socket at all — the cluster is emitted bare exactly as before.

#### Rollback

For one release the listeners stamped **both** identity keys:
`aether.source.spiffe_id` (what a pre-#842 cluster's matcher reads) and the
certificate-mapper key, so a rollback to release-two clusters stayed hitless —
the same dual-key overlap #815 used. **#1165 retired `aether.source.spiffe_id`**:
the access log's `source_spiffe_id` attribute now reads the mapper key (the
attribute NAME is unchanged), and a rollback below chart `0.93.0` is no longer
hitless — see *"Rollback floor: chart `0.93.0` (issue #1165)"* in §7. The netns
copy was kept (it still feeds `source_netns`).

Rolling **forward** to #842 replaced every mesh-originating filter chain (the
`filters` list gained an entry), so those chains drained once on
`--drain-time-s` (10 s here) — the same one-time cost release one paid; #1165
(one entry fewer) costs the same once more.
