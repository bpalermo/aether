// Package etcd implements the Registry interface using etcd as the backend.
//
// Keys are region-scoped and origin-first (proposal 006): each region owns a
// contiguous authoritative subtree, so a cross-region replicator mirrors one
// deterministic prefix and partitions stay disjoint regardless of pod-CIDR
// overlap across clusters. Full key layout:
//
//	<root>/<region>/clusters/<cluster>/services/<service>/protocols/<protocol>/endpoints/<ip>
//
// A registry instance OWNS one (region, cluster): it writes/deletes only under
// its own partition, but reads (List*, watch) range the whole root so consumers
// see the union of every region's local-authoritative and mirrored-in endpoints.
package etcd

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	commonlog "github.com/bpalermo/aether/common/log"
)

const (
	// DefaultKeyPrefix is the default root prefix for all registry keys in etcd.
	// Every region's subtree hangs off this root; see the package doc for the
	// full key layout.
	DefaultKeyPrefix = "/aether/v1/regions"

	// DefaultRegion and DefaultCluster scope a registry's OWN authoritative
	// partition when the caller leaves them unset (single-region/single-cluster).
	DefaultRegion  = "local"
	DefaultCluster = "local"

	// DefaultDialTimeout is the default timeout for connecting to etcd.
	DefaultDialTimeout = 5 * time.Second
)

// Config holds the configuration for connecting to etcd.
type Config struct {
	// Endpoints is the list of etcd endpoints to connect to.
	Endpoints []string
	// DialTimeout is the timeout for establishing a connection.
	DialTimeout time.Duration
	// KeyPrefix is the root prefix for all registry keys. Defaults to DefaultKeyPrefix.
	KeyPrefix string
	// Region and Cluster identify this registry's OWN authoritative partition
	// (proposal 006). Writes/deletes go under <KeyPrefix>/<Region>/clusters/<Cluster>/;
	// reads range the whole root. One registry instance owns one (region, cluster):
	// Region is shared by every registrar on the same regional etcd, Cluster is the
	// per-cluster name. Default to DefaultRegion/DefaultCluster when unset.
	Region  string
	Cluster string
}

// EtcdRegistry is a Registry implementation backed by etcd. See the package doc
// for the region-scoped, origin-first key layout.
type EtcdRegistry struct {
	log    *slog.Logger
	config Config
	client *clientv3.Client
	// keyPrefix is the root over all regions — used for reads and the watch.
	keyPrefix string
	// ownPrefix is this instance's authoritative partition
	// (<keyPrefix>/<region>/clusters/<cluster>) — used for writes and deletes.
	ownPrefix string

	// notify coalesces change signals from the etcd watch for consumers (the
	// registrar Syncer). Buffered cap-1, non-blocking send: a burst of watch
	// events collapses into a single pending signal. Satisfies
	// registry.ChangeNotifier so the registrar reacts at watch speed (~ms)
	// instead of waiting out the poll interval — closing the cross-replica
	// last-old-exit skew on the etcd backend.
	notify chan struct{}
	// watchCancel stops the background watch loop on Close.
	watchCancel context.CancelFunc
}

// NewEtcdRegistry creates a new etcd-backed Registry.
// Call Initialize before using the registry to establish the etcd client connection.
func NewEtcdRegistry(log *slog.Logger, cfg Config) *EtcdRegistry {
	keyPrefix := cfg.KeyPrefix
	if keyPrefix == "" {
		keyPrefix = DefaultKeyPrefix
	}
	region := cfg.Region
	if region == "" {
		region = DefaultRegion
	}
	cluster := cfg.Cluster
	if cluster == "" {
		cluster = DefaultCluster
	}

	dialTimeout := cfg.DialTimeout
	if dialTimeout == 0 {
		dialTimeout = DefaultDialTimeout
	}
	cfg.DialTimeout = dialTimeout

	return &EtcdRegistry{
		log:       commonlog.Named(log, "registry-etcd"),
		config:    cfg,
		keyPrefix: keyPrefix,
		ownPrefix: fmt.Sprintf("%s/%s/clusters/%s", keyPrefix, region, cluster),
		notify:    make(chan struct{}, 1),
	}
}

