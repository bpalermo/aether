package proxy

import (
	"fmt"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
)

const (
	// OutboundHTTPRouteName is the name of the outbound HTTP route configuration
	OutboundHTTPRouteName = "out_http"
)

// buildInboundRouteConfiguration creates a route configuration for inbound traffic.
// It routes all requests to the per-pod application cluster, which forwards the
// decrypted traffic to the pod's own application on loopback.
func buildInboundRouteConfiguration(appClusterName string) *routev3.RouteConfiguration {
	return &routev3.RouteConfiguration{
		Name: "in_http",
		VirtualHosts: []*routev3.VirtualHost{
			{
				Name:    "catch_all",
				Domains: []string{"*"},
				Routes: []*routev3.Route{
					{
						Match: &routev3.RouteMatch{
							PathSpecifier: &routev3.RouteMatch_Prefix{
								Prefix: "/",
							},
						},
						Action: &routev3.Route_Route{
							Route: &routev3.RouteAction{
								ClusterSpecifier: &routev3.RouteAction_Cluster{
									Cluster: appClusterName,
								},
							},
						},
					},
				},
			},
		},
	}
}

// BuildOutboundRouteConfiguration creates a route configuration for outbound traffic.
// It includes the provided virtual hosts along with a catch-all virtual host that returns 404.
// Each virtual host matches requests by cluster name and FQDN.
func BuildOutboundRouteConfiguration(vhosts []*routev3.VirtualHost) *routev3.RouteConfiguration {
	return &routev3.RouteConfiguration{
		Name:         OutboundHTTPRouteName,
		VirtualHosts: vhosts,
	}
}

// BuildOutboundClusterVirtualHost creates a virtual host that routes traffic to a specific cluster.
// The virtual host matches both the cluster name and its FQDN (cluster.aether.internal).
func BuildOutboundClusterVirtualHost(clusterName string) *routev3.VirtualHost {
	fqdn := fmt.Sprintf("%s.%s", clusterName, "aether.internal")
	return &routev3.VirtualHost{
		Name:    clusterName,
		Domains: []string{clusterName, fqdn},
		Routes: []*routev3.Route{
			{
				Match: &routev3.RouteMatch{
					PathSpecifier: &routev3.RouteMatch_Prefix{
						Prefix: "/",
					},
				},
				Action: &routev3.Route_Route{
					Route: &routev3.RouteAction{
						ClusterSpecifier: &routev3.RouteAction_Cluster{
							Cluster: clusterName,
						},
					},
				},
			},
		},
	}
}
