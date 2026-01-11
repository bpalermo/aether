package storage

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	smithyendpoints "github.com/aws/smithy-go/endpoints"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcdynamodb "github.com/testcontainers/testcontainers-go/modules/dynamodb"
	"google.golang.org/protobuf/types/known/structpb"
)

type dynamoDBResolver struct {
	HostPort string
}

func (r *dynamoDBResolver) ResolveEndpoint(_ context.Context, _ dynamodb.EndpointParameters) (smithyendpoints.Endpoint, error) {
	return smithyendpoints.Endpoint{
		URI: url.URL{Host: r.HostPort, Scheme: "http"},
	}, nil
}

func setupDynamoDBContainer(t *testing.T) (*dynamodb.Client, func()) {
	ctx := t.Context()

	container, err := tcdynamodb.Run(ctx, "amazon/dynamodb-local:latest")
	require.NoError(t, err)

	endpoint, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	// Create DynamoDB client with local endpoint using v2 SDK
	cfg, err := config.LoadDefaultConfig(
		t.Context(),
		config.WithCredentialsProvider(credentials.StaticCredentialsProvider{
			Value: aws.Credentials{
				AccessKeyID:     "DUMMYIDEXAMPLE",
				SecretAccessKey: "DUMMYEXAMPLEKEY",
			},
		}))
	require.NoError(t, err)

	client := dynamodb.NewFromConfig(cfg, dynamodb.WithEndpointResolverV2(&dynamoDBResolver{HostPort: endpoint}))

	cleanup := func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("failed to terminate container: %s", err)
		}
	}

	return client, cleanup
}

func createTestTable(t *testing.T, client *dynamodb.Client, tableName string) {
	ctx := t.Context()

	_, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(tableName),
		KeySchema: []types.KeySchemaElement{
			{
				AttributeName: aws.String("pk"),
				KeyType:       types.KeyTypeHash,
			},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{
				AttributeName: aws.String("pk"),
				AttributeType: types.ScalarAttributeTypeS,
			},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	require.NoError(t, err)

	// Wait for table to be active
	waiter := dynamodb.NewTableExistsWaiter(client)
	err = waiter.Wait(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(tableName),
	}, 10*time.Second)
	require.NoError(t, err)
}