// Initialize creates the etcd client connection and verifies connectivity.
// It must be called before any registry operations.
func (r *EtcdRegistry) Initialize(ctx context.Context) error {
	r.log.DebugContext(ctx, "initializing etcd client", "endpoints", r.config.Endpoints)

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   r.config.Endpoints,
		DialTimeout: r.config.DialTimeout,
	})
	if err != nil {
		r.log.ErrorContext(ctx, "failed to create etcd client", "error", err)
		return fmt.Errorf("failed to create etcd client: %w", err)
	}
	r.client = client

	// Verify connectivity by checking cluster status
	statusCtx, cancel := context.WithTimeout(ctx, r.config.DialTimeout)
	defer cancel()

	_, err = r.client.Status(statusCtx, r.config.Endpoints[0])
	if err != nil {
		if closeErr := client.Close(); closeErr != nil {
			r.log.DebugContext(ctx, "failed to close etcd client during cleanup", "error", closeErr)
		}
		r.client = nil
		r.log.ErrorContext(ctx, "failed to verify etcd connectivity", "error", err)
		return fmt.Errorf("failed to verify etcd connectivity: %w", err)
	}

	// Start the change watch over the key prefix so consumers learn of writes
	// (from this replica or any other registrar/agent) at watch speed. A
	// detached context keeps the watch alive for the registry's lifetime;
	// Close cancels it.
	watchCtx, cancel := context.WithCancel(context.Background())
	r.watchCancel = cancel
	go r.watchLoop(watchCtx)

	r.log.InfoContext(ctx, "etcd registry initialized", "endpoints", r.config.Endpoints)
	return nil
}

// Changes returns a channel that receives a (coalesced) signal whenever any
// key under the registry prefix changes. Consumers treat each receive as
// "something changed, re-read the registry". Satisfies registry.ChangeNotifier.
func (r *EtcdRegistry) Changes() <-chan struct{} { return r.notify }

// signalChange performs a non-blocking, coalescing send on notify.
func (r *EtcdRegistry) signalChange() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// watchLoop maintains a clientv3 watch over the key prefix, signaling
// consumers on every change. It re-establishes the watch on channel closure
// (compaction, leader change, transient error); the consumer's periodic poll
// is the backstop for any gap during a re-establish.
func (r *EtcdRegistry) watchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// WithPrevKV is unnecessary (consumers re-read), WithPrefix watches the
		// whole registry subtree. The watch starts from the current revision.
		wch := r.client.Watch(ctx, r.keyPrefix, clientv3.WithPrefix())
		r.log.DebugContext(ctx, "etcd change watch established", "prefix", r.keyPrefix)

		for resp := range wch {
			if err := resp.Err(); err != nil {
				r.log.DebugContext(ctx, "etcd watch error; will re-establish", "error", err.Error())
				break
			}
			if len(resp.Events) > 0 {
				r.signalChange()
			}
		}

		// Channel closed (ctx cancel or watch broke). Loop to re-establish
		// unless we are shutting down.
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// RegisterEndpoint registers an endpoint to a service and protocol in etcd,
// under THIS registry's own authoritative partition. The endpoint is serialized
// using protobuf and stored at:
// <ownPrefix>/services/<serviceName>/protocols/<protocol>/endpoints/<ip>
func (r *EtcdRegistry) RegisterEndpoint(ctx context.Context, serviceName string, protocol registryv1.Service_Protocol, endpoint *registryv1.ServiceEndpoint) error {
	ip := endpoint.GetIp()
	r.log.DebugContext(ctx,
		"registering endpoint",
		"service", serviceName,
		"protocol", protocol,
		"cluster", endpoint.GetClusterName(),
		"ip", ip,
	)

	// Marshal endpoint to protobuf binary format
	data, err := proto.Marshal(endpoint)
	if err != nil {
		r.log.ErrorContext(ctx, "failed to marshal endpoint", "error", err, "ip", ip)
		return fmt.Errorf("failed to marshal endpoint for IP %s: %w", ip, err)
	}

	key := r.endpointKey(serviceName, protocol, ip)
	_, err = r.client.Put(ctx, key, string(data))
	if err != nil {
		r.log.ErrorContext(ctx, "failed to register endpoint", "error", err, "ip", ip, "key", key)
		return fmt.Errorf("failed to register endpoint for IP %s: %w", ip, err)
	}

	r.log.InfoContext(ctx,
		"endpoint registered successfully",
		"service", serviceName,
		"cluster", endpoint.GetClusterName(),
		"ip", ip,
	)
	return nil
}

