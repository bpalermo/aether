package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// otlpLogTimeout bounds each OTLP log export so a missing or slow collector
// cannot block the batch log processor's export goroutine.
const otlpLogTimeout = 5 * time.Second

// SetupLogs creates an OTel LoggerProvider exporting records to the OTLP gRPC
// collector at cfg.OTLPEndpoint via a batching processor, carrying the same
// Resource (service.name/version + env attributes) as metrics and traces. Unlike
// metrics and traces, the provider is NOT installed globally: the caller wires it
// into the zap logger via an otelzap Core (see common/manager.SetupLogging), which
// is the only place that emits records. The returned shutdown function flushes and
// stops the provider.
//
// cfg.OTLPEndpoint must be non-empty: logs have no OTLP export path without a
// collector (stderr logging continues regardless via the tee'd zap core).
func SetupLogs(ctx context.Context, cfg Config) (provider *sdklog.LoggerProvider, shutdown func(context.Context) error, err error) {
	if cfg.OTLPEndpoint == "" {
		return nil, nil, fmt.Errorf("logs require an OTLP endpoint")
	}

	res, err := newResource(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create resource: %w", err)
	}

	exporter, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlploggrpc.WithInsecure(),
		otlploggrpc.WithTimeout(otlpLogTimeout),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create OTLP log exporter: %w", err)
	}

	provider = sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
	)

	return provider, provider.Shutdown, nil
}
