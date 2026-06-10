package cloudmap

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
)

// fakeClient is an in-memory implementation of the Client interface for testing.
// It simulates AWS Cloud Map's HTTP namespace behavior including namespace/service
// resolution, instance registration, and DiscoverInstances with QueryParameters filtering.
type fakeClient struct {
	mu sync.RWMutex

	// namespaces maps namespace name -> namespace ID
	namespaces map[string]string
	// services maps namespaceID -> (serviceName -> serviceID)
	services map[string]map[string]string
	// instances maps serviceID -> (instanceID -> attributes)
	instances map[string]map[string]map[string]string
	// health maps serviceID -> (instanceID -> custom health status)
	health map[string]map[string]types.HealthStatus
	// serviceNameByID maps serviceID -> serviceName (reverse lookup)
	serviceNameByID map[string]string
	// namespaceIDByServiceID maps serviceID -> namespaceID
	namespaceIDByServiceID map[string]string

	nextServiceID int
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		namespaces:             make(map[string]string),
		services:               make(map[string]map[string]string),
		instances:              make(map[string]map[string]map[string]string),
		health:                 make(map[string]map[string]types.HealthStatus),
		serviceNameByID:        make(map[string]string),
		namespaceIDByServiceID: make(map[string]string),
	}
}

// addNamespace pre-seeds a namespace (simulates CreateHttpNamespace).
func (f *fakeClient) addNamespace(name, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces[name] = id
	if f.services[id] == nil {
		f.services[id] = make(map[string]string)
	}
}

func (f *fakeClient) ListNamespaces(_ context.Context, _ *servicediscovery.ListNamespacesInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.ListNamespacesOutput, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Cloud Map's ListNamespaces only supports a TYPE filter (no name filter), so
	// the registry lists unfiltered and matches by name client-side. Mirror that
	// here by returning every namespace regardless of any filters on the input.
	nsList := make([]types.NamespaceSummary, 0, len(f.namespaces))
	for name, id := range f.namespaces {
		nsList = append(nsList, types.NamespaceSummary{
			Name: aws.String(name),
			Id:   aws.String(id),
		})
	}

	return &servicediscovery.ListNamespacesOutput{
		Namespaces: nsList,
	}, nil
}

func (f *fakeClient) ListServices(_ context.Context, input *servicediscovery.ListServicesInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.ListServicesOutput, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Find namespace ID from filters
	var nsID string
	for _, filter := range input.Filters {
		if filter.Name == types.ServiceFilterNameNamespaceId && len(filter.Values) > 0 {
			nsID = filter.Values[0]
		}
	}

	var svcList []types.ServiceSummary
	if nsServices, ok := f.services[nsID]; ok {
		for name, id := range nsServices {
			svcList = append(svcList, types.ServiceSummary{
				Name: aws.String(name),
				Id:   aws.String(id),
			})
		}
	}

	return &servicediscovery.ListServicesOutput{
		Services: svcList,
	}, nil
}

func (f *fakeClient) CreateService(_ context.Context, input *servicediscovery.CreateServiceInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.CreateServiceOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	nsID := aws.ToString(input.NamespaceId)
	name := aws.ToString(input.Name)

	if _, ok := f.services[nsID]; !ok {
		return nil, fmt.Errorf("namespace %s not found", nsID)
	}

	// Check if service already exists
	if id, exists := f.services[nsID][name]; exists {
		return &servicediscovery.CreateServiceOutput{
			Service: &types.Service{
				Id:   aws.String(id),
				Name: aws.String(name),
			},
		}, nil
	}

	f.nextServiceID++
	svcID := fmt.Sprintf("svc-%d", f.nextServiceID)

	f.services[nsID][name] = svcID
	f.serviceNameByID[svcID] = name
	f.namespaceIDByServiceID[svcID] = nsID
	f.instances[svcID] = make(map[string]map[string]string)

	return &servicediscovery.CreateServiceOutput{
		Service: &types.Service{
			Id:   aws.String(svcID),
			Name: aws.String(name),
		},
	}, nil
}

