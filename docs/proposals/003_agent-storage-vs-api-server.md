# 003: Agent pod storage — local files vs querying the API server

**Status:** Accepted (2026-06-10)

## Question

The agent keeps a per-pod JSON file (`{containerID}.json` under
`/var/lib/aether/registry`, protojson-encoded `CNIPod`) written at CNI ADD and
deleted at CNI DEL, with an in-memory cache loaded at startup. The agent also
has a node-scoped Pod informer (field selector `spec.nodeName`). Should the
file-based storage be replaced by reading pod state directly from the API
server?

## Decision

**Keep file-based storage as the source of truth.** Close two gaps instead:
persist the pod's Kubernetes UID, and export storage/informer divergence
metrics.

## Rationale

1. **The storage holds data the API server does not have.** The pinned network
   namespace path, sandbox container ID, and CNI-time PID come from the CNI
   invocation (kubelet → CNI args). They drive xDS listener generation and
   SPIRE workload subscriptions. There is no API-server substitute for the
   netns path — losing it means losing the pod's data-plane wiring.

2. **Restart safety.** On agent restart, `LoadListenersFromStorage` rebuilds
   the full xDS snapshot *before* the xDS server accepts Envoy connections.
   The informer cache starts empty and needs an API round trip to sync;
   making the initial snapshot depend on it would couple data-plane recovery
   to API-server availability — precisely wrong during control-plane outages
   or node drains, when the mesh must keep serving from local state.

3. **The hybrid is already correctly layered.** API-derived fields (labels,
   annotations, service account, `terminating`) are fetched once at CNI ADD
   (`enhanceCNIPod`) and snapshotted into storage — the contract at admission
   time. The informer supplies the one *live* signal that matters afterwards:
   `deletionTimestamp` for the early-drain termination watch. Storage = state
   at admission; informer = change notification. Migrating reads to the
   informer would buy nothing (the fields are immutable in practice for mesh
   purposes) and cost restart independence.

4. **Two real gaps, both cheap (fixed alongside this ADR):**
   - The pod **UID was not persisted**, so `runResubscribeStoredPods` had to
     re-query the API server per pod at startup to rebuild SPIRE
     `k8s:pod-uid` selectors — the agent's only hard startup API dependency.
     `CNIPod.kubernetes_uid` now removes it (with an API fallback for
     pre-upgrade files).
   - **Divergence was invisible**: skew between storage, the informer, and
     the registry only surfaced when the ghost sweep silently fixed it. The
     `aether.agent.storage.pods` vs `aether.agent.informer.pods` gauges and
     the `aether.agent.ghost_sweep.*` counters make it observable.

## Consequences

- Agent restart is fully independent of the API server for data-plane
  recovery (xDS rebuild and SPIRE resubscription both run from storage).
- Storage/informer skew is an alertable signal instead of an inferred one:
  `abs(aether_agent_storage_pods - aether_agent_informer_pods) > 0` sustained
  across sweep intervals warrants investigation.
- The CNI ADD path keeps its single API-server read (`enhanceCNIPod`); no new
  API load is introduced (the divergence count reads the informer cache).
