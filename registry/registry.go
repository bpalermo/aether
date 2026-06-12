// Package registry provides interfaces for service endpoint registration and discovery.
// It manages the lifecycle of service endpoints and allows querying available services
// and their endpoints. Implementations can use different backends (e.g., DynamoDB).
package registry

import (
	"context"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
)

// Registry manages service endpoint registration, deregistration, and discovery.
// It maintains a list of service endpoints and supports querying by service name and protocol.
type Registry interface {
	// Initialize initializes the registry. This should be called before using other methods.
	Initialize(ctx context.Context) error
	// Close releases any resources held by the registry.
	Close() error
	// RegisterEndpoint registers a single endpoint for a service with a specific protocol.
	RegisterEndpoint(ctx context.Context, serviceName string, protocol registryv1.Service_Protocol, endpoint *registryv1.ServiceEndpoint) error
	// UnregisterEndpoint unregisters an endpoint for a service by its IP address.
	UnregisterEndpoint(ctx context.Context, serviceName string, ip string) error
	// UnregisterEndpoints unregisters multiple endpoints for a service by their IP addresses.
	UnregisterEndpoints(ctx context.Context, serviceName string, ips []string) error
	// ListEndpoints returns all endpoints for a service with a specific protocol.
	ListEndpoints(ctx context.Context, service string, protocol registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error)
	// ListAllEndpoints returns all endpoints for all services of a specific protocol,
	// organized in a map keyed by service name.
	ListAllEndpoints(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error)
}

// ChangeNotifier is an optional capability for Registry implementations that can
// push change notifications. Backends that maintain a live view of the registry
// (e.g. the registrar watch stream) implement it so consumers can rebuild derived
// state — such as the agent's xDS cluster/endpoint/route snapshot — when endpoints
// change, instead of only at startup.
//
// Signals are coalesced: each receive means "the endpoint set changed, re-read the
// registry", not a per-event notification.
type ChangeNotifier interface {
	// Changes returns a channel that receives a signal whenever the set of
	// endpoints changes.
	Changes() <-chan struct{}
}

// ReadyWaiter is an optional capability for Registry implementations whose
// reads are served from an asynchronously populated cache (the registrar
// watch client). WaitReady blocks until the cache holds a complete snapshot
// or ctx ends; callers bound it with a timeout and may proceed with degraded
// reads on expiry. Synchronous backends (DynamoDB, etcd) do not implement it.
type ReadyWaiter interface {
	WaitReady(ctx context.Context) error
}

// ReconnectNotifier is an optional capability for Registry implementations
// backed by a watch stream. Reconnects signals (coalesced) each successful
// stream (re)connection; consumers re-assert state the far side may have lost
// across the reconnect (e.g. the agent re-registers its local pods, repairing
// a failed-over registrar's lost write-behind intents at reconnect speed).
type ReconnectNotifier interface {
	Reconnects() <-chan struct{}
}

// WatchScoper is an optional capability for Registry implementations backed
// by a watch stream. SetServiceFilter scopes the watch to the given services
// (the node's dependency set -- demand-scoped distribution): the registrar
// then fans out an endpoint change only to that service's consumers. nil
// restores the full watch; an empty non-nil set watches nothing. The
// implementation re-asserts the filter on every reconnect.
type WatchScoper interface {
	SetServiceFilter(services []string)
}

// AuthoritativeLister is an optional capability for Registry implementations
// whose ListAllEndpoints may serve from a local watch-fed cache. It lists from
// the authoritative source (an RPC to the registrar, the external registry),
// bypassing any cache. Reconciliation that decides what to (de)register must
// use this when present: a watch cache can be a stale superset of a fresh or
// failed-over registrar's snapshot — an empty snapshot emits no events, so the
// cache keeps the old world and a cache-based diff concludes nothing is
// missing (2026-06-11: backend switch left the registry empty while every
// agent's re-assert no-op'd against its own stale cache).
type AuthoritativeLister interface {
	ListAllEndpointsAuthoritative(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error)
}
