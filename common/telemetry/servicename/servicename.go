// Package servicename decides who names a component on its OpenTelemetry
// resource (#1562).
//
// Every component builds its resource from its own identity plus the
// environment: the charts carry the pod's k8s.* attributes in
// OTEL_RESOURCE_ATTRIBUTES. The SDK merges resource options in order and the
// later one wins, so a service.name inside that variable used to rename the
// component. Dashboards, alerts and the collector's routing select on the
// component's own name, and one container's attribute string is easily copied
// to the next, so that variable must not rename anything.
//
// OTEL_SERVICE_NAME stays an override: naming the service is all it does, so
// setting it is a decision, and the SDK itself ranks it above
// OTEL_RESOURCE_ATTRIBUTES.
//
// The package imports only the OTel SDK's resource package, which every
// telemetry-emitting binary already links, so the slim binaries (mesh-dns, the
// proxy supervisor, the prober) can use it.
package servicename

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

// overrideEnv is the one environment variable allowed to rename a component.
const overrideEnv = "OTEL_SERVICE_NAME"

// Option returns the resource option that sets service.name to the component's
// own name. Pass it AFTER resource.WithFromEnv(): the later option wins, which
// is what keeps a service.name in OTEL_RESOURCE_ATTRIBUTES from replacing it.
// It adds nothing when OTEL_SERVICE_NAME is set, so that override survives.
func Option(name string) resource.Option {
	return resource.WithDetectors(detector(name))
}

// detector yields the component's service.name unless OTEL_SERVICE_NAME names
// the service. The environment is read at detection time, like the SDK's own
// environment detector, and blank is unset, as it is there.
type detector string

func (d detector) Detect(context.Context) (*resource.Resource, error) {
	if strings.TrimSpace(os.Getenv(overrideEnv)) != "" {
		return resource.Empty(), nil
	}
	return resource.NewSchemaless(semconv.ServiceName(string(d))), nil
}
