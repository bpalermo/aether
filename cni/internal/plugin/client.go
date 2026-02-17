package plugin

import (
	"context"
	"fmt"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type CNIClient struct {
	logger *zap.Logger

	conn   *grpc.ClientConn
	client cniv1.CNIServiceClient
}

// NewCNIClient creates a new CNI service client connected via Unix socket
func NewCNIClient(logger *zap.Logger, socketPath string) (*CNIClient, error) {
	logger.Debug("creating CNI client", zap.String("socketPath", socketPath))
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		msg := "failed to connect to socket"
		logger.Error(msg, zap.Error(err))
		return nil, fmt.Errorf("%s %s: %w", msg, socketPath, err)
	}

	return &CNIClient{
		logger: logger,
		conn:   conn,
		client: cniv1.NewCNIServiceClient(conn),
	}, nil
}

// AddPod adds a pod to the registry
func (c *CNIClient) AddPod(ctx context.Context, pod *cniv1.CNIPod) (*cniv1.AddPodResponse, error) {
	c.logger.Debug("adding pod to registry",
		zap.String("name", pod.Name),
		zap.String("namespace", pod.Namespace),
		zap.String("containerId", pod.ContainerId),
		zap.String("networkNamespace", pod.NetworkNamespace))

	req := &cniv1.AddPodRequest{
		Pod: pod,
	}
	return c.client.AddPod(ctx, req)
}

// RemovePod removes a pod from the registry
func (c *CNIClient) RemovePod(ctx context.Context, cniPod *cniv1.CNIPod) (*cniv1.RemovePodResponse, error) {
	c.logger.Debug("removing pod to registry",
		zap.String("name", cniPod.Name),
		zap.String("namespace", cniPod.Namespace))

	req := &cniv1.RemovePodRequest{
		Pod: cniPod,
	}
	return c.client.RemovePod(ctx, req)
}

// Close closes the client connection
func (c *CNIClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
