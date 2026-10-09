// Package serviceresource builds the OpenTelemetry resource of a Go component
// (#1576). The node agent, registrar, controller and edge control plane (through
// common/telemetry/setup), mesh-dns, the proxy supervisor and the prober each
// built theirs from a copy of the same option list, so one rule had to be
// changed in four places (#1564). This is the one place now.
//
// What a resource is made of, and who decides each part:
//
//   - service.name: the component. OTEL_SERVICE_NAME may override it and a
//     service.name inside OTEL_RESOURCE_ATTRIBUTES may not (#1562); that rule
//     lives in common/telemetry/servicename.
//   - service.version: the build, and nothing else (#1575). It says which
//     binary produced a series, a log line or a span, which no deployment knows
//     better than the binary. There is no OTEL_SERVICE_VERSION convention, so
//     there is no override: a service.version in OTEL_RESOURCE_ATTRIBUTES is
//     dropped.
//   - everything else in OTEL_RESOURCE_ATTRIBUTES: the deployment. The charts
//     carry the pod's k8s.* attributes there, and all of it is kept.
//   - telemetry.sdk.*, process.* and host.*: the SDK's detectors.
//
// The SDK merges resource options in order and the later one wins, so the
// component's own two attributes come after the environment. That order is the
// whole fix for #1562 and #1575, and New is the only place it is written.
//
// The package imports only the OTel SDK's resource package and semconv (and
// servicename, which imports the same two), which every telemetry-emitting
// binary already links, so the slim binaries (mesh-dns, the proxy supervisor,
// the prober) can use it without gaining a module.
package serviceresource

import (
	"context"

	"aethermesh.dev/common/telemetry/servicename"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

// Option is a difference between one component's resource and the common one.
type Option func(*config)

type config struct {
	host bool
}

// WithoutHost leaves host.name off the resource. It is for a component that
// does not run in the host's network namespace: there host.name is the POD
// name, and a collector that derives a node label from host.name ahead of
// k8s.node.name then labels every series with a pod (#1041). Such a component
// takes its node from k8s.node.name in OTEL_RESOURCE_ATTRIBUTES alone.
func WithoutHost() Option {
	return func(c *config) { c.host = false }
}

// New builds the resource of the component called name, built at version.
//
// Like resource.New it can return a usable resource together with an error
// (a detector that failed, a schema URL conflict); the callers treat any error
// as no resource.
func New(ctx context.Context, name, version string, opts ...Option) (*resource.Resource, error) {
	cfg := config{host: true}
	for _, opt := range opts {
		opt(&cfg)
	}

	options := []resource.Option{
		// OTEL_RESOURCE_ATTRIBUTES first: every option below it wins a conflict.
		resource.WithFromEnv(),
		// After WithFromEnv, so OTEL_RESOURCE_ATTRIBUTES cannot rename the component (#1562).
		servicename.Option(name),
		// After WithFromEnv, so OTEL_RESOURCE_ATTRIBUTES cannot restate the build (#1575).
		resource.WithAttributes(semconv.ServiceVersion(version)),
		resource.WithTelemetrySDK(),
		resource.WithProcess(),
	}
	if cfg.host {
		options = append(options, resource.WithHost())
	}
	return resource.New(ctx, options...)
}
