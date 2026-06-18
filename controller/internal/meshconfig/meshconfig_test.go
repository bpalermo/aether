package meshconfig

import (
	"strings"
	"testing"

	"github.com/bpalermo/aether/common/config"
)

func TestProtoFromSpec_Valid(t *testing.T) {
	spec := map[string]any{
		"meshDomain": "aether.internal",
		"telemetry": map[string]any{
			"enabled":      true,
			"otlpEndpoint": "otel-collector.o11y.svc:4317",
			"accessLogs":   map[string]any{"enabled": true},
		},
		"security": map[string]any{"spireEnabled": true},
	}

	cfg, err := ProtoFromSpec(spec)
	if err != nil {
		t.Fatalf("ProtoFromSpec: %v", err)
	}
	if cfg.GetMeshDomain() != "aether.internal" {
		t.Errorf("meshDomain = %q", cfg.GetMeshDomain())
	}
	// Defaults are applied through the shared loader.
	if got := cfg.GetTelemetry().GetAccessLogs().GetSuccessSampleRate(); got != 100 {
		t.Errorf("default successSampleRate = %d, want 100", got)
	}
}

func TestProtoFromSpec_Invalid(t *testing.T) {
	spec := map[string]any{"meshDomain": "not a host!"}
	if _, err := ProtoFromSpec(spec); err == nil {
		t.Fatal("expected validation error for bad mesh domain")
	}
}

func TestProtoFromSpec_UnknownField(t *testing.T) {
	spec := map[string]any{"meshDomain": "aether.internal", "bogus": 1}
	if _, err := ProtoFromSpec(spec); err == nil {
		t.Fatal("expected error for unknown field")
	}
}

// The projected ConfigMap must round-trip back through the file loader the agent
// and registrar use, so what the controller writes is exactly what they accept.
func TestRenderConfigMapData_RoundTrips(t *testing.T) {
	spec := map[string]any{
		"meshDomain": "aether.internal",
		"telemetry": map[string]any{
			"otlpEndpoint": "otel-collector.o11y.svc:4317",
			"proxyTracing": map[string]any{"enabled": true, "sampleRate": 0.25},
		},
		"security": map[string]any{"spireEnabled": true},
	}
	cfg, err := ProtoFromSpec(spec)
	if err != nil {
		t.Fatalf("ProtoFromSpec: %v", err)
	}

	data, err := RenderConfigMapData(cfg)
	if err != nil {
		t.Fatalf("RenderConfigMapData: %v", err)
	}
	doc, ok := data[ConfigMapKey]
	if !ok {
		t.Fatalf("missing key %q in projected data", ConfigMapKey)
	}

	loaded, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("projected ConfigMap does not load: %v\n%s", err, doc)
	}
	if loaded.GetMeshDomain() != "aether.internal" {
		t.Errorf("round-tripped meshDomain = %q", loaded.GetMeshDomain())
	}
	if got := loaded.GetTelemetry().GetProxyTracing().GetSampleRate(); got != 0.25 {
		t.Errorf("round-tripped sampleRate = %v, want 0.25", got)
	}
	if !strings.Contains(doc, "aether.internal") {
		t.Errorf("projected document missing meshDomain:\n%s", doc)
	}
}
