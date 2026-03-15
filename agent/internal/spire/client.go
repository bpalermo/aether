package spire

import (
	"context"
	"fmt"
	"net"

	"github.com/go-logr/logr"
	delegatedidentityv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/agent/delegatedidentity/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client wraps the SPIRE Delegated Identity gRPC client.
type Client struct {
	conn   *grpc.ClientConn
	client delegatedidentityv1.DelegatedIdentityClient
	log    logr.Logger
}

// NewClient creates a new SPIRE Delegated Identity client connected to the
// admin socket at the given path.
func NewClient(ctx context.Context, socketPath string, log logr.Logger) (*Client, error) {
	conn, err := grpc.NewClient(
		"passthrough:///unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("dialing SPIRE admin socket %s: %w", socketPath, err)
	}

	return &Client{
		conn:   conn,
		client: delegatedidentityv1.NewDelegatedIdentityClient(conn),
		log:    log.WithName("spire-client"),
	}, nil
}

// SubscribeSVIDsByPID opens a SubscribeToX509SVIDs stream for the given
// container PID. The SPIRE agent attests the process and returns its SVIDs.
// The channel is closed when the stream ends or the context is cancelled.
func (c *Client) SubscribeSVIDsByPID(ctx context.Context, pid int32) (<-chan *delegatedidentityv1.SubscribeToX509SVIDsResponse, error) {
	stream, err := c.client.SubscribeToX509SVIDs(ctx, &delegatedidentityv1.SubscribeToX509SVIDsRequest{
		Pid: pid,
	})
	if err != nil {
		return nil, fmt.Errorf("subscribing to X509 SVIDs: %w", err)
	}

	ch := make(chan *delegatedidentityv1.SubscribeToX509SVIDsResponse)
	go func() {
		defer close(ch)
		for {
			resp, recvErr := stream.Recv()
			if recvErr != nil {
				if ctx.Err() == nil {
					c.log.Error(recvErr, "SVID subscription stream ended")
				}
				return
			}
			select {
			case ch <- resp:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

// SubscribeBundles opens a SubscribeToX509Bundles stream and returns a channel
// that receives bundle updates. The channel is closed when the stream ends or
// the context is canceled.
func (c *Client) SubscribeBundles(ctx context.Context) (<-chan *delegatedidentityv1.SubscribeToX509BundlesResponse, error) {
	stream, err := c.client.SubscribeToX509Bundles(ctx, &delegatedidentityv1.SubscribeToX509BundlesRequest{})
	if err != nil {
		return nil, fmt.Errorf("subscribing to X509 bundles: %w", err)
	}

	ch := make(chan *delegatedidentityv1.SubscribeToX509BundlesResponse)
	go func() {
		defer close(ch)
		for {
			resp, recvErr := stream.Recv()
			if recvErr != nil {
				if ctx.Err() == nil {
					c.log.Error(recvErr, "bundle subscription stream ended")
				}
				return
			}
			select {
			case ch <- resp:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

// Close closes the underlying gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
