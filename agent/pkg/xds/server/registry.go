package server

import (
	"context"
	"fmt"
	"sync"

	"github.com/bpalermo/aether/agent/pkg/xds/proxy"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/go-logr/logr"
)

type XdsRegistry struct {
	log logr.Logger

	mu            sync.RWMutex
	clusterCache  *ClusterCache
	listenerCache *ListenerCache
	endpointCache *EndpointCache

	eventChan chan *registryv1.Event

	nodeID string

	snapshot cachev3.SnapshotCache
	version  uint64
}

func NewXdsRegistry(nodeID string, log logr.Logger) *XdsRegistry {
	return &XdsRegistry{
		log.WithName("registry"),
		sync.RWMutex{},
		NewClusterCache(),
		NewListenerCache(),
		NewEndpointCache(),
		make(chan *registryv1.Event),
		nodeID,
		cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil),
		0,
	}
}

func (r *XdsRegistry) Start(ctx context.Context) error {
	for {
		select {
		case event := <-r.eventChan:
			if event == nil {
				continue
			}
			// Process the event
			if err := r.processEvent(ctx, event); err != nil {
				r.log.Error(err, "failed to process event", "event", event)
				continue
			}
		case <-ctx.Done():
			// Context canceled, cleanup and exit
			close(r.eventChan)
			return ctx.Err()
		}
	}
}

func (r *XdsRegistry) processEvent(ctx context.Context, event *registryv1.Event) error {
	r.log.Info("processing event", "type", event.GetResource())

	// Switch on the resource type
	switch res := event.GetResource().(type) {
	case *registryv1.Event_Pod_:
		return r.processPodEvent(ctx, event.Operation, res.Pod)
	case *registryv1.Event_NetworkNs:
		return r.processNetworkNs(ctx, event.Operation, res.NetworkNs)
	default:
		r.log.V(1).Info("unknown resource type", "event", event)
		return nil
	}
}

func (r *XdsRegistry) processPodEvent(ctx context.Context, op registryv1.Event_Operation, event *registryv1.Event_Pod) error {
	r.log.Info("processing pod event",
		"operation", op.String(),
		"name", event.GetName(),
		"namespace", event.GetNamespace(),
		"serviceName", event.GetServiceName(),
	)

	switch op {
	case registryv1.Event_CREATED, registryv1.Event_UPDATED:
		r.endpointCache.AddEndpoint(ClusterName(event.GetServiceName()), event)
		r.clusterCache.AddCluster(proxy.NewCluster(event))
	case registryv1.Event_DELETED:
		r.endpointCache.RemoveEndpoint(ClusterName(event.GetServiceName()), PodName(event.GetName()))
		r.clusterCache.RemoveCluster(event.GetServiceName())
	}

	return r.generateSnapshot(ctx)
}

func (r *XdsRegistry) processNetworkNs(ctx context.Context, op registryv1.Event_Operation, event *registryv1.Event_NetworkNamespace) error {
	r.log.Info("processing network namespace event", "operation", op.String(), "path", event.GetPath())

	switch op {
	case registryv1.Event_CREATED, registryv1.Event_UPDATED:
		// Convert types.Resource to []*listenerv3.Listener
		resources := proxy.GenerateListenersFromEvent(event)
		listeners := make([]*listenerv3.Listener, 0, len(resources))
		for _, res := range resources {
			if listener, ok := res.(*listenerv3.Listener); ok {
				listeners = append(listeners, listener)
			}
		}
		r.listenerCache.AddListeners(event.GetPath(), listeners)
	case registryv1.Event_DELETED:
		r.listenerCache.RemoveListeners(event.GetPath())
	}

	return r.generateSnapshot(ctx)
}

func (r *XdsRegistry) generateSnapshot(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	resources := map[resource.Type][]types.Resource{
		resource.EndpointType: r.generateEndpoints(),
		resource.ClusterType:  r.clusterCache.GetAllClusters(),
		resource.RouteType:    r.generateRoutes(),
		resource.ListenerType: r.listenerCache.GetAllListeners(),
	}

	snapshot, err := cachev3.NewSnapshot(fmt.Sprintf("%d", r.version), resources)
	if err != nil {
		return err
	}

	r.log.Info(
		"generated snapshot",
		"version", r.version,
		"clusters", len(resources[resource.ClusterType]),
		"listeners", len(resources[resource.ListenerType]),
		"endpoints", len(resources[resource.EndpointType]),
		"routes", len(resources[resource.RouteType]),
	)
	if err = r.snapshot.SetSnapshot(ctx, r.nodeID, snapshot); err != nil {
		return err
	}

	r.version = r.version + 1

	return nil
}

func (r *XdsRegistry) generateEndpoints() []types.Resource {
	var clas []types.Resource

	r.endpointCache.mu.RLock()
	defer r.endpointCache.mu.RUnlock()

	// Collect all ClusterLoadAssignments from the cache
	for _, cluster := range r.endpointCache.clusters {
		// Get the CLA, rebuilding if necessary
		if cluster.dirty {
			r.endpointCache.rebuildCLA(cluster)
			cluster.dirty = false
		}

		if cluster.cla != nil {
			clas = append(clas, cluster.cla)
		}
	}

	return clas
}

func (r *XdsRegistry) generateRoutes() []types.Resource {
	return make([]types.Resource, 0)
}

func (r *XdsRegistry) GetEventChan() chan<- *registryv1.Event {
	return r.eventChan
}
