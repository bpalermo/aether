// Package etcd implements the Registry interface using etcd as the backend.
//
// Keys are region-scoped and origin-first (proposal 006): each region owns a
// contiguous authoritative subtree, so a cross-region replicator mirrors one
// deterministic prefix and partitions stay disjoint regardless of pod-CIDR
// overlap across clusters. Full key layout:
//
//	<root>/<region>/clusters/<cluster>/ns/<namespace>/services/<service>/protocols/<protocol>/endpoints/<ip>
//
// A registry instance OWNS one (region, cluster): it writes/deletes only under
// its own partition, but reads (List*, watch) range the whole root so consumers
// see the union of every region's local-authoritative and mirrored-in endpoints.
package etcd

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/common/serviceref"
	"github.com/bpalermo/aether/registry/export"
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

// exportsMarker delimits the export-mark segment within a key, after the origin
// (region/cluster) prefix: <ownPrefix>/exports/<service> = <namespace>.
const exportsMarker = "/exports/"

// exportKey builds the key for one of THIS instance's own export marks.
func (r *EtcdRegistry) exportKey(service string) string {
	return r.ownPrefix + exportsMarker + service
}

// SetExport records that the local cluster exports the named mesh service from
// the given namespace (Kubernetes MCS-API ServiceExport). The mark lives under
// THIS instance's own authoritative partition; the namespace is the value so a
// remote importer can materialize the ServiceImport into the right namespace.
// Satisfies registry.ServiceExporter.
func (r *EtcdRegistry) SetExport(ctx context.Context, service, namespace string) error {
	key := r.exportKey(service)
	if _, err := r.client.Put(ctx, key, namespace); err != nil {
		r.log.ErrorContext(ctx, "failed to set service export", "error", err, "service", service, "key", key)
		return fmt.Errorf("failed to set export for service %s: %w", service, err)
	}
	r.log.InfoContext(ctx, "service export set", "service", service, "namespace", namespace)
	return nil
}

// UnsetExport removes the local cluster's export mark for the named service.
// Idempotent. Satisfies registry.ServiceExporter.
func (r *EtcdRegistry) UnsetExport(ctx context.Context, service string) error {
	key := r.exportKey(service)
	if _, err := r.client.Delete(ctx, key); err != nil {
		r.log.ErrorContext(ctx, "failed to unset service export", "error", err, "service", service, "key", key)
		return fmt.Errorf("failed to unset export for service %s: %w", service, err)
	}
	r.log.InfoContext(ctx, "service export unset", "service", service)
	return nil
}

// ListExports returns every export mark across all origins (clusters) — the
// clusterset-wide export view. It ranges the whole root and parses each key's
// origin cluster and service. Satisfies registry.ServiceExporter.
func (r *EtcdRegistry) ListExports(ctx context.Context) ([]export.ServiceExport, error) {
	resp, err := r.client.Get(ctx, r.keyPrefix, clientv3.WithPrefix())
	if err != nil {
		r.log.ErrorContext(ctx, "failed to list service exports", "error", err)
		return nil, fmt.Errorf("failed to list service exports: %w", err)
	}

	exports := make([]export.ServiceExport, 0)
	for _, kv := range resp.Kvs {
		key := string(kv.Key)
		i := strings.Index(key, exportsMarker)
		if i < 0 {
			continue
		}
		service := key[i+len(exportsMarker):]
		if service == "" || strings.Contains(service, "/") {
			continue
		}
		exports = append(exports, export.ServiceExport{
			Service:   service,
			Namespace: string(kv.Value),
			Cluster:   extractCluster(key),
		})
	}

	r.log.DebugContext(ctx, "listed service exports", "count", len(exports))
	return exports, nil
}

// configMarker delimits the config-projection segment within a key, after the origin
// (region/cluster) prefix: <ownPrefix>/config/<service> = marshaled
// ServiceConfigProjection. The service ("<ns>/<svc>" route-target key) is base64url-
// encoded so its embedded "/" does not split the etcd key path.
const configMarker = "/config/"

// configKey builds the key for one of THIS instance's own config projections.
func (r *EtcdRegistry) configKey(service string) string {
	return r.ownPrefix + configMarker + base64.RawURLEncoding.EncodeToString([]byte(service))
}

// SetConfig records this cluster's projected GAMMA config for a service under its own
// authoritative partition (proposal 026). origin_cluster is stamped to this instance's
// cluster so the stored value matches the key path. Satisfies registry.ConfigExporter.
func (r *EtcdRegistry) SetConfig(ctx context.Context, projection *registryv1.ServiceConfigProjection) error {
	if projection == nil || projection.GetService() == "" {
		return fmt.Errorf("config projection requires a service")
	}
	projection.OriginCluster = r.config.Cluster
	data, err := proto.Marshal(projection)
	if err != nil {
		return fmt.Errorf("failed to marshal config projection for %s: %w", projection.GetService(), err)
	}
	key := r.configKey(projection.GetService())
	if _, err := r.client.Put(ctx, key, string(data)); err != nil {
		r.log.ErrorContext(ctx, "failed to set config projection", "error", err, "service", projection.GetService(), "key", key)
		return fmt.Errorf("failed to set config for service %s: %w", projection.GetService(), err)
	}
	r.log.InfoContext(ctx, "config projection set", "service", projection.GetService(), "version", projection.GetVersion())
	return nil
}

