package server

import (
	"buf.build/go/protovalidate"
	"github.com/bpalermo/aether/agent/pkg/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry/types"
	"github.com/bpalermo/aether/xds"
	"github.com/go-logr/logr"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type CNIServer struct {
	cniv1.UnimplementedCNIServiceServer
	xds.Server

	log logr.Logger

	clusterName string
	proxyID     string

	storage  storage.Storage[*registryv1.RegistryPod]
	registry types.Registry

	k8sClient client.Client
}

// NewCNIServer creates a new CNI gRPC server
func NewCNIServer(proxyID string, localStorage storage.Storage[*registryv1.RegistryPod], registry types.Registry, log logr.Logger, k8sClient client.Client, cfg *CNIServerConfig) (*CNIServer, error) {
	validator, _ := protovalidate.New()

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(protovalidate_middleware.UnaryServerInterceptor(validator)),
	)

	cniSrv := &CNIServer{
		Server:      xds.NewServer(xds.NewServerConfig(xds.WithUDS(cfg.SocketPath)), log, xds.WithGRPCServer(grpcServer)),
		log:         log.WithName("cni-server"),
		clusterName: cfg.ClusterName,
		proxyID:     proxyID,
		storage:     localStorage,
		registry:    registry,
		k8sClient:   k8sClient,
	}

	cniv1.RegisterCNIServiceServer(grpcServer, cniSrv)

	return cniSrv, nil
}
