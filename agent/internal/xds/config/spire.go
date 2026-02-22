package config

import (
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	SpireAgentClusterName = "spire_sds"
	spireAgentSocketFile  = "/run/secrets/workload-spiffe-uds/socket/spire-agent.sock"
)

func NewLocalSpireCluster() *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name: SpireAgentClusterName,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: XDSConfigSourceADS(),
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			UpstreamHTTPProtocolOptionsKey: TypedConfig(Http2ProtocolOptions()),
		},
	}
}

func NewLocalSpireClusterLoadAssignment() *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: SpireAgentClusterName,
		Endpoints: []*endpointv3.LocalityLbEndpoints{
			{
				LbEndpoints: []*endpointv3.LbEndpoint{
					{
						HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
							Endpoint: &endpointv3.Endpoint{
								Address: &corev3.Address{
									Address: &corev3.Address_Pipe{
										Pipe: &corev3.Pipe{
											Path: spireAgentSocketFile,
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
}
