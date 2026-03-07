package ddb_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/constants"
	"github.com/bpalermo/aether/registry/internal/ddb"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcdynamodb "github.com/testcontainers/testcontainers-go/modules/dynamodb"
)

// startContainer creates a DynamoDB Local container and returns an aws.Config
// pointing at it. The container is terminated when the test finishes.
func startContainer(ctx context.Context, t *testing.T) aws.Config {
	t.Helper()

	container, err := tcdynamodb.Run(ctx, "amazon/dynamodb-local:2.5.4")
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	endpoint, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	return aws.Config{
		Region: "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(
			"DUMMYID", "DUMMYKEY", ""),
		BaseEndpoint: aws.String("http://" + endpoint),
	}
}

// createTable creates the service registry table in DynamoDB Local.
func createTable(ctx context.Context, t *testing.T, awsCfg aws.Config) {
	t.Helper()

	client := dynamodb.NewFromConfig(awsCfg)
	_, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(constants.DefaultDynamoDBServiceTableName),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("SK"), KeyType: types.KeyTypeRange},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("SK"), AttributeType: types.ScalarAttributeTypeS},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	require.NoError(t, err)
}

// setupRegistry creates a DynamoDB Local container, the service table, and
// returns a started DynamoDBRegistry ready for use.
func setupRegistry(ctx context.Context, t *testing.T) *ddb.DynamoDBRegistry {
	t.Helper()

	awsCfg := startContainer(ctx, t)
	createTable(ctx, t, awsCfg)

	registry := ddb.NewDynamoDBRegistry(logr.Discard(), awsCfg)
	require.NoError(t, registry.Start(ctx))

	return registry
}

func TestDynamoDBRegistry_Start(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()

	t.Run("successful start with existing table", func(t *testing.T) {
		awsCfg := startContainer(ctx, t)
		createTable(ctx, t, awsCfg)

		registry := ddb.NewDynamoDBRegistry(logr.Discard(), awsCfg)
		err := registry.Start(ctx)
		assert.NoError(t, err)
	})

	t.Run("fails when table does not exist", func(t *testing.T) {
		awsCfg := startContainer(ctx, t)

		registry := ddb.NewDynamoDBRegistry(logr.Discard(), awsCfg)
		err := registry.Start(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "does not exist")
	})
}

func TestDynamoDBRegistry_RegisterEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	ep := &registryv1.ServiceEndpoint{
		Ip:          "10.0.1.5",
		ClusterName: "test-cluster",
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

	err := registry.RegisterEndpoint(ctx, "frontend", registryv1.Service_HTTP, ep)
	assert.NoError(t, err)

	// Verify endpoint was registered by listing
	endpoints, err := registry.ListEndpoints(ctx, "frontend", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, endpoints, 1)

	assert.Equal(t, ep.Ip, endpoints[0].Ip)
	assert.Equal(t, ep.ClusterName, endpoints[0].ClusterName)
	assert.Equal(t, ep.Port, endpoints[0].Port)
	assert.Equal(t, ep.Weight, endpoints[0].Weight)
	assert.Equal(t, ep.Locality.Region, endpoints[0].Locality.Region)
	assert.Equal(t, ep.Locality.Zone, endpoints[0].Locality.Zone)
	assert.Equal(t, ep.Metadata["version"], endpoints[0].Metadata["version"])
	assert.Equal(t, ep.ContainerMetadata.ContainerId, endpoints[0].ContainerMetadata.ContainerId)
	assert.Equal(t, ep.KubernetesMetadata.Namespace, endpoints[0].KubernetesMetadata.Namespace)
}

func TestDynamoDBRegistry_RegisterMultipleEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	endpoints := []*registryv1.ServiceEndpoint{
		{Ip: "10.0.1.1", ClusterName: "cluster-1", Port: 8080, Weight: 100},
		{Ip: "10.0.1.2", ClusterName: "cluster-1", Port: 8080, Weight: 100},
		{Ip: "10.0.1.3", ClusterName: "cluster-1", Port: 8080, Weight: 50},
	}

	for _, ep := range endpoints {
		err := registry.RegisterEndpoint(ctx, "api-service", registryv1.Service_HTTP, ep)
		require.NoError(t, err)
	}

	listed, err := registry.ListEndpoints(ctx, "api-service", registryv1.Service_HTTP)
	require.NoError(t, err)
	assert.Len(t, listed, 3)

	ips := make(map[string]bool)
	for _, ep := range listed {
		ips[ep.Ip] = true
	}
	assert.True(t, ips["10.0.1.1"])
	assert.True(t, ips["10.0.1.2"])
	assert.True(t, ips["10.0.1.3"])
}

