// Package proxy provides functions to generate Envoy resource types
// (listeners, clusters, endpoints, routes, filter chains) from pod and
// service registry data. It encapsulates the details of building Envoy
// configurations for transparent traffic interception and service discovery.
package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// protocolOptionsForConfig returns the typed extension protocol options map
// entry for the given upstream protocol setting.
func protocolOptionsForConfig(cc *ClusterConfig) map[string]*anypb.Any {
	var opts = config.Http2ProtocolOptions()
	if cc.UpstreamProtocol == UpstreamProtocolHTTP1 {
		opts = config.Http1ProtocolOptions()
	}
	return map[string]*anypb.Any{
		config.UpstreamHTTPProtocolOptionsKey: config.TypedConfig(opts),
	}
}

// codecClientTypeForConfig returns the Envoy codec client type matching the
// configured upstream protocol.
func codecClientTypeForConfig(cc *ClusterConfig) typev3.CodecClientType {
	if cc.UpstreamProtocol == UpstreamProtocolHTTP1 {
		return typev3.CodecClientType_HTTP1
	}
	return typev3.CodecClientType_HTTP2
}

// NewClusterForService creates an Envoy cluster for a service.
// The cluster uses EDS for dynamic endpoint discovery via ADS.
// The upstream HTTP protocol is determined by the ClusterConfig.
func NewClusterForService(serviceName string, cc *ClusterConfig) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name: serviceName,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
		},
		TypedExtensionProtocolOptions: protocolOptionsForConfig(cc),
	}
}

// NewLocalClusterForService creates an Envoy cluster for a local service endpoint.
// The cluster binds to the configured local address and uses the target pod's network namespace.
// Health check thresholds, interval, timeout, and the upstream HTTP protocol are all
// configurable via ClusterConfig.
func NewLocalClusterForService(serviceName string, endpoint *registryv1.ServiceEndpoint, cc *ClusterConfig) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name: serviceName,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
		},
		TypedExtensionProtocolOptions: protocolOptionsForConfig(cc),
		UpstreamBindConfig: &corev3.BindConfig{
			SourceAddress: &corev3.SocketAddress{
				Address: cc.LocalClusterBindAddress,
				PortSpecifier: &corev3.SocketAddress_PortValue{
					PortValue: 0,
				},
				NetworkNamespaceFilepath: endpoint.GetContainerMetadata().GetNetworkNamespace(),
			},
		},
		HealthChecks: []*corev3.HealthCheck{
			{
				HealthyThreshold:   wrapperspb.UInt32(cc.HealthCheckHealthyThreshold),
				UnhealthyThreshold: wrapperspb.UInt32(cc.HealthCheckUnhealthyThreshold),
				Interval:           durationpb.New(cc.HealthCheckInterval),
				Timeout:            durationpb.New(cc.HealthCheckTimeout),
				HealthChecker: &corev3.HealthCheck_HttpHealthCheck_{
					HttpHealthCheck: &corev3.HealthCheck_HttpHealthCheck{
						Host:            endpoint.GetKubernetesMetadata().GetPodName(),
						Path:            "/",
						CodecClientType: codecClientTypeForConfig(cc),
					},
				},
			},
		},
	}
}
