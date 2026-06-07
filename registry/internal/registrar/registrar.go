// Package registrar implements the Registry interface using a Registrar gRPC service.
// It caches endpoints locally from a server-streaming watch and delegates writes
// to the Registrar, which in turn persists them to the external registry backend.
package registrar

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"time"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// initialBackoff is the starting backoff duration for reconnection.
	initialBackoff = 1 * time.Second
	// maxBackoff is the maximum backoff duration for reconnection.
	maxBackoff = 30 * time.Second
	// jitterFraction is the fraction of backoff to randomize (0.0–1.0).
	jitterFraction = 0.2
)

// Config holds configuration for connecting to the Registrar service.
type Config struct {
	// Address is the gRPC address of the Registrar service.
	Address string
	// ClusterName identifies this agent's cluster to the Registrar. Together with
	// NodeName it forms the unique watcher id; without it every agent collides on
	// the same id and the Registrar evicts their watch streams in a reconnect loop.
	ClusterName string
	// NodeName identifies this agent's node to the Registrar. See ClusterName.
	NodeName string
	// DialOptions are additional gRPC dial options (e.g., TLS credentials).
	// When empty, insecure credentials are used.
	DialOptions []grpc.DialOption
}

// RegistrarRegistry implements the Registry interface by communicating with a
// Registrar gRPC service. Reads are served from a local cache populated by a
// WatchEndpoints stream. Writes are delegated to the Registrar.
type RegistrarRegistry struct {
	log    logr.Logger
	config Config

	conn   *grpc.ClientConn
	client registrarv1.RegistrarServiceClient

	mu    sync.RWMutex
	cache map[string][]*registryv1.ServiceEndpoint // keyed by serviceName

	cancel context.CancelFunc
}

// NewRegistrarRegistry creates a new RegistrarRegistry.
func NewRegistrarRegistry(log logr.Logger, cfg Config) *RegistrarRegistry {
	return &RegistrarRegistry{
		log:    log.WithName("registrar-registry"),
		config: cfg,
		cache:  make(map[string][]*registryv1.ServiceEndpoint),
	}
}

