package server

import (
	"context"
	"fmt"

	"buf.build/go/protovalidate"
	"github.com/bpalermo/aether/agent/internal/envoy/admin"
	"github.com/bpalermo/aether/agent/internal/spire"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/constants"
	"github.com/bpalermo/aether/common/xds"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CNIServer is a gRPC server that implements the CNI plugin interface.
// It handles pod registration and deregistration requests, stores pod data locally,
// and registers service endpoints in the registry. The server also queries Kubernetes
// node metadata (region and zone) for topology-aware routing.
//
// CNIServer embeds xds.Server and implements the ServerCallback interface to query
// node metadata before accepting client connections.
type CNIServer struct {
	cniv1.UnimplementedCNIServiceServer
	xds.Server

	log logr.Logger

	clusterName string
	proxyID     string
	nodeName    string
	trustDomain string
	nodeRegion  string
	nodeZone    string
	nodeIP      string

	storage  storage.Storage[*cniv1.CNIPod]
	registry registry.Registry

	snapshotCache *cache.SnapshotCache
	spireBridge   *spire.Bridge
	envoyAdmin    *admin.Client

	k8sClient client.Client
}

var _ xds.ServerCallback = (*CNIServer)(nil)

// NewCNIServer creates a new CNI gRPC server.
// The server listens on a Unix domain socket and registers the CNI service with
// protovalidate middleware for request validation.
func NewCNIServer(clusterName string, nodeName string, proxyID string, trustDomain string, localStorage storage.Storage[*cniv1.CNIPod], registry registry.Registry, snapshotCache *cache.SnapshotCache, spireBridge *spire.Bridge, log logr.Logger, k8sClient client.Client, cfg *CNIServerConfig) (*CNIServer, error) {
	validator, _ := protovalidate.New()

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(protovalidate_middleware.UnaryServerInterceptor(validator)),
	)

	cniSrv := &CNIServer{
		Server:        xds.NewServer(xds.NewServerConfig(xds.WithUDS(cfg.SocketPath)), log, xds.WithGRPCServer(grpcServer)),
		log:           log.WithName("cni"),
		clusterName:   clusterName,
		nodeName:      nodeName,
		proxyID:       proxyID,
		trustDomain:   trustDomain,
		storage:       localStorage,
		registry:      registry,
		k8sClient:     k8sClient,
		snapshotCache: snapshotCache,
		spireBridge:   spireBridge,
		envoyAdmin:    admin.NewClient(cfg.EnvoyAdminAddress),
	}

	cniSrv.AddCallback(cniSrv)

	cniv1.RegisterCNIServiceServer(grpcServer, cniSrv)

	return cniSrv, nil
}

// PreListen queries Kubernetes node metadata before the server starts accepting connections.
// It retrieves the region and zone labels from the node object.
func (s *CNIServer) PreListen(ctx context.Context) error {
	s.log.V(2).Info("querying node metadata")
	region, zone, nodeIP, err := queryNodeMetadata(ctx, s.proxyID, s.k8sClient)
	if err != nil {
		return err
	}

	s.nodeRegion = region
	s.nodeZone = zone
	s.nodeIP = nodeIP

	s.log.V(1).Info("node metadata queried successfully", "region", region, "zone", zone, "nodeIP", nodeIP)

	// Delegated liveness: reflect each local pod's app health (from the proxy's
	// active health check) into the registry so it is marked unhealthy in every
	// client's EDS while the app is not serving.
	go s.runLivenessLoop(ctx)
	return nil
}

// queryNodeMetadata retrieves the region and zone labels and the InternalIP from
// a Kubernetes node. It returns the topology.kubernetes.io/region and
// topology.kubernetes.io/zone labels (empty if absent) and the node's InternalIP
// address, used as the node inbound (mTLS) target for endpoints on this node.
func queryNodeMetadata(ctx context.Context, proxyID string, client client.Client) (region, zone, nodeIP string, err error) {
	node := &corev1.Node{}
	if err := client.Get(ctx, types.NamespacedName{Name: proxyID}, node); err != nil {
		return "", "", "", fmt.Errorf("failed to get node: %w", err)
	}

	return node.Labels[constants.AnnotationKubernetesNodeTopologyRegion],
		node.Labels[constants.AnnotationKubernetesNodeTopologyZone],
		nodeInternalIP(node), nil
}

// nodeInternalIP returns the node's InternalIP address, or "" if none is present.
func nodeInternalIP(node *corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}
