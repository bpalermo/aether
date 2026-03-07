# Aether

A Kubernetes service mesh data plane built in Go. Aether runs a per-node agent (DaemonSet) that manages an Envoy xDS control plane and a CNI plugin for transparent traffic interception. It supports pluggable service registries (DynamoDB, etcd) for endpoint discovery.

## Architecture

```
┌─────────────────────────────────────── Node ───────────────────────────────────────┐
│                                                                                    │
│  ┌──────────────┐     ┌──────────────┐     ┌──────────────────────────────────┐    │
│  │  CNI Plugin   │────▶│  Agent       │────▶│  Envoy (xDS)                     │    │
│  │  (intercept)  │     │  (DaemonSet) │     │  listeners, clusters, endpoints  │    │
│  └──────────────┘     └──────┬───────┘     └──────────────────────────────────┘    │
│                              │                                                     │
└──────────────────────────────┼─────────────────────────────────────────────────────┘
                               │
                    ┌──────────▼──────────┐
                    │  Service Registry    │
                    │  (DynamoDB or etcd)  │
                    └─────────────────────┘
```

**Agent** — Runs on each node via `controller-runtime`. Manages the xDS server, CNI gRPC server, and registry connection as runnables. Generates Envoy configuration (listeners, clusters, endpoints, routes) from local pod data and the service registry.

**CNI Plugin** — Implements the CNI spec (Add/Del/Check/GC/Status) for transparent traffic interception. Communicates with the agent over a Unix domain socket for pod registration.

**Service Registry** — Pluggable backend for endpoint discovery. Supports DynamoDB (single-table design) and etcd (hierarchical key structure with protobuf serialization). Selected at runtime via `--registry-backend`.

## Getting Started

### Prerequisites

- [Bazelisk](https://github.com/bazelbuild/bazelisk) (Bazel 8.6.0)
- Go 1.26.0
- Docker (or Colima) for container images and integration tests

### Build

```bash
make build-agent           # Build the node agent
make build-cni-install     # Build the CNI installer
```

### Test

```bash
make test                  # Run all tests (requires Docker for integration tests)

# Unit tests only
bazel test --test_output=errors --test_tag_filters=-integration //...

# Integration tests (testcontainers)
bazel test --test_output=errors //registry/internal/ddb:ddb_test
bazel test --test_output=errors //registry/internal/etcd:etcd_test
```

### Container Images

```bash
make load-all              # Load all images into local Docker
make push-all              # Push all images to registry
```

### Adding Go Dependencies

```bash
bazel run @rules_go//go get <package>
bazel run //:gazelle
```
