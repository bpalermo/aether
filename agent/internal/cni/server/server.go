package server

import (
	"context"
	"fmt"

	"buf.build/go/protovalidate"
	"github.com/bpalermo/aether/agent/pkg/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/constants"
	"github.com/bpalermo/aether/registry"
	"github.com/bpalermo/aether/xds"
	"github.com/go-logr/logr"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type CNIServer struct {
	cniv1.UnimplementedCNIServiceServer
	xds.Server

	log logr.Logger

	clusterName string
	proxyID     string
	nodeName    string
	nodeRegion  string
	nodeZone    string

	storage  storage.Storage[*cniv1.CNIPod]
	registry registry.Registry

	k8sClient client.Client
}

var _ xds.ServerCallback = (*CNIServer)(nil)

// NewCNIServer creates a new CNI gRPC server
func NewCNIServer(clusterName string, nodeName string, proxyID string, localStorage storage.Storage[*cniv1.CNIPod], registry registry.Registry, log logr.Logger, k8sClient client.Client, cfg *CNIServerConfig) (*CNIServer, error) {
	validator, _ := protovalidate.New()

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(protovalidate_middleware.UnaryServerInterceptor(validator)),
	)

	cniSrv := &CNIServer{
		Server:      xds.NewServer(xds.NewServerConfig(xds.WithUDS(cfg.SocketPath)), log, xds.WithGRPCServer(grpcServer)),
		log:         log.WithName("cni"),
		clusterName: clusterName,
		nodeName:    nodeName,
		proxyID:     proxyID,
		storage:     localStorage,
		registry:    registry,
		k8sClient:   k8sClient,
	}

	cniSrv.AddCallback(cniSrv)

	cniv1.RegisterCNIServiceServer(grpcServer, cniSrv)

	return cniSrv, nil
}

func (s *CNIServer) PreListen(ctx context.Context) error {
	s.log.V(2).Info("querying node metadata")
	region, zone, err := queryNodeMetadata(ctx, s.proxyID, s.k8sClient)
	if err != nil {
		return err
	}

	s.nodeRegion = region
	s.nodeZone = zone

	s.log.V(1).Info("node metadata queried successfully", "region", region, "zone", zone)
	return nil
}

func queryNodeMetadata(ctx context.Context, proxyID string, client client.Client) (string, string, error) {
	node := &corev1.Node{}
	if err := client.Get(ctx, types.NamespacedName{Name: proxyID}, node); err != nil {
		return "", "", fmt.Errorf("failed to get node: %w", err)
	}

	return node.Labels[constants.AnnotationKubernetesNodeTopologyRegion], node.Labels[constants.AnnotationKubernetesNodeTopologyZone], nil
}
