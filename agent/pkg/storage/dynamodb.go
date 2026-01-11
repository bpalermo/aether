package storage

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type DynamoDBStorage[T proto.Message] struct {
	client    *dynamodb.Client
	tableName string
	newFunc   func() T
}

// DynamoDBItem represents an item stored in DynamoDB
type DynamoDBItem struct {
	PK   string `dynamodbav:"pk"`   // Partition key
	Data string `dynamodbav:"data"` // JSON serialized protobuf
}

// NewDynamoDBStorage creates a new DynamoDB storage instance
func NewDynamoDBStorage[T proto.Message](ctx context.Context, tableName string, newFunc func() T) (*DynamoDBStorage[T], error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &DynamoDBStorage[T]{
		client:    dynamodb.NewFromConfig(cfg),
		tableName: tableName,
		newFunc:   newFunc,
	}, nil
}

func (d *DynamoDBStorage[T]) AddResource(ctx context.Context, key string, resource T) error {
	// Marshal protobuf to JSON
	data, err := protojson.Marshal(resource)
	if err != nil {
		return fmt.Errorf("failed to marshal resource: %w", err)
	}

	item := DynamoDBItem{
		PK:   key,
		Data: string(data),
	}

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("failed to marshal item: %w", err)
	}

	_, err = d.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.tableName),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("failed to put item: %w", err)
	}

	return nil
}

func (d *DynamoDBStorage[T]) GetResource(ctx context.Context, key string) (T, error) {
	var resource T

	result, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.tableName),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: key},
		},
	})
	if err != nil {
		return resource, fmt.Errorf("failed to get item: %w", err)
	}

	if result.Item == nil {
		return resource, fmt.Errorf("resource not found: %s", key)
	}

	var item DynamoDBItem
	err = attributevalue.UnmarshalMap(result.Item, &item)
	if err != nil {
		return resource, fmt.Errorf("failed to unmarshal item: %w", err)
	}

	// Create new instance and unmarshal
	resource = d.newFunc()
	err = protojson.Unmarshal([]byte(item.Data), resource)
	if err != nil {
		return resource, fmt.Errorf("failed to unmarshal resource: %w", err)
	}

	return resource, nil
}

func (d *DynamoDBStorage[T]) RemoveResource(ctx context.Context, key string) error {

	_, err := d.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(d.tableName),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: key},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to delete item: %w", err)
	}

	return nil
}

func (d *DynamoDBStorage[T]) LoadAll() ([]T, error) {
	ctx := context.Background()
	var resources []T

	// Scan all items from the table
	paginator := dynamodb.NewScanPaginator(d.client, &dynamodb.ScanInput{
		TableName: aws.String(d.tableName),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to scan table: %w", err)
		}

		for _, item := range page.Items {
			var dbItem DynamoDBItem
			err = attributevalue.UnmarshalMap(item, &dbItem)
			if err != nil {
				return nil, fmt.Errorf("failed to unmarshal item: %w", err)
			}

			resource := d.newFunc()
			err = protojson.Unmarshal([]byte(dbItem.Data), resource)
			if err != nil {
				return nil, fmt.Errorf("failed to unmarshal resource: %w", err)
			}

			resources = append(resources, resource)
		}
	}

	return resources, nil
}
