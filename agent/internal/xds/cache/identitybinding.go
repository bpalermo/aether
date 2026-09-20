package cache

import (
	"context"
	"sort"
	"strings"

	"aethermesh.dev/agent/internal/xds/proxy"
)

// The outbound identity-binding discriminator (issue #638).
//
// WHAT THE BINDING ACTUALLY IS. Since issue #842 an mTLS-injected outbound
// cluster carries ONE transport socket (built by proxy.InjectUpstreamMTLS)
// whose custom_tls_certificate_selector resolves the client certificate per
// connection:
//
//   - the originating listener chain stamps the source pod's SPIFFE ID into
//     shared filter state (proxy.SourceIdentityCertMapperFilterStateKey, with
//     the hashable string factory);
//   - Envoy's filter_state_override certificate mapper reads that object off
//     TransportSocketOptions::downstreamSharedFilterStateObjects() and returns
//     its string AS THE SDS SECRET NAME — or, finding nothing, the configured
//     default_value, which is the node SVID;
//   - the on_demand_secret selector fetches that secret and the handshake uses
//     it.
//
// The filter-state value and the secret name are literally the same string, so
// "this connection presents a different secret than the identity it claims" is
// structurally impossible — the same property the old transport_socket_matches
// had, where the match name WAS the secret name.
//
// So the whole (source pod → cluster → client-cert secret) binding still
// reduces to ONE node-wide index: c.localWorkloads[netns] → SPIFFE ID. It is
// identical for every outbound cluster on the node — which is exactly the shape
// #638 observes in the field (one wrong identity presented toward many distinct
// clusters in a single time slice, never a per-cluster or per-endpoint scatter).
// This file still watches exactly the right table. What has changed twice is
// only WHERE the lookup happens: #815 moved it from the cluster into the
// listener, and #842 removed the cluster's half entirely — the cluster no longer
// names ANY workload identity, so a stale or wrong binding can now only come
// from the listener side, i.e. from this index.
//
// WHAT CAN THEREFORE GO WRONG is not the secret naming but that index:
//
//   - c.localWorkloads (guarded by localMu) and c.listeners (guarded by
//     listenerMu) are written NON-ATOMICALLY on both the add path (AddPod:
//     listeners, then setLocalWorkload) and the remove path (RemovePod: delete
//     listener, then removeLocalWorkload), and are merged separately again by
//     LoadListenersFromStorage on agent start.
//   - The index key is the pod's network-namespace PATH, which the kubelet/CNI
//     reuses. A missed or late CNI DEL leaves a departed pod's identity bound
//     to a netns path a NEW pod may already be using — the new pod's listener
//     is then generated stamping the departed (co-located) workload's SPIFFE ID
//     until the next AddPod write lands, which is precisely the "duration =
//     time to next push" fingerprint. Neither #815 release two nor #842
//     changed this: the wrong identity is baked into the listener rather than
//     looked up in the cluster, and the same c.localWorkloads entry is the
//     cause either way — #842 only made the cluster stop participating at all.
//
// This file makes that index observable: it names the binding each snapshot
// delivers, but only when it CHANGED, and cross-checks the presented identity
// against the pod that actually owns the netns (proxy.SpiffeIDFromPod over the
// listener entry's own CNIPod — the API-server-derived truth, #669).

// maxBindingChangeLines bounds how many per-binding INFO lines one snapshot may
// emit. A node's first snapshot legitimately binds every source pod to every
// outbound cluster; beyond this many changes a single summary line (carrying
// the distinct source→identity transitions, which is the diagnostic part) is
// logged instead so a cold start cannot flood the node's log budget.
const maxBindingChangeLines = 200

// maxSummaryTransitions bounds the transitions rendered on the summary line.
const maxSummaryTransitions = 20

// sourceBinding is what one local source pod's egress will present.
type sourceBinding struct {
	// pod is "<namespace>/<name>" of the pod owning the netns, or "" when no
	// listener entry owns it (an orphaned identity mapping).
	pod string
	// podIdentity is the SPIFFE ID derived from that pod's OWN namespace and
	// ServiceAccount — the identity it is entitled to present. "" when unknown.
	podIdentity string
	// presented is c.localWorkloads[netns]: the SPIFFE ID this netns's listener
	// chain stamps into filter state, and therefore — the certificate mapper
	// returns it verbatim — the SDS client-certificate secret every outbound
	// connection from this pod fetches. Should equal podIdentity.
	presented string
}

