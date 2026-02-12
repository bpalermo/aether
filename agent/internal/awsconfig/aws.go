package awsconfig

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
)

func LoadConfig(ctx context.Context) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"), // Fallback region
		config.WithEC2IMDSRegion(),     // Try EC2 metadata
	)
}
