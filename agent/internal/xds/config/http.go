package config

import (
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
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
// interval + 1 s + idle (<= 10 s) of its last packet; a live one ACKs the PING
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
// also closes an idle twin connection after QUICTwinNetworkIdleTimeout.
// Keep-alive PINGs only run while a stream is open, so for a connection with
// no open stream this is a second idle timeout next to the pool's
// (DefaultQUICTwinIdleTimeout); at equal values it costs no extra handshake.
// The client's deadline is always the earlier one (the server last heard the
// client's ACK of its final packet), so the client never sends into a
// connection the server already closed.
//
// WHY 8 s AND NOT LESS (aether#1093). The idle deadline cannot tell a dead
// peer from a peer, or a local worker, that is merely not being scheduled,
// and talos-main has recurring node-local stalls of Envoy worker threads
// (requests delayed 1-3.3 s, episodes of 5-7 s, outside rolls too; #1093).
//   - A DESTINATION worker stalled for >= the idle timeout cannot ACK the
//     PINGs, so every in-flight twin request to it fails 503 where today it
//     completes late.
//   - A SOURCE worker stalled for >= the idle timeout wakes with the idle
//     alarm expired. That is safe when the peer's packets are queued in the
//     socket: libevent activates fd events (event.c:2072, evsel->dispatch)
//     before expired timers (event.c:2091, timeout_process) in one FIFO
//     priority, Envoy stamps a read packet with the time it is READ
//     (network/utility.cc:599, :674), not when it arrived, so
//     QuicIdleNetworkDetector::OnPacketReceived pushes the deadline past now,
//     and re-arming the alarm removes its stale activation from the active
//     list (event.c:2796-2810). It is NOT safe when nothing is queued: a
//     request to a live but slow (silent) application, whose last ACK
//     predates the stall, is idled out, because QuicIdleNetworkDetector::OnAlarm
//     closes without re-checking the deadline (quic_idle_network_detector.cc:
//     28-30; QuicAlarm::Fire, quic_alarm.cc:82-93, does not either).
//
// So the timeout must exceed the longest stall we expect: 8 s, equal to the
// pool idle. The soak's failure COUNT is unchanged by any value (those
// requests were delivered and are not retryable); this only shortens the
// hang, and must not buy that with new 503s during stalls. Tighten once
// #1093 is fixed.
const (
	QUICTwinKeepaliveInterval  = 1 * time.Second
	QUICTwinNetworkIdleTimeout = 8 * time.Second
)

// MeshH2KeepaliveInterval, MeshH2KeepaliveTimeout and
// MeshH2KeepaliveJitterPercent are the HTTP/2 PING liveness of every h2 mesh
// service cluster (NewServiceCluster; aether#1104), the TCP sibling of the
// twins' QUIC liveness above and the same hole: a request the destination
// proxy received (TCP-ACKed), whose answer never comes back because the
// destination pod's network vanished. The node-shared destination proxy's
// inbound sockets live in the pod netns; CNI DEL deletes the veth, the proxy
// stays up and never closes them, so nothing -- no FIN, no RST -- can reach
// the source. With the request ACKed, the source has no unacked bytes, so no
// TCP retransmission timer runs either, and with neither TCP keepalive nor an
// HTTP/2 PING configured the request hung to the 15 s route timeout (504 UT).
// h2 carries waypointed (cross-cluster) and GAMMA weighted-split traffic since
// QUIC went unconditional, and the edge -> mesh hop; all three are
// NewServiceCluster.
//
// Envoy's h2 keepalive at the pin (source/common/http/http2/codec_impl.cc):
//   - The PING is sent every interval (+ 0..jitter% of it) after the previous
//     PING ACK, on EVERY connection -- with or without open streams; the send
//     timer is armed in the codec constructor and re-armed only by the ACK
//     (:1011-1027, onKeepaliveResponse :1058-1071). connection_idle_interval
//     is a different knob (a PING ahead of newStream on an idle connection,
//     :2420-2431) and is not used.
//   - The timeout timer is armed when the PING is sent (:1036-1056) and pushed
//     out by ANY frame received (:1216-1217), so a live peer streaming a large
//     response is never cut off by head-of-line blocking of its ACK.
//   - On expiry the connection is closed NoFlush with keepalive_timeout++
//     (:1073-1079): a dead peer is detected at most
//     interval*(1+jitter) + timeout = 1.15 s + 8 s <= ~9.2 s after its last
//     frame.
//   - The PING ACK is generated by the destination proxy's h2 codec, never by
//     the application, so a slow application is never touched; and the peer of
//     a NewServiceCluster connection is always an aether inbound HCM (directly,
//     or through the E/W tunnel's raw tcp_proxy). That is why these options are
//     NOT on Http2ProtocolOptions: the app hop (NewAppCluster) and the
//     authz/xDS/collector clusters talk to arbitrary h2 servers, and gRPC
//     servers answer frequent PINGs with GOAWAY too_many_pings.
//   - The pool's idle timer (CodecClient) follows streams only, so PINGs do not
//     keep an idle connection alive past UpstreamIdleTimeout.
//
// The close resets open streams as ConnectionTermination (CodecClient::onEvent:
// a LocalClose on a connected client, codec_client.cc:124-137), which the mesh
// retry policy does NOT retry: connect-failure covers only Local/Remote-
// ConnectionFailure and ConnectionTimeout, reset-before-request needs
// !upstream_request_started (router.cc:1650-1653), and a locally generated 503
// is not a 503 response (retry_state_impl.cc:433-490). The request was sent,
// so it may have reached the application: it answers 503 UC fast and is never
// replayed, GET and POST alike.
//
// WHY 8 s (aether#1093), the same budget and reason as
// QUICTwinNetworkIdleTimeout. A DESTINATION worker stalled for >= the timeout
// cannot ACK the PING, so in-flight h2 requests to it would fail 503 where today
// they complete late; talos-main has node-local worker stalls of 5-7 s. A
// SOURCE worker stall is safe for h2 (unlike QUIC's silent-application case):
// a live peer ACKs the PING within an RTT, so the ACK is already queued in the
// socket when the stalled worker wakes, and libevent runs fd events before
// expired timers (event.c:2072 before :2091) -- the read disables the timeout
// timer and removes its stale activation (event.c:2796-2810). Tighten once
// #1093 is fixed.
const (
	MeshH2KeepaliveInterval      = 1 * time.Second
	MeshH2KeepaliveTimeout       = 8 * time.Second
	MeshH2KeepaliveJitterPercent = 15.0
)

// MeshH2Keepalive is the h2 connection_keepalive of every mesh service
// cluster (aether#1104); see MeshH2KeepaliveInterval.
func MeshH2Keepalive() *corev3.KeepaliveSettings {
	return &corev3.KeepaliveSettings{
		Interval:       durationpb.New(MeshH2KeepaliveInterval),
		Timeout:        durationpb.New(MeshH2KeepaliveTimeout),
		IntervalJitter: &typev3.Percent{Value: MeshH2KeepaliveJitterPercent},
	}
}

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

// MeshHttp2ProtocolOptions is Http2ProtocolOptions plus the HTTP/2 PING
// dead-peer detection (MeshH2Keepalive; aether#1104). It is for clusters whose
// h2 peer is an aether inbound proxy -- NewServiceCluster -- and nothing else:
// an application or third-party h2 server may treat a PING a second as abuse.
func MeshHttp2ProtocolOptions() *httpv3.HttpProtocolOptions {
	po := Http2ProtocolOptions()
	po.GetExplicitHttpConfig().GetHttp2ProtocolOptions().ConnectionKeepalive = MeshH2Keepalive()
	return po
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
