# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Before you change anything

`AGENTS.md` § *Rules for agents* is the checklist for every change in this
repository (lint and test commands, what must never be printed or pushed, what a
chart or workflow change obliges you to do, how pull requests are stacked and
merged). `.claude/agents/` holds role agents for the recurring kinds of work:
`aether-adversarial-reviewer`, `aether-investigator`, `aether-ci-engineer`,
`aether-chart-engineer`, `aether-flake-fixer`.

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

# Line coverage of the unit suite over all first-party Go (LCOV + Cobertura in
# ./coverage-report); the same script .github/workflows/coverage.yaml runs.
# That workflow GATES pull requests: `coverage` fails when the total drops more
# than 1.0 point below main's (scripts/coverage-compare.sh; repository variable
# COVERAGE_MAX_DROP). See docs/runbook.md, "Code coverage".
make coverage                # or: scripts/coverage.sh [-- <bazel coverage flags>]

# Build
make build-agent             # or: bazel build //agent/cmd/agent/...
make build-mesh-dns          # or: bazel build //agent/cmd/mesh-dns/...
make build-proxy-supervisor  # or: bazel build //agent/cmd/proxy-supervisor/...
make build-uds-csi           # or: bazel build //agent/cmd/uds-csi/...
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

# Copy the Bazel-generated proto Go into the tree (git-ignored) and build with the
# plain go command — what CodeQL scans (.github/workflows/codeql.yaml; runbook).
# It is `go build ./...` + `go vet ./...` over the WHOLE module with no excluded
# package, and the CodeQL gate allows no extraction error. Bazel's `visibility`
# does not enforce Go's internal-package rule, so a package that imports
# `<tree>/internal/...` must live under `<tree>/` (#1311) — never add an exclusion.
make go-build-plain          # make materialize-go / materialize-go-clean

# Format code (Go, protobuf, Starlark, shell)
make format                  # or: bazel run //:format
make format-check            # Check only, no modifications

# Lint (buf, shellcheck, gocognit). buildifier's Starlark lint runs in format-check
make lint                    # or: bazel build --config=lint //...

# Lint the GitHub Actions workflows (pinned actionlint + the repo's ShellCheck;
# config .github/actionlint.yaml). A required CI job (`actionlint` in ci.yaml)
make actionlint              # or: bazel run //bazel/actionlint

# Add a Go dependency
bazel run @rules_go//go get <package>

# Container images
make load-agent-image        # Load agent image into local Docker
make load-mesh-dns-image     # Load mesh-dns image into local Docker
make load-proxy-supervisor-image  # Load proxy-supervisor image into local Docker
make load-uds-csi-image      # Load uds-csi image into local Docker
make load-cni-install-image  # Load cni-install image into local Docker
make load-registrar-image    # Load registrar image into local Docker
make load-all                # Load the six Make-built images (agent, mesh-dns,
                             # proxy-supervisor, uds-csi, cni-install, registrar)
make push-all                # Push those six (local/dev use only: releases are
                             # published by the signed .github/workflows/publish.yaml,
                             # never by push-all; the proxy image by proxy-release.yml)
