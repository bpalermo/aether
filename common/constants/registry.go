// Package constants defines the genuinely cross-tree basic constants used across
// the Aether codebase.
//
// It includes constants for:
//   - Service registry configuration (default table names, key prefixes)
//   - Service endpoint defaults (ports, weights)
//   - CNI plugin registry path
//   - Mesh-ignored namespaces
//
// Domain-specific constants live in sub-packages: annotations, labels, and mesh.
package constants

const (
	// DefaultDynamoDBServiceTableName is the default DynamoDB table name for storing service information
	DefaultDynamoDBServiceTableName = "AetherService"

	// DefaultEtcdKeyPrefix is the default prefix for all registry keys in etcd
	DefaultEtcdKeyPrefix = "/aether/services"
)
