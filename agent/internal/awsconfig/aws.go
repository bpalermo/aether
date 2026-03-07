// Package awsconfig provides AWS SDK configuration utilities.
package awsconfig

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
)

// LoadConfig loads the default AWS configuration using EC2 metadata when available.
// It falls back to us-east-1 if EC2 metadata is not available.
func LoadConfig(ctx context.Context) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"), // Fallback region
		config.WithEC2IMDSRegion(),     // Try EC2 metadata
	)
}
