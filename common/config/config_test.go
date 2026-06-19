package config

import (
	"strings"
	"testing"
)

func TestParse_Valid(t *testing.T) {
	doc := []byte(`
proxy:
  accessLogsEnabled: true
  accessLogSuccessSampleRate: 25
  tracingEnabled: true
  traceSampleRate: 0.5
  emitStatsPod: true
`)

	cfg, err := Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := cfg.GetProxy()
	if !p.GetAccessLogsEnabled() {
		t.Error("accessLogsEnabled = false, want true")
	}
	if got := p.GetAccessLogSuccessSampleRate(); got != 25 {
		t.Errorf("accessLogSuccessSampleRate = %d, want 25", got)
	}
	if got := p.GetTraceSampleRate(); got != 0.5 {
		t.Errorf("traceSampleRate = %v, want 0.5", got)
	}
	if !p.GetEmitStatsPod() {
		t.Error("emitStatsPod = false, want true")
	}
}

func TestParse_Empty(t *testing.T) {
	// An empty doc is valid: the proxy inherits everything from the aether config.
	cfg, err := Parse([]byte("{}"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.GetProxy() != nil {
		t.Errorf("expected nil proxy override, got %v", cfg.GetProxy())
	}
}

func TestParse_PresenceDistinguished(t *testing.T) {
	// An explicit false must be distinguishable from unset (optional fields).
	cfg, err := Parse([]byte("proxy:\n  tracingEnabled: false\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.GetProxy().TracingEnabled == nil {
		t.Fatal("tracingEnabled should be present (explicit false), got nil")
	}
	if cfg.GetProxy().GetTracingEnabled() {
		t.Error("tracingEnabled = true, want explicit false")
	}
}

// Unknown fields are ignored, not rejected: forward-compatibility for rolling
// upgrades (an older binary loading a newer projected config).
func TestParse_UnknownFieldIgnored(t *testing.T) {
	cfg, err := Parse([]byte("proxy:\n  tracingEnabled: true\n  futureKnob: 42\n"))
	if err != nil {
		t.Fatalf("Parse with unknown field should not error: %v", err)
	}
	if !cfg.GetProxy().GetTracingEnabled() {
		t.Error("known field tracingEnabled should still be parsed")
	}
}

func TestParse_Errors(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "percent over 100",
			doc:  "proxy:\n  accessLogSuccessSampleRate: 101\n",
			want: "validation",
		},
		{
			name: "sample rate over 1",
			doc:  "proxy:\n  traceSampleRate: 1.5\n",
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
