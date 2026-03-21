package cloudmap

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
)

// CloudMapRegistry is a Registry implementation backed by AWS Cloud Map.
// It stores service endpoints as Cloud Map instances within an HTTP namespace,
// using a poll-based discovery approach inspired by the AWS Cloud Map MCS Controller.
type CloudMapRegistry struct {
	log    logr.Logger
	client Client

	namespace   string
	clusterName string

	namespaceTTL time.Duration
	serviceTTL   time.Duration
	endpointTTL  time.Duration

	// namespaceID is resolved during Start and cached for the lifetime of the registry.
	namespaceID string

	// serviceCache maps service names to Cloud Map service IDs.
	serviceCache *ttlCache[string]
	// endpointCache maps "serviceName/protocol" to discovered endpoints.
	endpointCache *ttlCache[[]*registryv1.ServiceEndpoint]
}

// NewCloudMapRegistry creates a new Cloud Map-backed Registry.
func NewCloudMapRegistry(log logr.Logger, awsCfg aws.Config, clusterName string, opts ...Option) *CloudMapRegistry {
	r := &CloudMapRegistry{
		log:          log.WithName("registry-cloudmap"),
		client:       servicediscovery.NewFromConfig(awsCfg),
		namespace:   DefaultNamespace,
		clusterName: clusterName,
		namespaceTTL: DefaultNamespaceTTL,
		serviceTTL:   DefaultServiceTTL,
		endpointTTL:  DefaultEndpointTTL,
	}
	for _, opt := range opts {
		opt(r)
	}

	r.serviceCache = newTTLCache[string](r.serviceTTL)
	r.endpointCache = newTTLCache[[]*registryv1.ServiceEndpoint](r.endpointTTL)

	return r
}

// Initialize resolves the Cloud Map HTTP namespace.
func (r *CloudMapRegistry) Initialize(ctx context.Context) error {
	r.log.V(1).Info("initializing registry and resolving namespace", "namespace", r.namespace)

	nsID, err := r.resolveNamespaceID(ctx)
	if err != nil {
		return fmt.Errorf("failed to resolve namespace %q: %w", r.namespace, err)
	}
	r.namespaceID = nsID

	r.log.Info("Cloud Map registry initialized", "namespace", r.namespace, "namespaceID", r.namespaceID)
	return nil
}

// Close is a no-op for the Cloud Map registry as it uses stateless HTTP clients.
func (r *CloudMapRegistry) Close() error {
	return nil
}

// RegisterEndpoint registers a single endpoint for a service with a specific protocol.
func (r *CloudMapRegistry) RegisterEndpoint(ctx context.Context, serviceName string, protocol registryv1.Service_Protocol, endpoint *registryv1.ServiceEndpoint) error {
	ip := endpoint.GetIp()
	r.log.V(1).Info(
		"registering endpoint",
		"service", serviceName,
		"protocol", protocol,
		"cluster", endpoint.GetClusterName(),
		"ip", ip,
	)

	svcID, err := r.resolveOrCreateServiceID(ctx, serviceName)
	if err != nil {
		return fmt.Errorf("failed to resolve service %q: %w", serviceName, err)
	}

	attrs := marshalAttrs(protocol, endpoint)
	instID := instanceID(r.clusterName, ip)

	_, err = r.client.RegisterInstance(ctx, &servicediscovery.RegisterInstanceInput{
		ServiceId:  aws.String(svcID),
		InstanceId: aws.String(instID),
		Attributes: attrs,
	})
	if err != nil {
		r.log.Error(err, "failed to register instance", "service", serviceName, "ip", ip)
		return fmt.Errorf("failed to register instance for IP %s: %w", ip, err)
	}

	// Evict endpoint cache for this service (all protocols)
	r.evictEndpointCache(serviceName)

	r.log.Info(
		"endpoint registered successfully",
		"service", serviceName,
		"cluster", endpoint.GetClusterName(),
		"ip", ip,
	)
	return nil
}

// UnregisterEndpoint removes a single endpoint from the registry for all protocols.
func (r *CloudMapRegistry) UnregisterEndpoint(ctx context.Context, serviceName string, ip string) error {
	return r.UnregisterEndpoints(ctx, serviceName, []string{ip})
}

