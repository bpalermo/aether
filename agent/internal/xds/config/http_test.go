package config

import (
	"testing"

	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
)

func TestHttp1ProtocolOptions(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "returns valid HTTP/1 protocol options",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Http1ProtocolOptions()

			if got == nil {
				t.Fatal("expected non-nil HttpProtocolOptions")
			}

			explicit, ok := got.UpstreamProtocolOptions.(*httpv3.HttpProtocolOptions_ExplicitHttpConfig_)
			if !ok {
				t.Fatal("expected ExplicitHttpConfig")
			}

			http1, ok := explicit.ExplicitHttpConfig.ProtocolConfig.(*httpv3.HttpProtocolOptions_ExplicitHttpConfig_HttpProtocolOptions)
			if !ok {
				t.Fatal("expected Http1ProtocolOptions")
			}

			if http1.HttpProtocolOptions == nil {
				t.Fatal("expected non-nil Http1ProtocolOptions")
			}
		})
	}
}

func TestHttp2ProtocolOptions(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "returns valid HTTP/2 protocol options",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Http2ProtocolOptions()

			if got == nil {
				t.Fatal("expected non-nil HttpProtocolOptions")
			}

			explicit, ok := got.UpstreamProtocolOptions.(*httpv3.HttpProtocolOptions_ExplicitHttpConfig_)
			if !ok {
				t.Fatal("expected ExplicitHttpConfig")
			}

			http2, ok := explicit.ExplicitHttpConfig.ProtocolConfig.(*httpv3.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions)
			if !ok {
				t.Fatal("expected Http2ProtocolOptions")
			}

			if http2.Http2ProtocolOptions == nil {
				t.Fatal("expected non-nil Http2ProtocolOptions")
			}
		})
	}
}

// TestUseDownstreamProtocolOptions verifies the passthrough helper (issue #568)
// selects Envoy's USE_DOWNSTREAM_PROTOCOL mode and populates BOTH the HTTP/1 and
// HTTP/2 codec configs, so an h2c downstream (non-mesh OTLP gRPC through the
// redirect-all capture passthrough) dials h2c upstream instead of the
// ORIGINAL_DST default HTTP/1.1 that resets an h2-only server.
func TestUseDownstreamProtocolOptions(t *testing.T) {
	got := UseDownstreamProtocolOptions()
	if got == nil {
		t.Fatal("expected non-nil HttpProtocolOptions")
	}

	// Must be the use-downstream-protocol oneof, NOT an explicit http1/http2 config.
	// (An explicit config would pin the upstream protocol and reintroduce #568 for
	// whichever protocol it did not pin.)
	useDownstream, ok := got.UpstreamProtocolOptions.(*httpv3.HttpProtocolOptions_UseDownstreamProtocolConfig)
	if !ok {
		t.Fatalf("expected UseDownstreamProtocolConfig, got %T", got.UpstreamProtocolOptions)
	}

	cfg := useDownstream.UseDownstreamProtocolConfig
	if cfg == nil {
		t.Fatal("expected non-nil UseDownstreamHttpConfig")
	}
	// Both codecs must be present so Envoy has concrete config for whichever
	// protocol the sniffed downstream turns out to be: h1 (http/1.1 egress) and
	// h2c (the gRPC case #568 regresses on).
	if cfg.GetHttpProtocolOptions() == nil {
		t.Fatal("expected non-nil Http1ProtocolOptions (http/1.1 downstream must map to h1 upstream)")
	}
	if cfg.GetHttp2ProtocolOptions() == nil {
		t.Fatal("expected non-nil Http2ProtocolOptions (h2c downstream must map to h2c upstream — the #568 fix)")
	}
}

// TestUpstreamIdleTimeoutSet verifies the protocol-options helpers carry the
// 30s idle timeout. Service clusters pool per downstream connection for
// per-source mTLS; an orphaned pool's upstream connection is reclaimed ONLY by
// this timeout (Envoy default 1h leaked ~41k mTLS conns / 3.2 GiB per proxy
// under non-keepalive downstream traffic on talos-main, 2026-06-11).
func TestUpstreamIdleTimeoutSet(t *testing.T) {
	for name, opts := range map[string]*httpv3.HttpProtocolOptions{
		"http1":          Http1ProtocolOptions(),
		"http2":          Http2ProtocolOptions(),
		"use_downstream": UseDownstreamProtocolOptions(),
	} {
		idle := opts.GetCommonHttpProtocolOptions().GetIdleTimeout()
		if idle == nil {
			t.Fatalf("%s: idle timeout must be set (orphaned per-downstream pools leak without it)", name)
		}
		if idle.AsDuration() != UpstreamIdleTimeout {
			t.Fatalf("%s: idle timeout = %v, want %v", name, idle.AsDuration(), UpstreamIdleTimeout)
		}
	}
}