// foreign reports whether this source would present an identity that is not its
// own pod's. Only meaningful once the owning pod is known.
func (b sourceBinding) foreign() bool {
	return b.podIdentity != "" && b.podIdentity != b.presented
}

// bindingState is the previous snapshot's binding table, kept so the logging is
// edge-triggered (steady state is silent).
type bindingState struct {
	// sources is netns → binding, the node-wide index every outbound cluster
	// shares.
	sources map[string]sourceBinding
	// clusters is the set of mTLS-injected outbound cluster names, so a cluster
	// appearing for the first time is reported once for every source.
	clusters map[string]struct{}
}

// logIdentityBindings emits the (source pod → outbound cluster → SDS secret)
// bindings this snapshot delivers, but ONLY the ones that changed since the
// previous snapshot (or are seen for the first time), and WARNs about any
// source bound to a foreign identity. Called from generateSnapshot with
// snapshotMu held, which also serializes the stored state.
func (c *SnapshotCache) logIdentityBindings(ctx context.Context, version string) {
	sources := c.collectSourceBindings()
	clusters := c.mtlsClusterNames()

	c.bindingMu.Lock()
	prev := c.lastBindings
	c.lastBindings = bindingState{sources: sources, clusters: clusters}
	c.bindingMu.Unlock()

	changedSources, newClusters := diffBindings(prev, sources, clusters)
	if len(changedSources) == 0 && len(newClusters) == 0 {
		return
	}

	c.reportBindingMismatches(ctx, version, sources, changedSources, len(clusters))
	c.emitBindingChanges(ctx, version, sources, clusters, changedSources, newClusters)
}

// collectSourceBindings reads the netns → identity index and joins it with the
// pod that owns each netns. Returns nil while the node SVID is unserved: no
// upstream mTLS is injected then, so no cluster binds a client certificate.
//
// The two maps are read under their own locks, sequentially and never nested,
// so this adds no lock-ordering constraint. A binding read across that gap is
// exactly what the data plane would have been handed had the snapshot been cut
// mid-write, which is the state this discriminator exists to catch.
func (c *SnapshotCache) collectSourceBindings() map[string]sourceBinding {
	c.localMu.RLock()
	if c.nodeSpiffeID == "" {
		c.localMu.RUnlock()
		return nil
	}
	trustDomain := c.currentTrustDomain()
	sources := make(map[string]sourceBinding, len(c.localWorkloads))
	for netns, id := range c.localWorkloads {
		sources[netns] = sourceBinding{presented: id}
	}
	c.localMu.RUnlock()

	if len(sources) == 0 {
		return nil
	}

	c.listenerMu.RLock()
	for netns, entry := range c.listeners {
		b, ok := sources[netns]
		if !ok || entry.cniPod == nil {
			continue
		}
		b.pod = entry.cniPod.GetNamespace() + "/" + entry.cniPod.GetName()
		b.podIdentity = proxy.SpiffeIDFromPod(entry.cniPod, trustDomain)
		sources[netns] = b
	}
	c.listenerMu.RUnlock()

	return sources
}

// mtlsClusterNames returns the outbound clusters that carry the per-source
// transport-socket matcher (entry.mtlsCluster is the injected copy; a nil one
// means the bare cluster is emitted and binds no client certificate).
func (c *SnapshotCache) mtlsClusterNames() map[string]struct{} {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	names := make(map[string]struct{}, len(c.clusters))
	for name, entry := range c.clusters {
		if entry.mtlsCluster != nil {
			names[name] = struct{}{}
		}
	}
	return names
}

// diffBindings returns the sources whose binding is new or changed and the
// clusters seen for the first time, both sorted for deterministic log order.
// On the first call prev is zero, so everything is reported as new.
func diffBindings(prev bindingState, sources map[string]sourceBinding, clusters map[string]struct{}) (changedSources, newClusters []string) {
	for netns, b := range sources {
		if old, ok := prev.sources[netns]; !ok || old != b {
			changedSources = append(changedSources, netns)
		}
	}
	for name := range clusters {
		if _, ok := prev.clusters[name]; !ok {
			newClusters = append(newClusters, name)
		}
	}
	sort.Strings(changedSources)
	sort.Strings(newClusters)
	return changedSources, newClusters
}

