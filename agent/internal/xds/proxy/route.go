package proxy

import (
	"fmt"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
)

const (
	// OutboundHTTPRouteName is the name of the outbound HTTP route configuration
	OutboundHTTPRouteName = "out_http"
)

// buildInboundRouteConfiguration creates a route configuration for inbound traffic.
// It includes a catch-all virtual host that returns 404 for all requests.
func buildInboundRouteConfiguration() *routev3.RouteConfiguration {
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
						Action: &routev3.Route_DirectResponse{
							DirectResponse: &routev3.DirectResponseAction{
								Status: 404,
								Body: &corev3.DataSource{
									Specifier: &corev3.DataSource_InlineString{
										InlineString: "Ops! Probably something wrong. You shouldn´t reach here.\n",
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
