package proxy

import (
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	defaultLocalClusterUpstreamBindConfigAddress = "127.0.0.1"
)

func NewClusterForService(serviceName string) *clusterv3.Cluster {
	protocolOptions := config.Http2ProtocolOptions()

	return &clusterv3.Cluster{
		Name: serviceName,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			config.UpstreamHTTPProtocolOptionsKey: config.TypedConfig(protocolOptions),
		},
	}
}

func NewLocalClusterForService(serviceName string, endpoint *registryv1.ServiceEndpoint) *clusterv3.Cluster {
	protocolOptions := config.Http2ProtocolOptions()

	return &clusterv3.Cluster{
		Name: serviceName,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			config.UpstreamHTTPProtocolOptionsKey: config.TypedConfig(protocolOptions),
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
