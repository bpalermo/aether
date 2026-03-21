package registry

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/bpalermo/aether/registry/internal/cloudmap"
	"github.com/go-logr/logr"
)

// CloudMapOption configures a Cloud Map registry.
type CloudMapOption = cloudmap.Option

// WithCloudMapNamespace overrides the default Cloud Map HTTP namespace name.
var WithCloudMapNamespace = cloudmap.WithNamespace

// NewCloudMapRegistry creates a new Registry implementation backed by AWS Cloud Map.
// The clusterName identifies this cluster's endpoints in the shared Cloud Map namespace.
func NewCloudMapRegistry(log logr.Logger, awsCfg aws.Config, clusterName string, opts ...CloudMapOption) Registry {
	return cloudmap.NewCloudMapRegistry(log, awsCfg, clusterName, opts...)
}
