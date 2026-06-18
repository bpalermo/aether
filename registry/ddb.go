package registry

import (
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/bpalermo/aether/registry/internal/ddb"
)

// DynamoDBOption configures a DynamoDB registry.
type DynamoDBOption = ddb.Option

// WithDynamoDBTableName overrides the default DynamoDB table name.
var WithDynamoDBTableName = ddb.WithTableName

// NewDynamoDBRegistry creates a new Registry implementation backed by DynamoDB.
func NewDynamoDBRegistry(log *slog.Logger, awsCfg aws.Config, opts ...DynamoDBOption) Registry {
	return ddb.NewDynamoDBRegistry(log, awsCfg, opts...)
}