func (f *fakeClient) RegisterInstance(_ context.Context, input *servicediscovery.RegisterInstanceInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.RegisterInstanceOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	svcID := aws.ToString(input.ServiceId)
	instID := aws.ToString(input.InstanceId)

	svcInstances, ok := f.instances[svcID]
	if !ok {
		return nil, fmt.Errorf("service %s not found", svcID)
	}

	// AWS_INIT_HEALTH_STATUS seeds custom health for brand-new instances only
	// (matching Cloud Map: re-registrations keep the current health).
	if _, exists := svcInstances[instID]; !exists {
		if f.health[svcID] == nil {
			f.health[svcID] = make(map[string]types.HealthStatus)
		}
		switch input.Attributes["AWS_INIT_HEALTH_STATUS"] {
		case "UNHEALTHY":
			f.health[svcID][instID] = types.HealthStatusUnhealthy
		default:
			f.health[svcID][instID] = types.HealthStatusHealthy
		}
	}

	// Copy attributes (upsert semantics)
	attrs := make(map[string]string, len(input.Attributes))
	for k, v := range input.Attributes {
		attrs[k] = v
	}
	svcInstances[instID] = attrs

	return &servicediscovery.RegisterInstanceOutput{}, nil
}

func (f *fakeClient) UpdateInstanceCustomHealthStatus(_ context.Context, input *servicediscovery.UpdateInstanceCustomHealthStatusInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.UpdateInstanceCustomHealthStatusOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	svcID := aws.ToString(input.ServiceId)
	instID := aws.ToString(input.InstanceId)
	if _, ok := f.instances[svcID][instID]; !ok {
		return nil, fmt.Errorf("instance %s not found in service %s", instID, svcID)
	}
	if f.health[svcID] == nil {
		f.health[svcID] = make(map[string]types.HealthStatus)
	}
	if input.Status == types.CustomHealthStatusUnhealthy {
		f.health[svcID][instID] = types.HealthStatusUnhealthy
	} else {
		f.health[svcID][instID] = types.HealthStatusHealthy
	}
	return &servicediscovery.UpdateInstanceCustomHealthStatusOutput{}, nil
}

func (f *fakeClient) DeregisterInstance(_ context.Context, input *servicediscovery.DeregisterInstanceInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.DeregisterInstanceOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	svcID := aws.ToString(input.ServiceId)
	instID := aws.ToString(input.InstanceId)

	if svcInstances, ok := f.instances[svcID]; ok {
		delete(svcInstances, instID)
	}

	return &servicediscovery.DeregisterInstanceOutput{}, nil
}

func (f *fakeClient) DiscoverInstances(_ context.Context, input *servicediscovery.DiscoverInstancesInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.DiscoverInstancesOutput, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	nsName := aws.ToString(input.NamespaceName)
	svcName := aws.ToString(input.ServiceName)

	// Resolve namespace
	nsID, ok := f.namespaces[nsName]
	if !ok {
		return nil, fmt.Errorf("namespace %s not found", nsName)
	}

	// Resolve service
	svcID, ok := f.services[nsID][svcName]
	if !ok {
		// Service doesn't exist — return empty (not an error in real Cloud Map)
		return &servicediscovery.DiscoverInstancesOutput{}, nil
	}

	var results []types.HttpInstanceSummary
	for instID, attrs := range f.instances[svcID] {
		// Apply QueryParameters filter (AND semantics, matching Cloud Map behavior)
		if !matchesQueryParams(attrs, input.QueryParameters) {
			continue
		}
		results = append(results, types.HttpInstanceSummary{
			InstanceId:   aws.String(instID),
			Attributes:   copyAttrs(attrs),
			HealthStatus: f.health[svcID][instID],
		})
	}

	return &servicediscovery.DiscoverInstancesOutput{
		Instances: results,
	}, nil
}

// matchesQueryParams returns true if all query parameter key-value pairs
// are present in the instance attributes. This matches Cloud Map's AND filter behavior.
func matchesQueryParams(attrs map[string]string, queryParams map[string]string) bool {
	for k, v := range queryParams {
		if attrs[k] != v {
			return false
		}
	}
	return true
}

func copyAttrs(attrs map[string]string) map[string]string {
	cp := make(map[string]string, len(attrs))
	for k, v := range attrs {
		cp[k] = v
	}
	return cp
}
