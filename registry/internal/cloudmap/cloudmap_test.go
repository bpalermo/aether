package cloudmap

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"
)

var (
	testAWSCfg     aws.Config
	testNamespace  = "aether-test"
	testClusterPri = "cluster-primary"
	testClusterSec = "cluster-secondary"
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	container, err := localstack.Run(ctx,
		"localstack/localstack:4.4.0",
		testcontainers.WithEnv(map[string]string{
			"SERVICES": "servicediscovery",
		}),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start localstack container: %v\n", err)
		os.Exit(1)
	}

	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		fmt.Fprintf(os.Stderr, "failed to get host: %v\n", err)
		os.Exit(1)
	}

	port, err := container.MappedPort(ctx, "4566/tcp")
	if err != nil {
		_ = container.Terminate(ctx)
		fmt.Fprintf(os.Stderr, "failed to get mapped port: %v\n", err)
		os.Exit(1)
	}

	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	testAWSCfg = aws.Config{
		Region: "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(
			"test", "test", ""),
		BaseEndpoint: aws.String(endpoint),
	}

	// Create the HTTP namespace used by all tests.
	if err := createHTTPNamespace(ctx, testNamespace); err != nil {
		_ = container.Terminate(ctx)
		fmt.Fprintf(os.Stderr, "failed to create namespace: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	_ = container.Terminate(ctx)
	os.Exit(code)
}

// createHTTPNamespace creates an HTTP namespace in Cloud Map and waits for the
// operation to complete. Cloud Map namespace creation is asynchronous.
func createHTTPNamespace(ctx context.Context, name string) error {
	client := servicediscovery.NewFromConfig(testAWSCfg)

	out, err := client.CreateHttpNamespace(ctx, &servicediscovery.CreateHttpNamespaceInput{
		Name: aws.String(name),
	})
	if err != nil {
		return fmt.Errorf("CreateHttpNamespace: %w", err)
	}

	operationID := aws.ToString(out.OperationId)
	return waitForOperation(ctx, client, operationID, 30*time.Second)
}

// waitForOperation polls a Cloud Map operation until it succeeds or the timeout elapses.
func waitForOperation(ctx context.Context, client *servicediscovery.Client, operationID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.GetOperation(ctx, &servicediscovery.GetOperationInput{
			OperationId: aws.String(operationID),
		})
		if err != nil {
			return fmt.Errorf("GetOperation: %w", err)
		}

		switch resp.Operation.Status {
		case "SUCCESS":
			return nil
		case "FAIL":
			return fmt.Errorf("operation %s failed: %s", operationID, aws.ToString(resp.Operation.ErrorMessage))
		}

		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("operation %s timed out after %v", operationID, timeout)
}

// setupRegistry creates a CloudMapRegistry pointed at the test namespace with
// zero-duration cache TTLs so every call hits Cloud Map directly.
func setupRegistry(ctx context.Context, t *testing.T, clusterName string) *CloudMapRegistry {
	t.Helper()

	reg := NewCloudMapRegistry(
		logr.Discard(),
		testAWSCfg,
		clusterName,
		WithNamespace(testNamespace),
		WithEndpointTTL(0),
		WithServiceTTL(0),
		WithNamespaceTTL(0),
	)

	require.NoError(t, reg.Start(ctx))
	return reg
}

// --- Test: Start ---

func TestCloudMapRegistry_Start(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()

	t.Run("successful start with existing namespace", func(t *testing.T) {
		reg := NewCloudMapRegistry(
			logr.Discard(),
			testAWSCfg,
			testClusterPri,
			WithNamespace(testNamespace),
		)

		err := reg.Start(ctx)
		assert.NoError(t, err)
	})

	t.Run("fails when namespace does not exist", func(t *testing.T) {
		reg := NewCloudMapRegistry(
			logr.Discard(),
			testAWSCfg,
			testClusterPri,
			WithNamespace("nonexistent-namespace"),
		)

		err := reg.Start(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

// --- Test: RegisterEndpoint ---

func TestCloudMapRegistry_RegisterEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	reg := setupRegistry(ctx, t, testClusterPri)

	ep := &registryv1.ServiceEndpoint{
		Ip:          "10.0.1.5",
		ClusterName: testClusterPri,
		Port:        8080,
		Weight:      100,
		Locality: &registryv1.ServiceEndpoint_Locality{
			Region: "us-east-1",
			Zone:   "us-east-1a",
		},
		Metadata: map[string]string{
			"version": "v1",
		},
		ContainerMetadata: &registryv1.ServiceEndpoint_ContainerMetadata{
			ContainerId:      "container-123",
			NetworkNamespace: "/proc/1234/ns/net",
		},
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: "default",
			PodName:   "test-pod",
			NodeName:  "node-1",
		},
	}

	err := reg.RegisterEndpoint(ctx, "frontend-reg", registryv1.Service_HTTP, ep)
	require.NoError(t, err)

	endpoints, err := reg.ListEndpoints(ctx, "frontend-reg", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, endpoints, 1)

	got := endpoints[0]
	assert.Equal(t, ep.Ip, got.Ip)
	assert.Equal(t, ep.ClusterName, got.ClusterName)
	assert.Equal(t, ep.Port, got.Port)
	assert.Equal(t, ep.Weight, got.Weight)
	assert.Equal(t, ep.Locality.Region, got.Locality.Region)
	assert.Equal(t, ep.Locality.Zone, got.Locality.Zone)
	assert.Equal(t, ep.Metadata["version"], got.Metadata["version"])
	assert.Equal(t, ep.ContainerMetadata.ContainerId, got.ContainerMetadata.ContainerId)
	assert.Equal(t, ep.ContainerMetadata.NetworkNamespace, got.ContainerMetadata.NetworkNamespace)
	assert.Equal(t, ep.KubernetesMetadata.Namespace, got.KubernetesMetadata.Namespace)
	assert.Equal(t, ep.KubernetesMetadata.PodName, got.KubernetesMetadata.PodName)
	assert.Equal(t, ep.KubernetesMetadata.NodeName, got.KubernetesMetadata.NodeName)
}

// --- Test: RegisterEndpoint upsert ---

func TestCloudMapRegistry_RegisterEndpoint_Upsert(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	reg := setupRegistry(ctx, t, testClusterPri)

	ep1 := &registryv1.ServiceEndpoint{
		Ip:          "10.0.2.1",
		ClusterName: testClusterPri,
		Port:        8080,
		Weight:      100,
	}
	err := reg.RegisterEndpoint(ctx, "svc-upsert", registryv1.Service_HTTP, ep1)
	require.NoError(t, err)

	// Re-register the same IP with different attributes (upsert).
	ep2 := &registryv1.ServiceEndpoint{
		Ip:          "10.0.2.1",
		ClusterName: testClusterPri,
		Port:        9090,
		Weight:      200,
	}
	err = reg.RegisterEndpoint(ctx, "svc-upsert", registryv1.Service_HTTP, ep2)
	require.NoError(t, err)

	endpoints, err := reg.ListEndpoints(ctx, "svc-upsert", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, endpoints, 1)

	assert.Equal(t, "10.0.2.1", endpoints[0].Ip)
	assert.Equal(t, uint32(9090), endpoints[0].Port)
	assert.Equal(t, uint32(200), endpoints[0].Weight)
}

// --- Test: UnregisterEndpoint ---

func TestCloudMapRegistry_UnregisterEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	reg := setupRegistry(ctx, t, testClusterPri)

	eps := []*registryv1.ServiceEndpoint{
		{Ip: "10.0.3.1", ClusterName: testClusterPri, Port: 8080, Weight: 100},
		{Ip: "10.0.3.2", ClusterName: testClusterPri, Port: 8080, Weight: 100},
	}
	for _, ep := range eps {
		require.NoError(t, reg.RegisterEndpoint(ctx, "svc-unreg", registryv1.Service_HTTP, ep))
	}

	err := reg.UnregisterEndpoint(ctx, "svc-unreg", "10.0.3.1")
	require.NoError(t, err)

	listed, err := reg.ListEndpoints(ctx, "svc-unreg", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "10.0.3.2", listed[0].Ip)
}

// --- Test: UnregisterEndpoints ---

func TestCloudMapRegistry_UnregisterEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	reg := setupRegistry(ctx, t, testClusterPri)

	t.Run("removes multiple endpoints", func(t *testing.T) {
		eps := []*registryv1.ServiceEndpoint{
			{Ip: "10.0.4.1", ClusterName: testClusterPri, Port: 8080, Weight: 100},
			{Ip: "10.0.4.2", ClusterName: testClusterPri, Port: 8080, Weight: 100},
			{Ip: "10.0.4.3", ClusterName: testClusterPri, Port: 8080, Weight: 100},
		}
		for _, ep := range eps {
			require.NoError(t, reg.RegisterEndpoint(ctx, "svc-unregs", registryv1.Service_HTTP, ep))
		}

		err := reg.UnregisterEndpoints(ctx, "svc-unregs", []string{"10.0.4.1", "10.0.4.3"})
		require.NoError(t, err)

		listed, err := reg.ListEndpoints(ctx, "svc-unregs", registryv1.Service_HTTP)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, "10.0.4.2", listed[0].Ip)
	})

	t.Run("no-op with empty slice", func(t *testing.T) {
		err := reg.UnregisterEndpoints(ctx, "any-service", []string{})
		assert.NoError(t, err)
	})

	t.Run("no-op when service does not exist", func(t *testing.T) {
		err := reg.UnregisterEndpoints(ctx, "nonexistent-svc", []string{"1.2.3.4"})
		assert.NoError(t, err)
	})
}

