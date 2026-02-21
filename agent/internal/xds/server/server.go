package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry"
	"github.com/bpalermo/aether/xds"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/go-logr/logr"
	"go.uber.org/atomic"
)

type AgentXdsServer struct {
	xds.XdsServer

	log logr.Logger

	clusterName string
	nodeName    string

	storage  storage.Storage[*cniv1.CNIPod]
	registry registry.Registry

	cache cachev3.SnapshotCache

	version *atomic.Uint64
}

func NewXdsServer(ctx context.Context, clusterName string, nodeName string, registry registry.Registry, storage storage.Storage[*cniv1.CNIPod], log logr.Logger) (*AgentXdsServer, error) {
	cfg := xds.NewServerConfig(
		xds.WithUDS(constants.DefaultXdsSocketPath),
	)

	cache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)

	aXdsServer := &AgentXdsServer{
		XdsServer:   xds.NewXdsServer(ctx, cfg, cache, nil, log),
		log:         log.WithName("agent-xds"),
		clusterName: clusterName,
		nodeName:    nodeName,
		registry:    registry,
		storage:     storage,
		cache:       cache,
		version:     atomic.NewUint64(0),
	}

	aXdsServer.AddCallback(aXdsServer)

	return aXdsServer, nil
}

func (s *AgentXdsServer) PreListen(ctx context.Context) error {
	s.log.V(1).Info("generating initial snapshot")

	listeners, err := generateListeners(ctx, s.storage, s.log)
	if err != nil {
		return err
	}
	s.log.V(1).Info("generated listeners", "count", len(listeners))

	clusters, endpoints, err := generateClustersAndEndpoints(ctx, s.clusterName, s.nodeName, s.registry, s.log)
	if err != nil {
		return err
	}
	s.log.V(1).Info("generated clusters and endpoints", "count", len(clusters), "endpoints", len(endpoints))

	routes, err := generateRoutes(clusters, s.log)
	if err != nil {
		return err
	}
	s.log.V(1).Info("generated routes", "count", len(routes))

	v := s.generateSnapshotVersion()
	snapshot, err := cachev3.NewSnapshot(v, map[resource.Type][]types.Resource{
		resource.ListenerType:    listeners,
		resource.EndpointType:    endpoints,
		resource.ClusterType:     clusters,
		resource.RouteType:       routes,
		resource.VirtualHostType: make([]types.Resource, 0),
	})
	if err != nil {
		return err
	}

	s.log.V(1).Info("setting snapshot", "node", s.nodeName, "version", v)
	if err = s.cache.SetSnapshot(ctx, s.nodeName, snapshot); err != nil {
		return err
	}

	return nil
}

func (s *AgentXdsServer) generateSnapshotVersion() string {
	timestamp := time.Now().UnixMilli()
	version := s.version.Add(1)
	return fmt.Sprintf("%d.%d", timestamp, version)
}

func generateListeners(ctx context.Context, storage storage.Storage[*cniv1.CNIPod], log logr.Logger) ([]types.Resource, error) {
	log.V(2).Info("generating listeners")

	pods, err := storage.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	log.V(1).Info("found pods in local storage", "count", len(pods))

	// we assume each pod will have 2 HTTP listeners,
	// one for inbound and one for outbound
	var errs []error
	listeners := make([]types.Resource, 0, 2*len(pods))
	for _, pod := range pods {
		log.V(2).Info("generating listeners for pod", "pod", pod.GetName(), "namespace", pod.GetNamespace())
		inbound, outbound, listenerErr := proxy.GenerateListenersFromRegistryPod(pod)
		if listenerErr != nil {
			log.V(1).Error(listenerErr, "failed to generate listeners for pod", "pod", pod.GetName(), "namespace", pod.GetNamespace())
			errs = append(errs, listenerErr)
		}

		listeners = append(listeners, inbound, outbound)
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return listeners, nil
}

func generateClustersAndEndpoints(ctx context.Context, clusterName string, nodeName string, registry registry.Registry, log logr.Logger) ([]types.Resource, []types.Resource, error) {
	log.V(2).Info("generating endpoints")

	serviceEndpoints, err := registry.ListAllEndpoints(ctx, registryv1.Service_HTTP)
	if err != nil {
		return nil, nil, err
	}
	log.V(1).Info("found service endpoints in registry", "count", len(serviceEndpoints))

	clusters := make([]types.Resource, 0, len(serviceEndpoints))
	clas := make([]types.Resource, len(serviceEndpoints))
	for serviceName, endpoints := range serviceEndpoints {
		clusters = append(clusters, proxy.NewClusterForService(serviceName))
		cla := proxy.NewClusterLoadAssignment(serviceName)

		var localCluster *clusterv3.Cluster
		var localCla *endpointv3.ClusterLoadAssignment
		for _, endpoint := range endpoints {
			cla.Endpoints = append(cla.Endpoints, proxy.LocalityLbEndpointFromRegistryEndpoint(endpoint))

			if isLocal(clusterName, nodeName, endpoint) {
				localClusterName := fmt.Sprintf("local_%s", serviceName)

				if localCluster == nil {
					localCluster = proxy.NewLocalClusterForService(localClusterName, endpoint)
					clusters = append(clusters, localCluster)
				}

				if localCla == nil {
					localCla = proxy.NewClusterLoadAssignment(localClusterName)
					clas = append(clas, localCla)
				}

				localCla.Endpoints = append(localCla.Endpoints, proxy.LocalLocalityLbEndpointFromRegistryEndpoint(endpoint))
			}
		}

		clas = append(clas, cla)
	}

	return clusters, clas, nil
}

func generateRoutes(clusters []types.Resource, log logr.Logger) ([]types.Resource, error) {
	var errs []error

	log.V(2).Info("generating routes")

	vhosts := make([]*routev3.VirtualHost, 0, len(clusters))
	for _, res := range clusters {
		cluster, ok := res.(*clusterv3.Cluster)
		if !ok {
			log.Error(nil, "invalid resource type", "type", fmt.Sprintf("%T", res))
			errs = append(errs, fmt.Errorf("invalid resource type: %T", res))
			continue
		}
		vhosts = append(vhosts, proxy.BuildOutboundClusterVirtualHost(cluster.GetName()))
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	outRoute := proxy.BuildOutboundRouteConfiguration(vhosts)

	return []types.Resource{outRoute}, nil
}

func isLocal(clusterName string, nodeName string, endpoint *registryv1.ServiceEndpoint) bool {
	return clusterName == endpoint.GetClusterName() && nodeName == endpoint.GetKubernetesMetadata().GetNodeName()
}
