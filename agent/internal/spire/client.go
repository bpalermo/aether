package spire

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"strings"
	"time"

	commonlog "aethermesh.dev/common/log"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	brokerpb "github.com/spiffe/go-spiffe/v2/exp/proto/spiffe/broker"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/anypb"
)

// brokerSecurityHeader is the static gRPC metadata every request to a SPIFFE
// Broker Endpoint MUST carry (SPIFFE_Broker_Endpoint.md §3). It is an SSRF
// guard: a server rejects a request without it with InvalidArgument, so an
// attacker who can only make the agent emit a request — but not control its
// outgoing metadata — cannot reach the endpoint.
const (
	brokerSecurityHeaderKey   = "broker.spiffe.io"
	brokerSecurityHeaderValue = "true"
)

// spireAgentPathPrefix is the SPIFFE ID path prefix a SPIRE agent's own SVID
// carries (spiffe://<trust-domain>/spire/agent/<attestor>/...). The Broker
// Endpoint specification requires the broker to authenticate the provider
// (§5), so the client accepts a server only if it presents an ID in our own
// trust domain under this prefix — not merely "any member of the trust
// domain", which would let any workload that can bind the socket path
// impersonate the SPIRE agent and hand us poisoned SVIDs.
const spireAgentPathPrefix = "/spire/agent/"

// kubernetesPodsPlural / kubernetesCoreGroup identify the referenced object
// type. Pods are a core resource, which the specification spells as the
// literal group "core" (not the empty string the Kubernetes API uses).
const (
	kubernetesPodsPlural = "pods"
	kubernetesCoreGroup  = "core"
)

// firstResponseTimeout bounds the FIRST receive on a subscription stream.
//
// It is not a nicety. SPIRE resolves the reference and then blocks inside its
// own cache subscription until every matching registration entry has a minted
// SVID — before the handler sends anything. So a reference that resolves to an
// entry whose SVID never lands leaves Recv blocked forever, with no error, no
// log line and no retry: the pod silently never gets an identity. The bound
// turns that into an ordinary retry on the bridge's backoff, which also
// re-resolves the reference. It is generous enough that a slow but healthy mint
// completes inside it.
const firstResponseTimeout = 30 * time.Second

// PodRef identifies the pod the agent brokers an identity for. Both the
// namespaced key AND the UID are sent: the specification then requires the
// server to verify that the pod it resolved by key carries this UID, which is
// what makes a recycled pod name unable to inherit the previous pod's SVID.
type PodRef struct {
	Namespace string
	Name      string
	UID       string
}

// String renders the reference for logs.
func (r PodRef) String() string {
	return fmt.Sprintf("%s/%s (uid %s)", r.Namespace, r.Name, r.UID)
}

// BrokerClient is the narrow view of the SPIFFE Broker API the bridge consumes.
// It exists so tests can substitute a client without a socket; the production
// implementation is brokerClient.
type BrokerClient interface {
	// SubscribeX509SVID opens a SubscribeToX509SVID stream for the referenced
	// pod. It performs the FIRST receive synchronously, so a reference that does
	// not resolve (NotFound), is not entitled (PermissionDenied) or is malformed
	// (InvalidArgument) is reported as an error here rather than silently
	// closing a channel — the bridge needs the gRPC code to decide whether to
	// retry. The returned channel carries that first response followed by every
	// subsequent one, and is closed when the stream ends or ctx is cancelled.
	SubscribeX509SVID(ctx context.Context, ref PodRef) (<-chan *brokerpb.SubscribeToX509SVIDResponse, error)

	// Close releases the underlying connection.
	Close() error
}

