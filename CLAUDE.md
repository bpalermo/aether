# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is Aether

Aether is a Kubernetes service mesh data plane built in Go. It runs an **agent** (DaemonSet) on each node that manages an Envoy xDS control plane and a CNI plugin for transparent traffic interception. Services are registered in a pluggable registry (DynamoDB or etcd), and the agent generates Envoy configuration (listeners, clusters, endpoints, routes) for local pods.

## Build System

The project uses **Bazel 9.0.1** (via Bazelisk) with `rules_go` and Gazelle for Go, and `rules_img` for container images. Go module is `github.com/bpalermo/aether` with Go 1.26.2.

### Common Commands

```bash
# Run all tests
make test                    # or: bazel test --test_output=errors //...

# Run a single test target
bazel test //agent/internal/xds/config:config_test

# Build
make build-agent             # or: bazel build //agent/cmd/agent/...
make build-cni-install       # or: bazel build //cni/cmd/cni-install/...
make build-registrar         # or: bazel build //registrar/cmd/registrar/...

# Regenerate BUILD.bazel files after adding/changing Go files
make gazelle                 # or: bazel run //:gazelle

# Tidy module dependencies
make tidy                    # or: bazel mod tidy

# Format code (Go, protobuf, Starlark, shell)
make format                  # or: bazel run //:format
make format-check            # Check only, no modifications

# Lint (buf, buildifier, shellcheck)
make lint                    # or: bazel build --config=lint //...

# Add a Go dependency
bazel run @rules_go//go get <package>

# Container images
make load-agent-image        # Load agent image into local Docker
make load-cni-install-image  # Load cni-install image into local Docker
make load-registrar-image    # Load registrar image into local Docker
make load-all                # Load all images
make push-all                # Push all images
```

## Architecture

### Binaries (under `cmd/`)

- **`agent/cmd/agent`** - Node agent DaemonSet. Uses `controller-runtime` manager to run the xDS server and CNI gRPC server as runnables. CLI built with Cobra. Also hosts two subcommands: `agent edge` (the north-south edge gateway control plane, proposal 003/018) and `agent proxy-supervisor` (the Envoy hot-restart supervisor, proposal 001).
- **`registrar/cmd/registrar`** - In-cluster Registrar Deployment. Proxies registry operations, caches an endpoint snapshot, and streams changes to agents via gRPC. Uses `controller-runtime` manager with leader election. Also hosts the cross-cluster config-export controller (proposal 026).
- **`controller/cmd/controller`** - In-cluster Controller Deployment (leader-elected). Serves the admission webhooks (`MeshConfig`, `HTTPFilter`, `HTTPRoute` validation on `/validate`; a pod-mutating webhook on `/mutate` for mesh-domain `ndots` + namespace-based mesh injection) and reconciles each namespace's `MeshConfig` CR into a projected ConfigMap.
- **`cni/cmd/cni`** - CNI plugin binary invoked by the container runtime. Implements the CNI spec (Add/Del/Check/GC/Status) via `containernetworking/cni`.
- **`cni/cmd/cni-install`** - Init container that installs the CNI plugin binary and config onto the host.

### Core Packages