// --- Test: ListEndpoints by protocol ---

func TestCloudMapRegistry_ListEndpoints_ByProtocol(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	reg := setupRegistry(ctx, t, testClusterPri)

	httpEP := &registryv1.ServiceEndpoint{
		Ip:          "10.0.5.1",
		ClusterName: testClusterPri,
		Port:        8080,
		Weight:      100,
	}

	require.NoError(t, reg.RegisterEndpoint(ctx, "svc-proto", registryv1.Service_HTTP, httpEP))

	// Listing with the correct protocol returns the endpoint.
	httpEndpoints, err := reg.ListEndpoints(ctx, "svc-proto", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, httpEndpoints, 1)
	assert.Equal(t, "10.0.5.1", httpEndpoints[0].Ip)

	// Listing with a different protocol returns no endpoints.
	otherEndpoints, err := reg.ListEndpoints(ctx, "svc-proto", registryv1.Service_PROTOCOL_UNSPECIFIED)
	require.NoError(t, err)
	assert.Empty(t, otherEndpoints)
}

// --- Test: ListEndpoints multi-cluster ---

func TestCloudMapRegistry_ListEndpoints_MultiCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()

	// Two registries simulate two clusters writing to the same namespace.
	regA := setupRegistry(ctx, t, testClusterPri)
	regB := setupRegistry(ctx, t, testClusterSec)

	epA := &registryv1.ServiceEndpoint{
		Ip:          "10.1.0.1",
		ClusterName: testClusterPri,
		Port:        8080,
		Weight:      100,
	}
	epB := &registryv1.ServiceEndpoint{
		Ip:          "10.2.0.1",
		ClusterName: testClusterSec,
		Port:        8080,
		Weight:      100,
	}

	require.NoError(t, regA.RegisterEndpoint(ctx, "svc-multi", registryv1.Service_HTTP, epA))
	require.NoError(t, regB.RegisterEndpoint(ctx, "svc-multi", registryv1.Service_HTTP, epB))

	// Either registry should see both endpoints because DiscoverInstances is
	// not scoped by cluster -- all instances in the service are returned.
	endpoints, err := regA.ListEndpoints(ctx, "svc-multi", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, endpoints, 2)

	ips := map[string]bool{}
	for _, ep := range endpoints {
		ips[ep.Ip] = true
	}
	assert.True(t, ips["10.1.0.1"], "expected endpoint from cluster-primary")
	assert.True(t, ips["10.2.0.1"], "expected endpoint from cluster-secondary")
}

