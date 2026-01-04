package proxy

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
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

func buildOutboundRouteConfiguration() *routev3.RouteConfiguration {
	return &routev3.RouteConfiguration{
		Name: "out_http",
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
