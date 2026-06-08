// Package proxy provides functions to generate Envoy resource types
// (listeners, clusters, endpoints, routes, filter chains) from pod and
// service registry data. It encapsulates the details of building Envoy
// configurations for transparent traffic interception and service discovery.
package proxy

import (
	"fmt"
	"strconv"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/constants"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// defaultLocalClusterUpstreamBindConfigAddress is the default bind address for local cluster upstreams
	defaultLocalClusterUpstreamBindConfigAddress = "127.0.0.1"
	// appClusterPrefix marks the per-pod cluster that forwards decrypted inbound
	// traffic to the pod's own application on loopback.
	appClusterPrefix = "app_"
	// appLoopbackAddress is the loopback address the application listens on inside
	// the pod's network namespace.
	appLoopbackAddress = "127.0.0.1"
	// defaultAppHealthPath is the readiness path the app cluster health-checks when
	// the pod does not specify one.
	defaultAppHealthPath = "/"
)

// AppHealthPathFromPod returns the HTTP path used to health-check the pod's
// application, taken from the endpoint.aether.io/health-path annotation, defaulting
// to "/" when unset.
func AppHealthPathFromPod(cniPod *cniv1.CNIPod) string {
	if p, ok := cniPod.GetAnnotations()[constants.AnnotationEndpointHealthPath]; ok && p != "" {
		return p
	}
	return defaultAppHealthPath
}

// AppClusterName returns the name of the per-pod application cluster that the
// pod's inbound listener forwards decrypted traffic to. It is unique per pod so
// each inbound listener routes to its own application on loopback.
func AppClusterName(cniPod *cniv1.CNIPod) string {
	return fmt.Sprintf("%s%s", appClusterPrefix, cniPod.GetName())
}

// AppPortFromPod returns the port the pod's application listens on, taken from
// the endpoint.aether.io/port annotation, defaulting to the standard endpoint
// port when the annotation is absent or invalid.
func AppPortFromPod(cniPod *cniv1.CNIPod) uint16 {
	s, ok := cniPod.GetAnnotations()[constants.AnnotationEndpointPort]
	if !ok {
		return constants.DefaultEndpointPort
	}
	port, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return constants.DefaultEndpointPort
	}
	return uint16(port)
}

// NewAppCluster builds a STATIC cluster that forwards decrypted inbound traffic
// to the pod's own application at 127.0.0.1:<port>. The upstream connection is
// bound into the pod's network namespace so the loopback address reaches the
// application container, not the agent. This is the only cleartext hop in the
// mesh: it is intra-pod (Envoy -> app on loopback), never pod-to-pod. The app
// is assumed to speak HTTP/1.1, so no explicit HTTP/2 protocol options are set.
func NewAppCluster(name, netns string, port uint16, healthPath string) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name: name,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_STATIC,
		},
		// Active health check of the pod's application (delegated liveness): the
		// node-local agent scrapes this cluster's host health from the proxy admin
		// and reflects it into the registry so the endpoint is marked unhealthy in
		// every client's EDS while the app is not serving.
		HealthChecks: []*corev3.HealthCheck{
			{
				Timeout:            durationpb.New(1 * time.Second),
				Interval:           durationpb.New(5 * time.Second),
				HealthyThreshold:   wrapperspb.UInt32(1),
				UnhealthyThreshold: wrapperspb.UInt32(2),
				HealthChecker: &corev3.HealthCheck_HttpHealthCheck_{
					HttpHealthCheck: &corev3.HealthCheck_HttpHealthCheck{
						Path:            healthPath,
						CodecClientType: typev3.CodecClientType_HTTP1,
					},
				},
			},
		},
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{
				{
					LbEndpoints: []*endpointv3.LbEndpoint{
						{
							HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
								Endpoint: &endpointv3.Endpoint{
									Address: &corev3.Address{
										Address: &corev3.Address_SocketAddress{
											SocketAddress: &corev3.SocketAddress{
												Protocol: corev3.SocketAddress_TCP,
												Address:  appLoopbackAddress,
												PortSpecifier: &corev3.SocketAddress_PortValue{
													PortValue: uint32(port),
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
		},
		UpstreamBindConfig: &corev3.BindConfig{
			SourceAddress: &corev3.SocketAddress{
				Address: appLoopbackAddress,
				PortSpecifier: &corev3.SocketAddress_PortValue{
					PortValue: 0,
				},
				NetworkNamespaceFilepath: netns,
			},
		},
	}
}

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
