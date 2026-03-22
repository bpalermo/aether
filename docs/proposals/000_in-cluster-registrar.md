# Proposal: In-Cluster Registrar Service

**Status:** Draft
**Author:** Bruno Palermo
**Date:** 2026-03-22

## Problem Statement

Today every Aether agent (one per node) connects directly to the external registry backend (DynamoDB, etcd, or Cloud Map). This creates several issues:

1. **External dependency fan-out** -- An N-node cluster opens N persistent connections to the external registry. This increases cost (DynamoDB RCU/WCU, Cloud Map API calls), adds latency to pod startup (CNI plugin must wait for registry write), and creates a hard external dependency on every node.

2. **No local query path** -- When the xDS server builds a snapshot it calls `ListAllEndpoints` against the external registry. Every snapshot rebuild (on pod add/remove, or on startup) hits the remote backend. There is no local cache shared across agents on the same cluster.

3. **No cross-cluster broadcast primitive** -- Multi-cluster service discovery is implicit: agents in cluster A and cluster B happen to read the same DynamoDB table or Cloud Map namespace. There is no explicit mechanism to subscribe to changes, reconcile stale endpoints, or scope visibility between clusters.

4. **Blast radius** -- A registry outage (e.g. DynamoDB throttling, etcd leader election) degrades every node simultaneously. Agents have no local fallback for endpoint data they previously observed.

## Proposed Solution

Introduce an **in-cluster Registrar** Deployment (or HA pair) that acts as the sole bridge between the cluster and the external registry. Agents communicate with the Registrar over an in-cluster gRPC API instead of reaching the external backend directly.

```
┌──────────────────────── Cluster A ────────────────────────┐
│                                                           │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐                   │
│  │ Agent 1 │  │ Agent 2 │  │ Agent N │                   │
│  └────┬────┘  └────┬────┘  └────┬────┘                   │
│       │             │            │                        │
│       └─────────────┼────────────┘                        │
│                     │  gRPC (in-cluster)                  │
│               ┌─────▼──────┐                              │
│               │  Registrar │                              │
│               └─────┬──────┘                              │
│                     │                                     │
└─────────────────────┼─────────────────────────────────────┘
                      │  External (DynamoDB / etcd / CloudMap)
                ┌─────▼──────┐
                │  Registry  │
                │  Backend   │
                └─────┬──────┘
                      │
┌─────────────────────┼─────────────────────────────────────┐
│                     │                                     │
│               ┌─────▼──────┐                              │
│               │  Registrar │   Cluster B                  │
│               └─────┬──────┘                              │
│       ┌─────────────┼────────────┐                        │
│       │             │            │                        │
│  ┌────▼────┐  ┌─────▼───┐  ┌────▼────┐                   │
│  │ Agent 1 │  │ Agent 2 │  │ Agent N │                   │
│  └─────────┘  └─────────┘  └─────────┘                   │
│                                                           │
└───────────────────────────────────────────────────────────┘
```

## Design Goals

| Goal | Description |
|------|-------------|
| **Reduce external calls** | N agents share 1 connection to the external registry per cluster |
| **Local-first reads** | Agents read endpoints from the Registrar's in-memory state with sub-millisecond latency |
| **Cross-cluster broadcast** | Registrar syncs remote cluster endpoints and pushes changes to agents |
| **Resilience** | Agents serve last-known-good endpoint data during Registrar or registry outages |
| **Backward compatibility** | Direct-to-registry mode remains available as a fallback or for single-node dev setups |

## Architecture

### Components

#### 1. Registrar Service (`registrar/`)

A new Kubernetes Deployment running 1-2 replicas. Every replica is identical and fully independent -- no leader election or inter-replica coordination is needed.

**Responsibilities:**

- **Inbound registration**: Receives `RegisterEndpoint` / `UnregisterEndpoint` calls from agents and writes them to the external registry in batches.
- **Outbound sync**: Periodically polls or watches the external registry for all endpoints (including those from other clusters) and maintains an in-memory snapshot.
- **Change notification**: Streams endpoint change events to subscribed agents via gRPC server-streaming or xDS-style incremental push.
- **Health**: Exposes readiness/liveness probes. All replicas accept writes and serve reads independently.

#### 2. Agent Registrar Client

A new `registrar` registry backend option for agents (`--registry-backend=registrar`). Implements the existing `registry.Registry` interface by calling the Registrar's gRPC API instead of the external backend.

