# AGENTS.md

This file provides concise, high-signal guidance for coding agents working in the Aether repository. Start with **Rules for agents** below; role-specific agents live in `.claude/agents/`.

## Quick Start

**Build & Test:**
```bash
make test          # All tests (requires Docker for integration)
make test-unit     # Unit tests only (no Docker)
make test-race     # Whole tree under the Go race detector
make build-agent   # Build agent binary
```

`--config=race` (`.bazelrc`) is the supported spelling; prefer it scoped to what
you touched (`bazel test --config=race //agent/internal/meshdns:all`) — the whole
tree still has known test-only races, so a bare `//...` run reports failures you
did not cause.

**Format & Lint:**
```bash
make format        # Format all code
make lint          # Run linters (buf, shellcheck, gocognit)
make format-check  # CI-friendly check (fails on drift + buildifier lint)
make actionlint    # GitHub Actions workflows (pinned actionlint + ShellCheck)
```

## Toolchain

- **Bazel:** 9.2.0 (via Bazelisk). Use `bazel` commands directly or via `Makefile`.
- **Go:** 1.27.1
- **Container images:** Built with `rules_img`, pushed to distroless (`gcr.io/distroless/static-debian13:nonroot`).
- **Protobuf:** Uses `buf/validate` for validation.
- **Static analysis:** `nogo` is wired into the Go SDK (`go_sdk.nogo(nogo = "//:nogo")`), so vet-class analyzers run as a validation action on every Go target — findings fail `bazel build`, not just `make lint`. Scope lives in `nogo.json`.

## Architecture

