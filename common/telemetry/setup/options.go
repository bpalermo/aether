package setup

import metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

// DefaultMetricsBindAddress is the default address for the controller-runtime
// Prometheus metrics HTTP server.
const DefaultMetricsBindAddress = ":8081"

// ManagerMetricsOptions returns controller-runtime metrics server options.
// When enabled is false, the metrics server is disabled. When enabled is true,
// it binds to bindAddress (defaulting to DefaultMetricsBindAddress if empty).
func ManagerMetricsOptions(enabled bool, bindAddress string) metricsserver.Options {
	if !enabled {
		return metricsserver.Options{BindAddress: "0"}
	}
	if bindAddress == "" {
		bindAddress = DefaultMetricsBindAddress
	}
	return metricsserver.Options{BindAddress: bindAddress}
}