// reportBindingMismatches WARNs for every changed source whose bound secret is
// not its own pod's SPIFFE ID, and counts them. Mismatch status is a function
// of the source binding, so a mismatch appearing or clearing always shows up as
// a changed source — no persistent mismatch is reported twice for one state.
func (c *SnapshotCache) reportBindingMismatches(ctx context.Context, version string, sources map[string]sourceBinding, changed []string, clusterCount int) {
	var mismatches int64
	for _, netns := range changed {
		b := sources[netns]
		switch {
		case b.podIdentity == "":
			// No listener entry owns this netns: a departed (or not-yet-added)
			// pod's identity is still selectable by whatever stamps that netns.
			c.log.WarnContext(ctx, "outbound identity mapping has no owning pod",
				"source_netns", netns,
				"secret", b.presented,
				"clusters", clusterCount,
				"snapshot_version", version)
		case b.foreign():
			mismatches++
			c.log.WarnContext(ctx, "outbound cluster bound to a foreign identity",
				"source_pod", b.pod,
				"source_netns", netns,
				"source_spiffe_id", b.podIdentity,
				"bound_spiffe_id", b.presented,
				"secret", b.presented,
				"clusters", clusterCount,
				"snapshot_version", version)
		}
	}
	c.metrics.OutboundBindingMismatch(ctx, mismatches)
}

// emitBindingChanges logs one line per changed (source pod, cluster) pair. A
// changed source re-binds on every cluster; an unchanged source only re-binds
// on clusters that are new this snapshot.
func (c *SnapshotCache) emitBindingChanges(ctx context.Context, version string, sources map[string]sourceBinding, clusters map[string]struct{}, changedSources, newClusters []string) {
	total := len(changedSources)*len(clusters) + (len(sources)-len(changedSources))*len(newClusters)
	if total == 0 {
		return
	}
	if total > maxBindingChangeLines {
		c.logBindingSummary(ctx, version, sources, changedSources, total, len(clusters), len(newClusters))
		return
	}

	allClusters := sortedNames(clusters)
	changedSet := make(map[string]struct{}, len(changedSources))
	for _, netns := range changedSources {
		changedSet[netns] = struct{}{}
		c.logBindings(ctx, version, netns, sources[netns], allClusters)
	}

	if len(newClusters) == 0 {
		return
	}
	for _, netns := range sortedSources(sources) {
		if _, ok := changedSet[netns]; ok {
			continue
		}
		c.logBindings(ctx, version, netns, sources[netns], newClusters)
	}
}

// logBindings emits one binding line per cluster for a single source pod.
func (c *SnapshotCache) logBindings(ctx context.Context, version, netns string, b sourceBinding, clusters []string) {
	for _, cluster := range clusters {
		// source_spiffe_id is the identity the cluster WILL PRESENT; secret is
		// the SDS name it fetches for it (the same string — see the file
		// comment). pod_spiffe_id is what the owning pod is entitled to, so a
		// single line is self-diagnosing.
		c.log.InfoContext(ctx, "outbound identity binding",
			"source_pod", b.pod,
			"source_netns", netns,
			"source_spiffe_id", b.presented,
			"pod_spiffe_id", b.podIdentity,
			"cluster", cluster,
			"secret", b.presented,
			"snapshot_version", version)
	}
}

// logBindingSummary replaces the per-binding lines when a single snapshot
// re-binds more than maxBindingChangeLines pairs (a cold start, or a churn that
// touches every source). It keeps the diagnostic part — the distinct
// source→identity transitions — and drops only the per-cluster repetition,
// which is redundant: the binding is identical on every outbound cluster.
func (c *SnapshotCache) logBindingSummary(ctx context.Context, version string, sources map[string]sourceBinding, changedSources []string, total, clusterCount, newClusterCount int) {
	transitions := make([]string, 0, len(changedSources))
	for _, netns := range changedSources {
		if len(transitions) == maxSummaryTransitions {
			transitions = append(transitions, "...")
			break
		}
		b := sources[netns]
		transitions = append(transitions, b.pod+"="+b.presented)
	}

	c.log.InfoContext(ctx, "outbound identity bindings changed",
		"changes", total,
		"sources_changed", len(changedSources),
		"clusters", clusterCount,
		"clusters_new", newClusterCount,
		"transitions", strings.Join(transitions, " "),
		"snapshot_version", version)
}

// sortedNames returns the set's members in sorted order.
func sortedNames(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sortedSources returns the binding table's netns keys in sorted order.
func sortedSources(sources map[string]sourceBinding) []string {
	out := make([]string, 0, len(sources))
	for netns := range sources {
		out = append(out, netns)
	}
	sort.Strings(out)
	return out
}