Key behaviors:
- **Writes** (`RegisterEndpoint`, `UnregisterEndpoint`): Fire-and-forget to Registrar with local optimistic update. Registrar batches and persists externally.
- **Reads** (`ListEndpoints`, `ListAllEndpoints`): Served from agent-local cache populated by the Registrar's push stream. Falls back to Registrar RPC if stream is not yet established.
- **Reconnection**: Exponential backoff with jitter on stream disconnect. Serves stale data during disconnection.

### API Surface

```protobuf
// aether/registrar/v1/registrar.proto

service Registrar {
  // Write path -- agent → registrar → external registry
  rpc RegisterEndpoint(RegisterEndpointRequest) returns (RegisterEndpointResponse);
  rpc UnregisterEndpoint(UnregisterEndpointRequest) returns (UnregisterEndpointResponse);

  // Read path -- registrar pushes full state + deltas to agents
  rpc WatchEndpoints(WatchEndpointsRequest) returns (stream EndpointEvent);

  // Snapshot -- agent startup, full reconciliation
  rpc ListAllEndpoints(ListAllEndpointsRequest) returns (ListAllEndpointsResponse);
}

message EndpointEvent {
  enum EventType {
    FULL_SNAPSHOT = 0;
    ENDPOINT_ADDED = 1;
    ENDPOINT_REMOVED = 2;
    ENDPOINT_UPDATED = 3;
  }
  EventType type = 1;
  string service_name = 2;
  aether.registry.v1.Service.Protocol protocol = 3;
  aether.registry.v1.ServiceEndpoint endpoint = 4;  // present for ADD/UPDATE/REMOVE
  string version = 5;  // monotonic version for ordering
}

message WatchEndpointsRequest {
  string cluster_name = 1;     // agent's cluster identity
  string node_name = 2;        // agent's node identity
  string last_version = 3;     // resume token for reconnection
}
```

### Data Flow

#### Pod Add (write path)

```
CNI Plugin → Agent CNI Server → Agent Registrar Client
    → Registrar (in-cluster gRPC)
        → batch write to external registry
        → broadcast EndpointEvent(ADDED) to all watching agents
```

#### xDS Snapshot Build (read path)

```
Agent xDS Server → Agent Registrar Client (local cache)
    → returns cached endpoints (populated by WatchEndpoints stream)
```

#### Cross-Cluster Discovery

```
Registrar (Cluster B) writes endpoint to shared registry
    → Registrar (Cluster A) polls/watches external registry
        → detects new Cluster B endpoint
            → broadcasts EndpointEvent(ADDED) to Cluster A agents
```

### Sync Strategy

The Registrar maintains a **versioned in-memory snapshot** of all endpoints across all clusters:

| Strategy | Mechanism | Latency | Backend Support |
|----------|-----------|---------|-----------------|
| **Polling** | Periodic `ListAllEndpoints` against external registry | Configurable (default 5s) | All backends |
| **Watch** | etcd watch on `/aether/services` prefix | Near real-time | etcd only |
| **Event-driven** | Cloud Map change events via EventBridge | Near real-time | Cloud Map only |

The Registrar computes a diff between the previous and current snapshot, then broadcasts only the changed endpoints to watching agents. Each event carries a monotonic version string so agents can detect gaps and request a full snapshot.

### Batching and Write Coalescing

Agents may register/unregister endpoints in bursts (e.g. during a rolling deployment). The Registrar coalesces writes:

1. Incoming `RegisterEndpoint` / `UnregisterEndpoint` calls are queued in a bounded buffer.
2. A flush goroutine drains the buffer every **100ms** (configurable) or when the buffer reaches a threshold (e.g. 50 operations).
3. Operations are grouped by service name and merged: if the same IP is registered and unregistered in the same batch, the net effect is applied.
4. The merged batch is written to the external registry in a single transaction where possible (DynamoDB `TransactWriteItems`, etcd `Txn`).

### Failure Modes

| Failure | Impact | Mitigation |
|---------|--------|------------|
| Registrar replica down | Agents on that replica lose their watch stream; Kubernetes Service routes new connections to surviving replicas | Multiple replicas behind Service; agents retry with backoff; optional fallback to direct registry |
| External registry down | Registrar cannot persist writes or poll updates | Registrar queues writes in memory; agents unaffected for reads (local cache) |
| Network partition (agent ↔ Registrar) | Agent's watch stream disconnects | Agent serves stale cache; reconnects with `last_version` to resume without full snapshot |
| Network partition (Registrar ↔ external) | Registrar's sync stalls | Agents continue with last-known-good data; Registrar retries; alerts via metrics |

### Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: aether-registrar
  namespace: aether-system
