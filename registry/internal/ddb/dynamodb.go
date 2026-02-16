package ddb

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/constants"
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
		tableName: constants.DefaultDynamoDBServiceTableName,
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
// It stores the endpoint in a map attribute keyed by the endpoint IP.
func (r *DynamoDBRegistry) RegisterEndpoint(ctx context.Context, serviceName string, protocol string, endpoint *registryv1.ServiceEndpoint) error {
	ip := endpoint.GetIp()
	r.log.V(1).Info(
		"registering endpoint",
		"service", serviceName,
		"protocol", protocol,
		"cluster", endpoint.GetClusterName(),
		"ip", ip,
	)

	// Marshal endpoint to DynamoDB attribute value
	av, err := attributevalue.Marshal(endpoint)
	if err != nil {
		r.log.Error(err, "failed to marshal endpoint", "ip", ip)
		return fmt.Errorf("failed to marshal endpoint for IP %s: %w", ip, err)
	}

	// First, ensure the endpoints map exists
	_, err = r.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(r.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("service#%s", serviceName)},
			"SK": &types.AttributeValueMemberS{Value: fmt.Sprintf("protocol#%s", protocol)},
		},
		UpdateExpression: aws.String("SET endpoints = if_not_exists(endpoints, :empty)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":empty": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{}},
		},
	})
	if err != nil {
		r.log.Error(err, "failed to initialize endpoints map", "ip", ip)
		return fmt.Errorf("failed to initialize endpoints map for IP %s: %w", ip, err)
	}

	// Then set the endpoint in the map
	_, err = r.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(r.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("service#%s", serviceName)},
			"SK": &types.AttributeValueMemberS{Value: fmt.Sprintf("protocol#%s", protocol)},
		},
		UpdateExpression: aws.String("SET endpoints.#ip = :endpoint"),
		ExpressionAttributeNames: map[string]string{
			"#ip": ip,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":endpoint": av,
		},
	})
	if err != nil {
		r.log.Error(err, "failed to register endpoint", "ip", ip)
		return fmt.Errorf("failed to register endpoint for IP %s: %w", ip, err)
	}

	r.log.Info(
		"endpoint registered successfully",
		"service", serviceName,
		"cluster", endpoint.GetClusterName(),
		"ip", ip,
	)
	return nil
}

func (r *DynamoDBRegistry) UnregisterEndpoint(ctx context.Context, serviceName string, ip string) error {
	return r.UnregisterEndpoints(ctx, serviceName, []string{ip})
}

// UnregisterEndpoints removes endpoints from the registry for all protocols
func (r *DynamoDBRegistry) UnregisterEndpoints(ctx context.Context, serviceName string, ips []string) error {
	r.log.V(1).Info("unregistering endpoints",
		"service", serviceName,
		"count", len(ips),
	)

	if len(ips) == 0 {
		return nil
	}

	// Query all protocol items for this service
	queryInput := &dynamodb.QueryInput{
		TableName:              aws.String(r.tableName),
		KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :skprefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":       &types.AttributeValueMemberS{Value: fmt.Sprintf("service#%s", serviceName)},
			":skprefix": &types.AttributeValueMemberS{Value: "protocol#"},
		},
		ProjectionExpression: aws.String("PK, SK"),
	}

	result, err := r.client.Query(ctx, queryInput)
	if err != nil {
		r.log.Error(err, "failed to query protocols", "service", serviceName)
		return fmt.Errorf("failed to query protocols: %w", err)
	}

	// Build a single REMOVE expression for all IPs
	var removeExprs []string
	exprAttrNames := make(map[string]string)
	for i, ip := range ips {
		placeholder := fmt.Sprintf("#ip%d", i)
		removeExprs = append(removeExprs, fmt.Sprintf("endpoints.%s", placeholder))
		exprAttrNames[placeholder] = ip
	}
	updateExpr := "REMOVE " + strings.Join(removeExprs, ", ")

	// For each protocol item, remove all IPs in a single update
	for _, item := range result.Items {
		_, err := r.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(r.tableName),
			Key: map[string]types.AttributeValue{
				"PK": item["PK"],
				"SK": item["SK"],
			},
			UpdateExpression:         aws.String(updateExpr),
			ExpressionAttributeNames: exprAttrNames,
		})
		if err != nil {
			r.log.Error(err, "failed to unregister endpoints", "service", serviceName)
			return fmt.Errorf("failed to unregister endpoints: %w", err)
		}
	}

	r.log.Info("endpoints unregistered successfully", "service", serviceName, "count", len(ips))
	return nil
}

