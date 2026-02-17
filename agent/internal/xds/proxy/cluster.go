package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	upstreamHTTPProtocolOptionsKey = "envoy.extensions.upstreams.http.v3.HttpProtocolOptions"
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
