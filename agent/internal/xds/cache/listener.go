package cache

import (
	"context"
	"errors"
	"fmt"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
)

// AddPod generates inbound and outbound listeners for the given pod and adds
// them to the cache keyed by the pod's network namespace, then regenerates the
// listener snapshot. Returns an error if listener generation or snapshot
// generation fails.
func (c *SnapshotCache) AddPod(ctx context.Context, cniPod *cniv1.CNIPod, trustDomain string) error {
	netns := cniPod.GetNetworkNamespace()
	c.log.V(2).Info("adding listeners for pod", "pod", cniPod.GetName(), "namespace", cniPod.GetNamespace(), "netns", netns)

	inbound, outbound, appCluster, healthCluster, err := proxy.GenerateListenersFromRegistryPod(cniPod, trustDomain)
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
		appCluster:    appCluster,
		healthCluster: healthCluster,
		pod:           cniPod,
	}
	c.listenerMu.Unlock()

	c.setLocalWorkload(netns, proxy.SpiffeIDFromPod(cniPod, trustDomain), trustDomain)

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

	return c.generateListenerSnapshot(ctx)
}

// Listeners returns all cached listener resources (both inbound and outbound)
// as a flat slice. Thread-safe.
func (c *SnapshotCache) Listeners() []types.Resource {
	c.listenerMu.RLock()
	defer c.listenerMu.RUnlock()

	resources := make([]types.Resource, 0, 2*len(c.listeners))
	for _, entry := range c.listeners {
		resources = append(resources, entry.inbound, entry.outbound)
	}
	return resources
}

// nodeConnectResources builds the node-level tunnel ingress from the current local
// pods and the node identity: the CONNECT-terminating listener, one inner HCM
// listener per local pod (rebuilds XFCC and forwards to app_<pod>), and the
// internal_upstream clusters wiring them. It returns nil slices when no node SVID
// has been served yet (the CONNECT listener references it as its server
// certificate). Building from the cached pods keeps the R2 tunnel ingress in sync
// with pod adds and removes without disturbing the per-pod R1 inbound listeners.
// Thread-safe.
func (c *SnapshotCache) nodeConnectResources() (listeners []types.Resource, clusters []types.Resource) {
	c.localMu.RLock()
	nodeSpiffeID := c.nodeSpiffeID
	trustDomain := c.trustDomain
	c.localMu.RUnlock()

	if nodeSpiffeID == "" {
		return nil, nil
	}

	c.listenerMu.RLock()
	pods := make([]*cniv1.CNIPod, 0, len(c.listeners))
	for _, entry := range c.listeners {
		if entry.pod != nil {
			pods = append(pods, entry.pod)
		}
	}
	c.listenerMu.RUnlock()

	validationContextName := fmt.Sprintf("spiffe://%s", trustDomain)
	res := proxy.GenerateNodeConnectResources(pods, nodeSpiffeID, validationContextName)
	if res == nil {
		return nil, nil
	}
	for _, l := range res.Listeners {
		listeners = append(listeners, l)
	}
	for _, cl := range res.Clusters {
		clusters = append(clusters, cl)
	}
	return listeners, clusters
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
		if entry.appCluster != nil {
			resources = append(resources, entry.appCluster)
		}
		if entry.healthCluster != nil {
			resources = append(resources, entry.healthCluster)
		}
	}
	return resources
}

// LoadListenersFromStorage retrieves all pods from the given storage backend,
// generates inbound and outbound Envoy listeners for each pod, and populates
// the cache keyed by container network namespace. After populating the cache,
// it generates and sets a new listener snapshot.
//
// The trustDomain is the SPIFFE trust domain used for SDS secret naming.
//
// If listener generation fails for any pod, it logs the error and continues
// processing other pods. Returns an error with all accumulated errors if at
// least one pod failed, or if snapshot generation fails.
func (c *SnapshotCache) LoadListenersFromStorage(ctx context.Context, store storage.Storage[*cniv1.CNIPod], trustDomain string) error {
	c.log.V(2).Info("generating listeners")

	pods, err := store.GetAll(ctx)
	if err != nil {
		return err
	}
	c.log.V(1).Info("found pods in local storage", "count", len(pods))

	var errs []error
	local := make(map[string]string, len(pods))

	c.listenerMu.Lock()
	for _, pod := range pods {
		netns := pod.GetNetworkNamespace()
		c.log.V(2).Info("generating listeners for pod", "pod", pod.GetName(), "namespace", pod.GetNamespace(), "netns", netns)

		inbound, outbound, appCluster, healthCluster, listenerErr := proxy.GenerateListenersFromRegistryPod(pod, trustDomain)
		if listenerErr != nil {
			c.log.V(1).Error(listenerErr, "failed to generate listeners for pod", "pod", pod.GetName(), "namespace", pod.GetNamespace())
			errs = append(errs, listenerErr)
			continue
		}

		c.listeners[netns] = listenerEntry{
			inbound:       inbound,
			outbound:      outbound,
			appCluster:    appCluster,
			healthCluster: healthCluster,
			pod:           pod,
		}
		local[netns] = proxy.SpiffeIDFromPod(pod, trustDomain)
	}
	c.listenerMu.Unlock()

	c.localMu.Lock()
	c.localWorkloads = local
	c.trustDomain = trustDomain
	c.localMu.Unlock()

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	listeners := c.Listeners()
	c.log.V(1).Info("generated listeners", "count", len(listeners))

	return c.generateListenerSnapshot(ctx)
}

// generateListenerSnapshot regenerates the node snapshot after a listener change.
// It delegates to generateSnapshot, which emits a complete snapshot of all
// resource types so listener updates do not clobber clusters, routes or secrets.
func (c *SnapshotCache) generateListenerSnapshot(ctx context.Context) error {
	return c.generateSnapshot(ctx)
}