// IdentitySource is the agent's own Workload API identity. The bridge needs all
// three roles from it: the SVID it serves as the node identity, the SAME SVID as
// the client certificate for the mutually-authenticated Broker Endpoint, and the
// trust bundle it both validates the broker with and publishes as Envoy's
// validation context.
//
// It is satisfied by common/spire.WaitingSource, whose methods return
// ErrNoSVIDYet until SPIRE issues the first SVID (issue #740). That is a waiting
// state, never a failure: the TLS handshake simply cannot complete yet, the
// subscribe fails, and the bridge retries on its backoff.
type IdentitySource interface {
	x509svid.Source
	x509bundle.Source
}

// brokerClient is the production BrokerClient: a gRPC client for the SPIFFE
// Broker Endpoint served by the node's SPIRE agent over a Unix domain socket.
type brokerClient struct {
	conn *grpc.ClientConn
	api  brokerpb.APIClient
	log  *slog.Logger
}

// newBrokerClient dials the Broker Endpoint at socketPath with mutual TLS built
// from the agent's own identity source.
//
// Nothing is dialled here: grpc.NewClient is lazy, so a SPIRE agent that is not
// up yet (or an identity source with no SVID yet) costs nothing at construction
// and surfaces on the first subscribe, where the bridge's backoff handles it.
func newBrokerClient(socketPath string, source IdentitySource, log *slog.Logger) (*brokerClient, error) {
	if source == nil {
		return nil, fmt.Errorf("the SPIFFE Broker Endpoint requires mutual TLS: no identity source configured")
	}

	tlsCfg := tlsconfig.MTLSClientConfig(source, source, authorizeSPIREAgent(source))

	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithUnaryInterceptor(brokerHeaderUnaryInterceptor),
		grpc.WithStreamInterceptor(brokerHeaderStreamInterceptor),
	)
	if err != nil {
		return nil, fmt.Errorf("dialing the SPIFFE Broker Endpoint at %s: %w", socketPath, err)
	}

	return &brokerClient{
		conn: conn,
		api:  brokerpb.NewAPIClient(conn),
		log:  commonlog.Named(log, "spire-broker-client"),
	}, nil
}

// authorizeSPIREAgent authorizes the broker endpoint's server certificate: it
// must be a SPIFFE ID in OUR OWN trust domain (the one our own SVID was issued
// into, read at handshake time because at dial time there may be no SVID yet)
// whose path is a SPIRE agent path.
func authorizeSPIREAgent(source IdentitySource) tlsconfig.Authorizer {
	return func(id spiffeid.ID, _ [][]*x509.Certificate) error {
		own, err := source.GetX509SVID()
		if err != nil {
			return fmt.Errorf("cannot authorize the SPIFFE Broker Endpoint without this agent's own SVID: %w", err)
		}
		if id.TrustDomain() != own.ID.TrustDomain() {
			return fmt.Errorf("SPIFFE Broker Endpoint presented %q, which is not in this agent's trust domain %q",
				id, own.ID.TrustDomain())
		}
		if !strings.HasPrefix(id.Path(), spireAgentPathPrefix) {
			return fmt.Errorf("SPIFFE Broker Endpoint presented %q, which is not a SPIRE agent identity (path must start with %q)",
				id, spireAgentPathPrefix)
		}
		return nil
	}
}

// brokerHeaderUnaryInterceptor adds the mandatory security header to every
// unary call.
func brokerHeaderUnaryInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	return invoker(withBrokerHeader(ctx), method, req, reply, cc, opts...)
}

// brokerHeaderStreamInterceptor adds the mandatory security header to every
// streaming call.
func brokerHeaderStreamInterceptor(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return streamer(withBrokerHeader(ctx), desc, cc, method, opts...)
}

// withBrokerHeader returns ctx carrying the broker security header.
func withBrokerHeader(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, brokerSecurityHeaderKey, brokerSecurityHeaderValue)
}

