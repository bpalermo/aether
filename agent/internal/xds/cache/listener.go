package cache

import (
	"context"
	"errors"
	"fmt"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/agent/pkg/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

const (
	// listenerVersionLabel is used in snapshot version strings to identify
	// listener resource snapshots.
	listenerVersionLabel = "listener"
)

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

	c.listenerMu.Lock()
	for _, pod := range pods {
		netns := pod.GetNetworkNamespace()
		c.log.V(2).Info("generating listeners for pod", "pod", pod.GetName(), "namespace", pod.GetNamespace(), "netns", netns)

		inbound, outbound, listenerErr := proxy.GenerateListenersFromRegistryPod(pod, trustDomain)
		if listenerErr != nil {
			c.log.V(1).Error(listenerErr, "failed to generate listeners for pod", "pod", pod.GetName(), "namespace", pod.GetNamespace())
			errs = append(errs, listenerErr)
			continue
		}

		c.listeners[netns] = listenerEntry{
			inbound:  inbound,
			outbound: outbound,
		}
	}
	c.listenerMu.Unlock()

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	listeners := c.Listeners()
	c.log.V(1).Info("generated listeners", "count", len(listeners))

	return c.generateListenerSnapshot(ctx)
}

// generateListenerSnapshot creates a new snapshot with the current listeners,
// validates it for consistency, and sets it on the underlying snapshot cache.
// The snapshot version is generated using the listener version label. Returns an error
// if snapshot creation or validation fails.
func (c *SnapshotCache) generateListenerSnapshot(ctx context.Context) error {
	v := generateSnapshotVersion(listenerVersionLabel, c.version)

	listeners := c.Listeners()
	c.log.V(1).Info("setting snapshot", "version", v, "listeners", len(listeners))

	snapshot, err := cachev3.NewSnapshot(v, map[resourcev3.Type][]types.Resource{
		resourcev3.ListenerType: listeners,
	})
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	if err := snapshot.Consistent(); err != nil {
		return fmt.Errorf("snapshot inconsistency: %w", err)
	}

	if err := c.SetSnapshot(ctx, c.nodeName, snapshot); err != nil {
		return fmt.Errorf("failed to set snapshot: %w", err)
	}

	return nil
}