spec:
  replicas: 2
  selector:
    matchLabels:
      app: aether-registrar
  template:
    spec:
      containers:
      - name: registrar
        args:
        - --registry-backend=dynamodb       # or etcd, cloudmap
        - --cluster-name=cluster-a
        - --sync-interval=5s
        - --write-batch-interval=100ms
        ports:
        - name: grpc
          containerPort: 9443
        - name: health
          containerPort: 8081
```

Agents add:
```
--registry-backend=registrar
--registrar-address=aether-registrar.aether-system.svc:9443
```

## Implementation Phases

### Phase 1: Read-Through Proxy

- Registrar implements `Registry` interface, delegates all calls to the configured external backend.
- Agents connect to Registrar instead of external registry.
- No caching, no streaming -- pure proxy. Validates the gRPC API and deployment model.
- **Benefit**: Reduces external connections from N to 1 immediately.

### Phase 2: Cached Reads + Push Stream

- Registrar maintains in-memory endpoint snapshot via periodic sync.
- Implements `WatchEndpoints` stream for push-based updates.
- Agent registrar client caches endpoints locally, populated by the stream.
- `ListEndpoints` / `ListAllEndpoints` served from local agent cache.
- **Benefit**: Sub-millisecond reads, reduced external API calls.

### Phase 3: Write Batching

- Registrar queues and coalesces writes before flushing to external registry.
- Optimistic local broadcast: agents see the endpoint immediately via the push stream, before it's persisted externally.
- **Benefit**: Faster pod startup, reduced write costs, atomic bulk operations.

### Phase 4: Cross-Cluster Broadcast

- Registrar detects endpoints from other clusters during sync.
- Broadcasts remote cluster endpoints to local agents via the same `WatchEndpoints` stream.
- Agents don't need to distinguish local vs. remote -- the Registrar handles scoping.
- **Benefit**: Explicit multi-cluster service discovery with clear ownership.

### Phase 5: Watch-Based Sync (optional)

- For etcd and Cloud Map backends, replace polling with native watch/event mechanisms.
- Reduces sync latency from seconds to near real-time.
- **Benefit**: Faster cross-cluster convergence.

## Metrics and Observability

| Metric | Type | Description |
|--------|------|-------------|
| `registrar_connected_agents` | Gauge | Number of agents with active watch streams |
| `registrar_endpoint_count` | Gauge | Total endpoints in snapshot, by cluster |
| `registrar_sync_duration_seconds` | Histogram | External registry sync latency |
| `registrar_sync_errors_total` | Counter | Failed sync attempts |
| `registrar_write_batch_size` | Histogram | Operations per write batch |
| `registrar_write_queue_depth` | Gauge | Pending writes in buffer |
| `registrar_event_broadcast_total` | Counter | Events pushed to agents, by type |
| `agent_registrar_cache_age_seconds` | Gauge | Time since last cache update on agent |
| `agent_registrar_stream_reconnects_total` | Counter | Watch stream reconnection attempts |

## Open Questions

1. ~~**Leader election scope**~~: **Resolved** -- No leader election needed. Every replica is identical and fully independent: accepts writes, persists to the external registry, maintains its own snapshot, and broadcasts to its connected agents. HA is achieved by running multiple replicas behind a Kubernetes Service. This works because broadcast ordering is guaranteed per-stream (not per-cluster), pod lifecycle writes are node-local and serialized by the caller, agents use eventual consistency, and N=2-3 replicas polling every 5s is negligible load on any backend.

2. ~~**Scope filtering**~~: **Resolved** -- No scope filtering. All agents receive the full endpoint set. The volume of endpoint data in a mesh is bounded and small enough that filtering adds unnecessary complexity without meaningful savings. Agents already process all endpoints to build xDS snapshots with cross-service routing.

3. ~~**Consistency model**~~: **Resolved** -- Eventual consistency is the desired model. Agents do not need read-after-write guarantees for endpoint registration. The optimistic local broadcast (agent sees its own registration via the push stream shortly after writing) is sufficient. This keeps the design simple and avoids version-tracking handshakes between agents and the Registrar.

4. ~~**Migration path**~~: **Resolved** -- No migration path required. Aether is not yet in production, so all agents can switch to the Registrar backend atomically. Direct-to-registry mode will remain in the codebase for development and testing convenience but does not need coexistence support.

5. ~~**SPIRE integration**~~: **Resolved** -- Yes, the Registrar-to-agent gRPC connection will use mTLS via SPIRE SVIDs. This is consistent with the existing SPIRE integration in the agent and ensures zero-trust communication within the mesh control plane.
