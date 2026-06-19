package meshconfig

import (
	"encoding/json"
	"strings"
	"testing"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/bpalermo/aether/common/config"
	"google.golang.org/protobuf/proto"
)

func TestValidate(t *testing.T) {
	// nil spec (inherit everything) is valid.
	if err := Validate(nil); err != nil {
		t.Errorf("Validate(nil) = %v, want nil", err)
	}
	// in-range values are valid.
	ok := &configv1.MeshConfigSpec{Proxy: &configv1.ProxyTelemetry{TraceSampleRate: proto.Float64(0.5)}}
	if err := Validate(ok); err != nil {
		t.Errorf("Validate(valid) = %v, want nil", err)
	}
	// out-of-range trace rate is rejected.
	bad := &configv1.MeshConfigSpec{Proxy: &configv1.ProxyTelemetry{TraceSampleRate: proto.Float64(1.5)}}
	if err := Validate(bad); err == nil {
		t.Error("Validate(traceSampleRate=1.5) = nil, want error")
	}
}

func TestRenderConfigMapData_RoundTrips(t *testing.T) {
	spec := &configv1.MeshConfigSpec{Proxy: &configv1.ProxyTelemetry{
		TracingEnabled:  proto.Bool(true),
		TraceSampleRate: proto.Float64(0.25),
	}}
	data, err := RenderConfigMapData(spec)
	if err != nil {
		t.Fatalf("RenderConfigMapData: %v", err)
	}
	doc, ok := data[ConfigMapKey]
	if !ok {
		t.Fatalf("missing key %q", ConfigMapKey)
	}
	// What the controller projects must load through the agent's loader.
	loaded, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("projected ConfigMap does not load: %v\n%s", err, doc)
	}
	if got := loaded.GetProxy().GetTraceSampleRate(); got != 0.25 {
		t.Errorf("round-tripped traceSampleRate = %v, want 0.25", got)
	}
}

func TestRenderConfigMapData_NilSpec(t *testing.T) {
	data, err := RenderConfigMapData(nil)
	if err != nil {
		t.Fatalf("RenderConfigMapData(nil): %v", err)
	}
	if _, err := config.Parse([]byte(data[ConfigMapKey])); err != nil {
		t.Errorf("empty projection does not load: %v", err)
	}
}

// The typed MeshConfig jsonshim must round-trip the proto spec through protojson
// (camelCase field names, optional presence), not encoding/json.
func TestMeshConfigJSONShim(t *testing.T) {
	mc := &crdv1.MeshConfig{Spec: &configv1.MeshConfigSpec{Proxy: &configv1.ProxyTelemetry{
		AccessLogsEnabled: proto.Bool(true),
		TraceSampleRate:   proto.Float64(0.1),
	}}}
	raw, err := json.Marshal(mc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), "accessLogsEnabled") {
		t.Errorf("expected camelCase protojson field in:\n%s", raw)
	}

	var got crdv1.MeshConfig
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.Spec.GetProxy().GetAccessLogsEnabled() {
		t.Error("round-tripped accessLogsEnabled = false, want true")
	}
	if got.Spec.GetProxy().GetTraceSampleRate() != 0.1 {
		t.Errorf("round-tripped traceSampleRate = %v, want 0.1", got.Spec.GetProxy().GetTraceSampleRate())
	}

	// Unknown spec fields are IGNORED, not rejected (protobuf forward-compat for
	// rolling upgrades): an older decoder must tolerate a newer CR.
	var fwd crdv1.MeshConfig
	if err := json.Unmarshal([]byte(`{"spec":{"proxy":{"tracingEnabled":true,"futureKnob":42}}}`), &fwd); err != nil {
		t.Fatalf("unknown spec field should be ignored, got error: %v", err)
	}
	if !fwd.Spec.GetProxy().GetTracingEnabled() {
		t.Error("known field should still decode alongside an unknown one")
	}
}
