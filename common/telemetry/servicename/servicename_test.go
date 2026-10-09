package servicename

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/resource"
)

// TestOption drives the option the way the builders use it: after WithFromEnv.
func TestOption(t *testing.T) {
	tests := []struct {
		name         string
		resourceAttr string
		override     string
		want         string
	}{
		{name: "nothing in the environment", want: "aether-test"},
		{name: "service.name in OTEL_RESOURCE_ATTRIBUTES is ignored", resourceAttr: "service.name=other,k8s.node.name=n1", want: "aether-test"},
		{name: "OTEL_SERVICE_NAME overrides", resourceAttr: "k8s.node.name=n1", override: "explicit", want: "explicit"},
		{name: "OTEL_SERVICE_NAME overrides both", resourceAttr: "service.name=other,k8s.node.name=n1", override: "explicit", want: "explicit"},
		{name: "OTEL_SERVICE_NAME is trimmed like the SDK trims it", override: " explicit ", want: "explicit"},
		{name: "a blank OTEL_SERVICE_NAME is unset", resourceAttr: "service.name=other,k8s.node.name=n1", override: " \t", want: "aether-test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", tt.resourceAttr)
			t.Setenv(overrideEnv, tt.override)

			res, err := resource.New(context.Background(), resource.WithFromEnv(), Option("aether-test"))
			if err != nil {
				t.Fatalf("resource.New: %v", err)
			}
			got, _ := res.Set().Value("service.name")
			if got.AsString() != tt.want {
				t.Errorf("service.name = %q, want %q", got.AsString(), tt.want)
			}
			if tt.resourceAttr == "" {
				return
			}
			if node, _ := res.Set().Value("k8s.node.name"); node.AsString() != "n1" {
				t.Errorf("k8s.node.name = %q, want n1: the rest of OTEL_RESOURCE_ATTRIBUTES must be kept", node.AsString())
			}
		})
	}
}
