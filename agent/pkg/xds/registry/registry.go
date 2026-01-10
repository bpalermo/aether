package registry

import (
	"context"
	"fmt"
	"sync"

	"github.com/bpalermo/aether/agent/pkg/xds/registry/cache"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/go-logr/logr"
)

const (
	eventBuffer = 32
)

type XdsRegistry struct {
	log logr.Logger

	mu            sync.RWMutex
	clusterCache  *cache.ClusterCache
	listenerCache *cache.ListenerCache
	endpointCache *cache.EndpointCache
	routeCache    *cache.RouteCache

	eventChan chan *registryv1.Event

	nodeID string

	snapshot cachev3.SnapshotCache
	version  uint64
}

func NewXdsRegistry(nodeID string, log logr.Logger) *XdsRegistry {
	return &XdsRegistry{
		log.WithName("registry"),
		sync.RWMutex{},
		cache.NewClusterCache(),
		cache.NewListenerCache(),
		cache.NewEndpointCache(),
		cache.NewRouteCache(),
		make(chan *registryv1.Event, eventBuffer),
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
	r.log.Info("processing event", "operation", event.Operation)

	// Switch on the resource type
	switch res := event.GetResource().(type) {
	case *registryv1.Event_K8SPod:
		return r.processPodEvent(ctx, event.Operation, res.K8SPod)
	case *registryv1.Event_CniPod:
		return r.processCNIPod(ctx, event.Operation, res.CniPod)
	default:
		r.log.Info("unknown resource type", "event", event)
		return nil
	}
}

func (r *XdsRegistry) processPodEvent(ctx context.Context, op registryv1.Event_Operation, event *registryv1.KubernetesPod) error {
	r.log.Info("processing pod event",
		"operation", op.String(),
		"name", event.GetName(),
		"namespace", event.GetNamespace(),
		"serviceName", event.GetServiceName(),
	)

	switch op {
	case registryv1.Event_CREATED, registryv1.Event_UPDATED:
		// order here matters
		r.clusterCache.AddClusterOrUpdate(event)
		r.endpointCache.AddEndpoint(cache.ClusterName(event.ServiceName), event)
		r.routeCache.AddOutboundVirtualHost(event.ServiceName)
	case registryv1.Event_DELETED:
		r.clusterCache.RemoveCluster(event.ServiceName)
		r.endpointCache.RemoveEndpoint(event)
		r.routeCache.RemoveOutboundVirtualHost(event.ServiceName)
	}

	return r.generateSnapshot(ctx)
}

func (r *XdsRegistry) processCNIPod(ctx context.Context, op registryv1.Event_Operation, event *registryv1.CNIPod) error {
	r.log.Info("processing network namespace event", "operation", op.String(), "path", event.NetworkNamespace)

	switch op {
	case registryv1.Event_CREATED, registryv1.Event_UPDATED:
		r.listenerCache.AddListeners(event)
	case registryv1.Event_DELETED:
		r.listenerCache.RemoveListeners(event.NetworkNamespace)
	}

	return r.generateSnapshot(ctx)
}

func (r *XdsRegistry) generateSnapshot(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	resources := map[resource.Type][]types.Resource{
		resource.EndpointType: r.endpointCache.GetAllEndpoints(),
		resource.ClusterType:  r.clusterCache.GetAllClusters(),
		resource.RouteType:    r.routeCache.GetAllRouteConfiguration(),
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

func (r *XdsRegistry) GetSnapshot() cachev3.SnapshotCache {
	return r.snapshot
}

func (r *XdsRegistry) GetEventChan() chan<- *registryv1.Event {
	return r.eventChan
}
