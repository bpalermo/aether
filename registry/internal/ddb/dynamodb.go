package ddb

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/bpalermo/aether/constants"
	registryTypes "github.com/bpalermo/aether/registry/types"
	"github.com/go-logr/logr"
)

type DynamoDBRegistry struct {
	log logr.Logger

	client *dynamodb.Client

	tableName string
}

func NewDynamoDBRegistry(log logr.Logger, awsCfg aws.Config) *DynamoDBRegistry {
	return &DynamoDBRegistry{
		log:       log.WithName("registry-dynamodb"),
		client:    dynamodb.NewFromConfig(awsCfg),
		tableName: constants.DefaultDynamoDBEndpointTableName,
	}
}

func (r *DynamoDBRegistry) Start(ctx context.Context) error {
	r.log.V(1).Info("starting registry and checking for table existence", "table", r.tableName)

	_, err := r.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(r.tableName),
	})
	if err != nil {
		var notFoundErr *types.ResourceNotFoundException
		if errors.As(err, &notFoundErr) {
			r.log.Info("table does not exist", "table", r.tableName)
			return fmt.Errorf("table %s does not exist", r.tableName)
		}
		r.log.Error(err, "failed to verify table exists", "table", r.tableName)
		return fmt.Errorf("table %s is not accessible: %w", r.tableName, err)
	}

	r.log.V(1).Info("registry table exists", "table", r.tableName)
	r.log.Info("DynamoDB registry started", "table", r.tableName)
	return nil
}

// RegisterEndpoint registers the given endpoint to its service.
// It will register one endpoint for each of its IPs.
func (r *DynamoDBRegistry) RegisterEndpoint(ctx context.Context, endpoint registryTypes.Endpoint) error {
	r.log.V(1).Info(
		"registering endpoint",
		"service", endpoint.GetServiceName(),
		"cluster", endpoint.GetClusterName(),
		"ip", endpoint.GetIp(),
	)

	// Create endpoint item
	item := &DynamoDBEndpoint{
		PK:          fmt.Sprintf("service#%s", endpoint.GetServiceName()),
		SK:          fmt.Sprintf("endpoint#%s#protocol#%s", endpoint.GetIp(), endpoint.GetPortProtocol()),
		ServiceName: endpoint.GetServiceName(),
		ClusterName: endpoint.GetClusterName(),
		IP:          endpoint.GetIp(),
		Protocol:    string(endpoint.GetPortProtocol()),
		Port:        endpoint.GetPort(),
		Weight:      endpoint.GetWeight(),
		Locality: &EndpointLocality{
			Region: endpoint.GetRegion(),
			Zone:   endpoint.GetSubzone(),
		},
		AdditionalMetadata: endpoint.GetAdditionalMetadata(),
	}

	// Marshal endpoint to DynamoDB attribute values
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		r.log.Error(err, "failed to marshal endpoint", "ip", endpoint.GetIp())
		return fmt.Errorf("failed to marshal endpoint for IP %s: %w", endpoint.GetIp(), err)
	}

	// Save item to DynamoDB
	_, err = r.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(r.tableName),
		Item:      av,
	})
	if err != nil {
		r.log.Error(err, "failed to save endpoint", "ip", endpoint.GetIp())
		return fmt.Errorf("failed to save endpoint for IP %s: %w", endpoint.GetIp(), err)
	}

	r.log.Info(
		"endpoint registered successfully",
		"service", endpoint.GetServiceName(),
		"cluster", endpoint.GetClusterName(),
		"ip", endpoint.GetIp(),
	)
	return nil
}

// UnregisterEndpoints removes endpoints from the registry
func (r *DynamoDBRegistry) UnregisterEndpoints(ctx context.Context, serviceName string, ips []string) error {
	var errs []error
	for _, ip := range ips {
		err := r.unregisterEndpoint(ctx, serviceName, ip)
		if err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to unregister some endpoints: %v", errs)
	}

	r.log.Info("endpoint unregistered successfully", "service", serviceName)
	return nil
}

