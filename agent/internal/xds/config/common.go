// Package config provides Envoy configuration helpers for xDS resources.
// It contains utility functions for building common Envoy configuration patterns
// such as ADS config sources, protocol options, and SPIRE cluster configurations.
package config

import (
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// XDSConfigSourceADS creates a ConfigSource that uses ADS (Aggregated Discovery Service)
// for dynamic configuration updates.
func XDSConfigSourceADS() *corev3.ConfigSource {
	return &corev3.ConfigSource{
		ConfigSourceSpecifier: &corev3.ConfigSource_Ads{},
	}
}

// XDSConfigSourceADSWithInitialFetch is XDSConfigSourceADS with an EXPLICIT
// initial_fetch_timeout.
//
// What the field actually does, because the name invites the opposite reading
// (issue #817): it bounds how long the owning listener stays WARMING for the
// first config on this subscription. When it expires Envoy gives up waiting and
// moves the listener to active ANYWAY — with whatever it has, which for a route
// config that never arrived is nothing at all. So it is a fail-OPEN bound, not a
// gate: setting it can never stop a listener going active with an unresolved
// route table, it only decides how long the control plane is given first.
//
// 0 means "wait forever", which is NOT a safe alternative here: an agent that is
// permanently unable to publish would wedge the egress listener in warming for
// the life of the process, turning a degraded route table into a total
// data-plane outage on that node (and the proxy would never report Ready).
//
// Envoy's own default is 15s, so passing 15s is a PIN rather than a behaviour
// change — it makes the value protocol-visible, assertable in
// //test/envoy_validate, and immune to an upstream default drift.
func XDSConfigSourceADSWithInitialFetch(d time.Duration) *corev3.ConfigSource {
	return &corev3.ConfigSource{
		ConfigSourceSpecifier: &corev3.ConfigSource_Ads{},
		InitialFetchTimeout:   durationpb.New(d),
	}
}

// SDSConfigSourceFromCluster creates a ConfigSource that fetches secrets over a
// gRPC SDS stream to a named (static, bootstrap-defined) cluster. The edge
// proxy uses it to point its transport sockets at the SPIRE Agent's native
// Envoy SDS API (served on the Workload API socket), so Envoy fetches its own
// SVID and trust bundle straight from SPIRE — no agent-side SPIRE bridge. The
// node proxy keeps the ADS source (XDSConfigSourceADS) because it multiplexes
// many workload identities the agent delivers as snapshot secrets.
func SDSConfigSourceFromCluster(clusterName string) *corev3.ConfigSource {
	return &corev3.ConfigSource{
		ResourceApiVersion: corev3.ApiVersion_V3,
		ConfigSourceSpecifier: &corev3.ConfigSource_ApiConfigSource{
			ApiConfigSource: &corev3.ApiConfigSource{
				ApiType:             corev3.ApiConfigSource_GRPC,
				TransportApiVersion: corev3.ApiVersion_V3,
				GrpcServices: []*corev3.GrpcService{
					{
						TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
							EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: clusterName},
						},
					},
				},
			},
		},
	}
}

// TypedConfig wraps a protobuf message as a Google Any type.
// This is used to package Envoy extension configurations for transport in xDS messages.
//
// The inner message is marshalled DETERMINISTICALLY. An Any freezes its payload
// bytes at construction: the enclosing resource's later marshal (go-control-plane's
// MarshalResource, which does set Deterministic) re-encodes the Any's `value` field
// verbatim and cannot canonicalise what is inside it. So any map field in an
// extension config — structpb.Struct.fields is the one aether actually ships, in the
// aether_stats TypedStruct on every per-pod HCM — would otherwise serialise in Go's
// randomised map order and give the enclosing Listener/Cluster/Route a different
// hash on every rebuild of identical config. That is incident #135's mechanism
// (issue #772, races report S6) reaching an emitted resource through an Any rather
// than through a repeated field. See agent/internal/xds/cache/ordering.go.
func TypedConfig(config proto.Message) *anypb.Any {
	c := &anypb.Any{}
	if err := anypb.MarshalFrom(c, config, proto.MarshalOptions{Deterministic: true}); err != nil {
		return nil
	}
	return c
}
