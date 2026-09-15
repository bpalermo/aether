# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is Aether

Aether is a Kubernetes service mesh data plane built in Go. It runs an **agent** (DaemonSet) on each node that manages an Envoy xDS control plane and a CNI plugin for transparent traffic interception. Services are registered in a pluggable registry (Kubernetes or etcd), and the agent generates Envoy configuration (listeners, clusters, endpoints, routes) for local pods.

## Build System

The project uses **Bazel 9.2.0** (via Bazelisk) with `rules_go` and Gazelle for Go, and `rules_img` for container images. Go module is `aethermesh.dev` (vanity import path, proposal 035; hosted at github.com/bpalermo/aether) with Go 1.27.x (`MODULE.bazel`'s `go_sdk.download` carries the exact pin). `nogo` (vet-class static analysis) is wired via `go_sdk.nogo(nogo = "//:nogo")` and runs as a build-time validation action on **every** Go target — a vet finding fails the build, not just the lint pass; `nogo.json` scopes which analyzers apply where.

### Common Commands

```bash
# Run all tests
make test                    # or: bazel test --test_output=errors //...

# Run a single test target
bazel test //agent/internal/xds/config:config_test

# Go race detector. `--config=race` (.bazelrc) is the supported spelling; prefer
# it scoped to the packages you touched — the whole tree still has known
# test-only races, so a bare //... run reports failures you did not cause.
bazel test --config=race //agent/internal/meshdns:all
make test-race               # whole tree, same flag

# Build
make build-agent             # or: bazel build //agent/cmd/agent/...
make build-mesh-dns          # or: bazel build //agent/cmd/mesh-dns/...
make build-cni-install       # or: bazel build //cni/cmd/cni-install/...
make build-registrar         # or: bazel build //registrar/cmd/registrar/...

# Regenerate BUILD.bazel files after adding/changing Go files
make gazelle                 # or: bazel run //:gazelle

# Tidy module dependencies (syncs MODULE.bazel's use_repo with go.mod)
make tidy                    # or: bazel mod tidy

# Audit go.mod/go.sum (stale go.sum modules, unused direct requires). NEVER run
# `go mod tidy`: the generated proto packages are Bazel-only outputs, so it
# fails on every import of them and `-e` strips what only BUILD files need.
# See docs/runbook.md, "Go dependency hygiene".
make deps-audit              # or: scripts/go-deps-audit.sh — also a required
                             # CI job (`deps-audit` in .github/workflows/ci.yaml)
# Its rule: reclassify, don't drop. An unimported direct require usually exists
# only to force a CVE-clean version through MVS — move it to the `// indirect`
# block so the pin survives; deleting the line lets the vulnerable version back
# in. Bazel-only requires get a `// bazel-only:` annotation instead.

# Format code (Go, protobuf, Starlark, shell)
make format                  # or: bazel run //:format
make format-check            # Check only, no modifications

# Lint (buf, buildifier, shellcheck)
make lint                    # or: bazel build --config=lint //...

# Add a Go dependency
bazel run @rules_go//go get <package>

# Container images
make load-agent-image        # Load agent image into local Docker
make load-mesh-dns-image     # Load mesh-dns image into local Docker
make load-cni-install-image  # Load cni-install image into local Docker
make load-registrar-image    # Load registrar image into local Docker
make load-all                # Load all images
make push-all                # Push all images
```

## Architecture

### Binaries

There is no top-level `cmd/`: each component owns its own (`agent/cmd/`, `cni/cmd/`, `registrar/cmd/`, `controller/cmd/`, `prober/cmd/`).

- **`agent/cmd/agent`** - Node agent DaemonSet. Uses `controller-runtime` manager to run the xDS server and CNI gRPC server as runnables. CLI built with Cobra. Also hosts two subcommands: `agent edge` (the north-south edge gateway control plane, proposal 003/018) and `agent proxy-supervisor` (the Envoy hot-restart supervisor, proposal 001).
- **`agent/cmd/mesh-dns`** - Slim standalone mesh-DNS daemon (its own DaemonSet and its own image since #583). Serves `<svc>.<ns>.<mesh-domain>` from the record snapshot the node agent writes and forwards everything else upstream, so the resolver survives agent rolls (#578).
- **`agent/cmd/proxy-ready`** - The `aether-proxy` pod's exec readiness probe (#673). One flag (`--ready-marker`); exit 0 iff that path stats. Deliberately stdlib-only (~1.7MB vs the agent's 67MB) — `//agent/cmd/proxy-ready:deps_test` fails the build if it ever grows a dependency. Ships as an extra layer in the agent image and is staged onto the proxy pod by the `install-supervisor` initContainer.
- **`agent/cmd/mesh-dns-ready`** - The same pattern for the `aether-mesh-dns` pod (#683), guarded by `//agent/cmd/mesh-dns-ready:deps_test`. Bundled in the mesh-dns image the DaemonSet already runs, so the prober and the daemon that writes the marker are the same artifact and no chart/image skew is possible.
- **`prober/cmd/prober`** - Synthetic mesh-availability prober (proposal 013), shipped as its own chart (`charts/prober`) and its own image. A mesh-managed per-node DaemonSet that black-box probes the data plane from the *client* side across three tiers (liveness / reachability / mesh_dns) and emits `aether_probe_requests_total` — the external SLI the proxy's own stats cannot produce, because a source proxy cannot report its own outage.
- **`registrar/cmd/registrar`** - In-cluster Registrar Deployment. Proxies registry operations, caches an endpoint snapshot, and streams changes to agents via gRPC. Uses `controller-runtime` manager with leader election. Also hosts the cross-cluster config-export controller (proposal 026).
- **`controller/cmd/controller`** - In-cluster Controller Deployment (leader-elected). Serves the admission webhooks (`MeshConfig`, `HTTPFilter`, `EdgeConfig`, `EndpointPolicy`, `HTTPRoute` validation on `/validate`; a pod-mutating webhook on `/mutate` for mesh-domain `ndots` + namespace-based mesh injection) and reconciles each namespace's `MeshConfig` CR into a projected ConfigMap.
- **`cni/cmd/cni`** - CNI plugin binary invoked by the container runtime. Implements the CNI spec (Add/Del/Check/GC/Status) via `containernetworking/cni`.
- **`cni/cmd/cni-install`** - Init container that installs the CNI plugin binary and config onto the host.

### Core Packages

- **`common/xds/`** - Base gRPC server infrastructure and Envoy xDS server wrapping `go-control-plane`. `Server` provides lifecycle management (start, graceful shutdown, liveness/readiness) over Unix domain sockets or TCP. `XdsServer` embeds `Server` and registers Envoy discovery services (LDS, CDS, EDS, RDS, ADS).
- **`agent/internal/xds/`** - Agent-specific xDS logic. `cache/` builds and versions the Envoy snapshot (listeners, clusters, endpoints, routes, capture/`cap_http`, gamma, edge). `proxy/` generates Envoy resource types (listeners, clusters, endpoints, routes, filter chains) and converts `registryv1.GammaRoute` protos into Envoy config. `config/` has shared Envoy config helpers (SPIRE mTLS, HTTP connection manager).
- **`agent/internal/gamma/`** - The node agent's GAMMA reconciler. Watches `HTTPRoute`/`GRPCRoute`/`ReferenceGrant`/`HTTPFilter` parented to a Service, calls `common/gammaproject` to project them, and feeds the resulting routes into the xDS cache (outbound + capture paths). Gated by `--gamma`.
- **`agent/internal/l4route/`** - The node agent's L4 route reconciler: watches `TCPRoute`/`TLSRoute`/`UDPRoute` parented to a Service and projects them into the capture listener as weighted TCP-floor chains / SNI-routed TLS chains (proposal 018, Phase 3b). Unconditional since proposal 031 (the old `--l4-routes` flag was retired); each route type is gated only on its Gateway API CRD being installed. UDPRoute is served too — the CNI installs the UDP redirect and the agent builds a `udp_proxy` listener when UDPRoute backends exist — but UDP rides the mesh in plaintext (mTLS is a TCP/TLS construct; no DTLS).
- **`agent/internal/endpointpolicy/`** - The node agent's `EndpointPolicy` reconciler: watches the service-scoped UDS delivery CRD (proposal 034 Phase 1b) and projects `<ns>/<svc>` → socket into the xDS cache. CRD-presence-gated; the pod annotation `endpoint.aether.io/uds-socket` wins over a policy.
- **`agent/internal/configimport/`** - Cross-cluster config import (proposal 026, `--import-config`). A controller-runtime runnable that polls the registrar's `ListConfig` for `registryv1.ServiceConfigProjection`s peer clusters exported and materializes them into the node proxy's routes (merged with local; local wins; `--control-cluster` restricts trust to one origin).
- **`agent/internal/edge/`** - The `agent edge` control plane: watches Gateway API objects cluster-wide and serves xDS to a single-identity ingress Envoy (proposals 003/018/021/028).
- **`agent/internal/cni/server/`** - CNI gRPC server handling pod registration/deregistration. Uses protovalidate for request validation. Queries Kubernetes node metadata for topology-aware routing.
- **`common/gammaproject/`** - The **shared** Gateway API / GAMMA projector used by BOTH the agent's gamma reconciler and the registrar's config-export controller. `ProjectHTTPRule`/`ProjectGRPCRule` turn a route rule into a `registryv1.GammaRoute` proto; `ServiceParents`, `ServiceChainFilter`, `ServiceInboundFilter`, and `ServiceFilters` resolve `HTTPFilter` attachments.
- **`common/extensionfilter/`** - Single source of truth for the proxy-extension escape hatch (proposal 025): the allow-list of supported Envoy HTTP filters plus fail-closed validation/rendering of a filter's typed config. Shared by `gammaproject` and the controller's `HTTPFilter` webhook (which can't import agent internals).
- **`controller/internal/`** - The controller's webhooks and reconciler: `webhook/` dispatches the `/validate` admission endpoint by Kind to `meshconfig/`, `httpfilter/`, `edgeconfig/` (edge best-practices/HTTP3 hardening, proposal 029), `endpointpolicy/` (service-scoped UDS delivery, proposal 034), and `gatewayapi/` validators; `podmutate/` is the `/mutate` pod-mutating webhook (ndots + namespace mesh injection); `meshconfig/` reconciles each namespace's `MeshConfig` CR into a projected ConfigMap.
- **`registry/`** - Service registry interface with Kubernetes (`internal/k8s/`), etcd (`internal/etcd/`), and registrar (`internal/registrar/`) implementations. The registrar selects the backend via `--registry-backend`. Manages endpoint registration/discovery and, for etcd, the cross-cluster config plane (`ConfigExporter`/`ConfigImporter`).
- **`registrar/internal/server/`** - Registrar server: versioned endpoint snapshot, broadcaster for fan-out to agent watch streams, sync loop polling the external registry for changes.
- **`registrar/internal/configexport/`** - The registrar's cross-cluster config-export controller (proposal 026, leader-elected). Projects exported (`ServiceExport`-listed) `HTTPRoute`/`GRPCRoute` targets via `common/gammaproject` and writes `registryv1.ServiceConfigProjection`s to the shared registry (etcd config keys) for peer clusters to import.
- **`agent/internal/meshdns/`** - The in-process mesh-DNS resolver: answers `<svc>.<ns>.<mesh-domain>` from the generated mesh Services, persists its record table to a host-local snapshot file (`--mesh-dns-snapshot-path`) that the standalone `agent/cmd/mesh-dns` daemon watches, and self-checks for a wedged resolver.
- **`common/spire/`** - Shared SPIRE Workload API plumbing for every component. `WaitingSource` acquires the X.509 SVID and trust bundle in the background over the Workload API socket, retrying with jittered backoff and returning `ErrNoSVIDYet` (a *waiting* state, never fatal) until the first SVID lands; `ReadyChecker` turns that into a controller-runtime readiness check, with `NotReadyDwell` (2m, node agent only — its NotReady arms a node taint) and `ServiceNotReadyDwell` (0, everything behind a Service). Issue #740, PRs #741–#744; operator view in `docs/runbook.md`.
- **`agent/internal/identity/`** - The node agent's late-bound workload-identity facts, now that nothing blocks on SPIRE at wiring time. `TrustDomain` is a concurrency-safe holder for the trust domain the agent programs into Envoy (SDS resource names, SPIFFE IDs, peer validation): seeded with the mesh domain at wiring time and folded in whenever SPIRE gets around to issuing an SVID (#740).
- **`agent/internal/node/`** + **`controller/internal/nodetaint/`** - The proposal 033 node-taint lifecycle, split across the two writers. The agent **removes** `aether.io/agent-not-ready:NoSchedule` once its own readiness gates pass (CNI serving, conflist chained, identity ready); the controller's leader-elected guard **re-arms** it when a node's agent pod is missing or not-Ready past a grace period (reboot / crash gaps G1 and G2, #569). Neither does the other's half.
- **`agent/internal/cniconflist/`** - Keeps aether chained in the node's active CNI conflist (`--cni-conflist-reassert`). Aether installs itself as a chained plugin inside another CNI's conflist, and any competing writer that rewrites the file from its own template wins permanently — on Talos, kube-flannel's `cp -f` on every bootstrap-manifest re-sync silently unmeshed every subsequently-started pod (incident 2026-08-29, #645). The loop watches the mounted `net.d` with fsnotify plus a periodic re-check and only ever re-appends into an existing, valid conflist.
- **`registrar/internal/replicator/`** - The registrar's leader-elected cross-region etcd replicator (proposal 006 Phase 2). It watches only this region's own authoritative subtree and replays every change verbatim into each peer region's etcd, so mirroring is loop-free by construction; every mirrored key hangs off a per-peer origin-heartbeat lease that only this replicator refreshes, so a dead region's mirror expires on the peers with no peer-side GC.
- **`common/udspath/`** - Resolves a pod's `<volume>/<socket>` annotation onto its host path under kubelet's pod-volumes dir (`--kubelet-pods-dir`), enforcing the `emptyDir`-only shape and the 107-byte `AF_UNIX` budget (proposal 034).
- **`agent/storage/`** - Local file-based storage with in-memory caching and fsnotify file watching. Stores CNI pod data as protojson (`<key>.json`).
- **`api/`** - Protobuf definitions under `aether/cni/v1/`, `aether/registry/v1/`, `aether/registrar/v1/`, `aether/config/v1/` (`MeshConfig`, `HTTPFilter`, `EdgeConfig`, `EndpointPolicy`), and `aether/agent/v1/` (the node agent's persisted observed demand set, `ObservedUpstreams`; node-local state, not a wire API). Uses `buf/validate` for proto validation.
- **`common/apis/config/v1/`** - Kubernetes CRD Go types (`MeshConfig`, `HTTPFilter`, `EdgeConfig`, `EndpointPolicy`) wrapping the `aether/config/v1` protos with deepcopy/jsonshim glue.
- **`common/constants/`** - Shared Kubernetes labels, annotations (prefixes `aether.io/`, `endpoint.aether.io/`, `config.aether.io/`, `capture.aether.io/`), and registry/proxy/endpoint constants.
- **`common/file/`** - Atomic file write utilities with platform-specific fadvise support.

### Key Patterns

- gRPC servers use Unix domain sockets for node-local communication (agent xDS, CNI server).
- The agent uses `controller-runtime` Manager to orchestrate multiple runnables (xDS server, CNI server, registry).
- Envoy configuration is built via snapshot cache (`go-control-plane/pkg/cache/v3`) with versioned snapshots.
- `ServerCallback` interface (with `PreListen`) allows servers to do setup (e.g., generate initial xDS snapshot, query node metadata) before accepting connections.
- Proto files use `buf/validate` annotations.
- Gazelle manages BUILD.bazel files. Run `make gazelle` after modifying Go imports or adding files.
- Container images use distroless base (`gcr.io/distroless/static-debian13:nonroot`) and multi-arch builds (amd64/arm64).
- Formatting and linting use `aspect_rules_lint`. Formatters (gofumpt, buildifier, shfmt, buf) are configured in `bazel/format/BUILD.bazel`. Lint aspects (buf, buildifier, shellcheck) are defined in `bazel/lint/linters.bzl`. Use `--config=lint` to run lints, `--config=ci` to fail on violations.

## Testing

- Integration tests use [testcontainers-go](https://golang.testcontainers.org/) to run real dependencies (etcd) in Docker containers via Colima.
- Integration test targets use `size = "medium"` and `tags = ["integration"]` in BUILD.bazel.
- Tests guarded with `testing.Short()` skip to allow running unit-only with `--test_arg=-test.short`.
- Never modify production code when asked to add or fix tests only. Never remove existing test cases unless explicitly asked.
