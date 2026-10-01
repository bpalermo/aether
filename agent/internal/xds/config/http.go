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

// DefaultQUICTwinIdleTimeout is the idle timeout of an HTTP/3 twin's pool (the
// agent's --east-west-quic-idle-timeout; aether#1054). It is shorter than
// UpstreamIdleTimeout because a source h3 connection must not outlive the
// destination proxy's hot-restart parent. Once the parent stops reading its
// UDP sockets, a packet on a connection it owned reaches the child, which
// answers with a stateless reset the source accepts (the token is derived from
// the connection ID alone, so both epochs mint the same one). A connection the
// draining parent answered with GOAWAY (--drain-strategy immediate) leaves the
// source's pool on its own; an IDLE one receives no GOAWAY, and only this
// timeout retires it. The chart enforces idle + 5s < parentShutdownTime, so a
// connection that is idle when the drain starts is closed by the source before
// the parent exits. h1/h2 pools keep UpstreamIdleTimeout.
const DefaultQUICTwinIdleTimeout = 8 * time.Second

// QUICTwinKeepaliveInterval and QUICTwinNetworkIdleTimeout are an HTTP/3 twin's
// dead-peer detection (aether#1087). Without them a twin had none in the
// window that matters: a request the destination has received and ACKed,
// whose answer never comes back because the destination pod's network
// vanished (CNI DEL removes the veth while the request is in flight; the
// destination proxy's 503 is written into a netns with no route out). QUIC
// has no RST, and with the request ACKed the source has nothing in flight, so
// neither PTO nor QUICHE's 5-RTO blackhole detector is armed
// (QuicConnection::ShouldDetectBlackhole needs retransmittable bytes in
// flight). What is left is the transport idle timeout -- min(QUICHE's 600 s
// client default, the inbound listener's 300 s default) -- and the client
// keep-alive PING, QUICHE's kPingTimeoutSecs = 15 s. Both lose to the route's
// 15 s timeout, so the request hung for the full 15 s and answered 504 UT
// (2026-09-30 and 2026-10-01 soaks).
//
// With these two set, while a request stream is open
// (QuicSpdySession::ShouldKeepConnectionAlive) the source PINGs after
// QUICTwinKeepaliveInterval of silence (QUICHE arms that alarm with 1 s
// granularity), and QuicIdleNetworkDetector closes the connection
// QUICTwinNetworkIdleTimeout after the first ack-eliciting packet sent since
// the peer was last heard from. A dead peer is therefore detected within
// interval + 1 s + idle (<= 6 s) of its last packet; a live one ACKs the PING
// and is never touched, however slow its application is. That is why this is
// transport liveness and not a per-try timeout: it fails what is dead, never
// what is merely slow, and it lives on the twin CLUSTER, not on the route
// the twin shares with its h2 base.
//
// The closed connection resets its open streams as ConnectionTermination
// (quicErrorCodeToEnvoyLocalResetReason: QUIC_NETWORK_IDLE_TIMEOUT after the
// handshake), which the mesh retry policy does NOT retry: the request was
// sent, so it may have reached the application. It answers 503 UC fast; a
// GET and a POST alike are never replayed.
//
// The idle timeout is negotiated as min(client, server), so the destination
// also closes an idle twin connection after QUICTwinNetworkIdleTimeout. That
// caps the pool idle (DefaultQUICTwinIdleTimeout) for a connection with no
// open stream -- keep-alive PINGs only run while a stream is open -- which
// can only shorten how long an idle connection outlives a destination
// hot-restart parent (aether#1054). The client's deadline is always the
// earlier one (the server last heard the client's ACK of its final packet),
// so the client never sends into a connection the server already closed.
const (
	QUICTwinKeepaliveInterval  = 1 * time.Second
	QUICTwinNetworkIdleTimeout = 4 * time.Second
)

// QUICTwinTransportOptions is the QuicProtocolOptions of every HTTP/3 twin:
// QUICHE defaults except the aether#1087 dead-peer detection above.
func QUICTwinTransportOptions() *corev3.QuicProtocolOptions {
	return &corev3.QuicProtocolOptions{
		IdleNetworkTimeout: durationpb.New(QUICTwinNetworkIdleTimeout),
		ConnectionKeepalive: &corev3.QuicKeepAliveSettings{
			MaxInterval: durationpb.New(QUICTwinKeepaliveInterval),
		},
	}
}

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

// Http3ProtocolOptions is the HTTP/3 twin of Http2ProtocolOptions for a QUIC
// upstream (proposal 038 Phase 4b): explicit HTTP/3 and the twin idle timeout
// (idle <= 0 means DefaultQUICTwinIdleTimeout, which says why it is not
// UpstreamIdleTimeout; aether#1054), and the QUIC transport's dead-peer
// detection (QUICTwinTransportOptions; aether#1087). Envoy
// requires the explicit_http_config form for an h3 upstream and rejects a
// cluster that carries it without a QUIC transport socket, which is what makes
// the pairing in proxy.QUICClusterFrom checkable by `envoy --mode validate`.
func Http3ProtocolOptions(idle time.Duration) *httpv3.HttpProtocolOptions {
	if idle <= 0 {
		idle = DefaultQUICTwinIdleTimeout
	}
	return &httpv3.HttpProtocolOptions{
		CommonHttpProtocolOptions: &corev3.HttpProtocolOptions{
			IdleTimeout: durationpb.New(idle),
		},
		UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{
			ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
				ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_Http3ProtocolOptions{
					Http3ProtocolOptions: &corev3.Http3ProtocolOptions{
						QuicProtocolOptions: QUICTwinTransportOptions(),
					},
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