- **`xds/`** - Base gRPC server infrastructure and Envoy xDS server wrapping `go-control-plane`. `Server` provides lifecycle management (start, graceful shutdown, liveness/readiness) over Unix domain sockets or TCP. `XdsServer` embeds `Server` and registers Envoy discovery services (LDS, CDS, EDS, RDS, ADS).
- **`agent/internal/xds/`** - Agent-specific xDS logic. `cache/` builds and versions the Envoy snapshot (listeners, clusters, endpoints, routes, capture/`cap_http`, gamma, edge). `proxy/` generates Envoy resource types (listeners, clusters, endpoints, routes, filter chains) and converts `registryv1.GammaRoute` protos into Envoy config. `config/` has shared Envoy config helpers (SPIRE mTLS, HTTP connection manager).
- **`agent/internal/gamma/`** - The node agent's GAMMA reconciler. Watches `HTTPRoute`/`GRPCRoute`/`ReferenceGrant`/`HTTPFilter` parented to a Service, calls `common/gammaproject` to project them, and feeds the resulting routes into the xDS cache (outbound + capture paths). Gated by `--gamma`.
- **`agent/internal/configimport/`** - Cross-cluster config import (proposal 026, `--import-config`). A controller-runtime runnable that polls the registrar's `ListConfig` for `registryv1.ServiceConfigProjection`s peer clusters exported and materializes them into the node proxy's routes (merged with local; local wins; `--control-cluster` restricts trust to one origin).
- **`agent/internal/edge/`** - The `agent edge` control plane: watches Gateway API objects cluster-wide and serves xDS to a single-identity ingress Envoy (proposals 003/018/021/028).
- **`agent/internal/cni/server/`** - CNI gRPC server handling pod registration/deregistration. Uses protovalidate for request validation. Queries Kubernetes node metadata for topology-aware routing.
- **`common/gammaproject/`** - The **shared** Gateway API / GAMMA projector used by BOTH the agent's gamma reconciler and the registrar's config-export controller. `ProjectHTTPRule`/`ProjectGRPCRule` turn a route rule into a `registryv1.GammaRoute` proto; `ServiceParents`, `ServiceChainFilter`, `ServiceInboundFilter`, and `ServiceFilters` resolve `HTTPFilter` attachments.
- **`common/extensionfilter/`** - Single source of truth for the proxy-extension escape hatch (proposal 025): the allow-list of supported Envoy HTTP filters plus fail-closed validation/rendering of a filter's typed config. Shared by `gammaproject` and the controller's `HTTPFilter` webhook (which can't import agent internals).
- **`controller/internal/`** - The controller's webhooks and reconciler: `webhook/` dispatches the `/validate` admission endpoint by Kind to `meshconfig/`, `httpfilter/`, and `gatewayapi/` validators; `podmutate/` is the `/mutate` pod-mutating webhook (ndots + namespace mesh injection); `meshconfig/` reconciles each namespace's `MeshConfig` CR into a projected ConfigMap.
- **`registry/`** - Service registry interface with DynamoDB (`internal/ddb/`), etcd (`internal/etcd/`), and registrar (`internal/registrar/`) implementations. The registrar selects the backend via `--registry-backend`. Manages endpoint registration/discovery and, for etcd, the cross-cluster config plane (`ConfigExporter`/`ConfigImporter`).
- **`registrar/internal/server/`** - Registrar server: versioned endpoint snapshot, broadcaster for fan-out to agent watch streams, sync loop polling the external registry for changes.
- **`registrar/internal/configexport/`** - The registrar's cross-cluster config-export controller (proposal 026, leader-elected). Projects exported (`ServiceExport`-listed) `HTTPRoute`/`GRPCRoute` targets via `common/gammaproject` and writes `registryv1.ServiceConfigProjection`s to the shared registry (etcd config keys) for peer clusters to import.
- **`agent/pkg/storage/`** - Local file-based storage with in-memory caching and fsnotify file watching. Stores protobuf-serialized CNI pod data.
- **`api/`** - Protobuf definitions under `aether/cni/v1/`, `aether/registry/v1/`, `aether/registrar/v1/`, and `aether/config/v1/` (`MeshConfig`, `HTTPFilter`). Uses `buf/validate` for proto validation and `protoc-gen-dynamo` for DynamoDB marshaling.
- **`common/apis/config/v1/`** - Kubernetes CRD Go types (`MeshConfig`, `HTTPFilter`) wrapping the `aether/config/v1` protos with deepcopy/jsonshim glue.
- **`common/constants/`** - Shared Kubernetes labels, annotations (prefixes `aether.io/`, `endpoint.aether.io/`, `config.aether.io/`, `capture.aether.io/`), and registry/proxy/endpoint constants.
- **`common/file/`** - Atomic file write utilities with platform-specific fadvise support.

### Key Patterns

- gRPC servers use Unix domain sockets for node-local communication (agent xDS, CNI server).
- The agent uses `controller-runtime` Manager to orchestrate multiple runnables (xDS server, CNI server, registry).
- Envoy configuration is built via snapshot cache (`go-control-plane/pkg/cache/v3`) with versioned snapshots.
- `ServerCallback` interface (with `PreListen`) allows servers to do setup (e.g., generate initial xDS snapshot, query node metadata) before accepting connections.
- Proto files use `buf/validate` annotations and `protoc-gen-dynamo` for DynamoDB attribute mapping.
- Gazelle manages BUILD.bazel files. Run `make gazelle` after modifying Go imports or adding files.
- Container images use distroless base (`gcr.io/distroless/static-debian12:nonroot`) and multi-arch builds (amd64/arm64).
- Formatting and linting use `aspect_rules_lint`. Formatters (gofumpt, buildifier, shfmt, buf) are configured in `bazel/format/BUILD.bazel`. Lint aspects (buf, buildifier, shellcheck) are defined in `bazel/lint/linters.bzl`. Use `--config=lint` to run lints, `--config=ci` to fail on violations.

## Testing

- Integration tests use [testcontainers-go](https://golang.testcontainers.org/) to run real dependencies (etcd, DynamoDB Local) in Docker containers via Colima.
- Integration test targets use `size = "medium"` and `tags = ["integration"]` in BUILD.bazel.
- Tests guarded with `testing.Short()` skip to allow running unit-only with `--test_arg=-test.short`.
- Never modify production code when asked to add or fix tests only. Never remove existing test cases unless explicitly asked.
