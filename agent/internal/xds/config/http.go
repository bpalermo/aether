package config

import (
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"google.golang.org/protobuf/types/known/durationpb"
)

// UpstreamIdleTimeout bounds how long an idle upstream connection (and its
// pool) survives. Service clusters use connection_pool_per_downstream_connection
// for per-source mTLS, so when a downstream connection closes its dedicated
// pool is orphaned — it can never be selected again, and the only thing that
// reclaims its upstream connection is this idle timeout. Envoy's default is
// 1 HOUR: under non-keepalive downstream traffic that plateaus at
// rate×3600 leaked mTLS connections per proxy (observed: ~41k active upstream
// conns and 3.2 GiB heap within minutes on talos-main). 30s caps the orphan
// window; for live downstream connections an idle upstream is simply
// re-established on the next request.
const UpstreamIdleTimeout = 30 * time.Second

// Http1ProtocolOptions creates HTTP/1.1 protocol options for upstream clusters.
// This is used to configure Envoy to communicate with services that only support HTTP/1.1.
func Http1ProtocolOptions() *httpv3.HttpProtocolOptions {
	return &httpv3.HttpProtocolOptions{
		CommonHttpProtocolOptions: &corev3.HttpProtocolOptions{
			IdleTimeout: durationpb.New(UpstreamIdleTimeout),
		},
		UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{
			ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
				ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_HttpProtocolOptions{
					HttpProtocolOptions: &corev3.Http1ProtocolOptions{},
				},
			},
		},
	}
}

// Http2ProtocolOptions creates HTTP/2 protocol options for upstream clusters.
// This is used to configure Envoy to communicate with services that support HTTP/2.
func Http2ProtocolOptions() *httpv3.HttpProtocolOptions {
	return &httpv3.HttpProtocolOptions{
		CommonHttpProtocolOptions: &corev3.HttpProtocolOptions{
			IdleTimeout: durationpb.New(UpstreamIdleTimeout),
		},
		UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{
			ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
				ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{
					Http2ProtocolOptions: &corev3.Http2ProtocolOptions{},
				},
			},
		},
	}
}

// UseDownstreamProtocolOptions creates HTTP protocol options that make the
// upstream connection MIRROR the downstream protocol (Envoy's
// USE_DOWNSTREAM_PROTOCOL semantics): an HTTP/1.1 downstream dials HTTP/1.1
// upstream, an HTTP/2 (incl. h2c) downstream dials h2c upstream.
//
// This is required for the redirect-all capture passthrough (proposal 022):
// non-mesh traffic sniffed as cleartext HTTP by http_inspector transits the
// cap_http HCM and is forwarded to the ORIGINAL_DST passthrough cluster. Without
// these options the passthrough defaults to HTTP/1.1 upstream, so an h2c gRPC
// client to a non-mesh h2-only server (e.g. an OTLP otel-collector on :4317)
// gets "reset reason: protocol error" — the HCM dialed HTTP/1.1 to an h2-only
// upstream (issue #568). Mirroring the downstream protocol keeps both cleartext
// HTTP/1.1 and h2c non-mesh egress working through the passthrough.
//
// Both Http1 and Http2 option messages are populated so Envoy has the concrete
// codec config for whichever protocol the downstream turns out to be.
func UseDownstreamProtocolOptions() *httpv3.HttpProtocolOptions {
	return &httpv3.HttpProtocolOptions{
		CommonHttpProtocolOptions: &corev3.HttpProtocolOptions{
			IdleTimeout: durationpb.New(UpstreamIdleTimeout),
		},
		UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_UseDownstreamProtocolConfig{
			UseDownstreamProtocolConfig: &httpv3.HttpProtocolOptions_UseDownstreamHttpConfig{
				HttpProtocolOptions:  &corev3.Http1ProtocolOptions{},
				Http2ProtocolOptions: &corev3.Http2ProtocolOptions{},
			},
		},
	}
}
