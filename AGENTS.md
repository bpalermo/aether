# AGENTS.md

This file provides concise, high-signal guidance for OpenCode agents working in the Aether repository.

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
make lint          # Run linters (buf, buildifier, shellcheck)
make format-check  # CI-friendly check (fails on drift)
```

## Toolchain

- **Bazel:** 9.2.0 (via Bazelisk). Use `bazel` commands directly or via `Makefile`.
- **Go:** 1.27.1
- **Container images:** Built with `rules_img`, pushed to distroless (`gcr.io/distroless/static-debian13:nonroot`).
- **Protobuf:** Uses `buf/validate` for validation.
- **Static analysis:** `nogo` is wired into the Go SDK (`go_sdk.nogo(nogo = "//:nogo")`), so vet-class analyzers run as a validation action on every Go target — findings fail `bazel build`, not just `make lint`. Scope lives in `nogo.json`.

## Architecture

**Binaries:**
- `agent/cmd/agent` — Node DaemonSet. Manages xDS server (Envoy), CNI gRPC server, SPIRE bridge via `controller-runtime` Manager. Also hosts the `agent edge` subcommand, and `agent proxy-supervisor` as a deprecated alias of the binary below.
- `agent/cmd/proxy-supervisor` — Envoy hot-restart supervisor (own binary + own image since #772; PID 1 of the `aether-proxy` container). 15 MiB / 24 modules, no k8s, no xDS, no SPIRE — asserted by `:deps_test` and `scripts/check-proxy-supervisor-deps.sh`.
- `agent/cmd/mesh-dns` — Slim standalone mesh-DNS daemon (own DaemonSet + image). Serves pods from the record snapshot the agent writes.
- `registrar/cmd/registrar` — In-cluster Deployment. Proxies external registry (Kubernetes/etcd), maintains endpoint snapshot, streams to agents via gRPC.
- `controller/cmd/controller` — In-cluster Deployment (leader-elected). Serves the validating + pod-mutating admission webhooks and the `MeshConfig`→ConfigMap reconciler.
- `cni/cmd/cni` — CNI plugin binary (Add/Del/Check/GC/Status).
- `cni/cmd/cni-install` — Init container that installs the CNI plugin binary and config onto the host.
- `agent/cmd/proxy-ready` — Exec readiness probe for the `aether-proxy` pod (#673). One flag (`--ready-marker`), stdlib-only; `//agent/cmd/proxy-ready:deps_test` fails the build if it grows a dependency.
- `agent/cmd/mesh-dns-ready` — Same for the `aether-mesh-dns` pod (#683); bundled in the mesh-dns image, guarded by `//agent/cmd/mesh-dns-ready:deps_test`.
- `prober/cmd/prober` — Synthetic mesh-availability prober (proposal 013). Own chart (`charts/prober`) + own image; mesh-managed per-node DaemonSet that probes the data plane from the client side and emits `aether_probe_requests_total`.

**Notable packages** (beyond the ones named above):
- `common/spire` — the shared identity wait/readiness model (#740): `WaitingSource` (never fatal on a missing SPIRE), `ReadyChecker`, `NotReadyDwell` (2m, node agent only) / `ServiceNotReadyDwell` (0). `spiretest` also serves the fake Workload API and the fake SPIFFE Broker Endpoint the identity tests run against.
- `agent/internal/spire` — the SPIFFE **Broker API** client + the SDS bridge (proposal 036, replaced SPIRE's Delegated Identity API): per-pod `SubscribeToX509SVID` streams keyed by a `KubernetesObjectReference` (ns/name **and** UID) over mTLS on `--spire-broker-socket`; validation contexts from the agent's own Workload API bundle plus the pods' federated bundles. Needs SPIRE >= 1.15.2 with its experimental broker enabled.
- `agent/internal/identity` — late-bound trust domain, folded in when the first SVID arrives.
- `agent/internal/node` + `controller/internal/nodetaint` — proposal 033 taint lifecycle: the agent removes `aether.io/agent-not-ready`, the controller's leader-elected guard re-arms it.
- `agent/internal/cniconflist` — re-asserts aether's chained entry in the node's CNI conflist (#645).
- `registrar/internal/replicator` — leader-elected cross-region etcd mirroring under an origin-heartbeat lease (proposal 006).

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

## Git Workflow

- **Never commit directly to `main`.** Always create a feature branch (`feat/`, `fix/`, `deps/`, etc.) and open a PR.
- **Never push to `main` directly.** Use `git push -u origin <branch>` for feature branches only.
- After merging a PR, delete the feature branch locally (`git branch -d <branch>`) and remotely (`git fetch --prune origin` or `git push origin --delete <branch>`).
- When updating an existing PR branch, use `git commit --amend` and `git push --force-with-lease` (never force push without `--force-with-lease`).
