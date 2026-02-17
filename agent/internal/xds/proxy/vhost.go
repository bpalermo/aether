package proxy

import (
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
)

func NewServiceVirtualHost(svcName string) *routev3.VirtualHost {
	return &routev3.VirtualHost{
		Name:    svcName,
		Domains: []string{svcName},
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
							Cluster: svcName,
						},
					},
				},
			},
		},
	}
}
