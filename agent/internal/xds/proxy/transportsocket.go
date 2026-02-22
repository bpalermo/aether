package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	transport_sockets_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"google.golang.org/protobuf/proto"
)

const (
	tlsTransportSocketName = "envoy.transport_sockets.tls"
)

func DownstreamTransportSocket(sdsValidationContextName string) *corev3.TransportSocket {
	downstreamTlsContext := &transport_sockets_v3.DownstreamTlsContext{
		CommonTlsContext: &transport_sockets_v3.CommonTlsContext{
			ValidationContextType: &transport_sockets_v3.CommonTlsContext_ValidationContextSdsSecretConfig{
				ValidationContextSdsSecretConfig: &transport_sockets_v3.SdsSecretConfig{
					Name: sdsValidationContextName,
					SdsConfig: &corev3.ConfigSource{
						ResourceApiVersion: corev3.ApiVersion_V3,
						ConfigSourceSpecifier: &corev3.ConfigSource_ApiConfigSource{
							ApiConfigSource: &corev3.ApiConfigSource{
								ApiType: corev3.ApiConfigSource_GRPC,
								GrpcServices: []*corev3.GrpcService{
									{
										TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
											EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{
												ClusterName: config.SpireAgentClusterName,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	return transportSocket(downstreamTlsContext)
}

func transportSocket(tlsContext proto.Message) *corev3.TransportSocket {
	return &corev3.TransportSocket{
		Name: tlsTransportSocketName,
		ConfigType: &corev3.TransportSocket_TypedConfig{
			TypedConfig: config.TypedConfig(tlsContext),
		},
	}
}
