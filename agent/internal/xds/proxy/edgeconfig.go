package proxy

import (
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
)

// Edge best-practice defaults (Envoy edge doc; proposal 029). Applied when the
// corresponding EdgeConfigSpec field is unset — so an empty/absent EdgeConfig still
// yields a hardened edge.
const (
	edgeDefaultStreamIdleTimeout       = 300 * time.Second
	edgeDefaultRequestTimeout          = 300 * time.Second
	edgeDefaultIdleTimeout             = 3600 * time.Second
	edgeDefaultMaxConcurrentStreams    = 100
	edgeDefaultInitialStreamWindow     = 65536   // 64 KiB
	edgeDefaultInitialConnectionWindow = 1048576 // 1 MiB
	edgeDefaultPerConnBufferBytes      = 32 * 1024
)

// ApplyEdgeHardening applies the Envoy edge best-practices to an edge HCM +
// listener from the effective EdgeConfigSpec (proposal 029). A nil spec (or any
// unset field) uses the compiled best-practice default. Edge-only — the node-proxy
// HCMs have a different (mesh-internal) threat model.
func ApplyEdgeHardening(hcm *http_connection_managerv3.HttpConnectionManager, l *listenerv3.Listener, spec *configv1.EdgeConfigSpec) {
	// use_remote_address: default TRUE (the edge trusts the connection source, not a
	// forgeable XFF). This is also the fix for the previously-missing setting.
	hcm.UseRemoteAddress = wrapperspb.Bool(boolOr(spec.GetUseRemoteAddress(), true))
	hcm.XffNumTrustedHops = spec.GetXffNumTrustedHops().GetValue()

	if hcm.GetCommonHttpProtocolOptions() == nil {
		hcm.CommonHttpProtocolOptions = &corev3.HttpProtocolOptions{}
	}
	hcm.CommonHttpProtocolOptions.IdleTimeout = durationpb.New(durationOr(spec.GetIdleTimeout(), edgeDefaultIdleTimeout))
	hcm.CommonHttpProtocolOptions.HeadersWithUnderscoresAction = underscoresAction(spec)

	hcm.StreamIdleTimeout = durationpb.New(durationOr(spec.GetStreamIdleTimeout(), edgeDefaultStreamIdleTimeout))
	hcm.RequestTimeout = durationpb.New(durationOr(spec.GetRequestTimeout(), edgeDefaultRequestTimeout))

	hcm.Http2ProtocolOptions = &corev3.Http2ProtocolOptions{
		MaxConcurrentStreams:        wrapperspb.UInt32(uint32Or(spec.GetHttp2().GetMaxConcurrentStreams(), edgeDefaultMaxConcurrentStreams)),
		InitialStreamWindowSize:     wrapperspb.UInt32(uint32Or(spec.GetHttp2().GetInitialStreamWindowSize(), edgeDefaultInitialStreamWindow)),
		InitialConnectionWindowSize: wrapperspb.UInt32(uint32Or(spec.GetHttp2().GetInitialConnectionWindowSize(), edgeDefaultInitialConnectionWindow)),
	}

	if l != nil {
		l.PerConnectionBufferLimitBytes = wrapperspb.UInt32(uint32Or(spec.GetPerConnectionBufferLimitBytes(), edgeDefaultPerConnBufferBytes))
	}
}

func underscoresAction(spec *configv1.EdgeConfigSpec) corev3.HttpProtocolOptions_HeadersWithUnderscoresAction {
	switch spec.GetHeadersWithUnderscoresAction() {
	case configv1.EdgeConfigSpec_HEADERS_WITH_UNDERSCORES_ACTION_ALLOW:
		return corev3.HttpProtocolOptions_ALLOW
	case configv1.EdgeConfigSpec_HEADERS_WITH_UNDERSCORES_ACTION_DROP_HEADER:
		return corev3.HttpProtocolOptions_DROP_HEADER
	default: // UNSPECIFIED or REJECT_REQUEST → the best-practice default
		return corev3.HttpProtocolOptions_REJECT_REQUEST
	}
}

func boolOr(v *wrapperspb.BoolValue, def bool) bool {
	if v == nil {
		return def
	}
	return v.GetValue()
}

func uint32Or(v *wrapperspb.UInt32Value, def uint32) uint32 {
	if v == nil || v.GetValue() == 0 {
		return def
	}
	return v.GetValue()
}

// durationOr returns d, or def when d is nil. A zero duration is honored (request
// timeout 0 = disabled), so only nil falls to the default.
func durationOr(d *durationpb.Duration, def time.Duration) time.Duration {
	if d == nil {
		return def
	}
	return d.AsDuration()
}