func TestDynamoDBStorage_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	client, cleanup := setupDynamoDBContainer(t)
	defer cleanup()

	tableName := "test-endpoints"
	createTestTable(t, client, tableName)

	storage := &DynamoDBStorage[*registryv1.Endpoint]{
		client:    client,
		tableName: tableName,
		newFunc:   func() *registryv1.Endpoint { return &registryv1.Endpoint{} },
	}

	ctx := t.Context()

	t.Run("AddAndGetResource", func(t *testing.T) {
		endpoint := &registryv1.Endpoint{
			Address: &registryv1.Endpoint_Address{
				Address: "10.0.0.1",
				Port:    8080,
			},
			Locality: &registryv1.Endpoint_Locality{
				Region: "us-west",
				Zone:   "us-west-1a",
			},
		}

		// Add resource
		err := storage.AddResource(ctx, "endpoint-1", endpoint)
		assert.NoError(t, err)

		// Get resource
		retrieved, err := storage.GetResource(ctx, "endpoint-1")
		assert.NoError(t, err)
		assert.NotNil(t, retrieved.Address)
		assert.Equal(t, endpoint.Address.Address, retrieved.Address.Address)
		assert.Equal(t, endpoint.Address.Port, retrieved.Address.Port)
		assert.Equal(t, endpoint.Locality.Region, retrieved.Locality.Region)
	})

	t.Run("UpdateResource", func(t *testing.T) {
		endpoint := &registryv1.Endpoint{
			Address: &registryv1.Endpoint_Address{
				Address: "10.0.0.2",
				Port:    9090,
			},
		}

		// Add initial resource
		err := storage.AddResource(ctx, "endpoint-2", endpoint)
		assert.NoError(t, err)

		// Update resource
		endpoint.Address.Port = 9999
		endpoint.Metadata = &registryv1.Endpoint_Metadata{
			FilterMetadata: map[string]*structpb.Struct{
				"test": {
					Fields: map[string]*structpb.Value{
						"key": {Kind: &structpb.Value_StringValue{StringValue: "value"}},
					},
				},
			},
		}
		err = storage.AddResource(ctx, "endpoint-2", endpoint)
		assert.NoError(t, err)

		// Verify update
		retrieved, err := storage.GetResource(ctx, "endpoint-2")
		assert.NoError(t, err)
		assert.Equal(t, uint32(9999), retrieved.Address.Port)
		assert.NotNil(t, retrieved.Metadata)
	})

	t.Run("RemoveResource", func(t *testing.T) {
		endpoint := &registryv1.Endpoint{
			Address: &registryv1.Endpoint_Address{
				Address: "10.0.0.3",
				Port:    3000,
			},
		}

		// Add resource
		err := storage.AddResource(ctx, "endpoint-3", endpoint)
		assert.NoError(t, err)

		// Remove resource
		err = storage.RemoveResource(ctx, "endpoint-3")
		assert.NoError(t, err)

		// Verify removal
		_, err = storage.GetResource(ctx, "endpoint-3")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "resource not found")
	})

	t.Run("GetNonExistentResource", func(t *testing.T) {
		_, err := storage.GetResource(ctx, "non-existent")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "resource not found")
	})

	t.Run("LoadAll", func(t *testing.T) {
		// Clear the table first
		scan, err := client.Scan(ctx, &dynamodb.ScanInput{
			TableName: aws.String(tableName),
		})
		require.NoError(t, err)

		for _, item := range scan.Items {
			_, err := client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
				TableName: aws.String(tableName),
				Key: map[string]types.AttributeValue{
					"pk": item["pk"],
				},
			})
			require.NoError(t, err, "failed to delete item with key: %v", item["pk"])
		}

		// Add multiple resources
		endpoints := []*registryv1.Endpoint{
			{
				Address: &registryv1.Endpoint_Address{
					Address: "10.0.1.1",
					Port:    8001,
				},
			},
			{
				Address: &registryv1.Endpoint_Address{
					Address: "10.0.1.2",
					Port:    8002,
				},
			},
			{
				Address: &registryv1.Endpoint_Address{
					Address: "10.0.1.3",
					Port:    8003,
				},
			},
		}

		for i, endpoint := range endpoints {
			err := storage.AddResource(ctx, fmt.Sprintf("endpoint-%d", i), endpoint)
			assert.NoError(t, err)
		}

		// Load all resources
		allEndpoints, err := storage.LoadAll()
		assert.NoError(t, err)
		assert.Len(t, allEndpoints, 3)

		// Verify all endpoints are loaded
		addresses := make(map[string]bool)
		for _, endpoint := range allEndpoints {
			addresses[endpoint.Address.Address] = true
		}
		assert.True(t, addresses["10.0.1.1"])
		assert.True(t, addresses["10.0.1.2"])
		assert.True(t, addresses["10.0.1.3"])
	})
}

func TestDynamoDBStorage_ConcurrentOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	client, cleanup := setupDynamoDBContainer(t)
	defer cleanup()

	tableName := "test-concurrent"
	createTestTable(t, client, tableName)

	storage := &DynamoDBStorage[*registryv1.Endpoint]{
		client:    client,
		tableName: tableName,
		newFunc:   func() *registryv1.Endpoint { return &registryv1.Endpoint{} },
	}

	ctx := t.Context()

	// Test concurrent writes
	t.Run("ConcurrentWrites", func(t *testing.T) {
		errChan := make(chan error, 10)

		for i := 0; i < 10; i++ {
			go func(id int) {
				endpoint := &registryv1.Endpoint{
					Address: &registryv1.Endpoint_Address{
						Address: fmt.Sprintf("10.0.2.%d", id),
						Port:    uint32(9000 + id),
					},
				}
				errChan <- storage.AddResource(ctx, fmt.Sprintf("concurrent-%d", id), endpoint)
			}(i)
		}

		// Collect results
		for i := 0; i < 10; i++ {
			err := <-errChan
			assert.NoError(t, err)
		}

		// Verify all endpoints were written
		allEndpoints, err := storage.LoadAll()
		assert.NoError(t, err)
		assert.Len(t, allEndpoints, 10)
	})
}
