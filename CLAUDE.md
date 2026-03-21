# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is Aether

Aether is a Kubernetes service mesh data plane built in Go. It runs an **agent** (DaemonSet) on each node that manages an Envoy xDS control plane and a CNI plugin for transparent traffic interception. Services are registered in a pluggable registry (DynamoDB or etcd), and the agent generates Envoy configuration (listeners, clusters, endpoints, routes) for local pods.

## Build System

The project uses **Bazel 9.0.1** (via Bazelisk) with `rules_go` and Gazelle for Go, and `rules_img` for container images. Go module is `github.com/bpalermo/aether` with Go 1.26.0.

### Common Commands

```bash
# Run all tests
make test                    # or: bazel test --test_output=errors //...

# Run a single test target
bazel test //agent/internal/xds/config:config_test

# Build
make build-agent             # or: bazel build //agent/cmd/agent/...
make build-cni-install       # or: bazel build //cni/cmd/cni-install/...

# Regenerate BUILD.bazel files after adding/changing Go files
make gazelle                 # or: bazel run //:gazelle

# Tidy module dependencies
make tidy                    # or: bazel mod tidy

# Add a Go dependency
bazel run @rules_go//go get <package>

# Container images
make load-agent-image        # Load agent image into local Docker
make load-cni-install-image  # Load cni-install image into local Docker
make load-all                # Load all images
make push-all                # Push all images
```

## Architecture

### Binaries (under `cmd/`)

- **`agent/cmd/agent`** - Node agent DaemonSet. Uses `controller-runtime` manager to run the xDS server and CNI gRPC server as runnables. CLI built with Cobra.
- **`cni/cmd/cni`** - CNI plugin binary invoked by the container runtime. Implements the CNI spec (Add/Del/Check/GC/Status) via `containernetworking/cni`.
- **`cni/cmd/cni-install`** - Init container that installs the CNI plugin binary and config onto the host.

### Core Packages

- **`xds/`** - Base gRPC server infrastructure and Envoy xDS server wrapping `go-control-plane`. `Server` provides lifecycle management (start, graceful shutdown, liveness/readiness) over Unix domain sockets or TCP. `XdsServer` embeds `Server` and registers Envoy discovery services (LDS, CDS, EDS, RDS, ADS).
- **`agent/internal/xds/`** - Agent-specific xDS logic. `server/` builds Envoy snapshots from local pod storage + registry. `proxy/` generates Envoy resource types (listeners, clusters, endpoints, routes, filter chains). `config/` has shared Envoy config helpers (SPIRE mTLS, HTTP connection manager).
- **`agent/internal/cni/server/`** - CNI gRPC server handling pod registration/deregistration. Uses protovalidate for request validation. Queries Kubernetes node metadata for topology-aware routing.
- **`registry/`** - Service registry interface with DynamoDB (`internal/ddb/`) and etcd (`internal/etcd/`) implementations. The agent selects the backend via `--registry-backend` flag. Manages service endpoint registration and discovery.
- **`agent/pkg/storage/`** - Local file-based storage with in-memory caching and fsnotify file watching. Stores protobuf-serialized CNI pod data.
- **`api/`** - Protobuf definitions under `aether/cni/v1/` and `aether/registry/v1/`. Uses `buf/validate` for proto validation and `protoc-gen-dynamo` for DynamoDB marshaling.
- **`constants/`** - Shared Kubernetes labels, annotations, and registry constants.
- **`common/file/`** - Atomic file write utilities with platform-specific fadvise support.

### Key Patterns

- gRPC servers use Unix domain sockets for node-local communication (agent xDS, CNI server).
- The agent uses `controller-runtime` Manager to orchestrate multiple runnables (xDS server, CNI server, registry).
- Envoy configuration is built via snapshot cache (`go-control-plane/pkg/cache/v3`) with versioned snapshots.
- `ServerCallback` interface (with `PreListen`) allows servers to do setup (e.g., generate initial xDS snapshot, query node metadata) before accepting connections.
- Proto files use `buf/validate` annotations and `protoc-gen-dynamo` for DynamoDB attribute mapping.
- Gazelle manages BUILD.bazel files. Run `make gazelle` after modifying Go imports or adding files.
- Container images use distroless base (`gcr.io/distroless/static-debian12:nonroot`) and multi-arch builds (amd64/arm64).

## Testing

- Integration tests use [testcontainers-go](https://golang.testcontainers.org/) to run real dependencies (etcd, DynamoDB Local) in Docker containers via Colima.
- Integration test targets use `size = "medium"` and `tags = ["integration"]` in BUILD.bazel.
- Tests guarded with `testing.Short()` skip to allow running unit-only with `--test_arg=-test.short`.
- Never modify production code when asked to add or fix tests only. Never remove existing test cases unless explicitly asked.
