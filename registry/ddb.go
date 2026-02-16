package registry

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/bpalermo/aether/registry/internal/ddb"
	"github.com/go-logr/logr"
)

func NewDynamoDBRegistry(log logr.Logger, awsCfg aws.Config) Registry {
	return ddb.NewDynamoDBRegistry(log, awsCfg)
}
