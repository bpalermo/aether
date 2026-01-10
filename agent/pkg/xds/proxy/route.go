package proxy

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
)

const (
	OutboundHTTPRouteName = "out_http"
)

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

func BuildOutboundRouteConfiguration(vhosts []*routev3.VirtualHost) *routev3.RouteConfiguration {
	result := make([]*routev3.VirtualHost, len(vhosts)+1)
	result = append(vhosts, vhosts...)
	result = append(vhosts, &routev3.VirtualHost{
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
	})

	return &routev3.RouteConfiguration{
		Name:         OutboundHTTPRouteName,
		VirtualHosts: result,
	}
}

func BuildOutboundClusterVirtualHost(clusterName string) *routev3.VirtualHost {
	return &routev3.VirtualHost{
		Name:    clusterName,
		Domains: []string{clusterName},
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