// UnregisterEndpoint removes a single endpoint from the registry for all protocols.
func (r *EtcdRegistry) UnregisterEndpoint(ctx context.Context, serviceName string, ip string) error {
	return r.UnregisterEndpoints(ctx, serviceName, []string{ip})
}

// UnregisterEndpoints removes multiple endpoints from the registry for all protocols.
// It queries all protocol directories for the service and removes the specified IPs from each.
func (r *EtcdRegistry) UnregisterEndpoints(ctx context.Context, serviceName string, ips []string) error {
	r.log.DebugContext(ctx, "unregistering endpoints",
		"service", serviceName,
		"count", len(ips),
	)

	if len(ips) == 0 {
		return nil
	}

	// Get all protocols for this service by listing the protocols directory
	protocolsPrefix := r.protocolsPrefix(serviceName)
	resp, err := r.client.Get(ctx, protocolsPrefix, clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		r.log.ErrorContext(ctx, "failed to list protocols", "error", err, "service", serviceName)
		return fmt.Errorf("failed to list protocols: %w", err)
	}

	// Extract unique protocols from keys
	protocols := make(map[string]bool)
	for _, kv := range resp.Kvs {
		key := string(kv.Key)
		// Key format: <ownPrefix>/services/<service>/protocols/<protocol>/endpoints/<ip>
		parts := strings.Split(key, "/")
		for i, part := range parts {
			if part == "protocols" && i+1 < len(parts) {
				protocols[parts[i+1]] = true
				break
			}
		}
	}

	// Delete endpoints for each protocol
	for protocolStr := range protocols {
		protocol := parseProtocol(protocolStr)
		for _, ip := range ips {
			key := r.endpointKey(serviceName, protocol, ip)
			_, err := r.client.Delete(ctx, key)
			if err != nil {
				r.log.ErrorContext(ctx, "failed to delete endpoint", "error", err, "key", key)
				return fmt.Errorf("failed to delete endpoint %s: %w", key, err)
			}
		}
	}

	r.log.InfoContext(ctx, "endpoints unregistered successfully", "service", serviceName, "count", len(ips))
	return nil
}

// ListEndpoints retrieves all endpoints for a specific service and protocol,
// ACROSS every origin (region/cluster) — a consumer wants every endpoint of the
// service, not just this instance's own partition.
//
// Cost: because origin precedes service in the key (proposal 006, origin-first so
// the replicator mirrors one contiguous prefix), a single service's endpoints are
// NOT a contiguous range — this ranges the whole root and filters in memory, i.e.
// O(all endpoints) per call. That is acceptable and NOT worth a per-service
// secondary index: the only caller is the agent's COLD path (an ODCDS-observed
// service the re-filtered watch hasn't delivered yet, agent/internal/xds/cache/
// cluster.go), gated by the service catalog (nonexistent services cost nothing),
// hit at most once per service, and failure-tolerant (the watch catch-up repairs).
// In the deployed topology the agent uses the registrar backend (an in-memory
// per-service cache), so this etcd path runs only with --registry-backend=etcd
// directly. An index would add write-amplification to every register for a rarely
// taken read; revisit only if a hot direct-to-etcd consumer appears.
func (r *EtcdRegistry) ListEndpoints(ctx context.Context, service string, protocol registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	r.log.DebugContext(ctx, "listing endpoints", "service", service, "protocol", protocol)

	resp, err := r.client.Get(ctx, r.keyPrefix, clientv3.WithPrefix())
	if err != nil {
		r.log.ErrorContext(ctx, "failed to list endpoints", "error", err, "service", service)
		return nil, fmt.Errorf("failed to list endpoints: %w", err)
	}

	match := endpointsMatch(service, protocol)
	endpoints := make([]*registryv1.ServiceEndpoint, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		if !strings.Contains(string(kv.Key), match) {
			continue
		}
		var endpoint registryv1.ServiceEndpoint
		if err := proto.Unmarshal(kv.Value, &endpoint); err != nil {
			r.log.ErrorContext(ctx, "failed to unmarshal endpoint", "error", err, "key", string(kv.Key))
			continue
		}
		endpoints = append(endpoints, &endpoint)
	}

	r.log.DebugContext(ctx, "listed endpoints", "service", service, "protocol", protocol, "count", len(endpoints))
	return endpoints, nil
}

