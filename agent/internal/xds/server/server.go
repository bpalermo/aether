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
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/go-logr/logr"
	"go.uber.org/atomic"
)

type AgentXdsServer struct {
	xds.XdsServer

	log logr.Logger

	nodeID string

	storage  storage.Storage[*cniv1.CNIPod]
	registry registry.Registry

	cache cachev3.SnapshotCache

	version *atomic.Uint64
}

func NewXdsServer(ctx context.Context, nodeID string, registry registry.Registry, storage storage.Storage[*cniv1.CNIPod], log logr.Logger) (*AgentXdsServer, error) {
	cfg := xds.NewServerConfig(
		xds.WithUDS(constants.DefaultXdsSocketPath),
	)

	cache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)

	aXdsServer := &AgentXdsServer{
		XdsServer: xds.NewXdsServer(ctx, cfg, cache, nil, log),
		log:       log.WithName("agent-xds"),
		nodeID:    nodeID,
		registry:  registry,
		storage:   storage,
		cache:     cache,
		version:   atomic.NewUint64(0),
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

	clusters, endpoints, err := generateClustersAndEndpoints(ctx, s.registry, s.log)
	if err != nil {
		return err
	}
	s.log.V(1).Info("generated clusters and endpoints", "count", len(clusters), "endpoints", len(endpoints))

	v := s.generateSnapshotVersion()
	snapshot, err := cachev3.NewSnapshot(v, map[resource.Type][]types.Resource{
		resource.ListenerType:    listeners,
		resource.EndpointType:    endpoints,
		resource.ClusterType:     clusters,
		resource.RouteType:       make([]types.Resource, 0),
		resource.VirtualHostType: make([]types.Resource, 0),
	})
	if err != nil {
		return err
	}

	s.log.V(1).Info("setting snapshot", "node", s.nodeID, "version", v)
	if err = s.cache.SetSnapshot(ctx, s.nodeID, snapshot); err != nil {
		return err
	}

	return nil
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

func generateClustersAndEndpoints(ctx context.Context, registry registry.Registry, log logr.Logger) ([]types.Resource, []types.Resource, error) {
	log.V(2).Info("generating endpoints")

	serviceEndpoints, err := registry.ListAllEndpoints(ctx, registryv1.Service_HTTP)
	if err != nil {
		return nil, nil, err
	}
	log.V(1).Info("found service endpoints in registry", "count", len(serviceEndpoints))

	clusters := make([]types.Resource, 0, len(serviceEndpoints))
	clas := make([]types.Resource, len(serviceEndpoints))
	for clusterName, endpoints := range serviceEndpoints {
		cluster := proxy.NewClusterForService(clusterName)
		cla := proxy.NewClusterLoadAssignment(clusterName)

		for _, endpoint := range endpoints {
			cla.Endpoints = append(cla.Endpoints, proxy.LocalityLbEndpointFromRegistryEndpoint(endpoint))
		}

		clusters = append(clusters, cluster)
	}

	return clusters, clas, nil
}

func (s *AgentXdsServer) generateSnapshotVersion() string {
	timestamp := time.Now().UnixMilli()
	version := s.version.Add(1)
	return fmt.Sprintf("%d.%d", timestamp, version)
}
