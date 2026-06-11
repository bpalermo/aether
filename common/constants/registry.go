// Package constants defines shared constants used across the Aether codebase.
//
// It includes constants for:
//   - Service registry configuration (default table names, key prefixes, namespaces)
//   - Kubernetes labels and annotations for Aether integration
//   - Service endpoint defaults (ports, weights)
//   - CNI plugin configuration and socket paths
//
// These constants ensure consistency across agent, CNI plugin, and registry backends.
package constants

const (
	// DefaultDynamoDBServiceTableName is the default DynamoDB table name for storing service information
	DefaultDynamoDBServiceTableName = "AetherService"

	// DefaultEtcdKeyPrefix is the default prefix for all registry keys in etcd
	DefaultEtcdKeyPrefix = "/aether/services"
)