// ListAllEndpoints retrieves all endpoints for the given protocol across all services from etcd.
// Endpoints are organized by service name in the returned map.
func (r *EtcdRegistry) ListAllEndpoints(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	r.log.DebugContext(ctx, "listing all endpoints for protocol", "protocol", protocol)

	// Get all keys under the prefix
	resp, err := r.client.Get(ctx, r.keyPrefix, clientv3.WithPrefix())
	if err != nil {
		r.log.ErrorContext(ctx, "failed to list all endpoints", "error", err, "protocol", protocol)
		return nil, fmt.Errorf("failed to list all endpoints: %w", err)
	}

	endpointsByService := make(map[string][]*registryv1.ServiceEndpoint)
	protocolStr := protocol.String()

	for _, kv := range resp.Kvs {
		key := string(kv.Key)

		// Filter by protocol - key format:
		// <root>/<region>/clusters/<cluster>/services/<service>/protocols/<protocol>/endpoints/<ip>
		if !strings.Contains(key, fmt.Sprintf("/protocols/%s/endpoints/", protocolStr)) {
			continue
		}

		// Extract service name from key
		serviceName := r.extractServiceName(key)
		if serviceName == "" {
			continue
		}

		var endpoint registryv1.ServiceEndpoint
		if err := proto.Unmarshal(kv.Value, &endpoint); err != nil {
			r.log.ErrorContext(ctx, "failed to unmarshal endpoint", "error", err, "key", key)
			continue
		}

		endpointsByService[serviceName] = append(endpointsByService[serviceName], &endpoint)
	}

	r.log.DebugContext(ctx, "listed all endpoints", "protocol", protocol, "services", len(endpointsByService))
	return endpointsByService, nil
}

// Close closes the etcd client connection.
func (r *EtcdRegistry) Close() error {
	if r.watchCancel != nil {
		r.watchCancel()
		r.watchCancel = nil
	}
	if r.client != nil {
		err := r.client.Close()
		r.client = nil
		return err
	}
	return nil
}

// serviceMarker delimits the service-name segment within a key, after the
// origin (region/cluster) prefix.
const serviceMarker = "/services/"

// endpointKey builds the full key for one of THIS registry's own endpoints,
// under its authoritative partition.
// Format: <ownPrefix>/services/<serviceName>/protocols/<protocol>/endpoints/<ip>
func (r *EtcdRegistry) endpointKey(serviceName string, protocol registryv1.Service_Protocol, ip string) string {
	return fmt.Sprintf("%s%s%s/protocols/%s/endpoints/%s", r.ownPrefix, serviceMarker, serviceName, protocol.String(), ip)
}

// endpointsMatch is the substring an endpoint key carries for a given service
// and protocol, used to filter a root-range read across all origins.
func endpointsMatch(serviceName string, protocol registryv1.Service_Protocol) string {
	return fmt.Sprintf("%s%s/protocols/%s/endpoints/", serviceMarker, serviceName, protocol.String())
}

// protocolsPrefix builds the prefix for all protocols of one of THIS registry's
// own services (used to enumerate protocols before deleting own endpoints).
func (r *EtcdRegistry) protocolsPrefix(serviceName string) string {
	return fmt.Sprintf("%s%s%s/protocols/", r.ownPrefix, serviceMarker, serviceName)
}

// extractServiceName extracts the service name from a full key path, regardless
// of which origin (region/cluster) partition it lives under.
func (r *EtcdRegistry) extractServiceName(key string) string {
	i := strings.Index(key, serviceMarker)
	if i < 0 {
		return ""
	}
	rest := key[i+len(serviceMarker):]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// parseProtocol converts a protocol string to the enum value.
func parseProtocol(s string) registryv1.Service_Protocol {
	if val, ok := registryv1.Service_Protocol_value[s]; ok {
		return registryv1.Service_Protocol(val)
	}
	return registryv1.Service_PROTOCOL_UNSPECIFIED
}