// UnregisterEndpoints removes multiple endpoints from the registry for all protocols.
func (r *CloudMapRegistry) UnregisterEndpoints(ctx context.Context, serviceName string, ips []string) error {
	r.log.V(1).Info("unregistering endpoints",
		"service", serviceName,
		"count", len(ips),
	)

	if len(ips) == 0 {
		return nil
	}

	svcID, err := r.resolveServiceID(ctx, serviceName)
	if err != nil {
		r.log.Error(err, "failed to resolve service for deregister", "service", serviceName)
		return fmt.Errorf("failed to resolve service %q: %w", serviceName, err)
	}
	if svcID == "" {
		r.log.V(1).Info("service not found, nothing to deregister", "service", serviceName)
		return nil
	}

	for _, ip := range ips {
		instID := instanceID(r.clusterName, ip)
		_, err := r.client.DeregisterInstance(ctx, &servicediscovery.DeregisterInstanceInput{
			ServiceId:  aws.String(svcID),
			InstanceId: aws.String(instID),
		})
		if err != nil {
			r.log.Error(err, "failed to deregister instance", "service", serviceName, "ip", ip)
			return fmt.Errorf("failed to deregister instance for IP %s: %w", ip, err)
		}
	}

	// Evict endpoint cache for this service (all protocols)
	r.evictEndpointCache(serviceName)

	r.log.Info("endpoints unregistered successfully", "service", serviceName, "count", len(ips))
	return nil
}

// ListEndpoints returns all endpoints for a service with a specific protocol.
func (r *CloudMapRegistry) ListEndpoints(ctx context.Context, service string, protocol registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	r.log.V(1).Info("listing endpoints", "service", service, "protocol", protocol)

	cacheKey := endpointCacheKey(service, protocol)

	// Check endpoint cache
	if cached, ok := r.endpointCache.get(cacheKey); ok {
		r.log.V(1).Info("listed endpoints (cached)", "service", service, "protocol", protocol, "count", len(cached))
		return cached, nil
	}

	endpoints, err := r.discoverEndpoints(ctx, service, protocol)
	if err != nil {
		return nil, err
	}

	r.endpointCache.set(cacheKey, endpoints)

	r.log.V(1).Info("listed endpoints", "service", service, "protocol", protocol, "count", len(endpoints))
	return endpoints, nil
}

// ListAllEndpoints returns all endpoints for all services of a specific protocol.
func (r *CloudMapRegistry) ListAllEndpoints(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	r.log.V(1).Info("listing all endpoints for protocol", "protocol", protocol)

	serviceNames, err := r.listServiceNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list services: %w", err)
	}

	endpointsByService := make(map[string][]*registryv1.ServiceEndpoint)
	for _, svcName := range serviceNames {
		eps, err := r.ListEndpoints(ctx, svcName, protocol)
		if err != nil {
			r.log.Error(err, "failed to list endpoints for service", "service", svcName)
			continue
		}
		if len(eps) > 0 {
			endpointsByService[svcName] = eps
		}
	}

	r.log.V(1).Info("listed all endpoints", "protocol", protocol, "services", len(endpointsByService))
	return endpointsByService, nil
}

// resolveNamespaceID finds the Cloud Map namespace ID for the configured namespace name.
func (r *CloudMapRegistry) resolveNamespaceID(ctx context.Context) (string, error) {
	paginator := servicediscovery.NewListNamespacesPaginator(r.client, &servicediscovery.ListNamespacesInput{
		Filters: []types.NamespaceFilter{
			{
				Name:      types.NamespaceFilterNameType,
				Values:    []string{r.namespace},
				Condition: types.FilterConditionEq,
			},
		},
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to list namespaces: %w", err)
		}
		for _, ns := range page.Namespaces {
			if aws.ToString(ns.Name) == r.namespace {
				return aws.ToString(ns.Id), nil
			}
		}
	}

	return "", fmt.Errorf("namespace %q not found", r.namespace)
}

