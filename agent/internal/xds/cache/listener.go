package cache

import (
	"context"
	"errors"
	"io/fs"
	"os"

	agentconstants "github.com/bpalermo/aether/agent/constants"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
)

// netnsExists reports whether a pod's network-namespace path is still present.
// Overridable in tests (which use synthetic netns paths). A pod whose netns is
// gone is excluded from listener generation — see LoadListenersFromStorage.
var netnsExists = func(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// AddPod generates the inbound and outbound listeners and per-pod clusters for the
// given pod and adds them to the cache keyed by the pod's network namespace, then
// regenerates the listener snapshot. Returns an error if listener generation or
// snapshot generation fails.
func (c *SnapshotCache) AddPod(ctx context.Context, cniPod *cniv1.CNIPod, trustDomain string) error {
	netns := cniPod.GetNetworkNamespace()
	c.log.DebugContext(ctx, "adding listeners for pod", "pod", cniPod.GetName(), "namespace", cniPod.GetNamespace(), "netns", netns)

	inbound, outbound, appClusters, healthCluster, err := proxy.GenerateListenersFromRegistryPod(cniPod, trustDomain, c.meshDomain, c.emitStatsPod, !c.spireEnabled)
	if err != nil {
		return err
	}
	capture, err := c.generateCaptureListener(cniPod)
	if err != nil {
		return err
	}
	udpCapture, err := c.generateUDPCaptureListener(cniPod)
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
		capture:       capture,
		udpCapture:    udpCapture,
		cniPod:        cniPod,
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
	// Edge mode: public-facing listener(s) (no per-pod inbound/outbound
	// listeners, no health gateway — the edge runs no local workloads). The edge
	// serves:
	//   - HTTP listener (plain or TLS-terminating) from HTTPRoutes
	//   - TCP listener(s) for each Gateway TCP port from TCPRoutes
	//   - TLS passthrough listener(s) for each Gateway TLS port from TLSRoutes
	if c.edge {
		var resources []types.Resource

		// Always emit the dedicated readiness listener so the kubelet probe has a
		// stable target independent of which public listeners are bound. Under
		// Phase 2 the public listeners move to per-Gateway internal ports and
		// nothing binds the HTTPS port — probing it there fails and wedges the roll.
		readinessPort := c.edgeReadinessPort
		if readinessPort == 0 {
			readinessPort = proxy.DefaultEdgeReadinessPort
		}
		resources = append(resources, proxy.BuildEdgeReadinessListener(readinessPort))

		if c.hasPerGatewayAddressing() {
			// Proposal 021 Phase 2: per-Gateway listeners, each with a unique name
			// (edge_gw_<ns>_<gwname>_<internalPort>). No shared edge_http/edge_https
			// listeners — every Gateway gets its own listener bound on its allocated
			// internal port. L4 (TCP/TLS passthrough) listeners are still shared.
			resources = append(resources, c.edgeGatewayListeners()...)
		} else {
			// Phase 1 / fallback: shared edge_http / edge_https / edge_redirect
			// listeners on the configured global ports.
			c.edgeHTTPRedirectMu.RLock()
			httpRedirect := c.edgeHTTPRedirect
			c.edgeHTTPRedirectMu.RUnlock()

			if c.edgeTLSEnabled {
				// TLS listener on httpsPort, under a name DISTINCT from the :80 listener so
				// the two never collide in the snapshot/LDS (a shared name dropped :443).
				resources = append(resources,
					proxy.BuildEdgeListener(proxy.EdgeHTTPSListenerName, c.edgeHTTPSPort, c.edgeTLSSecretNames()),
				)
			}
			if httpRedirect {
				// At least one Gateway opted into HTTP→HTTPS redirect: emit the redirect
				// listener on the plain HTTP port (replaces a routing listener for that port).
				resources = append(resources, proxy.BuildEdgeRedirectListener(c.edgeHTTPPort))
			} else {
				// Default: the HTTP-port listener serves its attached HTTPRoutes directly.
				resources = append(resources, proxy.BuildEdgeListener(proxy.EdgeListenerName, c.edgeHTTPPort, nil))
			}
		}
		resources = append(resources, c.edgeTCPListeners()...)
		return resources
	}

	c.listenerMu.RLock()
	defer c.listenerMu.RUnlock()

	resources := make([]types.Resource, 0, 2*len(c.listeners)+1)
	probeClusters := make([]string, 0, len(c.listeners))
	for _, entry := range c.listeners {
		// appendListener filters out any nil / typed-nil / malformed listener
		// (empty Name AND no Address) so a single bad per-pod resource cannot make
		// Envoy NACK the entire LDS push ("address is necessary"), which would drop
		// every good listener in the same delta and wedge a pod added in the window.
		resources = appendListener(resources, entry.inbound)
		resources = appendListener(resources, entry.outbound)
		resources = appendListener(resources, entry.capture)
		resources = appendListener(resources, entry.udpCapture)
		if hc, ok := entry.healthCluster.(*clusterv3.Cluster); ok && hc != nil {
			probeClusters = append(probeClusters, hc.GetName())
		}
	}
	resources = append(resources, proxy.BuildHealthGatewayListener(agentconstants.DefaultProxyHealthSocketPath, probeClusters))
	return resources
}

// appendListener appends r to resources only when it is a usable Listener.
//
// It rejects three failure modes that would otherwise poison the LDS push:
//   - a nil interface (entry never populated);
//   - a *typed*-nil *listener.v3.Listener wrapped in a non-nil types.Resource
//     (the historical bug: generateUDPCaptureListener returned the nil pointer
//     directly when there were no UDPRoute backends, so the `!= nil` guard passed);
//   - a malformed listener with no Name AND no Address. go-control-plane assigns a
//     random UUID name to any nameless Listener, and Envoy then rejects it with
//     "error adding listener named '<UUID>': address is necessary" — NACK'ing the
//     whole delta and dropping the good listeners alongside it.
//
// A listener that has a Name but somehow lacks an Address is still kept here (it is
// not the typed-nil pathology); the generators always set both, so this only ever
// drops genuinely empty resources.
func appendListener(resources []types.Resource, r types.Resource) []types.Resource {
	if r == nil {
		return resources
	}
	l, ok := r.(*listenerv3.Listener)
	if !ok {
		// Not a Listener (should not happen for listener entries); keep it rather
		// than silently dropping an unexpected-but-valid resource.
		return append(resources, r)
	}
	if l == nil {
		// Typed-nil pointer wrapped in a non-nil interface.
		return resources
	}
	if l.GetName() == "" && l.GetAddress() == nil {
		// Nameless + addressless: the exact shape Envoy rejects with
		// "address is necessary". Drop it so the rest of the push survives.
		return resources
	}
	return append(resources, l)
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

		// Skip a pod whose network namespace no longer exists: a missed CNI DEL
		// left the storage entry behind, and programming its per-pod cluster
		// (NetworkNamespaceFilepath) faults Envoy opening the dead netns (talos
		// worker-01, 2026-06-19). The ghost sweep prunes the stale entry; this
		// keeps the bad config out of the snapshot at startup, before that runs.
		if netns != "" && !netnsExists(netns) {
			c.log.WarnContext(ctx, "skipping pod with missing network namespace (stale storage; CNI DEL likely missed)", "pod", pod.GetName(), "namespace", pod.GetNamespace(), "netns", netns)
			continue
		}
		c.log.DebugContext(ctx, "generating listeners for pod", "pod", pod.GetName(), "namespace", pod.GetNamespace(), "netns", netns)

		inbound, outbound, appClusters, healthCluster, listenerErr := proxy.GenerateListenersFromRegistryPod(pod, trustDomain, c.meshDomain, c.emitStatsPod, !c.spireEnabled)
		if listenerErr != nil {
			c.log.ErrorContext(ctx, "failed to generate listeners for pod", "error", listenerErr, "pod", pod.GetName(), "namespace", pod.GetNamespace())
			errs = append(errs, listenerErr)
			continue
		}
		capture, captureErr := c.generateCaptureListener(pod)
		if captureErr != nil {
			c.log.ErrorContext(ctx, "failed to generate capture listener for pod", "error", captureErr, "pod", pod.GetName(), "namespace", pod.GetNamespace())
			errs = append(errs, captureErr)
			continue
		}
		udpCapture, udpCaptureErr := c.generateUDPCaptureListener(pod)
		if udpCaptureErr != nil {
			c.log.ErrorContext(ctx, "failed to generate UDP capture listener for pod", "error", udpCaptureErr, "pod", pod.GetName(), "namespace", pod.GetNamespace())
			errs = append(errs, udpCaptureErr)
			continue
		}
		c.listeners[netns] = listenerEntry{
			inbound:       inbound,
			outbound:      outbound,
			capture:       capture,
			udpCapture:    udpCapture,
			cniPod:        pod,
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
