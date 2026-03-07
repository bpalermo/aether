package config

import (
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	// SpireAgentClusterName is the Envoy cluster name for the local SPIRE agent
	// used to retrieve SVIDs for mTLS.
	SpireAgentClusterName = "spire_sds"
	// spireAgentSocketFile is the path to the SPIRE agent's Unix domain socket
	// for secret discovery service requests.
	spireAgentSocketFile = "/run/secrets/workload-spiffe-uds/socket"
)

// NewLocalSpireCluster creates an Envoy cluster that connects to the local SPIRE agent
// via Unix domain socket for retrieving SVIDs. The cluster uses EDS for dynamic endpoint
// discovery and HTTP/2 for protocol communication.
func NewLocalSpireCluster() *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:           SpireAgentClusterName,
		ConnectTimeout: durationpb.New(250 * time.Millisecond),
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

// NewLocalSpireClusterLoadAssignment creates an endpoint assignment for the SPIRE agent cluster,
// pointing to the Unix domain socket where the SPIRE agent listens.
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