// Initialize connects to the Registrar and starts the background watch stream.
func (r *RegistrarRegistry) Initialize(ctx context.Context) error {
	opts := r.config.DialOptions
	if len(opts) == 0 {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	conn, err := grpc.NewClient(r.config.Address, opts...)
	if err != nil {
		return fmt.Errorf("failed to connect to registrar at %s: %w", r.config.Address, err)
	}

	r.conn = conn
	r.client = registrarv1.NewRegistrarServiceClient(conn)

	watchCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	go r.watchLoop(watchCtx)

	r.log.Info("initialized registrar registry", "address", r.config.Address)
	return nil
}

// Close shuts down the watch stream and closes the gRPC connection.
func (r *RegistrarRegistry) Close() error {
	if r.cancel != nil {
		r.cancel()
	}
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}

// RegisterEndpoint delegates to the Registrar's RegisterEndpoint RPC.
func (r *RegistrarRegistry) RegisterEndpoint(ctx context.Context, serviceName string, protocol registryv1.Service_Protocol, endpoint *registryv1.ServiceEndpoint) error {
	_, err := r.client.RegisterEndpoint(ctx, &registrarv1.RegisterEndpointRequest{
		ServiceName: serviceName,
		Protocol:    protocol,
		Endpoint:    endpoint,
	})
	return err
}

// UnregisterEndpoint delegates to the Registrar's UnregisterEndpoint RPC.
func (r *RegistrarRegistry) UnregisterEndpoint(ctx context.Context, serviceName string, ip string) error {
	_, err := r.client.UnregisterEndpoint(ctx, &registrarv1.UnregisterEndpointRequest{
		ServiceName: serviceName,
		Ips:         []string{ip},
	})
	return err
}

// UnregisterEndpoints delegates to the Registrar's UnregisterEndpoint RPC with multiple IPs.
func (r *RegistrarRegistry) UnregisterEndpoints(ctx context.Context, serviceName string, ips []string) error {
	_, err := r.client.UnregisterEndpoint(ctx, &registrarv1.UnregisterEndpointRequest{
		ServiceName: serviceName,
		Ips:         ips,
	})
	return err
}

// ListEndpoints returns endpoints for a service from the local cache.
// Falls back to the Registrar RPC if the cache is empty.
func (r *RegistrarRegistry) ListEndpoints(ctx context.Context, service string, protocol registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	r.mu.RLock()
	eps, ok := r.cache[service]
	r.mu.RUnlock()

	if ok {
		return eps, nil
	}

	// Fallback to RPC.
	return r.listEndpointsFromServer(ctx, service, protocol)
}

// ListAllEndpoints returns all endpoints from the local cache.
// Falls back to the Registrar RPC if the cache is empty.
func (r *RegistrarRegistry) ListAllEndpoints(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	r.mu.RLock()
	cacheLen := len(r.cache)
	r.mu.RUnlock()

	if cacheLen > 0 {
		r.mu.RLock()
		result := make(map[string][]*registryv1.ServiceEndpoint, len(r.cache))
		for k, v := range r.cache {
			result[k] = v
		}
		r.mu.RUnlock()
		return result, nil
	}

	// Fallback to RPC.
	return r.listAllEndpointsFromServer(ctx, protocol)
}

func (r *RegistrarRegistry) listEndpointsFromServer(ctx context.Context, service string, protocol registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	resp, err := r.client.ListAllEndpoints(ctx, &registrarv1.ListAllEndpointsRequest{
		Protocol: protocol,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list endpoints from registrar: %w", err)
	}

	svcEps, ok := resp.GetServices()[service]
	if !ok {
		return nil, nil
	}
	return svcEps.GetEndpoints(), nil
}

func (r *RegistrarRegistry) listAllEndpointsFromServer(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	resp, err := r.client.ListAllEndpoints(ctx, &registrarv1.ListAllEndpointsRequest{
		Protocol: protocol,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list all endpoints from registrar: %w", err)
	}

	result := make(map[string][]*registryv1.ServiceEndpoint, len(resp.GetServices()))
	for svcName, svcEps := range resp.GetServices() {
		result[svcName] = svcEps.GetEndpoints()
	}
	return result, nil
}

// watchLoop maintains a persistent WatchEndpoints stream, reconnecting with
// exponential backoff on disconnect.
func (r *RegistrarRegistry) watchLoop(ctx context.Context) {
	backoff := initialBackoff
	lastVersion := ""

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		stream, err := r.client.WatchEndpoints(ctx, &registrarv1.WatchEndpointsRequest{
			ClusterName: r.config.ClusterName,
			NodeName:    r.config.NodeName,
			LastVersion: lastVersion,
		})
		if err != nil {
			jitter := time.Duration(float64(backoff) * jitterFraction * rand.Float64())
			wait := backoff + jitter
			r.log.Error(err, "failed to start watch stream, retrying", "backoff", wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Reset backoff on successful connection.
		backoff = initialBackoff
		r.log.V(1).Info("watch stream connected")

		lastVersion = r.processStream(ctx, stream, lastVersion)
	}
}

// processStream reads events from the stream and updates the local cache.
// It returns the last version seen, for use as a resume token.
func (r *RegistrarRegistry) processStream(ctx context.Context, stream registrarv1.RegistrarService_WatchEndpointsClient, lastVersion string) string {
	snapshotCleared := false

	for {
		event, err := stream.Recv()
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				r.log.Error(err, "watch stream disconnected")
			}
			return lastVersion
		}

		// Clear cache before the first FULL_SNAPSHOT event to replace stale data.
		if event.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT && !snapshotCleared {
			r.mu.Lock()
			r.cache = make(map[string][]*registryv1.ServiceEndpoint)
			r.mu.Unlock()
			snapshotCleared = true
		}

		r.applyEvent(event)

		if event.GetVersion() != "" {
			lastVersion = event.GetVersion()
		}
	}
}

// applyEvent updates the local cache based on an endpoint event.
func (r *RegistrarRegistry) applyEvent(event *registrarv1.WatchEndpointsResponse) {
	r.mu.Lock()
	defer r.mu.Unlock()

	svcName := event.GetServiceName()
	ep := event.GetEndpoint()

	switch event.GetType() {
	case registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT:
		// Upsert for full snapshot events.
		r.upsertLocked(svcName, ep)

	case registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_UPDATED:
		r.upsertLocked(svcName, ep)

	case registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED:
		r.removeLocked(svcName, ep.GetIp())
	}
}

// upsertLocked adds or updates an endpoint in the cache. Caller must hold mu.
func (r *RegistrarRegistry) upsertLocked(svcName string, ep *registryv1.ServiceEndpoint) {
	eps := r.cache[svcName]
	for i, existing := range eps {
		if existing.GetIp() == ep.GetIp() {
			eps[i] = ep
			return
		}
	}
	r.cache[svcName] = append(eps, ep)
}

// removeLocked removes an endpoint from the cache by IP. Caller must hold mu.
func (r *RegistrarRegistry) removeLocked(svcName string, ip string) {
	eps := r.cache[svcName]
	for i, existing := range eps {
		if existing.GetIp() == ip {
			r.cache[svcName] = append(eps[:i], eps[i+1:]...)
			if len(r.cache[svcName]) == 0 {
				delete(r.cache, svcName)
			}
			return
		}
	}
}
