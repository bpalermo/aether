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