**Binaries:**
- `agent/cmd/agent` — Node DaemonSet. Manages xDS server (Envoy), CNI gRPC server, SPIRE bridge via `controller-runtime` Manager. Also hosts the `agent edge` subcommand.
- `agent/cmd/proxy-supervisor` — Envoy hot-restart supervisor (own binary + own image since #772; PID 1 of the `aether-proxy` container). 15 MiB / 24 modules, no k8s, no xDS, no SPIRE — asserted by `:deps_test` and `scripts/check-proxy-supervisor-deps.sh`.
- `agent/cmd/mesh-dns` — Slim standalone mesh-DNS daemon (own DaemonSet + image). Serves pods from the record snapshot the agent writes.
- `registrar/cmd/registrar` — In-cluster Deployment. Proxies external registry (Kubernetes/etcd), maintains endpoint snapshot, streams to agents via gRPC.
- `controller/cmd/controller` — In-cluster Deployment (leader-elected). Serves the validating + pod-mutating admission webhooks and the `MeshConfig`→ConfigMap reconciler.
- `cni/cmd/cni` — CNI plugin binary (Add/Del/Check/GC/Status).
- `cni/cmd/cni-install` — Init container that installs the CNI plugin binary and config onto the host.
- `agent/cmd/proxy-ready` — Exec readiness probe for the `aether-proxy` pod (#673). One flag (`--ready-marker`), stdlib-only; `//agent/cmd/proxy-ready:deps_test` fails the build if it grows a dependency. Ships in the proxy-supervisor image (since #772), staged by the `install-supervisor` initContainer.
- `agent/cmd/identity-ready` — The egress identity gate (#1053): the init container the controller's `/mutate` webhook injects first into every mesh pod (#1055); it waits on the pod's own SPIRE Workload API until the pod's SVID exists. Ships in the agent image; `//agent/cmd/identity-ready:deps_test` guards its link set.
- `agent/cmd/mesh-dns-ready` — Same for the `aether-mesh-dns` pod (#683); bundled in the mesh-dns image, guarded by `//agent/cmd/mesh-dns-ready:deps_test`.
- `prober/cmd/prober` — Synthetic mesh-availability prober (proposal 013). Own chart (`charts/prober`) + own image; mesh-managed per-node DaemonSet that probes the data plane from the client side and emits `aether_probe_requests_total`.

**Notable packages** (beyond the ones named above):
- `common/spire` — the shared identity wait/readiness model (#740): `WaitingSource` (never fatal on a missing SPIRE), `ReadyChecker`, `NotReadyDwell` (2m, node agent only) / `ServiceNotReadyDwell` (0). `spiretest` also serves the fake Workload API and the fake SPIFFE Broker Endpoint the identity tests run against.
- `agent/internal/spire` — the SPIFFE **Broker API** client + the SDS bridge (proposal 036, replaced SPIRE's Delegated Identity API): per-pod `SubscribeToX509SVID` streams keyed by a `KubernetesObjectReference` (ns/name **and** UID) over mTLS on `--spire-broker-socket`; validation contexts from the agent's own Workload API bundle plus the pods' federated bundles. Needs SPIRE >= 1.15.2 with its experimental broker enabled.
- `agent/internal/identity` — late-bound trust domain, folded in when the first SVID arrives.
- `agent/internal/node` + `controller/internal/nodetaint` — proposal 033 taint lifecycle: the agent removes `aether.io/agent-not-ready`, the controller's leader-elected guard re-arms it.
- `agent/internal/cniconflist` — re-asserts aether's chained entry in the node's CNI conflist (#645).
- `registrar/internal/replicator` — leader-elected cross-region etcd mirroring under an origin-heartbeat lease (proposal 006).
- `agent/internal/xds/cache/quicpairs.go`, `agent/internal/xds/quicdemand`, `agent/internal/xds/server` (`odcds.go`) — east-west QUIC twins: observed (SA, destination) pairs and their persisted `demand_confirmed` (#1076), the per-stream ODCDS subscription ledger (#1036/#1052), and on-demand twin admission (#1033).
- `agent/internal/xds/ack` — per-resource delta-xDS ACK/NACK tracking (no admin polling).
- `agent/internal/capture` — projects mesh-Service authorities into the `cap_http` capture route table.
- `agent/internal/proxy/hotrestart` + `agent/internal/supervisorcmd` — the Envoy hot-restart supervisor and its command (#772; child-silent signal #1058).
- `agent/internal/gatewaystatus` — Gateway API status writers scoped to aether's `controllerName`.
- `cni/internal/plugin` — the chained CNI plugin, including the TPROXY capture divert (proposal 038) and the mesh-DNS `:53` DNAT.

**Ports:** 18001 per-pod TCP capture (TPROXY target), 18008 inbound TCP + UDP/QUIC, 18009 east/west waypoint, 18021 edge readiness, 18054 host mesh-DNS, 18081 per-pod outbound HTTP, 18082 L4 TCP + UDP (`common/constants/mesh`, `agent/internal/xds/proxy`).

**Carried Envoy patches:** `proxy/bazel/patches/` (applied by `proxy/MODULE.bazel`; each one listed in `proxy/README.md`).

**Key patterns:**
- gRPC servers use Unix domain sockets for node-local communication.
- Agent uses `controller-runtime` Manager to orchestrate runnables (xDS, CNI, SPIRE, registry).
- Envoy config via snapshot cache (`go-control-plane/pkg/cache/v3`) with versioned snapshots.
- `ServerCallback` interface with `PreListen` allows setup before accepting connections.

## Workflow

**After adding/modifying Go files:**
```bash
make gazelle     # Regenerate BUILD.bazel
make tidy        # bazel mod tidy — sync MODULE.bazel's use_repo with go.mod
make deps-audit  # go.mod/go.sum hygiene; also a required CI job
```

**Dependencies:** add with `bazel run @rules_go//go -- get <module>@<version>`,
remove with `go mod edit -droprequire=<module>` (never `go get <module>@none`:
`@none` downgrades everything that requires the module). Then `make tidy` +
`make gazelle`. An unimported require that only pins a CVE-clean version is
reclassified `// indirect`, never deleted. See `docs/runbook.md` § *Go dependency
hygiene*.

**Integration tests:** Use `testcontainers-go` to run etcd in Docker. Run `./bazel/configure_colima.sh` once on macOS with Colima to configure Docker socket access.

**Test tags:**
- `size = "medium"` and `tags = ["integration"]` for integration tests.
- Use `--test_arg=-test.short` to skip integration tests.

## Proto & Codegen

- Proto files in `api/` under `aether/cni/v1/`, `aether/registry/v1/`, `aether/registrar/v1/`, `aether/config/v1/` (`MeshConfig`, `HTTPFilter`, `EdgeConfig`, `EndpointPolicy`).
- Run `make gazelle` after proto or import changes.

## Constraints

- Never modify production code when asked to add or fix tests only.
- Never remove existing test cases unless explicitly asked.
- Never run `go mod tidy` (or `go mod tidy -e`): the generated proto packages
  `aethermesh.dev/api/aether/*/v1` exist only as Bazel outputs, so it cannot
  resolve them, and `-e` strips modules that only generated code or a BUILD file
  needs. Use `make tidy` + `make deps-audit`.
- Formatting uses `gofumpt`, `buildifier`, `shfmt`, `buf`.
- Lint violations fail with `--config=lint` (aspect-based).
- SPIRE integration is enabled by default on the **agent** and **registrar**
  (`--spire-enabled=true`); use `--spire-enabled=false` to disable. The
  **controller** is the exception: its `--spire-enabled` defaults to `false` (it
  serves the webhook with the Helm self-signed cert unless SPIRE is turned on).

## Rules for agents

One place for the rules every brief used to repeat. They apply to people too;
an agent has no other way to learn them. The role agents in `.claude/agents/`
assume this section has been read.

**Build, lint, test**
- Lint exactly as CI does: `bazel build --config=lint --@aspect_rules_lint//lint:fail_on_violation //...`.
  **Never run `--config=ci` locally**: it implies `--config=remote` and needs
  the BuildBuddy key CI passes on the command line.
- `make format-check` before pushing (it also runs buildifier's linter on every
  BUILD and `.bzl` file). `make actionlint` when a workflow or a composite
  action changed. `scripts/check-shell-lint.sh` when a script was added.
- The race detector goes on the `go_test` targets you touched
  (`bazel test --config=race //pkg:pkg_test`), never on a wildcard that includes
  image targets: that fails in analysis (cgo is off for images).
- Do not build the `proxy/` workspace locally unless the task is about it: it is
  Envoy. Its checks run in the `proxy` workflow.
- A plain `go build ./...` needs the generated proto sources first:
  `make materialize-go`, then `make go-build-plain`, then `make materialize-go-clean`.

**Secrets and releases**
- Never print, `cat` or paste `user.bazelrc`, `proxy/user.bazelrc` or
  `~/.bazelrc`: they hold the BuildBuddy key. Never handle registry credentials.
- Never push an image or a chart by hand. `publish.yaml` and `proxy-release.yml`
  are the only publishers; leave both alone unless the task names them.
- Never dismiss a code-scanning alert, and never work around one by reshaping
  code so the scanner stops seeing it. Report it. One known false positive:
  any edit to `.github/workflows/publish-verify.yaml` can re-raise
  `actions/untrusted-checkout`, and code scanning is a required check, so that
  pull request waits for the owner. Prefer changing the scripts that workflow
  calls over changing its YAML.

**Changes that carry an obligation**
- Anything under `charts/` needs a bump of that chart's `Chart.yaml` (CI
  enforces it). Read the version on `origin/main` first: two open chart pull
  requests collide, and the second to merge takes the next patch number.
- A chart test is `helm_template_test` in the chart's `BUILD.bazel`; write it
  first and see it fail against the unchanged chart.
- Removing a CPU limit from a Go container means deciding `GOMAXPROCS` in the
  same change (without a limit the runtime sizes to the node's cores); see the
  agent, mesh-dns and uds-csi values for the pattern.
- New Go code needs tests: the `coverage` workflow fails a pull request whose
  total line coverage drops by more than 1.0 point against `main`.
- Protos are edition 2023 (never proto3) and change additively; an agent and a
  registrar one release apart must keep working.

**Workflows**
- Every action pinned by full commit SHA with the version in a trailing
  comment; `timeout-minutes` on every job; `permissions` per job, least
  privilege; no `${{ }}` inside a `run:` script except through `env:`.
- Kind, Bazel, conformance and format steps go through the composite actions in
  `.github/actions/`; the pin tests (`//e2e:kind_pin_test`, `//e2e:go_pin_test`)
  fail a workflow that bypasses them.
- The API cannot update a branch that touches `.github/workflows` without the
  `workflow` token scope: rebase over git and `push --force-with-lease`.

**Pull requests**
- One concern per pull request. Never merge your own unless told to; never
  `--admin`; squash only. Required on `main`: `ci`, `proxy`, `codeql`, and the
  code-scanning app's `CodeQL`.
- Pull requests that would each rewrite the same lock or manifest file
  (`MODULE.bazel.lock`, `go.sum`, a `Chart.yaml`, one workflow file) are a
  stack: use the `gh stack` extension, with branches named `upgrade/<slug>`
  (the only pattern whose stacked pull requests get the gates). A stack merges
  with `gh stack merge`, never `gh pr merge`, and needs `gh stack sync` first
  when `main` has moved.
- A check that failed with no step run (no runner, a registry 502) is
  infrastructure: say so and re-run it; do not "fix" the pull request.
- Agent-authored commits end with the session's attribution trailers, and the
  pull request body says an agent wrote it.

**Tests and findings**
- A test-only task never changes production code and never removes a test case.
  A flaky-test fix is proven twice: a stress run with zero failures, and a
  temporary mutation showing the test can still fail.
- One issue per finding, with the evidence in it; never an umbrella issue. Do
  not comment on `envoyproxy/envoy`.
- Separate what you measured from what you inferred, and say what you could
  not verify.

**Clusters**
- Experiments run on kind, with the pinned version (`e2e/kind-version.sh`
  refuses an older binary); put the kube context back where you found it
  afterwards.
- A shared cluster is read-only unless the owner authorised that specific
  write. A helm upgrade, a rollout or a delete is never implied by a task.
- The platform's GitOps repository takes pull requests; its owner merges them.

## Git Workflow

- **Never commit directly to `main`.** Always create a feature branch (`feat/`, `fix/`, `deps/`, etc.) and open a PR.
- **Never push to `main` directly.** Use `git push -u origin <branch>` for feature branches only.
- After merging a PR, delete the feature branch locally (`git branch -d <branch>`) and remotely (`git fetch --prune origin` or `git push origin --delete <branch>`).
- When updating an existing PR branch, use `git commit --amend` and `git push --force-with-lease` (never force push without `--force-with-lease`).
