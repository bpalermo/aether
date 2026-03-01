package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	transport_sockets_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	tlsTransportSocketName = "envoy.transport_sockets.tls"
)

func DownstreamTransportSocket(tlsCertificateSecretName string, validationContextName string) *corev3.TransportSocket {
	downstreamTlsContext := &transport_sockets_v3.DownstreamTlsContext{
		RequireClientCertificate: wrapperspb.Bool(true),
		CommonTlsContext: &transport_sockets_v3.CommonTlsContext{
			TlsCertificateSdsSecretConfigs: []*transport_sockets_v3.SdsSecretConfig{
				{
					Name: tlsCertificateSecretName,
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
			ValidationContextType: &transport_sockets_v3.CommonTlsContext_ValidationContextSdsSecretConfig{
				ValidationContextSdsSecretConfig: &transport_sockets_v3.SdsSecretConfig{
					Name: validationContextName,
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

func UpstreamTransportSocket(validationContextName string) *corev3.TransportSocket {
	upstreamTlsContext := &transport_sockets_v3.DownstreamTlsContext{
		RequireClientCertificate: wrapperspb.Bool(true),
		CommonTlsContext: &transport_sockets_v3.CommonTlsContext{
			ValidationContextType: &transport_sockets_v3.CommonTlsContext_ValidationContextSdsSecretConfig{
				ValidationContextSdsSecretConfig: &transport_sockets_v3.SdsSecretConfig{
					Name: validationContextName,
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

	return transportSocket(upstreamTlsContext)
}

func transportSocket(msg proto.Message) *corev3.TransportSocket {
	return &corev3.TransportSocket{
		Name: tlsTransportSocketName,
		ConfigType: &corev3.TransportSocket_TypedConfig{
			TypedConfig: config.TypedConfig(msg),
		},
	}
}