// UnsetConfig removes the local cluster's config projection for the named service.
// Idempotent. Satisfies registry.ConfigExporter.
func (r *EtcdRegistry) UnsetConfig(ctx context.Context, service string) error {
	key := r.configKey(service)
	if _, err := r.client.Delete(ctx, key); err != nil {
		r.log.ErrorContext(ctx, "failed to unset config projection", "error", err, "service", service, "key", key)
		return fmt.Errorf("failed to unset config for service %s: %w", service, err)
	}
	r.log.InfoContext(ctx, "config projection unset", "service", service)
	return nil
}

// ListConfig returns every config projection across all origins (clusterset-wide). It
// ranges the whole root and unmarshals each /config/ value, setting origin_cluster
// authoritatively from the key path (defending against a forged origin in the value).
// Satisfies registry.ConfigExporter.
func (r *EtcdRegistry) ListConfig(ctx context.Context) ([]*registryv1.ServiceConfigProjection, error) {
	resp, err := r.client.Get(ctx, r.keyPrefix, clientv3.WithPrefix())
	if err != nil {
		r.log.ErrorContext(ctx, "failed to list config projections", "error", err)
		return nil, fmt.Errorf("failed to list config projections: %w", err)
	}
	projections := make([]*registryv1.ServiceConfigProjection, 0)
	for _, kv := range resp.Kvs {
		key := string(kv.Key)
		if !strings.Contains(key, configMarker) {
			continue
		}
		p := &registryv1.ServiceConfigProjection{}
		if err := proto.Unmarshal(kv.Value, p); err != nil {
			r.log.WarnContext(ctx, "skipping unparseable config projection", "key", key, "error", err)
			continue
		}
		// The key path is the authority for the origin, not the stored value.
		p.OriginCluster = extractCluster(key)
		projections = append(projections, p)
	}
	r.log.DebugContext(ctx, "listed config projections", "count", len(projections))
	return projections, nil
}

// extractCluster pulls the origin cluster name from a full key path. Keys are
// <root>/<region>/clusters/<cluster>/… (proposal 006 origin-first layout).
func extractCluster(key string) string {
	const marker = "/clusters/"
	i := strings.Index(key, marker)
	if i < 0 {
		return ""
	}
	rest := key[i+len(marker):]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		return rest[:j]
	}
	return rest
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

// nsMarker / serviceMarker delimit the namespace and service segments of a key,
// after the origin (region/cluster) prefix: a service is keyed
// .../ns/<namespace>/services/<service>/... (proposal 020 Part 1, namespace-aware).
const (
	nsMarker      = "/ns/"
	serviceMarker = "/services/"
)

// serviceKeySegment renders a namespace-qualified "<ns>/<svc>" service key as the
// etcd path segment "/ns/<ns>/services/<svc>" (proposal 020 Part 1). A malformed
// (non-namespaced) key falls back to the legacy "/services/<key>" so a stray key
// still produces a usable path rather than corrupting the tree.
func serviceKeySegment(serviceName string) string {
	if ref, ok := serviceref.ParseKey(serviceName); ok {
		return nsMarker + ref.Namespace + serviceMarker + ref.Name
	}
	return serviceMarker + serviceName
}

// endpointKey builds the full key for one of THIS registry's own endpoints,
// under its authoritative partition.
// Format: <ownPrefix>/ns/<ns>/services/<svc>/protocols/<protocol>/endpoints/<ip>
func (r *EtcdRegistry) endpointKey(serviceName string, protocol registryv1.Service_Protocol, ip string) string {
	return fmt.Sprintf("%s%s/protocols/%s/endpoints/%s", r.ownPrefix, serviceKeySegment(serviceName), protocol.String(), ip)
}

// endpointsMatch is the substring an endpoint key carries for a given service
// and protocol, used to filter a root-range read across all origins.
func endpointsMatch(serviceName string, protocol registryv1.Service_Protocol) string {
	return fmt.Sprintf("%s/protocols/%s/endpoints/", serviceKeySegment(serviceName), protocol.String())
}

// protocolsPrefix builds the prefix for all protocols of one of THIS registry's
// own services (used to enumerate protocols before deleting own endpoints).
func (r *EtcdRegistry) protocolsPrefix(serviceName string) string {
	return fmt.Sprintf("%s%s/protocols/", r.ownPrefix, serviceKeySegment(serviceName))
}

// extractServiceName extracts the namespace-qualified "<ns>/<svc>" service key
// from a full key path (.../ns/<ns>/services/<svc>/...), regardless of which
// origin (region/cluster) partition it lives under. Falls back to the legacy
// namespace-free scheme for any key without the /ns/ segment.
func (r *EtcdRegistry) extractServiceName(key string) string {
	if i := strings.Index(key, nsMarker); i >= 0 {
		ns, after, found := strings.Cut(key[i+len(nsMarker):], serviceMarker)
		if found && ns != "" {
			svc := after
			if j := strings.IndexByte(svc, '/'); j >= 0 {
				svc = svc[:j]
			}
			if svc != "" {
				return serviceref.New(ns, svc).Key()
			}
		}
		return ""
	}
	// Legacy namespace-free fallback.
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