// SubscribeX509SVID implements BrokerClient.
func (c *brokerClient) SubscribeX509SVID(ctx context.Context, ref PodRef) (<-chan *brokerpb.SubscribeToX509SVIDResponse, error) {
	req, err := podReferenceRequest(ref)
	if err != nil {
		return nil, err
	}

	// streamCtx outlives this call: it is what keeps the stream open for the
	// goroutine below, and it is cancelled when that goroutine ends or ctx does.
	streamCtx, cancelStream := context.WithCancel(ctx)

	stream, err := c.api.SubscribeToX509SVID(streamCtx, req)
	if err != nil {
		cancelStream()
		return nil, fmt.Errorf("subscribing to X.509 SVIDs for %s: %w", ref, err)
	}

	first, err := recvFirst(ctx, stream, ref, cancelStream)
	if err != nil {
		return nil, err
	}

	ch := make(chan *brokerpb.SubscribeToX509SVIDResponse, 1)
	ch <- first
	go c.pump(streamCtx, stream, ch, ref, cancelStream)

	return ch, nil
}

// recvFirst performs the first receive synchronously, because reference
// resolution happens server-side at request time: NotFound / PermissionDenied /
// InvalidArgument arrive here and nowhere else, and swallowing them into a
// closed channel would erase the distinction between "retry, the kubelet has not
// listed the pod yet" and "never retry, the request is malformed".
//
// It is bounded because the server may block before sending anything at all
// (see firstResponseTimeout). On any error the stream is cancelled.
func recvFirst(
	ctx context.Context,
	stream grpc.ServerStreamingClient[brokerpb.SubscribeToX509SVIDResponse],
	ref PodRef,
	cancelStream context.CancelFunc,
) (*brokerpb.SubscribeToX509SVIDResponse, error) {
	deadline := time.AfterFunc(firstResponseTimeout, cancelStream)
	first, err := stream.Recv()
	timedOut := !deadline.Stop()
	if err == nil {
		return first, nil
	}

	cancelStream()
	if timedOut && ctx.Err() == nil {
		return nil, fmt.Errorf("the SPIFFE Broker Endpoint sent no X.509 SVID update for %s within %s",
			ref, firstResponseTimeout)
	}
	return nil, fmt.Errorf("receiving the first X.509 SVID update for %s: %w", ref, err)
}

// pump forwards every subsequent response onto ch and closes it when the stream
// ends or the context is cancelled.
func (c *brokerClient) pump(
	streamCtx context.Context,
	stream grpc.ServerStreamingClient[brokerpb.SubscribeToX509SVIDResponse],
	ch chan<- *brokerpb.SubscribeToX509SVIDResponse,
	ref PodRef,
	cancelStream context.CancelFunc,
) {
	defer close(ch)
	defer cancelStream()

	for {
		resp, err := stream.Recv()
		if err != nil {
			if streamCtx.Err() == nil {
				c.log.ErrorContext(streamCtx, "X.509 SVID subscription stream ended", "error", err, "pod", ref.String())
			}
			return
		}
		select {
		case ch <- resp:
		case <-streamCtx.Done():
			return
		}
	}
}

// podReferenceRequest wraps a PodRef as the specification's
// KubernetesObjectReference, packed into the request's WorkloadReference Any.
func podReferenceRequest(ref PodRef) (*brokerpb.SubscribeToX509SVIDRequest, error) {
	packed, err := anypb.New(&brokerpb.KubernetesObjectReference{
		Type: &brokerpb.KubernetesObjectType{
			Plural: kubernetesPodsPlural,
			Group:  kubernetesCoreGroup,
		},
		Key: &brokerpb.KubernetesObjectKey{
			Namespace: ref.Namespace,
			Name:      ref.Name,
		},
		Uid: ref.UID,
	})
	if err != nil {
		return nil, fmt.Errorf("packing the pod reference for %s: %w", ref, err)
	}
	return &brokerpb.SubscribeToX509SVIDRequest{
		Reference: &brokerpb.WorkloadReference{Reference: packed},
	}, nil
}

// Close closes the underlying gRPC connection.
func (c *brokerClient) Close() error {
	return c.conn.Close()
}
