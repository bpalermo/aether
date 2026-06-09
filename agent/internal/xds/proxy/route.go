package proxy

import (
	"fmt"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// OutboundHTTPRouteName is the name of the outbound HTTP route configuration
	OutboundHTTPRouteName = "out_http"
)

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
						// Swap :authority for the selected endpoint's hostname (the dest
						// pod IP) so the destination node demuxes the request to that pod.
						HostRewriteSpecifier: &routev3.RouteAction_AutoHostRewrite{
							AutoHostRewrite: wrapperspb.Bool(true),
						},
					},
				},
			},
		},
	}
}
