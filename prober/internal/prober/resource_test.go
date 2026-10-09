package prober

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
)

// TestResourceServiceName pins who names the component on its OTel resource (#1562).
//
// A service.name inside OTEL_RESOURCE_ATTRIBUTES must not rename it: that variable is
// the chart's carrier for the pod's k8s.* attributes, and dashboards, alerts and the
// collector's routing select on the component's own name. Every other attribute in
// the variable is kept. OTEL_SERVICE_NAME is the one variable whose whole purpose is
// to name the service, so it stays an explicit override, as it was before #1562.
func TestResourceServiceName(t *testing.T) {
	tests := []struct {
		name         string
		resourceAttr string
		serviceName  string
		want         string
	}{
		{name: "no service name in the environment", resourceAttr: "k8s.node.name=n1", want: telemetryServiceName},
		{name: "service.name in OTEL_RESOURCE_ATTRIBUTES does not rename the component", resourceAttr: "service.name=other,k8s.node.name=n1", want: telemetryServiceName},
		{name: "OTEL_SERVICE_NAME is an explicit override", resourceAttr: "k8s.node.name=n1", serviceName: "explicit", want: "explicit"},
		{name: "OTEL_SERVICE_NAME wins over OTEL_RESOURCE_ATTRIBUTES", resourceAttr: "service.name=other,k8s.node.name=n1", serviceName: "explicit", want: "explicit"},
		{name: "a blank OTEL_SERVICE_NAME is no override", resourceAttr: "service.name=other,k8s.node.name=n1", serviceName: "  ", want: telemetryServiceName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", tt.resourceAttr)
			t.Setenv("OTEL_SERVICE_NAME", tt.serviceName)

			res, err := newResource(context.Background(), "v1.2.3")
			if err != nil {
				t.Fatalf("building the resource: %v", err)
			}
			if got := resourceAttr(res, "service.name"); got != tt.want {
				t.Errorf("service.name = %q, want %q", got, tt.want)
			}
			if got := resourceAttr(res, "service.version"); got != "v1.2.3" {
				t.Errorf("service.version = %q, want %q", got, "v1.2.3")
			}
			if got := resourceAttr(res, "k8s.node.name"); got != "n1" {
				t.Errorf("k8s.node.name = %q, want %q: the rest of OTEL_RESOURCE_ATTRIBUTES must be kept", got, "n1")
			}
		})
	}
}

// TestResourceServiceVersion pins who states the component's version on its OTel
// resource (#1575).
//
// service.version is the build's: it says which binary produced a series, a log line
// or a span, and no deployment can know that better than the binary. A service.version
// inside OTEL_RESOURCE_ATTRIBUTES (an attribute string copied from another container,
// or a chart's app version) must not replace it. Every other attribute in the variable
// is kept. There is no OTEL_SERVICE_VERSION convention and no override.
func TestResourceServiceVersion(t *testing.T) {
	tests := []struct {
		name         string
		resourceAttr string
		serviceName  string
	}{
		{name: "no service.version in the environment", resourceAttr: "k8s.node.name=n1"},
		{name: "service.version in OTEL_RESOURCE_ATTRIBUTES does not replace the build's", resourceAttr: "service.version=other,k8s.node.name=n1"},
		{name: "nor does it beside an OTEL_SERVICE_NAME override", resourceAttr: "service.version=other,k8s.node.name=n1", serviceName: "explicit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", tt.resourceAttr)
			t.Setenv("OTEL_SERVICE_NAME", tt.serviceName)

			res, err := newResource(context.Background(), "v1.2.3")
			if err != nil {
				t.Fatalf("building the resource: %v", err)
			}
			if got := resourceAttr(res, "service.version"); got != "v1.2.3" {
				t.Errorf("service.version = %q, want %q: the build's version", got, "v1.2.3")
			}
			if got := resourceAttr(res, "k8s.node.name"); got != "n1" {
				t.Errorf("k8s.node.name = %q, want %q: the rest of OTEL_RESOURCE_ATTRIBUTES must be kept", got, "n1")
			}
		})
	}
}

// resourceAttr returns the string value of key on res, or "" when it is absent.
func resourceAttr(res *resource.Resource, key attribute.Key) string {
	v, _ := res.Set().Value(key)
	return v.AsString()
}
