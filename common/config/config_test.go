package config

import (
	"strings"
	"testing"
)

func TestParse_Valid(t *testing.T) {
	doc := []byte(`
meshDomain: aether.internal
telemetry:
  enabled: true
  otlpEndpoint: otel-collector.o11y.svc:4317
  statsEmitPod: true
  accessLogs:
    enabled: true
    successSampleRate: 25
  proxyTracing:
    enabled: true
    sampleRate: 0.5
  tracing:
    sampleRate: 0.2
    export: true
  logs:
    enabled: true
security:
  spireEnabled: true
`)

	cfg, err := Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got := cfg.GetMeshDomain(); got != "aether.internal" {
		t.Errorf("meshDomain = %q, want aether.internal", got)
	}
	if !cfg.GetTelemetry().GetEnabled() {
		t.Error("telemetry.enabled = false, want true")
	}
	if got := cfg.GetTelemetry().GetOtlpEndpoint(); got != "otel-collector.o11y.svc:4317" {
		t.Errorf("otlpEndpoint = %q", got)
	}
	if got := cfg.GetTelemetry().GetAccessLogs().GetSuccessSampleRate(); got != 25 {
		t.Errorf("successSampleRate = %d, want 25", got)
	}
	if got := cfg.GetTelemetry().GetProxyTracing().GetSampleRate(); got != 0.5 {
		t.Errorf("proxyTracing.sampleRate = %v, want 0.5", got)
	}
	if !cfg.GetSecurity().GetSpireEnabled() {
		t.Error("security.spireEnabled = false, want true")
	}
}

func TestParse_Defaults(t *testing.T) {
	// Sub-messages present but rate/percent fields unset -> historical defaults.
	doc := []byte(`
meshDomain: aether.internal
telemetry:
  accessLogs:
    enabled: true
  proxyTracing:
    enabled: true
  tracing:
    export: false
`)

	cfg, err := Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.GetTelemetry().GetAccessLogs().GetSuccessSampleRate(); got != 100 {
		t.Errorf("default successSampleRate = %d, want 100", got)
	}
	if got := cfg.GetTelemetry().GetProxyTracing().GetSampleRate(); got != 0.01 {
		t.Errorf("default proxyTracing.sampleRate = %v, want 0.01", got)
	}
	if got := cfg.GetTelemetry().GetTracing().GetSampleRate(); got != 0.1 {
		t.Errorf("default tracing.sampleRate = %v, want 0.1", got)
	}
}

func TestParse_ExplicitZeroPreserved(t *testing.T) {
	// An explicit 0 must NOT be overwritten by the default.
	doc := []byte(`
meshDomain: aether.internal
telemetry:
  proxyTracing:
    enabled: true
    sampleRate: 0
`)
	cfg, err := Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.GetTelemetry().GetProxyTracing().GetSampleRate(); got != 0 {
		t.Errorf("explicit sampleRate = %v, want 0", got)
	}
}

func TestParse_Errors(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "missing mesh domain",
			doc:  "telemetry:\n  enabled: true\n",
			want: "validation",
		},
		{
			name: "invalid mesh domain",
			doc:  "meshDomain: not a hostname!\n",
			want: "validation",
		},
		{
			name: "unknown field",
			doc:  "meshDomain: aether.internal\nbogusKey: 1\n",
			want: "MeshConfig schema",
		},
		{
			name: "percent over 100",
			doc:  "meshDomain: aether.internal\ntelemetry:\n  accessLogs:\n    successSampleRate: 101\n",
			want: "validation",
		},
		{
			name: "sample rate over 1",
			doc:  "meshDomain: aether.internal\ntelemetry:\n  proxyTracing:\n    sampleRate: 1.5\n",
			want: "validation",
		},
		{
			name: "otlp endpoint without port",
			doc:  "meshDomain: aether.internal\ntelemetry:\n  otlpEndpoint: justahost\n",
			want: "validation",
		},
		{
			name: "not yaml",
			doc:  "\tthis: : is broken",
			want: "YAML",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want error containing %q", tt.doc, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.want)
			}
		})
	}
}