// resolveServiceID looks up the Cloud Map service ID for a service name (read-only).
// Returns empty string if the service does not exist.
func (r *CloudMapRegistry) resolveServiceID(ctx context.Context, serviceName string) (string, error) {
	if id, ok := r.serviceCache.get(serviceName); ok {
		return id, nil
	}

	paginator := servicediscovery.NewListServicesPaginator(r.client, &servicediscovery.ListServicesInput{
		Filters: []types.ServiceFilter{
			{
				Name:      types.ServiceFilterNameNamespaceId,
				Values:    []string{r.namespaceID},
				Condition: types.FilterConditionEq,
			},
		},
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to list services: %w", err)
		}
		for _, svc := range page.Services {
			name := aws.ToString(svc.Name)
			id := aws.ToString(svc.Id)
			r.serviceCache.set(name, id)
			if name == serviceName {
				return id, nil
			}
		}
	}

	return "", nil
}

// resolveOrCreateServiceID resolves the service ID, creating the service if it doesn't exist.
func (r *CloudMapRegistry) resolveOrCreateServiceID(ctx context.Context, serviceName string) (string, error) {
	id, err := r.resolveServiceID(ctx, serviceName)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}

	out, err := r.client.CreateService(ctx, &servicediscovery.CreateServiceInput{
		Name:        aws.String(serviceName),
		NamespaceId: aws.String(r.namespaceID),
	})
	if err != nil {
		return "", fmt.Errorf("failed to create service %q: %w", serviceName, err)
	}

	id = aws.ToString(out.Service.Id)
	r.serviceCache.set(serviceName, id)
	return id, nil
}

// discoverEndpoints polls Cloud Map for instances of a service filtered by protocol.
// Uses DiscoverInstances with QueryParameters to filter by protocol and optionally by cluster.
func (r *CloudMapRegistry) discoverEndpoints(ctx context.Context, serviceName string, protocol registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	out, err := r.client.DiscoverInstances(ctx, &servicediscovery.DiscoverInstancesInput{
		NamespaceName: aws.String(r.namespace),
		ServiceName:   aws.String(serviceName),
		HealthStatus:  types.HealthStatusFilterAll,
		QueryParameters: map[string]string{
			attrProtocol: protocol.String(),
		},
	})
	if err != nil {
		r.log.Error(err, "failed to discover instances", "service", serviceName)
		return nil, fmt.Errorf("failed to discover instances for %q: %w", serviceName, err)
	}

	endpoints := make([]*registryv1.ServiceEndpoint, 0, len(out.Instances))
	for _, inst := range out.Instances {
		ep, err := unmarshalEndpointFromSummary(inst)
		if err != nil {
			r.log.Error(err, "failed to unmarshal instance", "instanceId", aws.ToString(inst.InstanceId))
			continue
		}
		endpoints = append(endpoints, ep)
	}

	return endpoints, nil
}

// listServiceNames returns the names of all services in the namespace.
func (r *CloudMapRegistry) listServiceNames(ctx context.Context) ([]string, error) {
	paginator := servicediscovery.NewListServicesPaginator(r.client, &servicediscovery.ListServicesInput{
		Filters: []types.ServiceFilter{
			{
				Name:      types.ServiceFilterNameNamespaceId,
				Values:    []string{r.namespaceID},
				Condition: types.FilterConditionEq,
			},
		},
	})

	var names []string
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list services: %w", err)
		}
		for _, svc := range page.Services {
			name := aws.ToString(svc.Name)
			id := aws.ToString(svc.Id)
			r.serviceCache.set(name, id)
			names = append(names, name)
		}
	}

	return names, nil
}

// endpointCacheKey builds a cache key for endpoint lookups keyed by service and protocol.
func endpointCacheKey(service string, protocol registryv1.Service_Protocol) string {
	return fmt.Sprintf("%s/%s", service, protocol.String())
}

// evictEndpointCache removes all cached endpoint entries for a service (all protocols).
// Since cache keys are "serviceName/protocol", we iterate known protocol values.
func (r *CloudMapRegistry) evictEndpointCache(serviceName string) {
	for _, proto := range registryv1.Service_Protocol_value {
		r.endpointCache.delete(endpointCacheKey(serviceName, registryv1.Service_Protocol(proto)))
	}
}
