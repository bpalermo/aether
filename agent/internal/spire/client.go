package spire

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	commonlog "github.com/bpalermo/aether/common/log"
	delegatedidentityv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/agent/delegatedidentity/v1"
	apitypes "github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client wraps the SPIRE Delegated Identity gRPC client.
type Client struct {
	conn   *grpc.ClientConn
	client delegatedidentityv1.DelegatedIdentityClient
	log    *slog.Logger
}

// NewClient creates a new SPIRE Delegated Identity client connected to the
// admin socket at the given path.
func NewClient(ctx context.Context, socketPath string, log *slog.Logger) (*Client, error) {
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
		log:    commonlog.Named(log, "spire-client"),
	}, nil
}

// SubscribeSVIDsBySelectors opens a SubscribeToX509SVIDs stream for the given
// workload selectors. The SPIRE agent returns the SVIDs of every registration
// entry whose selectors are a subset of those provided, without attesting a
// process. The channel is closed when the stream ends or the context is cancelled.
func (c *Client) SubscribeSVIDsBySelectors(ctx context.Context, selectors []*apitypes.Selector) (<-chan *delegatedidentityv1.SubscribeToX509SVIDsResponse, error) {
	stream, err := c.client.SubscribeToX509SVIDs(ctx, &delegatedidentityv1.SubscribeToX509SVIDsRequest{
		Selectors: selectors,
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
					c.log.ErrorContext(ctx, "SVID subscription stream ended", "error", recvErr)
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
					c.log.ErrorContext(ctx, "bundle subscription stream ended", "error", recvErr)
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
