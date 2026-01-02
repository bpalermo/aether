package server

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/xds/proxy"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type XdsServer struct {
	log logr.Logger

	address string

	snapshotCache cachev3.SnapshotCache
	grpcServer    *grpc.Server

	mgr              manager.Manager
	registryInitChan <-chan struct{}
	entryChan        <-chan *registryv1.RegistryEntry

	proxyServiceNodeID string
}

type XdsServerOption func(*XdsServer)

func NewXdsServer(mgr manager.Manager, log logr.Logger, proxyServiceNodeID string, registryInitChan <-chan struct{}, entryChan <-chan *registryv1.RegistryEntry, opts ...XdsServerOption) *XdsServer {
	srv := &XdsServer{
		mgr:                mgr,
		log:                log.WithName("xds"),
		address:            constants.DefaultXdsSocketPath,
		proxyServiceNodeID: proxyServiceNodeID,
		registryInitChan:   registryInitChan,
		entryChan:          entryChan,
	}

	for _, option := range opts {
		option(srv)
	}

	srv.snapshotCache = cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil)
	server := serverv3.NewServer(context.Background(), srv.snapshotCache, nil)
	srv.grpcServer = grpc.NewServer()

	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(srv.grpcServer, server)
	endpointservice.RegisterEndpointDiscoveryServiceServer(srv.grpcServer, server)
	clusterservice.RegisterClusterDiscoveryServiceServer(srv.grpcServer, server)
	routeservice.RegisterRouteDiscoveryServiceServer(srv.grpcServer, server)
	listenerservice.RegisterListenerDiscoveryServiceServer(srv.grpcServer, server)

	return srv
}
func (s *XdsServer) Start(ctx context.Context) error {
	// Create listener
	listener, err := net.Listen("unix", s.address)
	if err != nil {
		return err
	}

	if err := os.Chmod(s.address, os.ModePerm); err != nil {
		// Close listener before returning error
		if err = listener.Close(); err != nil {
			s.log.Error(err, "failed to close listener")
		}
		return fmt.Errorf("failed to set socket file permissions: %v", err)
	}

	s.log.Info("XDS server uds", "address", s.address)

	// Monitor context cancellation
	go func() {
		<-ctx.Done()
		s.log.Info("context cancelled, initiating graceful shutdown")
		if err = listener.Close(); err != nil {
			s.log.Error(err, "failed to close listener")
		}
		s.grpcServer.GracefulStop()
	}()

	// Wait for the controller cache to be synced
	if r := s.mgr.GetCache().WaitForCacheSync(ctx); !r {
		return fmt.Errorf("cache sync failed")
	}

	// wait for the registry channel to close before populate the initial snapshot
	entries := make([]*registryv1.RegistryEntry, 0)
	for {
		select {
		case <-s.registryInitChan:
			// Initialization is complete, proceed with snapshot
			s.log.Info("registry initialization complete. generating initial snapshot", "entries", len(entries))
			if err = s.initialSnapshot(ctx, entries); err != nil {
				s.log.Error(err, "failed to initialize snapshot")
				return err
			}
			return s.grpcServer.Serve(listener)

		case entry := <-s.entryChan:
			if entry != nil {
				entries = append(entries, entry)
			}

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *XdsServer) initialSnapshot(ctx context.Context, entries []*registryv1.RegistryEntry) error {
	s.log.Info("creating initial snapshot")

	clusters, endpoints, err := s.collectServices(ctx)

	s.log.Info("initial snapshot", "entries", len(entries), "clusters", len(clusters), "endpoints", len(endpoints))

	snapshot, err := cachev3.NewSnapshot(fmt.Sprintf("%v.0", 0), map[resource.Type][]types.Resource{
		resource.EndpointType: endpoints,
		resource.ClusterType:  clusters,
		resource.RouteType:    make([]types.Resource, 0),
		resource.ListenerType: proxy.GenerateListenersFromEntries(entries),
	})
	if err != nil {
		return err
	}

	if err = s.snapshotCache.SetSnapshot(ctx, s.proxyServiceNodeID, snapshot); err != nil {
		return err
	}

	return nil
}

func (s *XdsServer) GetCache() cachev3.SnapshotCache {
	return s.snapshotCache
}

func (s *XdsServer) collectServices(ctx context.Context) ([]types.Resource, []types.Resource, error) {
	podList := &corev1.PodList{}

	// Use ListOptions to filter
	listOpts := []client.ListOption{
		client.HasLabels{constants.AetherServiceLabel},
	}

	if err := s.mgr.GetClient().List(ctx, podList, listOpts...); err != nil {
		return nil, nil, err
	}
	if len(podList.Items) == 0 {
		s.log.Info("no pods found with service label")
		return make([]types.Resource, 0), make([]types.Resource, 0), nil
	}
	s.log.Info("found pods with service label", "total", len(podList.Items))

	// Convert to slice of pointers
	cdsData := map[string]*clusterv3.Cluster{}
	edsData := map[string]*endpointv3.ClusterLoadAssignment{}
	for i := range podList.Items {
		s.log.Info("processing pod", "pod", podList.Items[i].Name)
		pod := &podList.Items[i]

		if pod.Status.PodIP == "" {
			s.log.Info("IP address has not yet been allocated for pod, skipping", "pod", pod.Name)
			continue
		}

		serviceName := getServiceNameFromPod(pod)
		if serviceName == "" {
			s.log.Info("pod has no service label, skipping", "pod", pod.Name)
			continue
		}

		if _, ok := cdsData[serviceName]; !ok {
			cdsData[serviceName] = proxy.NewCluster(serviceName, pod)
		}

		if _, ok := edsData[serviceName]; !ok {
			edsData[serviceName] = proxy.NewClusterLoadAssignment(serviceName)
		}

		edsData[serviceName].Endpoints = append(edsData[serviceName].Endpoints, proxy.LocalityLbEndpointFromPod(pod))
	}

	edsResources := make([]types.Resource, 0, len(edsData))
	for _, cla := range edsData {
		edsResources = append(edsResources, cla)
	}

	cdsResources := make([]types.Resource, 0, len(cdsData))
	for _, c := range cdsData {
		cdsResources = append(cdsResources, c)
	}

	return cdsResources, edsResources, nil
}

func getServiceNameFromPod(pod *corev1.Pod) string {
	serviceName, ok := pod.Labels[constants.AetherServiceLabel]
	if !ok {
		// this is not expected to happen as we rely on the validation webhook to prevent empty service names
		return ""
	}
	return serviceName
}
