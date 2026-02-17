package server

import (
	"context"
	"fmt"
	"time"

	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/storage"
	"github.com/bpalermo/aether/agent/pkg/xds/proxy"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
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
	s.Log.V(1).Info("generating initial snapshot")

	listeners, err := s.generateListeners(ctx)
	if err != nil {
		return err
	}

	clusters, endpoints, err := s.generateClustersAndEndpoints(ctx)
	if err != nil {
		return err
	}

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

	s.Log.V(1).Info("setting snapshot", "node", s.nodeID, "version", v)
	if err = s.cache.SetSnapshot(ctx, s.nodeID, snapshot); err != nil {
		return err
	}

	return nil
}

func (s *AgentXdsServer) generateListeners(ctx context.Context) ([]types.Resource, error) {
	s.Log.V(2).Info("generating listeners")

	pods, err := s.storage.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	s.Log.V(2).Info("found pods in registry", "count", len(pods))

	// we assume each pod will have 2 HTTP listeners,
	// one for inbound and one for outbound
	listeners := make([]types.Resource, 2*len(pods))
	for _, pod := range pods {
		s.Log.V(2).Info("generated listeners for pod", "pod", pod.GetName(), "namespace", pod.GetNamespace())
		inbound, outbound := proxy.GenerateListenersFromRegistryPod(pod)
		listeners = append(listeners, inbound, outbound)
	}

	return listeners, nil
}

func (s *AgentXdsServer) generateClustersAndEndpoints(ctx context.Context) ([]types.Resource, []types.Resource, error) {
	s.Log.V(2).Info("generating endpoints")

	serviceEndpoints, err := s.registry.ListAllEndpoints(ctx, "http")
	if err != nil {
		return nil, nil, err
	}

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