// --- Test: ListAllEndpoints ---

func TestCloudMapRegistry_ListAllEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	reg := setupRegistry(ctx, t, testClusterPri)

	services := map[string][]*registryv1.ServiceEndpoint{
		"svc-all-fe": {
			{Ip: "10.0.6.1", ClusterName: testClusterPri, Port: 8080, Weight: 100},
			{Ip: "10.0.6.2", ClusterName: testClusterPri, Port: 8080, Weight: 100},
		},
		"svc-all-be": {
			{Ip: "10.0.7.1", ClusterName: testClusterPri, Port: 9090, Weight: 100},
		},
		"svc-all-db": {
			{Ip: "10.0.8.1", ClusterName: testClusterPri, Port: 5432, Weight: 100},
			{Ip: "10.0.8.2", ClusterName: testClusterPri, Port: 5432, Weight: 100},
			{Ip: "10.0.8.3", ClusterName: testClusterPri, Port: 5432, Weight: 100},
		},
	}

	for svcName, eps := range services {
		for _, ep := range eps {
			require.NoError(t, reg.RegisterEndpoint(ctx, svcName, registryv1.Service_HTTP, ep))
		}
	}

	allEndpoints, err := reg.ListAllEndpoints(ctx, registryv1.Service_HTTP)
	require.NoError(t, err)

	// The namespace is shared across all tests, so there may be services from
	// other tests. Verify at least the three services we created are present
	// with the expected endpoint counts.
	assert.GreaterOrEqual(t, len(allEndpoints), 3, "expected at least 3 services")
	assert.Len(t, allEndpoints["svc-all-fe"], 2)
	assert.Len(t, allEndpoints["svc-all-be"], 1)
	assert.Len(t, allEndpoints["svc-all-db"], 3)
}
