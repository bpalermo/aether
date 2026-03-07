// Package proxy provides functions to generate Envoy resource types
// (listeners, clusters, endpoints, routes, filter chains) from pod and
// service registry data. It encapsulates the details of building Envoy
// configurations for transparent traffic interception and service discovery.
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
	// defaultLocalClusterUpstreamBindConfigAddress is the default bind address for local cluster upstreams
	defaultLocalClusterUpstreamBindConfigAddress = "127.0.0.1"
)

// NewClusterForService creates an Envoy cluster for a service.
// The cluster uses EDS for dynamic endpoint discovery via ADS.
// Upstream connections use HTTP/2 protocol.
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

// NewLocalClusterForService creates an Envoy cluster for a local service endpoint.
// The cluster binds to 127.0.0.1 and uses the target pod's network namespace.
// It includes health checks and uses HTTP/2 for upstream connections.
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
				Timeout:            durationpb.New(1 * time.Second),
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
