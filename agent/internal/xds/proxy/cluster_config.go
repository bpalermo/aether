package proxy

import "time"

// ClusterConfig holds tunable parameters for Envoy cluster generation.
// Default values match the previous hardcoded behavior.
type ClusterConfig struct {
	// LocalClusterBindAddress is the source address used for upstream connections
	// in local clusters (default "127.0.0.1").
	LocalClusterBindAddress string

	// HealthCheckHealthyThreshold is the number of consecutive successful health
	// checks before an endpoint is considered healthy (default 1).
	HealthCheckHealthyThreshold uint32

	// HealthCheckUnhealthyThreshold is the number of consecutive failed health
	// checks before an endpoint is considered unhealthy (default 1).
	HealthCheckUnhealthyThreshold uint32

	// HealthCheckInterval is the time between health check attempts (default 5s).
	HealthCheckInterval time.Duration

	// HealthCheckTimeout is the maximum time to wait for a health check response
	// (default 1s).
	HealthCheckTimeout time.Duration

	// UpstreamProtocol selects the HTTP protocol version for upstream cluster
	// connections. Supported values are "h2" (HTTP/2, the default) and "http1"
	// (HTTP/1.1).
	UpstreamProtocol string
}

const (
	// UpstreamProtocolH2 selects HTTP/2 for upstream connections.
	UpstreamProtocolH2 = "h2"
	// UpstreamProtocolHTTP1 selects HTTP/1.1 for upstream connections.
	UpstreamProtocolHTTP1 = "http1"
)

// DefaultClusterConfig returns a ClusterConfig with values matching the
// previous hardcoded defaults for full backward compatibility.
func DefaultClusterConfig() *ClusterConfig {
	return &ClusterConfig{
		LocalClusterBindAddress:       "127.0.0.1",
		HealthCheckHealthyThreshold:   1,
		HealthCheckUnhealthyThreshold: 1,
		HealthCheckInterval:           5 * time.Second,
		HealthCheckTimeout:            1 * time.Second,
		UpstreamProtocol:              UpstreamProtocolH2,
	}
}