func (r *DynamoDBRegistry) unregisterEndpoint(ctx context.Context, serviceName string, ip string) error {
	r.log.V(1).Info("unregistering endpoint",
		"service", serviceName)

	// First, query for all endpoints matching the prefix
	queryInput := &dynamodb.QueryInput{
		TableName:              aws.String(r.tableName),
		KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :skprefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":       &types.AttributeValueMemberS{Value: fmt.Sprintf("service#%s", serviceName)},
			":skprefix": &types.AttributeValueMemberS{Value: fmt.Sprintf("endpoint#%s", ip)},
		},
	}

	result, err := r.client.Query(ctx, queryInput)
	if err != nil {
		r.log.Error(err, "failed to query endpoints for deletion", "service", serviceName, "ip", ip)
		return fmt.Errorf("failed to query endpoints: %w", err)
	}

	if len(result.Items) == 0 {
		r.log.V(1).Info("no endpoints found to delete", "service", serviceName, "ip", ip)
		return nil
	}

	// Delete each matching item
	for _, item := range result.Items {
		// Build the delete input with composite keys
		input := &dynamodb.DeleteItemInput{
			TableName: aws.String(r.tableName),
			Key: map[string]types.AttributeValue{
				"PK": item["PK"],
				"SK": item["SK"],
			}, // Return old values for logging/debugging
			ReturnValues: types.ReturnValueAllOld,
		}

		// Execute delete
		res, deleteErr := r.client.DeleteItem(ctx, input)
		if deleteErr != nil {
			r.log.Error(deleteErr, "failed to delete endpoint",
				"service", serviceName,
				"ip", ip)
		}

		// Check if an item was actually deleted
		if len(res.Attributes) == 0 {
			r.log.V(1).Info("endpoint not found", "service", serviceName, "ip", ip)
			return fmt.Errorf("endpoint not found: service=%s, ip=%s", serviceName, ip)
		}
	}

	return nil
}

// ListEndpoints returns the endpoints registered for the given service and its protocol.
func (r *DynamoDBRegistry) ListEndpoints(ctx context.Context, service string, protocol string) ([]registryTypes.Endpoint, error) {
	r.log.V(1).Info("listing endpoints", "service", service, "protocol", protocol)

	input := &dynamodb.QueryInput{
		TableName: aws.String(r.tableName),
	}

	if protocol != "" {
		// Query with SK prefix for a specific protocol
		input.KeyConditionExpression = aws.String("PK = :pk AND begins_with(SK, :skprefix)")
		input.ExpressionAttributeValues = map[string]types.AttributeValue{
			":pk":       &types.AttributeValueMemberS{Value: fmt.Sprintf("service#%s", service)},
			":skprefix": &types.AttributeValueMemberS{Value: fmt.Sprintf("protocol#%s#", protocol)},
		}
	} else {
		// Query all endpoints for the service
		input.KeyConditionExpression = aws.String("PK = :pk")
		input.ExpressionAttributeValues = map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: fmt.Sprintf("service#%s", service)},
		}
	}

	result, err := r.client.Query(ctx, input)
	if err != nil {
		r.log.Error(err, "failed to query endpoints", "service", service)
		return nil, fmt.Errorf("failed to query endpoints: %w", err)
	}

	endpoints := make([]registryTypes.Endpoint, 0, len(result.Items))
	for _, item := range result.Items {
		var endpoint registryTypes.Endpoint
		if err := attributevalue.UnmarshalMap(item, &endpoint); err != nil {
			r.log.Error(err, "failed to unmarshal endpoint item")
			continue
		}
		endpoints = append(endpoints, endpoint)
	}

	r.log.V(1).Info("listed endpoints", "service", service, "protocol", protocol, "count", len(endpoints))
	return endpoints, nil
}
