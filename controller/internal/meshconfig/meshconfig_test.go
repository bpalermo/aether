package meshconfig

import (
	"strings"
	"testing"

	"github.com/bpalermo/aether/common/config"
)

func TestProtoFromSpec_Valid(t *testing.T) {
	spec := map[string]any{
		"proxy": map[string]any{
			"accessLogsEnabled": true,
			"tracingEnabled":    true,
			"traceSampleRate":   0.25,
		},
	}

	cfg, err := ProtoFromSpec(spec)
	if err != nil {
		t.Fatalf("ProtoFromSpec: %v", err)
	}
	if !cfg.GetProxy().GetAccessLogsEnabled() {
		t.Error("accessLogsEnabled = false, want true")
	}
	if got := cfg.GetProxy().GetTraceSampleRate(); got != 0.25 {
		t.Errorf("traceSampleRate = %v, want 0.25", got)
	}
}

func TestProtoFromSpec_Invalid(t *testing.T) {
	spec := map[string]any{"proxy": map[string]any{"traceSampleRate": 1.5}}
	if _, err := ProtoFromSpec(spec); err == nil {
		t.Fatal("expected validation error for traceSampleRate > 1")
	}
}

func TestProtoFromSpec_UnknownField(t *testing.T) {
	spec := map[string]any{"proxy": map[string]any{"bogus": 1}}
	if _, err := ProtoFromSpec(spec); err == nil {
		t.Fatal("expected error for unknown field")
	}
}

// The projected ConfigMap must round-trip back through the file loader the agent
// uses, so what the controller writes is exactly what the agent accepts.
func TestRenderConfigMapData_RoundTrips(t *testing.T) {
	spec := map[string]any{
		"proxy": map[string]any{
			"tracingEnabled":  true,
			"traceSampleRate": 0.25,
		},
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
	if got := loaded.GetProxy().GetTraceSampleRate(); got != 0.25 {
		t.Errorf("round-tripped traceSampleRate = %v, want 0.25", got)
	}
	if !strings.Contains(doc, "traceSampleRate") {
		t.Errorf("projected document missing proxy override:\n%s", doc)
	}
}
