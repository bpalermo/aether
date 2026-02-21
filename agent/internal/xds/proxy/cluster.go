package proxy

import (
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	upstreamHTTPProtocolOptionsKey = "envoy.extensions.upstreams.http.v3.HttpProtocolOptions"

	defaultLocalClusterUpstreamBindConfigAddress = "127.0.0.1"
)

func NewClusterForService(serviceName string) *clusterv3.Cluster {
	protocolOptions := http2ProtocolOptions()

	return &clusterv3.Cluster{
		Name: serviceName,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			upstreamHTTPProtocolOptionsKey: config.TypedConfig(protocolOptions),
		},
	}
}

func NewLocalClusterForService(serviceName string, endpoint *registryv1.ServiceEndpoint) *clusterv3.Cluster {
	protocolOptions := http2ProtocolOptions()

	return &clusterv3.Cluster{
		Name: serviceName,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			upstreamHTTPProtocolOptionsKey: config.TypedConfig(protocolOptions),
		},
		UpstreamBindConfig: &corev3.BindConfig{
			SourceAddress: &corev3.SocketAddress{
				Address: defaultLocalClusterUpstreamBindConfigAddress,
				PortSpecifier: &corev3.SocketAddress_PortValue{
					PortValue: 0,
				},
				NetworkNamespaceFilepath: endpoint.GetContainerMetadata().GetNetworkNamespace(),
			},
		},
		HealthChecks: []*corev3.HealthCheck{
			{
				HealthyThreshold:   wrapperspb.UInt32(1),
				UnhealthyThreshold: wrapperspb.UInt32(1),
				Interval:           durationpb.New(5 * time.Second),
				HealthChecker: &corev3.HealthCheck_HttpHealthCheck_{
					HttpHealthCheck: &corev3.HealthCheck_HttpHealthCheck{
						Host:            endpoint.GetKubernetesMetadata().GetPodName(),
						Path:            "/",
						CodecClientType: typev3.CodecClientType_HTTP2,
					},
				},
			},
		},
	}
}

func http1ProtocolOptions() *httpv3.HttpProtocolOptions {
	return &httpv3.HttpProtocolOptions{
		UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{
			ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
				ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_HttpProtocolOptions{
					HttpProtocolOptions: &corev3.Http1ProtocolOptions{},
				},
			},
		},
	}
}

func http2ProtocolOptions() *httpv3.HttpProtocolOptions {
	return &httpv3.HttpProtocolOptions{
		UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{
			ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
				ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{
					Http2ProtocolOptions: &corev3.Http2ProtocolOptions{},
				},
			},
		},
	}
}
