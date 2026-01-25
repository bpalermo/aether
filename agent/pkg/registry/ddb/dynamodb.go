package ddb

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/registry"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
)

type DynamoDBRegistry struct {
	log logr.Logger

	client *dynamodb.Client

	tableName string
}

var _ registry.Registry = (*DynamoDBRegistry)(nil)

func NewDynamoDBRegistry(log logr.Logger, awsCfg aws.Config) *DynamoDBRegistry {
	return &DynamoDBRegistry{
		log:       log.WithName("dynamodb-registry"),
		client:    dynamodb.NewFromConfig(awsCfg),
		tableName: constants.DefaultDynamoDBEndpointTableName,
	}
}

// RegisterEndpoint registers the given endpoint to its service.
// It will register one endpoint for each of its IPs.
func (r *DynamoDBRegistry) RegisterEndpoint(ctx context.Context, pod *registryv1.RegistryPod) error {
	r.log.V(1).Info(
		"registering endpoint",
		"service", pod.GetServiceName(),
		"cluster", pod.GetClusterName(),
		"pod", pod.CniPod.GetName(),
	)

	// Validate input
	cniPod := pod.GetCniPod()
	if cniPod == nil || len(cniPod.GetIps()) == 0 {
		return fmt.Errorf("pod has no IPs to register")
	}

	// Prepare endpoints for each IP
	var writeRequests []types.WriteRequest
	for _, ip := range cniPod.GetIps() {
		var port uint16

		// Handle the oneof service_port field
		switch portSpec := pod.GetServicePort().GetPortSpecifier().(type) {
		case *registryv1.RegistryPod_ServicePort_PortNumber:
			port = uint16(portSpec.PortNumber)
		default:
			return fmt.Errorf("unsupported service port type: %T", portSpec)
		}

		// Create endpoint item
		endpoint := &DynamoDBEndpoint{
			PK:          fmt.Sprintf("service#%s", pod.GetServiceName()),
			SK:          fmt.Sprintf("protocol#%s#endpoint#%s", pod.GetPortProtocol(), ip),
			ServiceName: pod.GetServiceName(),
			ClusterName: pod.GetClusterName(),
			ContainerID: cniPod.GetContainerId(),
			Namespace:   cniPod.GetNamespace(),
			PodName:     cniPod.GetName(),
			IP:          ip,
			Protocol:    pod.GetPortProtocol().String(),
			Port:        port,
			Weight:      pod.GetEndpointWeight(),
		}

		// Add locality if present
		if pod.GetPodLocality() != nil {
			endpoint.Locality = &EndpointLocality{
				Region: pod.GetPodLocality().GetRegion(),
				Zone:   pod.GetPodLocality().GetZone(),
			}
		}

		// Add additional metadata if present
		endpoint.AdditionalMetadata = pod.GetAdditionalMetadata()

		// Marshal endpoint to DynamoDB attribute values
		av, err := attributevalue.MarshalMap(endpoint)
		if err != nil {
			r.log.Error(err, "failed to marshal endpoint", "ip", ip)
			return fmt.Errorf("failed to marshal endpoint for IP %s: %w", ip, err)
		}

		// Add to batch write requests
		writeRequests = append(writeRequests, types.WriteRequest{
			PutRequest: &types.PutRequest{
				Item: av,
			},
		})
	}

	// Batch write all endpoints (DynamoDB limits to 25 items per batch)
	for i := 0; i < len(writeRequests); i += 25 {
		end := i + 25
		if end > len(writeRequests) {
			end = len(writeRequests)
		}

		batch := writeRequests[i:end]
		input := &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]types.WriteRequest{
				r.tableName: batch,
			},
		}

		output, err := r.client.BatchWriteItem(ctx, input)
		if err != nil {
			r.log.Error(err, "failed to batch write endpoints", "batch_start", i, "batch_end", end)
			return fmt.Errorf("failed to batch write endpoints: %w", err)
		}

		// Handle unprocessed items (retry logic could be added here)
		if len(output.UnprocessedItems) > 0 {
			r.log.V(1).Info("unprocessed items in batch write", "count", len(output.UnprocessedItems[r.tableName]))
			// For now, just log - could implement retry logic
		}
	}

	r.log.Info(
		"endpoints registered successfully",
		"service", pod.GetServiceName(),
		"cluster", pod.GetClusterName(),
		"pod", pod.GetCniPod().GetName(),
		"ip_count", len(pod.GetCniPod().GetIps()),
	)
	return nil
}

// UnregisterEndpoints removes endpoints from the registry
func (r *DynamoDBRegistry) UnregisterEndpoints(ctx context.Context, pod *registryv1.RegistryPod) error {
	var errors []error
	for _, ip := range pod.CniPod.GetIps() {
		err := r.unregisterEndpoint(ctx, pod, ip)
		if err != nil {
			errors = append(errors, err)
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("failed to unregister some endpoints: %v", errors)
	}

	r.log.Info("endpoint unregistered successfully", "service", pod.GetServiceName(), "protocol", pod.PortProtocol)
	return nil
}

func (r *DynamoDBRegistry) unregisterEndpoint(ctx context.Context, pod *registryv1.RegistryPod, ip string) error {
	r.log.V(1).Info("unregistering endpoint",
		"service", pod.GetServiceName(),
		"protocol", pod.GetPortProtocol())

	// Build the delete input with composite keys
	input := &dynamodb.DeleteItemInput{
		TableName: aws.String(r.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("service#%s", pod.GetServiceName())},
			"SK": &types.AttributeValueMemberS{Value: fmt.Sprintf("protocol#%s#endpoint#%s", pod.GetPortProtocol(), ip)},
		}, // Return old values for logging/debugging
		ReturnValues: types.ReturnValueAllOld,
	}

	// Execute delete
	result, err := r.client.DeleteItem(ctx, input)
	if err != nil {
		r.log.Error(err, "failed to delete endpoint",
			"service", pod.GetServiceName(),
			"protocol", pod.GetPortProtocol())
		return fmt.Errorf("failed to delete endpoint: %w", err)
	}

	// Check if an item was actually deleted
	if len(result.Attributes) == 0 {
		r.log.V(1).Info("endpoint not found", "service", pod.GetServiceName(), "protocol", pod.PortProtocol)
		return fmt.Errorf("endpoint not found: service=%s, protocol=%s", pod.GetServiceName(), pod.PortProtocol)
	}

	return nil
}

// ListEndpoints returns the endpoints registered for the given service and its protocol.
func (r *DynamoDBRegistry) ListEndpoints(ctx context.Context, service string, protocol string) ([]registry.Endpoint, error) {
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

	endpoints := make([]registry.Endpoint, 0, len(result.Items))
	for _, item := range result.Items {
		var endpoint registry.Endpoint
		if err := attributevalue.UnmarshalMap(item, &endpoint); err != nil {
			r.log.Error(err, "failed to unmarshal endpoint item")
			continue
		}
		endpoints = append(endpoints, endpoint)
	}

	r.log.V(1).Info("listed endpoints", "service", service, "protocol", protocol, "count", len(endpoints))
	return endpoints, nil
}
