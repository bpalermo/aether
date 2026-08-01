# AGENTS.md

This file provides concise, high-signal guidance for OpenCode agents working in the Aether repository.

## Quick Start

**Build & Test:**
```bash
make test          # All tests (requires Docker for integration)
make test-unit     # Unit tests only (no Docker)
make build-agent   # Build agent binary
```

**Format & Lint:**
```bash
make format        # Format all code
make lint          # Run linters (buf, buildifier, shellcheck)
make format-check  # CI-friendly check (fails on drift)
```

## Toolchain

- **Bazel:** 9.0.2 (via Bazelisk). Use `bazel` commands directly or via `Makefile`.
- **Go:** 1.26.2
- **Container images:** Built with `rules_img`, pushed to distroless (`gcr.io/distroless/static-debian12:nonroot`).
- **Protobuf:** Uses `buf/validate` for validation, `protoc-gen-dynamo` for DynamoDB marshaling.

## Architecture

**Three binaries:**
- `agent/cmd/agent` — Node DaemonSet. Manages xDS server (Envoy), CNI gRPC server, SPIRE bridge via `controller-runtime` Manager.
- `registrar/cmd/registrar` — In-cluster Deployment. Proxies external registry (DynamoDB/etcd), maintains endpoint snapshot, streams to agents via gRPC.
- `cni/cmd/cni` — CNI plugin binary (Add/Del/Check/GC/Status).

**Key patterns:**
- gRPC servers use Unix domain sockets for node-local communication.
- Agent uses `controller-runtime` Manager to orchestrate runnables (xDS, CNI, SPIRE, registry).
- Envoy config via snapshot cache (`go-control-plane/pkg/cache/v3`) with versioned snapshots.
- `ServerCallback` interface with `PreListen` allows setup before accepting connections.

## Workflow

**After adding/modifying Go files:**
```bash
make gazelle     # Regenerate BUILD.bazel
make tidy        # Update go.mod dependencies
```

**Integration tests:** Use `testcontainers-go` to run etcd/DynamoDB Local in Docker. Run `./bazel/configure_colima.sh` once on macOS with Colima to configure Docker socket access.

**Test tags:**
- `size = "medium"` and `tags = ["integration"]` for integration tests.
- Use `--test_arg=-test.short` to skip integration tests.

## Proto & Codegen

- Proto files in `api/` under `aether/cni/v1/`, `aether/registry/v1/`, `aether/registrar/v1/`.
- DynamoDB marshaling via `protoc-gen-dynamo` (suffix: `pb.dynamo.go`).
- Run `make gazelle` after proto or import changes.

## Constraints

- Never modify production code when asked to add or fix tests only.
- Never remove existing test cases unless explicitly asked.
- Formatting uses `gofumpt`, `buildifier`, `shfmt`, `buf`.
- Lint violations fail with `--config=lint` (aspect-based).
- SPIRE integration enabled by default; use `--spire-enabled=false` to disable.

## Git Workflow

- **Never commit directly to `main`.** Always create a feature branch (`feat/`, `fix/`, `deps/`, etc.) and open a PR.
- **Never push to `main` directly.** Use `git push -u origin <branch>` for feature branches only.
- After merging a PR, delete the feature branch locally (`git branch -d <branch>`) and remotely (`git fetch --prune origin` or `git push origin --delete <branch>`).
- When updating an existing PR branch, use `git commit --amend` and `git push --force-with-lease` (never force push without `--force-with-lease`).
