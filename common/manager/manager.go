package manager

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/common/telemetry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// Result holds the bootstrapped manager and an optional telemetry shutdown function.
type Result struct {
	Manager  ctrl.Manager
	Shutdown func(context.Context) error // nil if OTel not enabled
}

// Bootstrap sets up telemetry (if enabled), creates a controller-runtime Manager,
// and registers the standard health and readiness probes.
func Bootstrap(ctx context.Context, cfg Config, serviceName, serviceVersion string) (*Result, error) {
	var shutdown func(context.Context) error

	if cfg.OTelEnabled {
		var err error
		shutdown, err = telemetry.Setup(ctx, telemetry.Config{
			ServiceName:    serviceName,
			ServiceVersion: serviceVersion,
			OTLPEndpoint:   cfg.OTLPEndpoint,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to setup telemetry: %w", err)
		}
	}

	m, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		HealthProbeBindAddress: cfg.HealthProbeBindAddress,
		Metrics:                telemetry.ManagerMetricsOptions(cfg.MetricsEnabled, cfg.MetricsBindAddress),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create manager: %w", err)
	}

	if err = m.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("failed to set up health check: %w", err)
	}
	if err = m.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("failed to set up ready check: %w", err)
	}
	if err = m.AddReadyzCheck("cache-sync", telemetry.CacheSyncChecker(m)); err != nil {
		return nil, fmt.Errorf("failed to set up cache sync ready check: %w", err)
	}

	return &Result{
		Manager:  m,
		Shutdown: shutdown,
	}, nil
}
