package plugin

import (
	"context"
	"fmt"
	"time"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/retry"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// addTimeout is the context timeout for AddPod gRPC calls.
	addTimeout = 5 * time.Second
	// delTimeout is the context timeout for RemovePod gRPC calls.
	delTimeout = 5 * time.Second
	// checkTimeout is the context timeout for check-related gRPC calls.
	checkTimeout = 1 * time.Second

	// maxRetries is the maximum number of retries for transient failures.
	maxRetries = 3
	// baseBackoff is the initial backoff duration between retries.
	baseBackoff = 100 * time.Millisecond
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

// AddPod adds a pod to the registry with a timeout and retry logic for transient failures.
func (c *CNIClient) AddPod(ctx context.Context, pod *cniv1.CNIPod) (*cniv1.AddPodResponse, error) {
	c.logger.Debug("adding pod to registry",
		zap.String("name", pod.Name),
		zap.String("namespace", pod.Namespace),
		zap.String("containerId", pod.ContainerId),
		zap.String("networkNamespace", pod.NetworkNamespace))

	req := &cniv1.AddPodRequest{
		Pod: pod,
	}

	var resp *cniv1.AddPodResponse
	err := retry.Do(ctx, retry.Config{
		MaxAttempts: maxRetries,
		BaseBackoff: baseBackoff,
		IsRetryable: retry.GRPCTransient,
	}, func(ctx context.Context) error {
		callCtx, cancel := context.WithTimeout(ctx, addTimeout)
		defer cancel()

		var callErr error
		resp, callErr = c.client.AddPod(callCtx, req)
		return callErr
	})

	if err != nil {
		return nil, err
	}
	return resp, nil
}

// RemovePod removes a pod from the registry with a timeout. Del operations
// are not retried to avoid issues with duplicate deletions.
func (c *CNIClient) RemovePod(ctx context.Context, podName string, namespace string, containerId string) (*cniv1.RemovePodResponse, error) {
	c.logger.Debug("removing pod from registry",
		zap.String("containerId", containerId),
		zap.String("name", podName),
		zap.String("namespace", namespace))

	req := &cniv1.RemovePodRequest{
		Name:        podName,
		Namespace:   namespace,
		ContainerId: containerId,
	}

	callCtx, cancel := context.WithTimeout(ctx, delTimeout)
	defer cancel()

	return c.client.RemovePod(callCtx, req)
}

// VerifyPodRegistered verifies that a pod is still registered with the agent
// by re-sending an AddPod request. The agent's AddPod is idempotent — it will
// succeed if the pod is already registered. A non-SUCCESS result indicates the
// pod is no longer tracked by the agent.
func (c *CNIClient) VerifyPodRegistered(ctx context.Context, pod *cniv1.CNIPod) error {
	c.logger.Debug("verifying pod registration with agent",
		zap.String("name", pod.Name),
		zap.String("namespace", pod.Namespace),
		zap.String("containerId", pod.ContainerId))

	req := &cniv1.AddPodRequest{
		Pod: pod,
	}

	callCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	res, err := c.client.AddPod(callCtx, req)
	if err != nil {
		return fmt.Errorf("failed to verify pod registration: %w", err)
	}

	if res.Result != cniv1.AddPodResponse_SUCCESS {
		return fmt.Errorf("pod verification failed: agent returned %v", res.Result)
	}

	return nil
}

// CheckAgentConnection verifies the gRPC connection to the agent is healthy
// by attempting to connect within the given context deadline.
func (c *CNIClient) CheckAgentConnection(ctx context.Context) error {
	c.conn.Connect()

	state := c.conn.GetState()
	if state == connectivity.Ready {
		return nil
	}

	if !c.conn.WaitForStateChange(ctx, state) {
		return fmt.Errorf("agent connection check timed out in state %v", state)
	}

	state = c.conn.GetState()
	if state != connectivity.Ready {
		return fmt.Errorf("agent connection is not ready: %v", state)
	}

	return nil
}

// Close closes the client connection
func (c *CNIClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
