package config

import (
	"testing"
	"time"

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

// TestHttp3ProtocolOptionsIdleTimeout pins the h3 twin's idle timeout
// (aether#1054): the caller's value when set, DefaultQUICTwinIdleTimeout
// otherwise, and never the 30s h1/h2 value by accident.
func TestHttp3ProtocolOptionsIdleTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero means default", 0, DefaultQUICTwinIdleTimeout},
		{"negative means default", -time.Second, DefaultQUICTwinIdleTimeout},
		{"explicit", 5 * time.Second, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Http3ProtocolOptions(tc.in)
			if got := opts.GetCommonHttpProtocolOptions().GetIdleTimeout().AsDuration(); got != tc.want {
				t.Fatalf("idle timeout = %v, want %v", got, tc.want)
			}
			if opts.GetExplicitHttpConfig().GetHttp3ProtocolOptions() == nil {
				t.Fatal("expected explicit HTTP/3 protocol options")
			}
		})
	}
	if DefaultQUICTwinIdleTimeout >= UpstreamIdleTimeout {
		t.Fatalf("DefaultQUICTwinIdleTimeout %v must be below UpstreamIdleTimeout %v (aether#1054)", DefaultQUICTwinIdleTimeout, UpstreamIdleTimeout)
	}
}

// TestHttp3ProtocolOptionsDetectADeadPeer pins the twin's QUIC transport
// liveness (aether#1087) whatever pool idle timeout the caller passes: a
// keep-alive PING while a stream is open and an idle_network_timeout that
// closes a connection whose peer stopped answering, both well inside the
// 15 s route timeout they exist to beat. h1/h2 options carry nothing QUIC.
func TestHttp3ProtocolOptionsDetectADeadPeer(t *testing.T) {
	for _, idle := range []time.Duration{0, 5 * time.Second, 30 * time.Second} {
		q := Http3ProtocolOptions(idle).GetExplicitHttpConfig().GetHttp3ProtocolOptions().GetQuicProtocolOptions()
		if q == nil {
			t.Fatalf("idle %v: no quic_protocol_options", idle)
		}
		if got := q.GetIdleNetworkTimeout().AsDuration(); got != QUICTwinNetworkIdleTimeout {
			t.Errorf("idle %v: idle_network_timeout = %v, want %v", idle, got, QUICTwinNetworkIdleTimeout)
		}
		if got := q.GetConnectionKeepalive().GetMaxInterval().AsDuration(); got != QUICTwinKeepaliveInterval {
			t.Errorf("idle %v: keepalive max_interval = %v, want %v", idle, got, QUICTwinKeepaliveInterval)
		}
		if q.GetConnectionKeepalive().GetInitialInterval() != nil {
			t.Errorf("idle %v: initial_interval set; only max_interval is part of the bound", idle)
		}
	}
	// Envoy truncates idle_network_timeout to whole seconds (convertQuicConfig:
	// DurationUtil::durationToSeconds) and documents max_interval >= 1 s.
	if QUICTwinNetworkIdleTimeout%time.Second != 0 || QUICTwinNetworkIdleTimeout < time.Second {
		t.Errorf("QUICTwinNetworkIdleTimeout %v must be a whole number of seconds >= 1 s", QUICTwinNetworkIdleTimeout)
	}
	if QUICTwinKeepaliveInterval < time.Second {
		t.Errorf("QUICTwinKeepaliveInterval %v is below Envoy's documented 1 s floor", QUICTwinKeepaliveInterval)
	}
	// The keep-alive must fire well before the idle deadline, or a live but
	// quiet peer (a slow application) would be idled out.
	if 2*QUICTwinKeepaliveInterval >= QUICTwinNetworkIdleTimeout {
		t.Errorf("keepalive %v leaves no room for a live peer to answer before the %v idle deadline", QUICTwinKeepaliveInterval, QUICTwinNetworkIdleTimeout)
	}
	if bound := QUICTwinKeepaliveInterval + time.Second + QUICTwinNetworkIdleTimeout; bound >= 15*time.Second {
		t.Errorf("dead-peer bound %v does not beat the 15 s route timeout", bound)
	}
	for name, po := range map[string]interface {
		GetExplicitHttpConfig() *httpv3.HttpProtocolOptions_ExplicitHttpConfig
	}{"h1": Http1ProtocolOptions(), "h2": Http2ProtocolOptions()} {
		if po.GetExplicitHttpConfig().GetHttp3ProtocolOptions() != nil {
			t.Errorf("%s options carry HTTP/3 settings", name)
		}
	}
}