// ListEndpoints returns the endpoints registered for the given service and its protocol.
func (r *DynamoDBRegistry) ListEndpoints(ctx context.Context, service string, protocol string) ([]*registryv1.ServiceEndpoint, error) {
	r.log.V(1).Info("listing endpoints", "service", service, "protocol", protocol)

	if protocol == "" {
		return nil, fmt.Errorf("protocol is required")
	}

	input := &dynamodb.QueryInput{
		TableName:              aws.String(r.tableName),
		KeyConditionExpression: aws.String("PK = :pk AND SK = :sk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: fmt.Sprintf("service:%s", service)},
			":sk": &types.AttributeValueMemberS{Value: fmt.Sprintf("protocol:%s", protocol)},
		},
	}

	result, err := r.client.Query(ctx, input)
	if err != nil {
		r.log.Error(err, "failed to query endpoints", "service", service)
		return nil, fmt.Errorf("failed to query endpoints: %w", err)
	}

	endpoints := make([]*registryv1.ServiceEndpoint, 0, len(result.Items))
	for _, item := range result.Items {
		var endpoint registryv1.ServiceEndpoint
		if err := attributevalue.UnmarshalMap(item, &endpoint); err != nil {
			r.log.Error(err, "failed to unmarshal endpoint item")
			continue
		}
		endpoints = append(endpoints, &endpoint)
	}

	r.log.V(1).Info("listed endpoints", "service", service, "protocol", protocol, "count", len(endpoints))
	return endpoints, nil
}

// ListAllEndpoints returns all endpoints registered for the given protocol.
func (r *DynamoDBRegistry) ListAllEndpoints(ctx context.Context, protocol string) (map[string][]*registryv1.ServiceEndpoint, error) {
	r.log.V(1).Info("listing all endpoints for protocol", "protocol", protocol)

	input := &dynamodb.ScanInput{
		TableName:     aws.String(r.tableName),
		Segment:       aws.Int32(0), // Current segment
		TotalSegments: aws.Int32(4), // Split into 4 parallel scans
	}

	// Filter by protocol if specified
	if protocol != "" {
		input.FilterExpression = aws.String("Protocol = :protocol")
		input.ExpressionAttributeValues = map[string]types.AttributeValue{
			":protocol": &types.AttributeValueMemberS{Value: protocol},
		}
	}

	endpointsByService := make(map[string][]*registryv1.ServiceEndpoint)

	// Use paginator to handle large result sets
	paginator := dynamodb.NewScanPaginator(r.client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			r.log.Error(err, "failed to scan endpoints", "protocol", protocol)
			return nil, fmt.Errorf("failed to scan endpoints: %w", err)
		}

		for _, item := range page.Items {
			var endpoint registryv1.ServiceEndpoint
			if err := attributevalue.UnmarshalMap(item, &endpoint); err != nil {
				r.log.Error(err, "failed to unmarshal endpoint item")
				continue
			}
			// Extract service name from PK (format: "service#<serviceName>")
			if pkAttr, ok := item["PK"].(*types.AttributeValueMemberS); ok {
				serviceName := pkAttr.Value
				if len(serviceName) > 8 && serviceName[:8] == "service#" {
					serviceName = serviceName[8:]
				}
				endpointsByService[serviceName] = append(endpointsByService[serviceName], &endpoint)
			}
		}
	}

	return endpointsByService, nil
}