func TestDynamoDBRegistry_UnregisterEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	endpoints := []*registryv1.ServiceEndpoint{
		{Ip: "10.0.1.1", ClusterName: "cluster-1", Port: 8080, Weight: 100},
		{Ip: "10.0.1.2", ClusterName: "cluster-1", Port: 8080, Weight: 100},
	}

	for _, ep := range endpoints {
		err := registry.RegisterEndpoint(ctx, "backend", registryv1.Service_HTTP, ep)
		require.NoError(t, err)
	}

	err := registry.UnregisterEndpoint(ctx, "backend", "10.0.1.1")
	require.NoError(t, err)

	listed, err := registry.ListEndpoints(ctx, "backend", registryv1.Service_HTTP)
	require.NoError(t, err)
	assert.Len(t, listed, 1)
	assert.Equal(t, "10.0.1.2", listed[0].Ip)
}

func TestDynamoDBRegistry_UnregisterEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	endpoints := []*registryv1.ServiceEndpoint{
		{Ip: "10.0.1.1", ClusterName: "cluster-1", Port: 8080, Weight: 100},
		{Ip: "10.0.1.2", ClusterName: "cluster-1", Port: 8080, Weight: 100},
		{Ip: "10.0.1.3", ClusterName: "cluster-1", Port: 8080, Weight: 100},
	}

	for _, ep := range endpoints {
		err := registry.RegisterEndpoint(ctx, "workers", registryv1.Service_HTTP, ep)
		require.NoError(t, err)
	}

	err := registry.UnregisterEndpoints(ctx, "workers", []string{"10.0.1.1", "10.0.1.3"})
	require.NoError(t, err)

	listed, err := registry.ListEndpoints(ctx, "workers", registryv1.Service_HTTP)
	require.NoError(t, err)
	assert.Len(t, listed, 1)
	assert.Equal(t, "10.0.1.2", listed[0].Ip)
}

func TestDynamoDBRegistry_UnregisterEndpoints_EmptyList(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	err := registry.UnregisterEndpoints(ctx, "nonexistent", []string{})
	assert.NoError(t, err)
}

func TestDynamoDBRegistry_ListEndpoints_Empty(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	endpoints, err := registry.ListEndpoints(ctx, "nonexistent", registryv1.Service_HTTP)
	require.NoError(t, err)
	assert.Empty(t, endpoints)
}

func TestDynamoDBRegistry_ListAllEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	services := map[string][]*registryv1.ServiceEndpoint{
		"frontend": {
			{Ip: "10.0.1.1", ClusterName: "cluster-1", Port: 8080, Weight: 100},
			{Ip: "10.0.1.2", ClusterName: "cluster-1", Port: 8080, Weight: 100},
		},
		"backend": {
			{Ip: "10.0.2.1", ClusterName: "cluster-1", Port: 9090, Weight: 100},
		},
		"database": {
			{Ip: "10.0.3.1", ClusterName: "cluster-1", Port: 5432, Weight: 100},
			{Ip: "10.0.3.2", ClusterName: "cluster-1", Port: 5432, Weight: 100},
			{Ip: "10.0.3.3", ClusterName: "cluster-1", Port: 5432, Weight: 100},
		},
	}

	for service, endpoints := range services {
		for _, ep := range endpoints {
			err := registry.RegisterEndpoint(ctx, service, registryv1.Service_HTTP, ep)
			require.NoError(t, err)
		}
	}

	allEndpoints, err := registry.ListAllEndpoints(ctx, registryv1.Service_HTTP)
	require.NoError(t, err)

	assert.Len(t, allEndpoints, 3)
	assert.Len(t, allEndpoints["frontend"], 2)
	assert.Len(t, allEndpoints["backend"], 1)
	assert.Len(t, allEndpoints["database"], 3)
}

func TestDynamoDBRegistry_ListAllEndpoints_Empty(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	allEndpoints, err := registry.ListAllEndpoints(ctx, registryv1.Service_HTTP)
	require.NoError(t, err)
	assert.Empty(t, allEndpoints)
}

func TestDynamoDBRegistry_OverwriteEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	ep1 := &registryv1.ServiceEndpoint{
		Ip:          "10.0.1.1",
		ClusterName: "cluster-1",
		Port:        8080,
		Weight:      100,
	}
	err := registry.RegisterEndpoint(ctx, "service", registryv1.Service_HTTP, ep1)
	require.NoError(t, err)

	ep2 := &registryv1.ServiceEndpoint{
		Ip:          "10.0.1.1",
		ClusterName: "cluster-2",
		Port:        9090,
		Weight:      200,
	}
	err = registry.RegisterEndpoint(ctx, "service", registryv1.Service_HTTP, ep2)
	require.NoError(t, err)

	endpoints, err := registry.ListEndpoints(ctx, "service", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, endpoints, 1)

	assert.Equal(t, "10.0.1.1", endpoints[0].Ip)
	assert.Equal(t, "cluster-2", endpoints[0].ClusterName)
	assert.Equal(t, uint32(9090), endpoints[0].Port)
	assert.Equal(t, uint32(200), endpoints[0].Weight)
}