```

## Architecture

### Binaries

There is no top-level `cmd/`: each component owns its own (`agent/cmd/`, `cni/cmd/`, `registrar/cmd/`, `controller/cmd/`, `prober/cmd/`).

- **`agent/cmd/agent`** - Node agent DaemonSet. Uses `controller-runtime` manager to run the xDS server and CNI gRPC server as runnables. CLI built with Cobra. Also hosts the `agent edge` subcommand (the north-south edge gateway control plane, proposal 003/018). The supervisor is NOT an agent subcommand any more — the `agent proxy-supervisor` alias was removed after #772.
- **`agent/cmd/proxy-supervisor`** - The Envoy hot-restart supervisor (proposal 001), PID 1 of the `aether-proxy` container. Its own binary and its own image since #772: it used to be an `agent` subcommand, so the proxy pod staged and ran the whole 65 MiB agent binary (controller-runtime, client-go, go-control-plane, SPIRE) to fork a child process. It is a fraction of that now. `//agent/cmd/proxy-supervisor:deps_test` (forbidden heavyweights plus a binary-size ceiling) and `scripts/check-proxy-supervisor-deps.sh` are the source of truth for what it may link. For the current module count, run `go version -m` on the built binary.
- **`agent/cmd/uds-csi`** - The `csi.aether.io` CSI node plugin (proposal 039), the **only** carrier for UDS-delivered workload sockets since 039 Phase 2 (chart 2.0.0, which removed the `emptyDir` carrier), its own image (`uds-csi`) and its own privileged DaemonSet (`charts/aether` `udsCsi.enabled`, default **on**; the same switch renders the agent's `--uds-csi-root` and the proxy's read-only mount of `udsCsi.root`). Mounts a per-pod tmpfs (`nosuid,nodev,noexec,nosymfollow`, `mode=2770,gid=<fsGroup>`) at `/run/aether/uds/<pod-uid>` and binds it onto the kubelet's target path; serves the kubelet plugin-registration API itself (no `node-driver-registrar`). Logic in `agent/internal/udscsi` (mounts behind a `Mounter` interface so unit tests need no root). `//agent/cmd/uds-csi:deps_test` and `scripts/check-uds-csi-deps.sh` keep it free of client-go/controller-runtime/SPIRE. Kind e2e: `e2e/uds-csi.sh`.
- **`agent/cmd/mesh-dns`** - Slim standalone mesh-DNS daemon (its own DaemonSet and its own image since #583). Serves `<svc>.<ns>.<mesh-domain>` from the record snapshot the node agent writes and forwards everything else upstream, so the resolver survives agent rolls (#578). `//agent/cmd/mesh-dns:deps_test` pins its module set (an allow-list; the guard's `maxModules` is the budget) and `scripts/check-mesh-dns-deps.sh` forbids the agent's heavyweights in the build graph.
- **`agent/cmd/proxy-ready`** - The `aether-proxy` pod's exec readiness probe (#673). One flag (`--ready-marker`); exit 0 iff that path stats. Deliberately stdlib-only (~1.7MB vs the agent's 67MB) — `//agent/cmd/proxy-ready:deps_test` fails the build if it ever grows a dependency. Ships as an extra layer in the **proxy-supervisor** image since #772 (`agent/cmd/proxy-supervisor/BUILD.bazel`, `tars_layer`), not the agent image, and is staged onto the proxy pod by the `install-supervisor` initContainer, which already runs that image.
- **`agent/cmd/agent-ready`** - The `aether-agent` pod's own exec liveness/readiness probe (proposal 041). Asks the agent over its pod-local `--health-socket` (in the pod's `/tmp` emptyDir), so a surge-rolled standby is never answered for by the other agent on the node. Stdlib-only, no `net/http`; `//agent/cmd/agent-ready:deps_test` asserts the linked ELF lists no module. Ships as `/agent-ready` in the agent image.
- **`agent/cmd/mesh-dns-ready`** - The same pattern for the `aether-mesh-dns` pod (#683), guarded by `//agent/cmd/mesh-dns-ready:deps_test`. Bundled in the mesh-dns image the DaemonSet already runs, so the prober and the daemon that writes the marker are the same artifact and no chart/image skew is possible.
- **`agent/cmd/identity-ready`** - The egress identity gate (#1053): the init container the controller's `/mutate` webhook injects first into every mesh pod (default on, `controller.webhook.identityGate.enabled`; opt out with `aether.io/identity-gate: "false"`). Waits on the pod's own SPIRE Workload API (a `csi.spiffe.io` volume in the init container only) until the pod's SVID exists, so the app cannot send before it has an identity. gRPC + go-spiffe's generated Workload API client only — `//agent/cmd/identity-ready:deps_test` holds an explicit module allow-list. Ships as an extra layer in the agent image.
- **`prober/cmd/prober`** - Synthetic mesh-availability prober (proposal 013), shipped as its own chart (`charts/prober`) and its own image. A mesh-managed per-node DaemonSet that black-box probes the data plane from the *client* side across three tiers (liveness / reachability / mesh_dns) and emits `aether_probe_requests_total` — the external SLI the proxy's own stats cannot produce, because a source proxy cannot report its own outage.
- **`registrar/cmd/registrar`** - In-cluster Registrar Deployment. Proxies registry operations, caches an endpoint snapshot, and streams changes to agents via gRPC. Uses `controller-runtime` manager with leader election. Also hosts the cross-cluster config-export controller (proposal 026).
- **`controller/cmd/controller`** - In-cluster Controller Deployment (leader-elected). Serves the admission webhooks (`MeshConfig`, `HTTPFilter`, `EdgeConfig`, `EndpointPolicy`, `HTTPRoute` validation on `/validate`; a pod-mutating webhook on `/mutate` for mesh-domain `ndots` + namespace-based mesh injection + the `identity-ready` init container) and reconciles each namespace's `MeshConfig` CR into a projected ConfigMap.
- **`cni/cmd/cni`** - CNI plugin binary invoked by the container runtime. Implements the CNI spec (Add/Del/Check/GC/Status) via `containernetworking/cni`. Exec'd per pod ADD/DEL, so `//cni/cmd/cni:deps_test` pins its module set (an allow-list; the guard's `maxModules` is the budget) and `scripts/check-cni-deps.sh` forbids client-go/controller-runtime/xDS/SPIRE/OpenTelemetry in the build graph. The plugin exports no telemetry of its own since #1166/#1185. It forwards its timings and post-ADD outcome to the agent on the CNI RPCs (`cni/internal/plugin/timings.go`).
- **`cni/cmd/cni-install`** - Init container that installs the CNI plugin binary and config onto the host.

### Core Packages

- **`common/grpcserver/`** - Base gRPC server infrastructure, Envoy-free. `Server` provides lifecycle management (start, graceful shutdown, liveness/readiness, bind gate) over Unix domain sockets or TCP. The registrar and the agent's CNI server embed it directly, so they do not link `go-control-plane`.
- **`common/xds/`** - Envoy xDS server wrapping `go-control-plane`. `XdsServer` embeds `grpcserver.Server` and registers Envoy discovery services (LDS, CDS, EDS, RDS, ADS).
- **`agent/internal/xds/`** - Agent-specific xDS logic. `cache/` builds and versions the Envoy snapshot (listeners, clusters, endpoints, routes, capture/`cap_http`, gamma, edge). `proxy/` generates Envoy resource types (listeners, clusters, endpoints, routes, filter chains) and converts `registryv1.GammaRoute` protos into Envoy config. `config/` has shared Envoy config helpers (SPIRE mTLS, HTTP connection manager).
  - **`cache/quicpairs.go`** - East-west QUIC (proposal 038 Phase 4): the observed (source ServiceAccount, destination) pairs a `quic:` twin cluster is built for (first use, #1020), their persistence in `ObservedUpstreams` (`demand_confirmed`, #1076), the dormant/republish handling and the post-start fetch-window prune (`--east-west-quic-pair-fetch-window`, #1033/#1073).
  - **`quicdemand/`** - The per-stream ledger of the `quic:` twins the proxy holds an on-demand (ODCDS) subscription for (#1036), held per xDS stream = per proxy generation (#1052). Envoy keeps one ODCDS subscription per name for life, so a subscribed twin must never be forgotten: its pair goes dormant and is republished instead.
  - **`server/`** - The agent's xDS server: snapshot refresh plus `odcds.go`, which watches named (on-demand) CDS subscriptions and decides whether to admit a requested `quic:` twin (real first use vs a stream-reset re-subscription, #1033).
  - **`ack/`** - Tracks Envoy's delta-xDS ACK/NACKs — the agent's confirmation that an update reached the proxy, replacing admin `/config_dump` polling. What a proxy holds is kept per xDS stream (one stream = one proxy process) and dropped with it (#1624): a pod ADD waits for a connected proxy to hold the listener at the version the agent publishes, a pod DEL for every proxy that has asked for its listeners to be known not to hold it.
- **`agent/internal/capture/`** - The transparent-capture controller: watches the generated selectorless mesh Services and projects their `cluster.local` authorities into the snapshot cache for the `cap_http` route table the per-pod capture listeners serve.
- **`agent/internal/proxy/hotrestart/`** + **`agent/internal/supervisorcmd/`** - The Envoy hot-restart supervisor (proposal 001): `hotrestart/` is the supervisor itself (epochs, cross-pod handoff, watchdogs, the child-silent signal and kill-both-epochs path of #1058); `supervisorcmd/` is its cobra command, kept outside `agent/internal/cmd` so `agent/cmd/proxy-supervisor` links none of the agent (#772).
- **`agent/internal/gatewaystatus/`** - Shared Gateway API status writers (`RouteParentStatus`, Gateway/GatewayClass conditions) that only touch entries carrying aether's own `controllerName` — the conformance on-ramp.
- **`cni/internal/`** - The CNI side: `plugin/` is the chained plugin (ADD/DEL/CHECK, netns pinning, the in-netns readiness probe) and programs the TPROXY capture divert (`capture.go`, proposal 038) and the mesh-DNS `:53` DNAT (`dns.go`); `install/` + `cmd/` are `cni-install`; `cri/`, `log/`, `util/`, `constants/` are helpers. There is no `telemetry/` any more (#1185). The plugin forwards its timings to the agent (`plugin/timings.go`), and the agent records them.
- **`agent/internal/gamma/`** - The node agent's GAMMA reconciler. Watches `HTTPRoute`/`GRPCRoute`/`ReferenceGrant`/`HTTPFilter` parented to a Service, calls `common/gammaproject` to project them, and feeds the resulting routes into the xDS cache (outbound + capture paths). Gated by `--gamma`.
- **`agent/internal/l4route/`** - The node agent's L4 route reconciler: watches `TCPRoute`/`TLSRoute`/`UDPRoute` parented to a Service and projects them into the capture listener as weighted TCP-floor chains / SNI-routed TLS chains (proposal 018, Phase 3b). Unconditional since proposal 031 (the old `--l4-routes` flag was retired); each route type is gated only on its Gateway API CRD being installed. UDPRoute is served too — since proposal 038 (#947) the CNI captures both TCP and UDP with a TPROXY-style mark-and-divert (the IP header stays intact, so the original destination survives), and UDP lands on a per-VIP `udp_proxy` listener on 18082 — but UDP rides the mesh in plaintext (mTLS is a TCP/TLS construct; no DTLS).
- **`agent/internal/endpointpolicy/`** - The node agent's `EndpointPolicy` reconciler: watches the service-scoped UDS delivery CRD (proposal 034 Phase 1b) and projects `<ns>/<svc>` → socket into the xDS cache. CRD-presence-gated; the pod annotation `endpoint.aether.io/uds-socket` wins over a policy.
- **`agent/internal/configimport/`** - Cross-cluster config import (proposal 026, `--import-config`). A controller-runtime runnable that polls the registrar's `ListConfig` for `registryv1.ServiceConfigProjection`s peer clusters exported and materializes them into the node proxy's routes (merged with local; local wins; `--control-cluster` restricts trust to one origin).
- **`agent/internal/edge/`** - The `agent edge` control plane: watches Gateway API objects cluster-wide and serves xDS to a single-identity ingress Envoy (proposals 003/018/021/028).
- **`agent/internal/cni/server/`** - CNI gRPC server handling pod registration/deregistration. Uses protovalidate for request validation. Queries Kubernetes node metadata for topology-aware routing.
- **`common/gammaproject/`** - The **shared** Gateway API / GAMMA projector used by BOTH the agent's gamma reconciler and the registrar's config-export controller. `ProjectHTTPRule`/`ProjectGRPCRule` turn a route rule into a `registryv1.GammaRoute` proto; `ServiceParents`, `ServiceChainFilter`, `ServiceInboundFilter`, and `ServiceFilters` resolve `HTTPFilter` attachments.
- **`common/l4project/`** - The **shared** L4 projector: turns a Gateway API `TCPRoute`/`TLSRoute`/`UDPRoute` backendRef list into weighted data-plane backends (core-Service filtering, ReferenceGrant admission, weight defaulting with `weight: 0` as drain, namespace-qualified `<ns>/<svc>` keys). Used by BOTH `agent/internal/l4route` and `agent/internal/edge/gatewayapi`; the caller supplies the cluster namer (`tcp:` vs `udp:`). The L4 sibling of `common/gammaproject`.
- **`common/referencegrant/`** - The single copy of the Gateway API ReferenceGrant check and the backendRef key/namespace helpers (#1168/#1172), including `ResolveBackends`. Used by `common/gammaproject`, `common/l4project`, the agent's `gamma` and `l4route` reconcilers, and `agent/internal/edge/gatewayapi`.
- **`common/extensionfilter/`** - Single source of truth for the proxy-extension escape hatch (proposal 025): the allow-list of supported Envoy HTTP filters plus fail-closed validation/rendering of a filter's typed config. Shared by `gammaproject` and the controller's `HTTPFilter` webhook (which can't import agent internals).
- **`controller/internal/`** - The controller's webhooks and reconciler: `webhook/` dispatches the `/validate` admission endpoint by Kind to `meshconfig/`, `httpfilter/`, `edgeconfig/` (edge best-practices/HTTP3 hardening, proposal 029), `endpointpolicy/` (service-scoped UDS delivery, proposal 034), and `gatewayapi/` validators; `podmutate/` is the `/mutate` pod-mutating webhook (ndots + namespace mesh injection + the egress identity gate); `meshconfig/` reconciles each namespace's `MeshConfig` CR into a projected ConfigMap.
- **`registry/`** - Service registry interface with Kubernetes (`internal/k8s/`), etcd (`internal/etcd/`), and registrar (`internal/registrar/`) implementations. The registrar selects the backend via `--registry-backend`. Manages endpoint registration/discovery and, for etcd, the cross-cluster config plane (`ConfigExporter`/`ConfigImporter`).
- **`registrar/internal/server/`** - Registrar server: versioned endpoint snapshot, broadcaster for fan-out to agent watch streams, sync loop polling the external registry for changes. Since #1204 the snapshot version names the snapshot's contents rather than counting changes. On etcd it is the store revision plus a content hash: `<rev>.<hash>`, or `<rev>+<hash>` with a write-behind overlay (`registry.RevisionedLister`). On the kubernetes backend it is `hash:<hash>`. Only the hash decides whether a reconnecting agent is current (`Snapshot.WatchStart`). The version rides only on `SNAPSHOT_COMPLETE` and on the last event of a batch (#1193/#1203).
- **`registrar/internal/configexport/`** - The registrar's cross-cluster config-export controller (proposal 026, leader-elected). Projects exported (`ServiceExport`-listed) `HTTPRoute`/`GRPCRoute` targets via `common/gammaproject` and writes `registryv1.ServiceConfigProjection`s to the shared registry (etcd config keys) for peer clusters to import.
- **`agent/internal/meshdns/`** - The in-process mesh-DNS resolver: answers `<svc>.<ns>.<mesh-domain>` from the generated mesh Services, persists its record table to a host-local snapshot file (`--mesh-dns-snapshot-path`) that the standalone `agent/cmd/mesh-dns` daemon watches, and self-checks for a wedged resolver.
- **`agent/internal/spire/`** - The node agent's identity bridge: a **SPIFFE Broker API** client (proposal 036, which replaced SPIRE's proprietary Delegated Identity API and its admin socket) plus the SDS bridge that feeds the xDS cache. For every pod on the node it opens a `SubscribeToX509SVID` stream carrying a `KubernetesObjectReference` (`pods`/`core`, namespace + name + UID) over mutual TLS on `--spire-broker-socket`, so **SPIRE** resolves and attests the pod — every selector its Kubernetes attestor can produce becomes usable in a registration entry. A reference resolves at request time, so a CNI ADD can race the kubelet pod list; `NotFound` is retried on the jittered backoff and counted. Validation contexts are built from the agent's **own** Workload API trust bundle plus the union of the `federated_bundles` the live pod streams carry — the Broker API has no node-wide bundle stream. Requires SPIRE >= 1.15.2 with its experimental broker enabled.
- **`common/spire/`** - Shared SPIRE Workload API plumbing for every component. `WaitingSource` acquires the X.509 SVID and trust bundle in the background over the Workload API socket, retrying with jittered backoff and returning `ErrNoSVIDYet` (a *waiting* state, never fatal) until the first SVID lands; `ReadyChecker` turns that into a controller-runtime readiness check, with `NotReadyDwell` (2m, node agent only — its NotReady arms a node taint) and `ServiceNotReadyDwell` (0, everything behind a Service). Issue #740, PRs #741–#744; operator view in `docs/runbook.md`.
- **`agent/internal/identity/`** - The node agent's late-bound workload-identity facts, now that nothing blocks on SPIRE at wiring time. `TrustDomain` is a concurrency-safe holder for the trust domain the agent programs into Envoy (SDS resource names, SPIFFE IDs, peer validation): seeded with the mesh domain at wiring time and folded in whenever SPIRE gets around to issuing an SVID (#740).
- **`agent/internal/node/`** + **`controller/internal/nodetaint/`** - The proposal 033 node-taint lifecycle, split across the two writers. The agent **removes** `aether.io/agent-not-ready:NoSchedule` once its own readiness gates pass (CNI serving, conflist chained, identity ready); the controller's leader-elected guard **re-arms** it when a node's agent pod is missing or not-Ready past a grace period (reboot / crash gaps G1 and G2, #569). Neither does the other's half. The taint helpers both writers share live in `common/taint/`, because the controller cannot import agent internals.
- **`agent/internal/ownership/`** - Which node agent owns its node (proposal 041). The owner holds an exclusive `flock(2)` on a lock file under `/run/aether` for its whole life. A surge-rolled successor that finds the lock taken runs as a **standby**: it builds everything a first serve needs but binds no node socket and writes no node file. When it acquires the lock it takes over, and only then binds `xds.sock`/`cni.sock` and starts the held-back writers. Paired with `agent/cmd/agent-ready` and the chart's `agent.updateStrategy.surge`.
- **`common/drain/`** - The timing of the two-phase endpoint drain (#152) that more than one component must agree on. `PoolCloseDelay` sizes phase 2 (UNHEALTHY, pool close) to the pod's preStop/grace. The agent's CNI server and the registrar's kubernetes backend (#1144) both read it, so they cannot drift.
- **`agent/internal/cniconflist/`** - Keeps aether chained in the node's active CNI conflist (`--cni-conflist-reassert`). Aether installs itself as a chained plugin inside another CNI's conflist, and any competing writer that rewrites the file from its own template wins permanently — on Talos, kube-flannel's `cp -f` on every bootstrap-manifest re-sync silently unmeshed every subsequently-started pod (incident 2026-08-29, #645). The loop watches the mounted `net.d` with fsnotify plus a periodic re-check and only ever re-appends into an existing, valid conflist.
- **`registrar/internal/replicator/`** - The registrar's leader-elected cross-region etcd replicator (proposal 006 Phase 2). It watches only this region's own authoritative subtree and replays every change verbatim into each peer region's etcd, so mirroring is loop-free by construction; every mirrored key hangs off a per-peer origin-heartbeat lease that only this replicator refreshes, so a dead region's mirror expires on the peers with no peer-side GC.
- **`common/udspath/`** - Resolves a pod's `<volume>/<socket>` UDS request (annotation or `EndpointPolicy`) onto `<uds-csi-root>/<pod-uid>/<file>` (proposals 034/039): the volume must be the pod's single inline `csi.aether.io` volume (`CNIPod.uds_csi_volume`, recorded at CNI ADD), the file a clean segment within the 107-byte `AF_UNIX` budget (54 bytes at the default root, `MaxFileLen`). Every failure is a classified `Reason` (`not_csi`, `volume_not_declared`, …) — the agent counts them as `aether.agent.uds.resolve_failures{reason}`, and the controller's pod webhook (`controller/internal/podmutate/udscarrier.go`) and EndpointPolicy webhook run the same rules at admission.
- **`agent/storage/`** - Local file-based storage with in-memory caching and fsnotify file watching. Stores CNI pod data as protojson (`<key>.json`).
- **`api/`** - Protobuf definitions under `aether/cni/v1/`, `aether/registry/v1/`, `aether/registrar/v1/`, `aether/config/v1/` (`MeshConfig`, `HTTPFilter`, `EdgeConfig`, `EndpointPolicy`), `aether/kubelet/pluginregistration/v1/` (a copy of the kubelet's plugin-registration API, proto package kept as `pluginregistration` — wire contract; `bazel/lint/buf.yaml` scopes the naming-rule ignores to that file), and `aether/agent/v1/` (the node agent's persisted observed demand set, `ObservedUpstreams`; node-local state, not a wire API). Uses `buf/validate` for proto validation.
- **`common/apis/config/v1/`** - Kubernetes CRD Go types (`MeshConfig`, `HTTPFilter`, `EdgeConfig`, `EndpointPolicy`) wrapping the `aether/config/v1` protos with deepcopy/jsonshim glue.
- **`common/constants/`** - Shared Kubernetes labels, annotations (prefixes `aether.io/`, `endpoint.aether.io/`, `config.aether.io/`, `capture.aether.io/`), and registry/proxy/endpoint constants.
- **`common/file/`** - Atomic file write utilities with platform-specific fadvise support.
- **`agent/test/`** - Test harnesses that drive the agent's xDS generators against the real pinned Envoy, kept under `agent/` because they import `agent/internal/xds/...` (Go's internal-package rule; they lived in top-level `test/` until #1311). `envoy_validate/` runs `envoy --mode validate` over aether-generated bootstraps (`generate/` writes them to disk; `validate-built-proxy.sh` re-runs the gate against a locally built proxy, which `proxy.yml` does); `mtlspool/` runs live two-identity mTLS / QUIC pools; `envoyargs/` holds the supervisor's `--envoy-arg` check and its prediction of Envoy's default worker count against the pinned Envoy's own argument parser and worker count (#1433, #1442); `envoybin/` locates the pinned binary in runfiles. Tagged `envoy-validate` (no Docker), part of the unit leg. Not product code: `scripts/coverage.sh` excludes the subtree (`EXCLUDED_SUBTREES`). Top-level `test/` keeps what imports no `internal/` package (`test/e2e`, `test/conformance`).

### Data-plane ports

`common/constants/mesh/mesh.go` (18001, 18054, 18081, 18082) and `agent/internal/xds/proxy/` (`ingress.go` 18008, `edge.go` 18009/18021): **18001** per-pod TCP capture (the TPROXY divert target for every captured TCP port); **18008** mesh inbound, TCP and UDP/QUIC on the same number; **18009** host-netns east/west waypoint tunnel (019); **18021** edge readiness listener; **18054** the host mesh-DNS resolver (pod `:53` is DNATed to `HOST_IP:18054`); **18081** per-pod outbound HTTP; **18082** the L4 port, TCP and UDP (UDP capture binds it directly: a datagram reply's source port is the bound port).

### Carried Envoy patches

The proxy image (`proxy/`, its own Bazel workspace) builds a pinned Envoy plus source patches in `proxy/bazel/patches/`, applied in order by `single_version_override(patches = …)` in `proxy/MODULE.bazel`; `proxy/README.md` lists what each one fixes and its upstream PR. Their tests run as `//bazel/patches:carried_patch_tests` in that workspace. A patch goes away with the first pin bump that contains it upstream.

### Key Patterns

- gRPC servers use Unix domain sockets for node-local communication (agent xDS, CNI server).
- The agent uses `controller-runtime` Manager to orchestrate multiple runnables (xDS server, CNI server, registry).
- Envoy configuration is built via snapshot cache (`go-control-plane/pkg/cache/v3`) with versioned snapshots.
- `ServerCallback` interface (with `PreListen`) allows servers to do setup (e.g., generate initial xDS snapshot, query node metadata) before accepting connections.
- Proto files use `buf/validate` annotations.
- Gazelle manages BUILD.bazel files. Run `make gazelle` after modifying Go imports or adding files.
- Container images use distroless base (`gcr.io/distroless/static-debian13:nonroot`) and multi-arch builds (amd64/arm64).
- Formatting and linting use `aspect_rules_lint`. Formatters (gofumpt, buildifier, shfmt, buf) are configured in `bazel/format/BUILD.bazel`. Lint aspects (buf, shellcheck, gocognit) are defined in `bazel/lint/linters.bzl`; buildifier's Starlark *lint* rides the formatter (`starlark_check_args`/`starlark_fix_args`), so `make format-check` reports it and `make format` applies its mechanical fixes. Use `--config=lint` to run lints, `--config=ci` to fail on violations.

## Testing

- Integration tests use [testcontainers-go](https://golang.testcontainers.org/) to run real dependencies (etcd) in Docker containers via Colima.
- Integration test targets use `size = "medium"` and `tags = ["integration"]` in BUILD.bazel.
- Tests guarded with `testing.Short()` skip to allow running unit-only with `--test_arg=-test.short`.
- Never modify production code when asked to add or fix tests only. Never remove existing test cases unless explicitly asked.
- The one exception (`AGENTS.md`, "Tests and findings"): an agent MAY remove a production function that only tests reference, together with the test that only calls it, when the PR shows (by whole-module analysis, not grep alone) that nothing else references it.
- "One release" of compatibility means one minor version of the `aether` chart: deprecated in 2.5.x, removable in 2.6.0, never within the same minor (`AGENTS.md`, "Compatibility window").
