package cache

import (
	"context"
	"errors"

	agentconstants "github.com/bpalermo/aether/agent/constants"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
)

// AddPod generates the inbound and outbound listeners and per-pod clusters for the
// given pod and adds them to the cache keyed by the pod's network namespace, then
// regenerates the listener snapshot. Returns an error if listener generation or
// snapshot generation fails.
func (c *SnapshotCache) AddPod(ctx context.Context, cniPod *cniv1.CNIPod, trustDomain string) error {
	netns := cniPod.GetNetworkNamespace()
	c.log.DebugContext(ctx, "adding listeners for pod", "pod", cniPod.GetName(), "namespace", cniPod.GetNamespace(), "netns", netns)

	inbound, outbound, appClusters, healthCluster, err := proxy.GenerateListenersFromRegistryPod(cniPod, trustDomain, c.meshDomain, c.emitStatsPod)
	if err != nil {
		return err
	}

	c.listenerMu.Lock()
	if c.listeners == nil {
		c.listeners = make(map[string]listenerEntry)
	}
	c.listeners[netns] = listenerEntry{
		inbound:       inbound,
		outbound:      outbound,
		appClusters:   clustersToResources(appClusters),
		healthCluster: healthCluster,
	}
	c.listenerMu.Unlock()

	c.setLocalWorkload(netns, proxy.SpiffeIDFromPod(cniPod, trustDomain), trustDomain)

	// Contribute the pod's own service and declared upstreams to the node
	// dependency set; a change signals the refresher to rebuild the scoped
	// cluster snapshot (the pod's upstream clusters must be distributed
	// before its first request — the declared warm path).
	c.setPodDependencies(netns, cniPod)

	return c.generateListenerSnapshot(ctx)
}

// RemovePod removes the inbound and outbound listeners for the pod with the
// given container network namespace, then regenerates the listener snapshot.
// If the pod does not exist in the cache, it returns nil without error.
// Returns an error if snapshot generation fails.
func (c *SnapshotCache) RemovePod(ctx context.Context, netns string) error {
	c.listenerMu.Lock()
	_, exists := c.listeners[netns]
	if exists {
		delete(c.listeners, netns)
	}
	c.listenerMu.Unlock()

	if !exists {
		return nil
	}

	c.removeLocalWorkload(netns)

	// Shrink the node dependency set; clusters only this pod depended on are
	// dropped on the next scoped reload (after the retention grace).
	c.removePodDependencies(netns)

	return c.generateListenerSnapshot(ctx)
}

// Listeners returns all cached inbound and outbound listener resources plus
// the health gateway listener (per-pod health_check filters over the
// health_<pod> clusters, probed by the liveness loop) as a flat slice.
// Thread-safe.
func (c *SnapshotCache) Listeners() []types.Resource {
	c.listenerMu.RLock()
	defer c.listenerMu.RUnlock()

	resources := make([]types.Resource, 0, 2*len(c.listeners)+1)
	probeClusters := make([]string, 0, len(c.listeners))
	for _, entry := range c.listeners {
		resources = append(resources, entry.inbound, entry.outbound)
		if hc, ok := entry.healthCluster.(*clusterv3.Cluster); ok && hc != nil {
			probeClusters = append(probeClusters, hc.GetName())
		}
	}
	resources = append(resources, proxy.BuildHealthGatewayListener(agentconstants.DefaultProxyHealthSocketPath, probeClusters))
	return resources
}

// appClusters returns the per-pod application clusters (one per managed pod)
// as a resource slice. These STATIC clusters forward decrypted inbound traffic
// to each pod's own application on loopback. They are kept alongside listeners
// rather than in the registry-driven cluster map so registry reloads never drop
// them. Thread-safe.
func (c *SnapshotCache) appClusters() []types.Resource {
	c.listenerMu.RLock()
	defer c.listenerMu.RUnlock()

	resources := make([]types.Resource, 0, 2*len(c.listeners))
	for _, entry := range c.listeners {
		resources = append(resources, entry.appClusters...)
		if entry.healthCluster != nil {
			resources = append(resources, entry.healthCluster)
		}
	}
	return resources
}

// clustersToResources converts a slice of concrete app clusters to the
// resource slice stored in a listenerEntry.
func clustersToResources(clusters []*clusterv3.Cluster) []types.Resource {
	resources := make([]types.Resource, 0, len(clusters))
	for _, c := range clusters {
		resources = append(resources, c)
	}
	return resources
}

// LoadListenersFromStorage retrieves all pods from the given storage backend,
// generates the inbound and outbound Envoy listeners and per-pod clusters for each
// pod, and populates the cache keyed by container network namespace. After populating
// the cache, it generates and sets a new listener snapshot.
//
// The trustDomain is the SPIFFE trust domain used for SDS secret naming.
//
// If listener generation fails for any pod, it logs the error and continues
// processing other pods. Returns an error with all accumulated errors if at
// least one pod failed, or if snapshot generation fails.
func (c *SnapshotCache) LoadListenersFromStorage(ctx context.Context, store storage.Storage[*cniv1.CNIPod], trustDomain string) error {
	c.log.DebugContext(ctx, "generating listeners")

	pods, err := store.GetAll(ctx)
	if err != nil {
		return err
	}
	c.log.DebugContext(ctx, "found pods in local storage", "count", len(pods))

	var errs []error
	local := make(map[string]string, len(pods))

	c.listenerMu.Lock()
	for _, pod := range pods {
		netns := pod.GetNetworkNamespace()
		c.log.DebugContext(ctx, "generating listeners for pod", "pod", pod.GetName(), "namespace", pod.GetNamespace(), "netns", netns)

		inbound, outbound, appClusters, healthCluster, listenerErr := proxy.GenerateListenersFromRegistryPod(pod, trustDomain, c.meshDomain, c.emitStatsPod)
		if listenerErr != nil {
			c.log.ErrorContext(ctx, "failed to generate listeners for pod", "error", listenerErr, "pod", pod.GetName(), "namespace", pod.GetNamespace())
			errs = append(errs, listenerErr)
			continue
		}

		c.listeners[netns] = listenerEntry{
			inbound:       inbound,
			outbound:      outbound,
			appClusters:   clustersToResources(appClusters),
			healthCluster: healthCluster,
		}
		local[netns] = proxy.SpiffeIDFromPod(pod, trustDomain)
		// Contribute to the node dependency set so the scoped registry load
		// that follows (PreListen) carries these pods' upstreams.
		c.setPodDependencies(netns, pod)
	}
	c.listenerMu.Unlock()

	// Merge (never replace) into localWorkloads: this load runs concurrently with
	// the CNI server, and a wholesale replacement would wipe the netns→SPIFFE-ID
	// mapping of a pod whose AddPod landed between the storage GetAll above and
	// this write — silently downgrading that pod's outbound mTLS to the node
	// certificate (the matcher's no-match path). At startup the map is empty, so
	// merge and replace are otherwise equivalent.
	c.localMu.Lock()
	for netns, id := range local {
		c.localWorkloads[netns] = id
	}
	c.trustDomain = trustDomain
	c.localMu.Unlock()

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	listeners := c.Listeners()
	c.log.DebugContext(ctx, "generated listeners", "count", len(listeners))

	return c.generateListenerSnapshot(ctx)
}

// generateListenerSnapshot regenerates the node snapshot after a listener change.
// It delegates to generateSnapshot, which emits a complete snapshot of all
// resource types so listener updates do not clobber clusters, routes or secrets.
func (c *SnapshotCache) generateListenerSnapshot(ctx context.Context) error {
	return c.generateSnapshot(ctx)
}
