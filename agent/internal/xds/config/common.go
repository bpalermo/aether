// Package config provides Envoy configuration helpers for xDS resources.
// It contains utility functions for building common Envoy configuration patterns
// such as ADS config sources, protocol options, and SPIRE cluster configurations.
package config

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// XDSConfigSourceADS creates a ConfigSource that uses ADS (Aggregated Discovery Service)
// for dynamic configuration updates.
func XDSConfigSourceADS() *corev3.ConfigSource {
	return &corev3.ConfigSource{
		ConfigSourceSpecifier: &corev3.ConfigSource_Ads{},
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
