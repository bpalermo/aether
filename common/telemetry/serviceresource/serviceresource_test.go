package serviceresource

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
)

// TestNewIdentity pins who decides each part of a component's identity: the
// component names itself unless OTEL_SERVICE_NAME does (#1562), the build
// states the version and nothing overrides it (#1575), and the rest of
// OTEL_RESOURCE_ATTRIBUTES is kept.
func TestNewIdentity(t *testing.T) {
	tests := []struct {
		name         string
		resourceAttr string
		serviceName  string
		wantName     string
	}{
		{name: "nothing about the service in the environment", resourceAttr: "k8s.node.name=n1", wantName: "aether-test"},
		{name: "service.name in OTEL_RESOURCE_ATTRIBUTES does not rename the component", resourceAttr: "service.name=other,k8s.node.name=n1", wantName: "aether-test"},
		{name: "service.version in OTEL_RESOURCE_ATTRIBUTES does not replace the build's", resourceAttr: "service.version=other,k8s.node.name=n1", wantName: "aether-test"},
		{name: "neither does when both are there", resourceAttr: "service.name=other,service.version=other,k8s.node.name=n1", wantName: "aether-test"},
		{name: "OTEL_SERVICE_NAME is an explicit override", resourceAttr: "k8s.node.name=n1", serviceName: "explicit", wantName: "explicit"},
		{name: "OTEL_SERVICE_NAME renames and leaves the build's version alone", resourceAttr: "service.name=other,service.version=other,k8s.node.name=n1", serviceName: "explicit", wantName: "explicit"},
		{name: "a blank OTEL_SERVICE_NAME is no override", resourceAttr: "service.name=other,k8s.node.name=n1", serviceName: "  ", wantName: "aether-test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", tt.resourceAttr)
			t.Setenv("OTEL_SERVICE_NAME", tt.serviceName)

			// Both shapes: an option must not change who decides the identity.
			for shape, opts := range map[string][]Option{"default": nil, "without host": {WithoutHost()}} {
				res, err := New(context.Background(), "aether-test", "v1.2.3", opts...)
				if err != nil {
					t.Fatalf("%s: New: %v", shape, err)
				}
				if got := attr(res, "service.name"); got != tt.wantName {
					t.Errorf("%s: service.name = %q, want %q", shape, got, tt.wantName)
				}
				if got := attr(res, "service.version"); got != "v1.2.3" {
					t.Errorf("%s: service.version = %q, want %q: the build's version", shape, got, "v1.2.3")
				}
				if got := attr(res, "k8s.node.name"); got != "n1" {
					t.Errorf("%s: k8s.node.name = %q, want %q: the rest of OTEL_RESOURCE_ATTRIBUTES must be kept", shape, got, "n1")
				}
			}
		})
	}
}

// TestNewDetectors pins what the SDK's detectors add, and the one difference
// between components: host.name is on the resource unless WithoutHost says the
// component's hostname is not a node (#1041).
func TestNewDetectors(t *testing.T) {
	tests := []struct {
		name     string
		opts     []Option
		wantHost bool
	}{
		{name: "default", wantHost: true},
		{name: "WithoutHost", opts: []Option{WithoutHost()}, wantHost: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A host.name from the environment would hide what the option does.
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "k8s.node.name=n1")
			t.Setenv("OTEL_SERVICE_NAME", "")

			res, err := New(context.Background(), "aether-test", "v1.2.3", tt.opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			for _, key := range []attribute.Key{"telemetry.sdk.name", "telemetry.sdk.language", "telemetry.sdk.version"} {
				if !res.Set().HasValue(key) {
					t.Errorf("%s is missing: the telemetry SDK detector", key)
				}
			}
			if !res.Set().HasValue("process.pid") {
				t.Error("process.pid is missing: the process detector")
			}
			if got := res.Set().HasValue("host.name"); got != tt.wantHost {
				t.Errorf("host.name present = %v, want %v", got, tt.wantHost)
			}
		})
	}
}

// attr returns the string value of key on res, or "" when it is absent.
func attr(res *resource.Resource, key attribute.Key) string {
	v, _ := res.Set().Value(key)
	return v.AsString()
}
